package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type IndexerClient struct {
	url    string
	client *http.Client

	// Per-client circuit breaker. Every request path goes through query(), so
	// putting it here protects single-network pages, merged views and the sync
	// loop alike — an indexer that is down (or a testnet configured before it
	// launches) fails fast instead of making each caller wait out the timeout.
	mu        sync.Mutex
	failures  int
	skipUntil time.Time
}

// errIndexerUnavailable is returned while the breaker is open.
var errIndexerUnavailable = errors.New("indexer unavailable, not retried yet")

// errQueryTooLarge reports that a query's result set hit the indexer's element
// cap. It describes the query, not the indexer's health.
var errQueryTooLarge = errors.New("indexer result set hit the element cap")

// indexerElementCap is the tx-indexer's server-side limit on records per query.
// On reaching it the resolver stops iterating and returns the rows it has
// alongside a GraphQL error, so a capped response is partial rather than empty.
// There is no limit or offset argument to ask for the next page with
// (getTransactions takes only `where` and `order`), which leaves the block-height
// filter as the only way to move through a result set larger than the cap.
const indexerElementCap = 10000

const (
	clientBreakerThreshold = 2
	clientBreakerCooldown  = 30 * time.Second
)

// breakerOpen reports whether requests are currently being short-circuited.
func (c *IndexerClient) breakerOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.skipUntil)
}

// recordResult updates the breaker after a request.
func (c *IndexerClient) recordResult(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.failures, c.skipUntil = 0, time.Time{}
		return
	}
	c.failures++
	if c.failures >= clientBreakerThreshold {
		c.skipUntil = time.Now().Add(clientBreakerCooldown)
	}
}

func NewIndexerClient(url string) *IndexerClient {
	// Normalize URL: ensure it ends with /query
	url = strings.TrimRight(url, "/")
	if strings.HasSuffix(url, "/graphql") {
		url += "/query"
	}
	return &IndexerClient{
		url: url,
		client: &http.Client{
			// A page cannot wait 30s on one indexer; callers add their own
			// tighter deadlines on top of this backstop.
			Timeout: 10 * time.Second,
		},
	}
}

// gqlEscape sanitizes a string for safe use inside GraphQL string literals.
func gqlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

func (c *IndexerClient) query(ctx context.Context, query string, vars map[string]any, result any) error {
	if c.breakerOpen() {
		return errIndexerUnavailable
	}
	err := c.doQuery(ctx, query, vars, result)
	// Caller-side cancellation and a capped result set both say nothing about the
	// indexer's health, so neither counts against the breaker.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, errQueryTooLarge) {
		c.recordResult(err)
	}
	return err
}

func (c *IndexerClient) doQuery(ctx context.Context, query string, vars map[string]any, result any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var gqlResp gqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, string(respBody[:min(200, len(respBody))]))
	}

	if len(gqlResp.Errors) > 0 {
		for _, e := range gqlResp.Errors {
			if !strings.Contains(e.Message, "max elements per query") {
				continue
			}
			// The resolver stops iterating at the cap and returns the rows it
			// already has alongside this error, so decode the partial page before
			// reporting it. Only this error carries usable data — for anything
			// else `data` is meaningless and must not reach the caller.
			if err := json.Unmarshal(gqlResp.Data, result); err != nil {
				return fmt.Errorf("decode capped response: %w", err)
			}
			return fmt.Errorf("%w: %s", errQueryTooLarge, e.Message)
		}
		return fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	return json.Unmarshal(gqlResp.Data, result)
}

// LatestBlockHeight returns the latest indexed block height.
func (c *IndexerClient) LatestBlockHeight(ctx context.Context) (int, error) {
	var result struct {
		LatestBlockHeight int `json:"latestBlockHeight"`
	}
	err := c.query(ctx, `{ latestBlockHeight }`, nil, &result)
	return result.LatestBlockHeight, err
}

type Transaction struct {
	Index       int         `json:"index"`
	Hash        string      `json:"hash"`
	Success     bool        `json:"success"`
	BlockHeight int         `json:"block_height"`
	GasWanted   int         `json:"gas_wanted"`
	GasUsed     int         `json:"gas_used"`
	GasFee      *Coin       `json:"gas_fee"`
	Memo        string      `json:"memo"`
	Messages    []TxMessage `json:"messages"`
	Response    *TxResponse `json:"response"`
	ContentRaw  string      `json:"content_raw,omitempty"`
	Network     string      `json:"network,omitempty"`
	BlockTime   string      `json:"block_time,omitempty"`
	ChainID     string      `json:"chain_id,omitempty"`
}

type Coin struct {
	Amount int    `json:"amount"`
	Denom  string `json:"denom"`
}

type TxMessage struct {
	TypeURL string       `json:"typeUrl"`
	Route   string       `json:"route"`
	Value   MessageValue `json:"value"`
}

type MessageValue struct {
	Typename string `json:"__typename"`

	// MsgAddPackage
	Creator string      `json:"creator,omitempty"`
	Package *MemPackage `json:"package,omitempty"`

	// MsgCall
	Caller  string   `json:"caller,omitempty"`
	PkgPath string   `json:"pkg_path,omitempty"`
	Func    string   `json:"func,omitempty"`
	Args    []string `json:"args,omitempty"`

	// BankMsgSend
	FromAddress string `json:"from_address,omitempty"`
	ToAddress   string `json:"to_address,omitempty"`
	Amount      string `json:"amount,omitempty"`

	// Common
	Send       string `json:"send,omitempty"`
	MaxDeposit string `json:"max_deposit,omitempty"`
}

type MemPackage struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Files []MemFile `json:"files"`
}

type MemFile struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type TxResponse struct {
	Log    string    `json:"log"`
	Info   string    `json:"info"`
	Error  string    `json:"error"`
	Data   string    `json:"data"`
	Events []TxEvent `json:"events"`
}

type TxEvent struct {
	Typename   string      `json:"__typename"`
	Type       string      `json:"type,omitempty"`
	PkgPath    string      `json:"pkg_path,omitempty"`
	Attrs      []EventAttr `json:"attrs,omitempty"`
	BytesDelta int         `json:"bytes_delta,omitempty"`
	FeeDelta   *Coin       `json:"fee_delta,omitempty"`
	FeeRefund  *Coin       `json:"fee_refund,omitempty"`
}

type EventAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Light fields for list views — no file bodies
const txFieldsLight = `
	hash
	success
	block_height
	gas_wanted
	gas_used
	gas_fee { amount denom }
	memo
	messages {
		typeUrl
		route
		value {
			__typename
			... on MsgAddPackage {
				creator
				package { name path files { name } }
				send
			}
			... on MsgCall {
				caller
				send
				pkg_path
				func
				args
			}
			... on MsgRun {
				caller
				send
				package { name path files { name } }
			}
			... on BankMsgSend {
				from_address
				to_address
				amount
			}
		}
	}
	response {
		log
		info
		error
		data
		events {
			__typename
			... on GnoEvent {
				type
				pkg_path
				attrs { key value }
			}
			... on StorageDepositEvent {
				type
				bytes_delta
				fee_delta { amount denom }
				pkg_path
			}
			... on StorageUnlockEvent {
				type
				bytes_delta
				fee_refund { amount denom }
				pkg_path
			}
		}
	}
`

// Full fields including file bodies — for single tx detail and sync
const txFields = `
	hash
	success
	block_height
	gas_wanted
	gas_used
	gas_fee { amount denom }
	content_raw
	memo
	messages {
		typeUrl
		route
		value {
			__typename
			... on MsgAddPackage {
				creator
				package { name path files { name body } }
				send
			}
			... on MsgCall {
				caller
				send
				pkg_path
				func
				args
			}
			... on MsgRun {
				caller
				send
				package { name path files { name body } }
			}
			... on BankMsgSend {
				from_address
				to_address
				amount
			}
		}
	}
	response {
		log
		info
		error
		data
		events {
			__typename
			... on GnoEvent {
				type
				pkg_path
				attrs { key value }
			}
			... on StorageDepositEvent {
				type
				bytes_delta
				fee_delta { amount denom }
				pkg_path
			}
			... on StorageUnlockEvent {
				type
				bytes_delta
				fee_refund { amount denom }
				pkg_path
			}
		}
	}
`

// transactionsFromHeight fetches one page of transactions above lastHeight (from
// genesis when nil), oldest first. truncated reports that the indexer stopped at
// its element cap and more rows remain; resume by passing the block height of the
// last returned row.
//
// The cap is the page size. The resolver returns the rows it has alongside its
// error, so there is nothing to size or guess at: ask for everything above the
// cursor and take what comes back.
//
// ASC ordering is load-bearing, not cosmetic. Truncation keeps the *first* rows
// the resolver iterated, so ascending gives the contiguous next page directly
// above the cursor. Descending would hand back the newest rows instead — and
// since the sync cursors are derived from the highest stored block height,
// storing those would jump the cursor to the tip and orphan everything between,
// silently and permanently.
func (c *IndexerClient) transactionsFromHeight(
	ctx context.Context,
	lastHeight *int,
	extraWhere, fields string,
) (txs []Transaction, truncated bool, err error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}

	heightWhere := ""
	if lastHeight != nil {
		heightWhere = fmt.Sprintf(`block_height: { gt: %d }`, *lastHeight)
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { %s %s }
			order: { heightAndIndex: ASC }
		) { %s }
	}`, heightWhere, extraWhere, fields)

	err = c.query(ctx, q, nil, &result)
	if err != nil && !errors.Is(err, errQueryTooLarge) {
		return nil, false, err
	}
	if err == nil {
		return result.GetTransactions, false, nil
	}

	// Truncated. The cap can fall inside a block, and the caller resumes with an
	// exclusive `gt` on the last row's height, which would skip the rest of that
	// block — so give back only the heights known to be complete.
	txs = dropTrailingHeight(result.GetTransactions)
	if len(txs) == 0 {
		return nil, false, fmt.Errorf(
			"a single block holds more than %d matching transactions: %w", indexerElementCap, err)
	}
	return txs, true, nil
}

// dropTrailingHeight removes the rows sharing the highest block height in an
// ascending page, which is the one truncation may have cut in half.
func dropTrailingHeight(txs []Transaction) []Transaction {
	if len(txs) == 0 {
		return txs
	}
	last := txs[len(txs)-1].BlockHeight
	end := len(txs)
	for end > 0 && txs[end-1].BlockHeight == last {
		end--
	}
	return txs[:end]
}

// GetAllPackages fetches a page of MsgAddPackage transactions above lastHeight.
// See transactionsFromHeight for the paging contract.
func (c *IndexerClient) GetAllPackages(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight,
		`messages: { value: { MsgAddPackage: {} } }`, txFields)
}

// GetRecentTransactions fetches the most recent transactions, limited to maxResults.
func (c *IndexerClient) GetRecentTransactions(ctx context.Context, maxResults int) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {}
			order: { heightAndIndex: DESC }
		) { %s }
	}`, txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	if err != nil {
		return nil, err
	}
	if maxResults > 0 && len(result.GetTransactions) > maxResults {
		return result.GetTransactions[:maxResults], nil
	}
	return result.GetTransactions, err
}

// Window sizes for paged transaction fetches. The indexer has no limit argument,
// so the only way to bound a query is by block height; cost tracks the number of
// rows returned, not the width of the window, so widening is cheap.
const (
	initialTxWindow = 20000
	txWindowGrowth  = 8
)

// GetRecentTransactionsPage fetches at least `need` of the most recent
// transactions by querying a bounded height window, widening it until enough are
// found or the chain start is reached.
//
// The unbounded alternative (`where: {}`) downloads every transaction the chain
// has ever had — 14MB and 4.5s on a modest testnet — because the indexer exposes
// no limit or pagination argument. Bounding by height is the only lever there is.
func (c *IndexerClient) GetRecentTransactionsPage(ctx context.Context, need int) ([]Transaction, error) {
	return c.recentTransactionsWindowed(ctx, need, "", func() ([]Transaction, error) {
		return c.GetRecentTransactions(ctx, 0)
	})
}

// recentTransactionsWindowed is the shared widening-window fetch. extraWhere is
// an optional additional filter fragment merged into the where clause; unbounded
// falls back to the caller's own full-fetch.
func (c *IndexerClient) recentTransactionsWindowed(
	ctx context.Context,
	need int,
	extraWhere string,
	unbounded func() ([]Transaction, error),
) ([]Transaction, error) {
	if need <= 0 {
		return unbounded()
	}
	tip, err := c.LatestBlockHeight(ctx)
	if err != nil {
		return nil, err
	}

	for window := initialTxWindow; ; window *= txWindowGrowth {
		from := tip - window
		if from < 0 {
			from = 0
		}

		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { block_height: { gt: %d } %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, from, extraWhere, txFieldsLight)
		if err := c.query(ctx, q, nil, &result); err != nil {
			return nil, err
		}

		// Enough rows, or we have already reached genesis and there are no more.
		if len(result.GetTransactions) >= need || from == 0 {
			return result.GetTransactions, nil
		}
	}
}

// GetTransactionsFromHeight fetches a page of transactions above lastHeight.
// See transactionsFromHeight for the paging contract.
func (c *IndexerClient) GetTransactionsFromHeight(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight, "", txFieldsLight)
}

// GetTransactionsByPkgPath fetches MsgCall transactions for a specific package.
func (c *IndexerClient) GetTransactionsByPkgPath(ctx context.Context, pkgPath string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { messages: { value: { MsgCall: { pkg_path: { eq: "%s" } } } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, gqlEscape(pkgPath), txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetTransactionByHash fetches a single transaction by hash.
func (c *IndexerClient) GetTransactionByHash(ctx context.Context, hash string) (*Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { hash: { eq: "%s" } }
		) { %s }
	}`, gqlEscape(hash), txFields)
	err := c.query(ctx, q, nil, &result)
	if err != nil {
		return nil, err
	}
	if len(result.GetTransactions) == 0 {
		return nil, fmt.Errorf("transaction not found: %s", hash)
	}
	return &result.GetTransactions[0], nil
}

// GetTransactionsByAddress fetches transactions involving an address.
func (c *IndexerClient) GetTransactionsByAddress(ctx context.Context, addr string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {
				_or: [
					{ messages: { value: { MsgCall: { caller: { eq: "%s" } } } } }
					{ messages: { value: { MsgAddPackage: { creator: { eq: "%s" } } } } }
					{ messages: { value: { MsgRun: { caller: { eq: "%s" } } } } }
					{ messages: { value: { BankMsgSend: { from_address: { eq: "%s" } } } } }
					{ messages: { value: { BankMsgSend: { to_address: { eq: "%s" } } } } }
				]
			}
			order: { heightAndIndex: DESC }
		) { %s }
	}`, gqlEscape(addr), gqlEscape(addr), gqlEscape(addr), gqlEscape(addr), gqlEscape(addr), txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	// Cap at 200 most recent to avoid huge responses
	if len(result.GetTransactions) > 200 {
		return result.GetTransactions[:200], err
	}
	return result.GetTransactions, err
}

// GetMsgRunTransactions fetches a page of MsgRun transactions above lastHeight.
// See transactionsFromHeight for the paging contract.
func (c *IndexerClient) GetMsgRunTransactions(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight,
		`messages: { value: { MsgRun: {} } }`, txFields)
}

type Block struct {
	Hash               string `json:"hash"`
	Height             int    `json:"height"`
	ChainID            string `json:"chain_id"`
	Time               string `json:"time"`
	NumTxs             int    `json:"num_txs"`
	TotalTxs           int    `json:"total_txs"`
	ProposerAddressRaw string `json:"proposer_address_raw"`
}

const blockFields = `
	hash
	height
	chain_id
	time
	num_txs
	total_txs
	proposer_address_raw
`

// GetRecentBlocks fetches recent blocks by querying a height range from the tip.
func (c *IndexerClient) GetRecentBlocks(ctx context.Context, limit int) ([]Block, error) {
	if limit <= 0 {
		limit = 50
	}
	// Get latest height first
	latest, err := c.LatestBlockHeight(ctx)
	if err != nil {
		return nil, err
	}
	fromHeight := max(latest-limit, 0)

	var result struct {
		GetBlocks []Block `json:"getBlocks"`
	}
	q := fmt.Sprintf(`{
		getBlocks(
			where: { height: { gt: %d } }
			order: { height: DESC }
		) { %s }
	}`, fromHeight, blockFields)
	err = c.query(ctx, q, nil, &result)
	return result.GetBlocks, err
}

// GetBlocksInRange fetches all blocks between fromHeight and toHeight inclusive.
func (c *IndexerClient) GetBlocksInRange(ctx context.Context, fromHeight, toHeight int) ([]Block, error) {
	var result struct {
		GetBlocks []Block `json:"getBlocks"`
	}
	q := fmt.Sprintf(`{
		getBlocks(
			where: { height: { gt: %d, lt: %d } }
			order: { height: ASC }
		) { %s }
	}`, fromHeight-1, toHeight+1, blockFields)
	err := c.query(ctx, q, nil, &result)
	return result.GetBlocks, err
}

// rangeStampDensity decides between one range query and one query per height.
//
// A range query returns every block in the span, not just the wanted ones, so it
// only pays off when the heights are dense. Measured against a live indexer: 20
// transactions spread over 20k blocks cost 4.5s and 579KB as a range (10k blocks
// returned) versus 0.3s fetched individually and concurrently. Densities near 1
// invert that — consecutive blocks are one cheap query instead of N.
const rangeStampDensity = 2

// GetBlockTimesForHeights returns a height→time map for the given heights.
//
// One range query when the heights are dense, one query per height when they are
// scattered. Getting this backwards is expensive in both directions.
func (c *IndexerClient) GetBlockTimesForHeights(ctx context.Context, heights []int) (map[int]string, error) {
	if len(heights) == 0 {
		return nil, nil
	}
	lo, hi := heights[0], heights[0]
	for _, h := range heights {
		if h < lo {
			lo = h
		}
		if h > hi {
			hi = h
		}
	}
	if span := hi - lo + 1; span > rangeStampDensity*len(heights) {
		return c.GetBlocksByHeights(ctx, heights)
	}

	blocks, err := c.GetBlocksInRange(ctx, lo, hi)
	if err != nil {
		// A failed range query is not fatal: the per-height path still works.
		return c.GetBlocksByHeights(ctx, heights)
	}
	times := make(map[int]string, len(blocks))
	for _, b := range blocks {
		times[b.Height] = b.Time
	}
	return times, nil
}

// GetBlocksByHeights fetches blocks for a specific set of heights and returns a height→time map.
// Fetches each block individually with up to 10 concurrent requests.
func (c *IndexerClient) GetBlocksByHeights(ctx context.Context, heights []int) (map[int]string, error) {
	if len(heights) == 0 {
		return nil, nil
	}
	m := make(map[int]string, len(heights))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for _, h := range heights {
		wg.Add(1)
		go func(height int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			block, err := c.GetBlock(ctx, height)
			if err == nil && block != nil {
				mu.Lock()
				m[height] = block.Time
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	return m, nil
}

// GetBlock fetches a single block by height. A nil, nil return means the
// query succeeded but the indexer has nothing at that height — genesis not
// yet reached from the other side, or a pruned/nonexistent height. Callers
// that need to tell "confirmed absent" apart from "could not ask" (network
// error, indexer down) rely on this distinction; do not collapse it back into
// a single error.
func (c *IndexerClient) GetBlock(ctx context.Context, height int) (*Block, error) {
	var result struct {
		GetBlocks []Block `json:"getBlocks"`
	}
	q := fmt.Sprintf(`{
		getBlocks(
			where: { height: { eq: %d } }
		) { %s }
	}`, height, blockFields)
	err := c.query(ctx, q, nil, &result)
	if err != nil {
		return nil, err
	}
	if len(result.GetBlocks) == 0 {
		return nil, nil
	}
	return &result.GetBlocks[0], nil
}

// GetTransactionsByRealm fetches calls to a specific realm function.
func (c *IndexerClient) GetTransactionsByRealmFunc(ctx context.Context, pkgPath, funcName string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { messages: { value: { MsgCall: { pkg_path: { eq: "%s" }, func: { eq: "%s" } } } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, gqlEscape(pkgPath), gqlEscape(funcName), txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetTransactionsByBlock fetches transactions in a specific block.
func (c *IndexerClient) GetTransactionsByBlock(ctx context.Context, height int) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { block_height: { eq: %d } }
			order: { heightAndIndex: ASC }
		) { %s }
	}`, height, txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetStorageEvents fetches storage deposit/unlock events for a package.
func (c *IndexerClient) GetStorageEvents(ctx context.Context, pkgPath string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {
				response: {
					events: {
						_or: [
							{ StorageDepositEvent: { pkg_path: { eq: "%s" } } }
							{ StorageUnlockEvent: { pkg_path: { eq: "%s" } } }
						]
					}
				}
			}
			order: { heightAndIndex: DESC }
		) {
			hash block_height gas_used gas_wanted gas_fee { amount denom } success
			response {
				events {
					__typename
					... on StorageDepositEvent { type bytes_delta fee_delta { amount denom } pkg_path }
					... on StorageUnlockEvent { type bytes_delta fee_refund { amount denom } pkg_path }
				}
			}
		}
	}`, gqlEscape(pkgPath), gqlEscape(pkgPath))
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetGasUsageForRealm fetches all txs interacting with a realm for gas stats.
func (c *IndexerClient) GetGasUsageForRealm(ctx context.Context, pkgPath string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {
				_or: [
					{ messages: { value: { MsgCall: { pkg_path: { eq: "%s" } } } } }
					{ messages: { value: { MsgAddPackage: { package: { path: { eq: "%s" } } } } } }
				]
			}
			order: { heightAndIndex: DESC }
		) {
			hash block_height gas_used gas_wanted gas_fee { amount denom } success
			messages { value { __typename ... on MsgCall { func } } }
		}
	}`, gqlEscape(pkgPath), gqlEscape(pkgPath))
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetRecentTransactionsWithEvents fetches recent transactions that have GnoEvents.
func (c *IndexerClient) GetRecentTransactionsWithEvents(ctx context.Context, need int) ([]Transaction, error) {
	return c.recentTransactionsWindowed(ctx, need,
		"response: { events: { GnoEvent: {} } }",
		func() ([]Transaction, error) {
			var result struct {
				GetTransactions []Transaction `json:"getTransactions"`
			}
			q := fmt.Sprintf(`{
		getTransactions(
			where: { response: { events: { GnoEvent: {} } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, txFieldsLight)
			err := c.query(ctx, q, nil, &result)
			return result.GetTransactions, err
		})
}

// GetEventsByPkgPath fetches transactions that emitted GnoEvents for a package.
func (c *IndexerClient) GetEventsByPkgPath(ctx context.Context, pkgPath string) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { response: { events: { GnoEvent: { pkg_path: { eq: "%s" } } } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, gqlEscape(pkgPath), txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetGovDAOTransactions fetches transactions involving govdao realms.
func (c *IndexerClient) GetGovDAOTransactions(ctx context.Context) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { messages: { value: { MsgCall: { pkg_path: { like: "%%govdao%%"} } } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}
