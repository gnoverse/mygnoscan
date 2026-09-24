package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	// urls are interchangeable endpoints for the same chain, in the order the
	// operator listed them. active indexes the one in use.
	urls   []string
	client *http.Client

	// Per-client circuit breaker. Every request path goes through query(), so
	// putting it here protects single-network pages, merged views and the sync
	// loop alike — an indexer that is down (or a testnet configured before it
	// launches) fails fast instead of making each caller wait out the timeout.
	mu        sync.Mutex
	failures  int
	skipUntil time.Time
	active    int
	lastProbe time.Time
	// fingerprint of the chain this pool is serving, learned from the first
	// endpoint that answers. Members that disagree are never selected.
	fingerprint string

	// typeSupport caches, per GraphQL type name, whether this chain's indexer
	// defines it. A type is absent from the map until the first query asks.
	typeSupport map[string]bool
}

// ErrUnavailable is returned while the breaker is open.
var ErrUnavailable = errors.New("indexer unavailable, not retried yet")

// ErrQueryTooLarge reports that a query's result set hit the indexer's element
// cap. It describes the query, not the indexer's health.
var ErrQueryTooLarge = errors.New("indexer result set hit the element cap")

// ErrNotFound reports that the indexer answered and the answer was "no such
// record". Like the element cap, it describes the question rather than the
// indexer, and must never be mistaken for a network being down.
//
// A hash lives on exactly one chain, so every unfiltered lookup produces a
// not-found from every *other* configured network. Counting those as failures
// meant three transaction views in a row tripped the breaker on all of them,
// and for the next minute every merged page — transactions, blocks, events —
// was served from the one chain that happened to hold those hashes.
var ErrNotFound = errors.New("not found")

// indexerNotFoundMessage is how the tx-indexer words a miss. It arrives as a
// GraphQL error rather than an empty result set, so it has to be recognised by
// text; there is no code to switch on.
const indexerNotFoundMessage = "item not found in storage"

// ElementCap is the tx-indexer's server-side limit on records per query.
// On reaching it the resolver stops iterating and returns the rows it has
// alongside a GraphQL error, so a capped response is partial rather than empty.
// There is no limit or offset argument to ask for the next page with
// (getTransactions takes only `where` and `order`), which leaves the block-height
// filter as the only way to move through a result set larger than the cap.
const ElementCap = 10000

const (
	clientBreakerThreshold = 2
	clientBreakerCooldown  = 30 * time.Second
)

// breakerOpen reports whether requests are currently being short-circuited.
func (c *Client) breakerOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.skipUntil)
}

// recordResult updates the breaker after a request.
func (c *Client) recordResult(err error) {
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

// Request budgets. Serving a page and running a background sync have nothing in
// common: one has a browser waiting on it, the other is catching up on history
// and is measured against the size of the chain.
//
// Sharing one budget meant the sync inherited the page's. A cold sync of
// sapphire asks for every MsgAddPackage with full file bodies — 32s and 12MB for
// 368 rows — which cannot fit in ten seconds, so SyncAll failed on its first
// step and retried the identical request every 30s forever.
const (
	serveClientTimeout = 10 * time.Second
	syncClientTimeout  = 2 * time.Minute
)

// NewClient returns a client for request paths that have a caller
// waiting. Callers add their own tighter deadlines on top of this backstop.
func NewClient(urls ...string) *Client {
	return newIndexerClient(urls, serveClientTimeout)
}

// NewSyncClient returns a client for the background sync loop.
//
// Deliberately a separate client, not just a longer timeout: the breaker is
// per-client, so a sync struggling against a slow indexer no longer opens the
// breaker that page queries share, and vice versa.
func NewSyncClient(urls ...string) *Client {
	return newIndexerClient(urls, syncClientTimeout)
}

func newIndexerClient(urls []string, timeout time.Duration) *Client {
	normalized := make([]string, 0, len(urls))
	for _, url := range urls {
		// Normalize URL: ensure it ends with /query
		url = strings.TrimRight(url, "/")
		if strings.HasSuffix(url, "/graphql") {
			url += "/query"
		}
		if url != "" {
			normalized = append(normalized, url)
		}
	}
	return &Client{
		urls:   normalized,
		client: &http.Client{Timeout: timeout, Transport: indexerTransport},
	}
}

// indexerTransport is the connection pool every indexer client shares.
//
// Leaving Transport nil falls back to http.DefaultTransport, whose
// MaxIdleConnsPerHost is 2. GetBlocksByHeights fans out ten concurrent
// requests at one endpoint, so eight of every ten were opening a fresh TCP
// and TLS connection to a host the process had just finished talking to.
// The pool is shared across the serve and sync clients on purpose: they hold
// separate timeouts and separate circuit breakers, which is what has to stay
// separate, but they talk to the same hosts.
var indexerTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ForceAttemptHTTP2:     true,
}

// activeURL is the endpoint currently in use, or "" when none was configured.
// ActiveURL is the endpoint this pool is currently selecting, and Endpoints is
// every endpoint it may select from.
//
// Exported for the sanity page. A pool of two where one member answers nothing
// looks identical from outside to a pool of two that are both healthy, which is
// how gno.land's mainnet indexer rejected every query for more than a day
// behind a working fallback without anything saying so.
func (c *Client) ActiveURL() string { return c.activeURL() }

// Endpoints lists the pool's members in configured order.
func (c *Client) Endpoints() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.urls))
	copy(out, c.urls)
	return out
}

func (c *Client) activeURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.urls) == 0 {
		return ""
	}
	return c.urls[c.active]
}

// The message types only newer tx-indexers define.
//
// They are selected inside the shared transaction field set, so an indexer that
// does not know them rejects *every* transaction query with a
// GRAPHQL_VALIDATION_FAILED rather than just the package one — which is how a
// single unsupported type took a whole chain's sync offline:
//
//	sync packages: indexer returned 422: Unknown type "MsgEnablePackage"
//
// pearl's indexer predates them. The rows it had already synced stayed, so the
// breakage was invisible until a schema migration dropped those tables and the
// resync could not refill them.
//
// They are two groups, not one, because they shipped to the indexer separately
// and a chain can have either without the other. gno.land's own mainnet
// indexer defines MsgEnablePackage and does not define MsgCreateSession, so
// one probe standing for both is not a simplification, it is a wrong answer:
// asking about the type the chain has and then selecting the type it does not
// fails every query while the probe reports support.
const inertFragments = `
			... on MsgEnablePackage {
				approver
				pkg_path
				pkg_hash
				pkg_height
			}
			... on MsgRejectPackage {
				sender
				pkg_path
			}`

const sessionFragments = `
			... on MsgCreateSession {
				creator
				session_key
				expires_at
				allow_paths
				spend_limit
				spend_period
			}
			... on MsgRevokeSession {
				creator
				session_key
			}
			... on MsgRevokeAllSessions {
				creator
			}`

// transferFragments is the native-coin movement group.
//
// Gated like the other two, and the gate is not theoretical: probed
// 2026-09-23, `indexer.gno.land` defines TransferEvent and
// `indexer.pearl.testnets.gno.land` answers `__type: null` for it. Selecting it
// unconditionally would therefore 422 **every** transaction query on pearl and
// take that chain's whole sync down, not just the one view that wants coins.
// The same gap already shows on the deployed site: `/api/realm/defi?network=pearl`
// answers a raw `Field "TransferEvent" is not defined by type "NestedFilterEvent"`,
// because CoinFlows filters on it with no probe in front.
const transferFragments = `
			... on TransferEvent {
				from
				to
				coins
			}`

// The type each fragment group is gated on: one representative per group, and
// the rest of the group shipped to the indexer in the same release as it.
const (
	inertProbeType    = "MsgEnablePackage"
	sessionProbeType  = "MsgCreateSession"
	transferProbeType = "TransferEvent"
)

// SupportsTransferEvents reports whether this chain's indexer can answer
// anything about native coin movement at all.
//
// Exported because the difference between "this realm never moved a coin" and
// "this chain cannot be asked" is a fact a reader needs, and the handler cannot
// tell them apart from an empty result.
func (c *Client) SupportsTransferEvents(ctx context.Context) bool {
	return c.supportsType(ctx, transferProbeType)
}

// supportsType reports whether this chain's indexer defines a GraphQL type,
// asking it once per type and remembering the answer.
//
// Asked rather than inferred from an error, so the first query of a sync pass
// does not have to fail to find out. A probe that cannot reach the indexer
// returns true: assuming support keeps behaviour identical to before this
// existed, and the query that follows will fail for the real reason rather than
// being silently trimmed because a health check blipped.
func (c *Client) supportsType(ctx context.Context, typeName string) bool {
	c.mu.Lock()
	known, seen := c.typeSupport[typeName]
	c.mu.Unlock()
	if seen {
		return known
	}

	// Never probe through an open breaker. The probe bypasses query() to avoid
	// recursing into field selection, which also means it bypasses the breaker
	// — so without this it is the one request that still goes out on a chain
	// already declared unreachable.
	if c.breakerOpen() {
		return true
	}

	var result struct {
		Type *struct {
			Name string `json:"name"`
		} `json:"__type"`
	}
	err := c.doQuery(ctx, c.activeURL(), `{ __type(name: "`+typeName+`") { name } }`, nil, &result)
	supported := err != nil || result.Type != nil

	c.mu.Lock()
	if err == nil {
		if c.typeSupport == nil {
			c.typeSupport = map[string]bool{}
		}
		c.typeSupport[typeName] = supported
	}
	c.mu.Unlock()
	return supported
}

// lightFields, bodyFields and fullFields are the transaction selection sets,
// trimmed to what this indexer actually understands.
func (c *Client) lightFields(ctx context.Context) string {
	return c.trimFields(ctx, txFieldsLight)
}

// bodyFields is the set for readers that want package sources and nothing more:
// the file bodies, without content_raw. Named for what it carries rather than
// for the sync, because the sync is no longer its only caller.
func (c *Client) bodyFields(ctx context.Context) string {
	return c.trimFields(ctx, txFieldsBodies)
}

func (c *Client) fullFields(ctx context.Context) string {
	return c.trimFields(ctx, txFields)
}

// trimFields drops each optional fragment group this indexer cannot parse.
//
// Independently, because the groups are independent: a chain that knows the
// inert lifecycle but not sessions keeps the first and loses only the second,
// rather than losing both or, as before, keeping both and failing every query.
func (c *Client) trimFields(ctx context.Context, fields string) string {
	if !c.supportsType(ctx, inertProbeType) {
		fields = strings.ReplaceAll(fields, inertFragments, "")
	}
	if !c.supportsType(ctx, sessionProbeType) {
		fields = strings.ReplaceAll(fields, sessionFragments, "")
	}
	if !c.supportsType(ctx, transferProbeType) {
		fields = strings.ReplaceAll(fields, transferFragments, "")
	}
	return fields
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

// endpointProbeInterval is how often a multi-endpoint pool re-picks. Short
// enough that a stalled indexer is abandoned within minutes, long enough that
// the probe traffic is negligible next to ordinary queries.
const endpointProbeInterval = 2 * time.Minute

// probeTimeout bounds a single endpoint probe. A probe is a health check, so a
// slow answer is itself a reason to prefer someone else.
const probeTimeout = 10 * time.Second

// endpointState is what a probe learns about one endpoint.
type endpointState struct {
	index       int
	tip         int
	fingerprint string
	err         error
}

// probe asks one endpoint for its tip and the identity of the chain it serves.
//
// Both in one request: the tip alone cannot distinguish "further along" from
// "a different chain entirely", and picking the highest tip across two chains
// would silently splice them together.
func (c *Client) probe(ctx context.Context, index int, url string) endpointState {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var result struct {
		LatestBlockHeight int `json:"latestBlockHeight"`
		GetBlocks         []struct {
			ChainID string `json:"chain_id"`
			Hash    string `json:"hash"`
		} `json:"getBlocks"`
	}
	q := `{
		latestBlockHeight
		getBlocks(where: { height: { eq: 1 } }) { chain_id hash }
	}`
	if err := c.doQuery(ctx, url, q, nil, &result); err != nil {
		return endpointState{index: index, err: err}
	}
	st := endpointState{index: index, tip: result.LatestBlockHeight}
	// Same identity the reset check uses: chain ID alone is not enough, because
	// a reset network keeps its ID and comes back with a different block 1.
	if len(result.GetBlocks) > 0 && result.GetBlocks[0].Hash != "" {
		st.fingerprint = result.GetBlocks[0].ChainID + ":" + result.GetBlocks[0].Hash
	}
	return st
}

// selectEndpoint picks the endpoint furthest along the chain we are already
// following, and is the whole reason this pool is ordered by freshness rather
// than by "first one that answers".
//
// The failure that motivated it did not look like a failure: gno.land mainnet's
// indexer sat at block 785 while the chain was at 36,000, answering every query
// promptly and correctly for the 785 blocks it knew about. A pool that fails
// over only on error would have stayed on it forever, and the explorer would
// have gone on reporting a stalled chain as a healthy one.
//
// Endpoints whose fingerprint disagrees with the pool's are never selected. A
// fast, healthy, wrong chain is the worst member a pool can have.
func (c *Client) selectEndpoint(ctx context.Context) {
	c.mu.Lock()
	if len(c.urls) < 2 || time.Since(c.lastProbe) < endpointProbeInterval {
		c.mu.Unlock()
		return
	}
	c.lastProbe = time.Now()
	urls := append([]string(nil), c.urls...)
	known := c.fingerprint
	c.mu.Unlock()

	states := make([]endpointState, len(urls))
	var wg sync.WaitGroup
	for i, url := range urls {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			states[i] = c.probe(ctx, i, url)
		}(i, url)
	}
	wg.Wait()

	// Adopt an identity on the first probe from the earliest endpoint in
	// configured order that can prove one — never from whichever endpoint is
	// furthest along.
	//
	// Deriving it from the highest tip lets the wrong chain define the pool by
	// being bigger, which is precisely backwards: a busy unrelated chain would
	// win and the operator's own endpoint would then be excluded as the
	// impostor. The first entry is the operator's stated intent; the rest are
	// alternates that have to match it. Caught by
	// TestPoolRefusesAnEndpointOnAnotherChain, which passed a 90,000-block
	// `gnoland1` off as `gnoland-1`.
	//
	// Afterwards the identity is fixed: a member that comes back as a different
	// chain is excluded rather than allowed to redefine the pool.
	if known == "" {
		for _, st := range states {
			if st.err == nil && st.fingerprint != "" {
				known = st.fingerprint
				break
			}
		}
	}

	chosen, chosenTip := -1, -1
	for _, st := range states {
		if st.err != nil {
			continue
		}
		// An endpoint that cannot prove its chain is still usable when nobody
		// can — a chain too young to have a block 1 is a real state — but it
		// loses to any endpoint that agrees with the pool's identity.
		if known != "" && st.fingerprint != "" && st.fingerprint != known {
			continue
		}
		if st.tip > chosenTip {
			chosen, chosenTip = st.index, st.tip
		}
	}
	if chosen < 0 {
		return
	}

	c.mu.Lock()
	if known != "" {
		c.fingerprint = known
	}
	c.active = chosen
	c.mu.Unlock()
}

// failOver moves to the next endpoint and reports whether there was one. It is
// the error-driven half of the pool: selectEndpoint handles the endpoint that
// is wrong without being broken, this handles the one that is simply down.
func (c *Client) failOver() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.urls) < 2 {
		return "", false
	}
	c.active = (c.active + 1) % len(c.urls)
	// The next query re-picks rather than sitting on whoever happened to be
	// next in line.
	c.lastProbe = time.Time{}
	return c.urls[c.active], true
}

// answered reports whether an error came from the indexer rather than from
// failing to reach it. A chain that says "no such transaction" has answered,
// and asking a second endpoint the same question just wastes a round-trip.
func answered(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrQueryTooLarge) ||
		errors.Is(err, context.Canceled) ||
		strings.HasPrefix(err.Error(), "graphql error:")
}

func (c *Client) query(ctx context.Context, query string, vars map[string]any, result any) error {
	if c.breakerOpen() {
		return ErrUnavailable
	}
	c.selectEndpoint(ctx)
	err := c.doQuery(ctx, c.activeURL(), query, vars, result)
	if err != nil && !answered(err) {
		if next, ok := c.failOver(); ok {
			err = c.doQuery(ctx, next, query, vars, result)
		}
	}
	// Caller-side cancellation, a capped result set and a miss all say nothing
	// about the indexer's health, so none of them counts against the breaker.
	// A deadline deliberately does: that is the caller reporting the indexer was
	// too slow, which is the signal the breaker exists to act on.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrQueryTooLarge) && !errors.Is(err, ErrNotFound) {
		c.recordResult(err)
	}
	return err
}

func (c *Client) doQuery(ctx context.Context, url, query string, vars map[string]any, result any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
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

	// Report a refused request as what it is. A rate-limited indexer answers with
	// an HTML error page, which otherwise surfaces as "invalid character '<'" and
	// sends the reader looking for a bug in the query rather than at the status
	// code. Sync catch-up is exactly the traffic that earns a 403.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("indexer returned %s: %s", resp.Status, string(respBody[:min(200, len(respBody))]))
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
			return fmt.Errorf("%w: %s", ErrQueryTooLarge, e.Message)
		}
		if strings.Contains(gqlResp.Errors[0].Message, indexerNotFoundMessage) {
			return fmt.Errorf("%w: %s", ErrNotFound, gqlResp.Errors[0].Message)
		}
		return fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	return json.Unmarshal(gqlResp.Data, result)
}

// LatestBlockHeight returns the latest indexed block height.
func (c *Client) LatestBlockHeight(ctx context.Context) (int, error) {
	var result struct {
		LatestBlockHeight int `json:"latestBlockHeight"`
	}
	err := c.query(ctx, `{ latestBlockHeight }`, nil, &result)
	return result.LatestBlockHeight, err
}

// Supply is the chain's money, which is also its storage capacity.
//
// Every byte of realm state locks StoragePrice ugnot, so total/price is the
// hard ceiling on how much the chain can ever hold. The monorepo does the same
// sum in a comment: "1.333B GNOT == 13.33TB"
// (gno.land/pkg/sdk/vm/params.go, storagePriceDefault).
//
// The amounts are strings because they do not fit a JSON number safely: total
// is 1.333e15 ugnot, past 2^53 once a testnet mints more, and the indexer
// serves them as strings for that reason.
type Supply struct {
	Denom     string `json:"denom"`
	Height    int    `json:"height"`
	Total     string `json:"total"`
	Locked    string `json:"locked"`
	Spendable string `json:"spendable"`
}

// GetSupply reads the total, locked and spendable amounts for a denom.
func (c *Client) GetSupply(ctx context.Context, denom string) (*Supply, error) {
	var result struct {
		GetSupply *Supply `json:"getSupply"`
	}
	err := c.query(ctx,
		`query($denom: String!) { getSupply(denom: $denom) { denom height total locked spendable } }`,
		map[string]any{"denom": denom}, &result)
	if err != nil {
		return nil, err
	}
	if result.GetSupply == nil {
		return nil, fmt.Errorf("indexer returned no supply for %q", denom)
	}
	return result.GetSupply, nil
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

	// MsgEnablePackage
	Approver  string `json:"approver,omitempty"`
	PkgHash   string `json:"pkg_hash,omitempty"`
	PkgHeight int    `json:"pkg_height,omitempty"`

	// MsgRejectPackage
	Sender string `json:"sender,omitempty"`

	// MsgCreateSession / MsgRevokeSession / MsgRevokeAllSessions.
	//
	// SessionKey is the account a session delegates signing to: transactions it
	// signs still name the creator as their caller, so this is what links a
	// session-signed transaction back to the grant that authorised it.
	SessionKey  string   `json:"session_key,omitempty"`
	ExpiresAt   int64    `json:"expires_at,omitempty"`
	AllowPaths  []string `json:"allow_paths,omitempty"`
	SpendLimit  string   `json:"spend_limit,omitempty"`
	SpendPeriod int      `json:"spend_period,omitempty"`

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

	// TransferEvent. The chain emits one on every bank transfer, a realm's own
	// banker moves included, which is what makes a balance derivable at all.
	// The shared templates carry the fragment now, gated on transferProbeType,
	// so these are populated on every fetch from a chain whose indexer defines
	// the type and zero on one that does not. The syncer writes them to
	// coin_transfers from the pass it already runs.
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Coins string `json:"coins,omitempty"`
}

type EventAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// txFieldsTemplate is the single source for the transaction selection sets.
// They differ only in whether they carry package file bodies and content_raw,
// and a second literal of ninety-odd lines would drift from this one. trimFields
// also strips the optional fragment groups by exact substring, which keeps
// working only while every set is cut from the same template.
const txFieldsTemplate = `
	hash
	success
	block_height
	gas_wanted
	gas_used
	gas_fee { amount denom }
%[1]s	memo
	messages {
		typeUrl
		route
		value {
			__typename
			... on MsgAddPackage {
				creator
				package { name path files { name%[2]s } }
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
				package { name path files { name%[2]s } }
			}
			... on BankMsgSend {
				from_address
				to_address
				amount
			}
			... on MsgEnablePackage {
				approver
				pkg_path
				pkg_hash
				pkg_height
			}
			... on MsgRejectPackage {
				sender
				pkg_path
			}
			... on MsgCreateSession {
				creator
				session_key
				expires_at
				allow_paths
				spend_limit
				spend_period
			}
			... on MsgRevokeSession {
				creator
				session_key
			}
			... on MsgRevokeAllSessions {
				creator
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
			... on TransferEvent {
				from
				to
				coins
			}
		}
	}
`

// txSelection renders the template. fileBodies adds the package sources the
// sync stores; contentRaw adds the encoded signed transaction, which carries
// those same sources a second time and is read only by SignerAddress.
func txSelection(fileBodies, contentRaw bool) string {
	raw := ""
	if contentRaw {
		raw = "\tcontent_raw\n"
	}

	body := ""
	if fileBodies {
		body = " body"
	}

	return fmt.Sprintf(txFieldsTemplate, raw, body)
}

var (
	// txFieldsLight drops file bodies, for list views
	txFieldsLight = txSelection(false, false)
	// txFieldsBodies carries package file bodies and not content_raw, which
	// would only repeat them: the sync and the govdao provenance view
	txFieldsBodies = txSelection(true, false)
	// txFields adds content_raw, for the single transaction detail path, the
	// only place SignerAddress has anything to derive from
	txFields = txSelection(true, true)
)

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
func (c *Client) transactionsFromHeight(
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
	if err != nil && !errors.Is(err, ErrQueryTooLarge) {
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
			"a single block holds more than %d matching transactions: %w", ElementCap, err)
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
func (c *Client) GetAllPackages(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight,
		`messages: { value: { MsgAddPackage: {} } }`, c.bodyFields(ctx))
}

// GetRecentTransactions fetches the most recent transactions, limited to maxResults.
func (c *Client) GetRecentTransactions(ctx context.Context, maxResults int) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {}
			order: { heightAndIndex: DESC }
		) { %s }
	}`, c.lightFields(ctx))
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
// so the only way to bound a query is by block height.
//
// Start small and widen. The two errors are not symmetric: overshooting costs a
// payload that grows with the chain's density and is unbounded in practice —
// a 20,000-block window on sapphire returns 45MB to serve a 20-row page, which
// does not fit in the client's timeout — while undershooting costs one more
// round trip against a chain sparse enough that round trips are cheap. gno.land
// returns nothing at all for its most recent 20,000 blocks and has to widen
// regardless, so a large starting window buys nothing there either.
const (
	initialTxWindow = 100
	txWindowGrowth  = 8
)

// GetRecentTransactionsPage fetches at least `need` of the most recent
// transactions by querying a bounded height window, widening it until enough are
// found or the chain start is reached.
//
// The unbounded alternative (`where: {}`) downloads every transaction the chain
// has ever had — 14MB and 4.5s on a modest testnet — because the indexer exposes
// no limit or pagination argument. Bounding by height is the only lever there is.
func (c *Client) GetRecentTransactionsPage(ctx context.Context, need int) ([]Transaction, error) {
	return c.GetRecentTransactionsFiltered(ctx, need, "", "")
}

// messageTypeFilter maps a message type name to its where-clause fragment.
//
// Filtering at the indexer rather than over a fetched page is what makes the
// filter honest: a page-local filter describes the window it happens to have
// loaded, not the chain, so "3 deploys" means "3 in the last 500 rows".
func messageTypeFilter(msgType string) string {
	switch msgType {
	case "MsgCall", "MsgAddPackage", "MsgRun", "BankMsgSend":
		return fmt.Sprintf("messages: { value: { %s: {} } }", msgType)
	}
	return ""
}

// GetRecentTransactionsFiltered fetches recent transactions, optionally narrowed
// to one message type and/or success state.
//
// A narrowed query needs a wider height window to find the same number of rows —
// deploys are rare next to calls — which the widening window already handles: it
// keeps doubling until it has enough or reaches genesis.
func (c *Client) GetRecentTransactionsFiltered(ctx context.Context, need int, msgType, success string) ([]Transaction, error) {
	where := messageTypeFilter(msgType)
	if success == "true" || success == "false" {
		if where != "" {
			where += " "
		}
		where += "success: { eq: " + success + " }"
	}

	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		return c.GetRecentTransactions(ctx, 0)
	})
}

// recentTransactionsWindowed is the shared widening-window fetch. extraWhere is
// an optional additional filter fragment merged into the where clause; unbounded
// falls back to the caller's own full-fetch.
func (c *Client) recentTransactionsWindowed(
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
			// Clamped, not left negative: the loop below exits on `from == 0`,
			// meaning "the whole chain has been scanned". Letting it run negative
			// makes that condition unreachable and the widening never stops.
			from = 0
		}

		// `gt` excludes the bound, so a window reaching the bottom of the chain
		// still misses block 0 — and genesis transactions live there.
		//
		// On a freshly launched chain that is everything: gno.land mainnet went
		// live with 89 curated packages deployed at genesis, all at height 0, and
		// the transactions page showed nothing while the stats row counted 96.
		//
		// The bottom window therefore carries no lower bound at all. `gte` would
		// read better but FilterInt does not have it — it offers only exists, eq,
		// gt and lt — and `gt: -1` is not an escape hatch either: the indexer
		// answers a negative bound with a null result set rather than everything.
		// Above the bottom the exclusive bound stays, so successive windows do
		// not re-read their own edge.
		heightFilter := fmt.Sprintf("block_height: { gt: %d }", from)
		if from == 0 {
			heightFilter = ""
		}

		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, heightFilter, extraWhere, c.lightFields(ctx))
		if err := c.query(ctx, q, nil, &result); err != nil {
			// A capped result set is not a failure here, it is the answer.
			//
			// This query is DESC, so the rows the resolver kept before it stopped
			// are the newest ones — exactly what a "recent" view is asking for.
			// Widening the window would only reach further past a cap already hit.
			//
			// Mirror of the sync path: transactionsFromHeight walks ASC from a
			// cursor, where truncation hides older rows and must be paged through.
			// Here the walk is DESC from the tip, so truncation hides only rows the
			// caller did not want.
			if errors.Is(err, ErrQueryTooLarge) && len(result.GetTransactions) >= need {
				return result.GetTransactions, nil
			}
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
func (c *Client) GetTransactionsFromHeight(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight, "", c.lightFields(ctx))
}

// GetTransactionByHash fetches a single transaction by hash.
func (c *Client) GetTransactionByHash(ctx context.Context, hash string) (*Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { hash: { eq: "%s" } }
		) { %s }
	}`, gqlEscape(hash), c.fullFields(ctx))
	err := c.query(ctx, q, nil, &result)
	if err != nil {
		return nil, err
	}
	if len(result.GetTransactions) == 0 {
		return nil, fmt.Errorf("transaction %w: %s", ErrNotFound, hash)
	}
	return &result.GetTransactions[0], nil
}

// addressTxLimit bounds an address page. The view shows recent activity, not an
// account's whole history, and the indexer has no way to make the latter cheap.
const addressTxLimit = 200

// GetTransactionsByAddress fetches recent transactions involving an address.
//
// Windowed from the tip like every other "recent" query. Unbounded, this asked
// the indexer to scan the whole chain for five address predicates at once: on
// sapphire it took longer than the ten-second client deadline, so /api/address
// answered 500 for the busiest accounts — and the timeout counted against the
// per-network breaker, taking that chain out of every merged view for a minute.
// One address page could degrade the whole site.
func (c *Client) GetTransactionsByAddress(ctx context.Context, addr string) ([]Transaction, error) {
	e := gqlEscape(addr)
	involves := fmt.Sprintf(`_or: [
					{ messages: { value: { MsgCall: { caller: { eq: "%s" } } } } }
					{ messages: { value: { MsgAddPackage: { creator: { eq: "%s" } } } } }
					{ messages: { value: { MsgRun: { caller: { eq: "%s" } } } } }
					{ messages: { value: { BankMsgSend: { from_address: { eq: "%s" } } } } }
					{ messages: { value: { BankMsgSend: { to_address: { eq: "%s" } } } } }
				]`, e, e, e, e, e)

	txs, err := c.recentTransactionsWindowed(ctx, addressTxLimit, involves,
		func() ([]Transaction, error) {
			var result struct {
				GetTransactions []Transaction `json:"getTransactions"`
			}
			q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, involves, c.lightFields(ctx))
			err := c.query(ctx, q, nil, &result)
			return result.GetTransactions, err
		})
	if err != nil {
		return nil, err
	}
	if len(txs) > addressTxLimit {
		return txs[:addressTxLimit], nil
	}
	return txs, nil
}

// GetMsgRunTransactions fetches a page of MsgRun transactions above lastHeight.
// See transactionsFromHeight for the paging contract.
func (c *Client) GetMsgRunTransactions(ctx context.Context, lastHeight *int) ([]Transaction, bool, error) {
	return c.transactionsFromHeight(ctx, lastHeight,
		`messages: { value: { MsgRun: {} } }`, c.bodyFields(ctx))
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
func (c *Client) GetRecentBlocks(ctx context.Context, limit int) ([]Block, error) {
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
func (c *Client) GetBlocksInRange(ctx context.Context, fromHeight, toHeight int) ([]Block, error) {
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
func (c *Client) GetBlockTimesForHeights(ctx context.Context, heights []int) (map[int]string, error) {
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
func (c *Client) GetBlocksByHeights(ctx context.Context, heights []int) (map[int]string, error) {
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
func (c *Client) GetBlock(ctx context.Context, height int) (*Block, error) {
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

// GetTransactionsByBlock fetches transactions in a specific block.
func (c *Client) GetTransactionsByBlock(ctx context.Context, height int) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { block_height: { eq: %d } }
			order: { heightAndIndex: ASC }
		) { %s }
	}`, height, c.lightFields(ctx))
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetStorageEvents fetches storage deposit/unlock events for a package.
func (c *Client) GetStorageEvents(ctx context.Context, pkgPath string) ([]Transaction, error) {
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
func (c *Client) GetGasUsageForRealm(ctx context.Context, pkgPath string) ([]Transaction, error) {
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
func (c *Client) GetRecentTransactionsWithEvents(ctx context.Context, need int) ([]Transaction, error) {
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
	}`, c.lightFields(ctx))
			err := c.query(ctx, q, nil, &result)
			return result.GetTransactions, err
		})
}

// GetEventsByPkgPath fetches transactions that emitted GnoEvents for a package.
func (c *Client) GetEventsByPkgPath(ctx context.Context, pkgPath string, need int) ([]Transaction, error) {
	where := fmt.Sprintf(`response: { events: { GnoEvent: { pkg_path: { eq: "%s" } } } }`, gqlEscape(pkgPath))
	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, where, c.lightFields(ctx))
		err := c.query(ctx, q, nil, &result)
		return result.GetTransactions, err
	})
}

// GetGovDAOTransactions fetches transactions involving govdao realms.
func (c *Client) GetGovDAOTransactions(ctx context.Context, need int) ([]Transaction, error) {
	// The indexer's `like` is a plain substring match: `%` is matched literally,
	// not as a wildcard. Probed against a live indexer on a realm with known
	// calls:
	//
	//	like: "blog"                       -> 16 rows
	//	like: "gno.land/r/gnoland/blog"    -> 16 rows
	//	like: "%blog%"                     ->  0 rows
	//	like: "gno.land/r/gnoland/blog%"   ->  0 rows
	//
	// So the previous pattern, "%govdao%", was unmatchable twice over: no path
	// contains a literal percent sign, and the realm is gno.land/r/gov/dao —
	// with a slash — so even "govdao" would have found nothing. This view has
	// returned an empty list since it was written.
	//
	// "gov/dao" is specific enough: gnoswap ships gov/staker and gov/governance,
	// which a bare "gov" would also pick up, and it keeps the versioned
	// subpackages (gov/dao/v3/impl and friends).
	const where = `messages: { value: { MsgCall: { pkg_path: { like: "gov/dao"} } } }`
	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, where, c.lightFields(ctx))
		err := c.query(ctx, q, nil, &result)
		return result.GetTransactions, err
	})
}

// GovDAORealm is the realm whose proposal lifecycle the governance views are
// about. Duplicated from store.GovDAOPathPrefix rather than imported: the
// indexer package sits below store and must not depend on it.
const GovDAORealm = "gno.land/r/gov/dao"

// GetGovDAOProposalCreations fetches every transaction that emitted gov/dao's
// own ProposalCreated event, with the message bodies but not content_raw: the
// rows are MsgRun transactions carrying a whole script, and content_raw would
// ship each of those scripts a second time for a field this view never reads.
//
// This is the only exact link between a proposal ID and the transaction that
// created it, and it is exact because gov/dao emits the ID as an event
// attribute. Everything the explorer had before was a guess: MsgRun carries
// its script in the message rather than in indexed arguments, so "which
// script created proposal 6" was a substring match over locally synced
// source text, which matched every script that ever mentioned both gov/dao
// and the executor package — six candidates on mainnet for proposal 6, five
// of them belonging to other proposals, one of them 124,770 blocks older
// than the proposal it was offered for.
//
// Unwindowed on purpose, unlike every other list query here. One
// ProposalCreated event exists per proposal ever created, so the result set
// is bounded by governance activity (eight rows on mainnet, 2026-09-19)
// rather than by chain length — and proposal #0 sits at block 36,170, so a
// windowed scan would have to widen to genesis to find it anyway.
func (c *Client) GetGovDAOProposalCreations(ctx context.Context) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { response: { events: { GnoEvent: { pkg_path: { eq: "%s" }, type: { eq: "ProposalCreated" } } } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, gqlEscape(GovDAORealm), c.bodyFields(ctx))
	if err := c.query(ctx, q, nil, &result); err != nil {
		return nil, err
	}
	return result.GetTransactions, nil
}

// GetPackageEnableTransactions fetches every MsgEnablePackage this chain has
// seen — the approval half of the "inert" code-submission policy's package
// lifecycle (see inert.go). Unlike gov/dao's old substring predicate, these
// fields are typed and marked @filterable in the indexer's own schema, so
// this is an ordinary indexed lookup, not a scan.
func (c *Client) GetPackageEnableTransactions(ctx context.Context, need int) ([]Transaction, error) {
	const where = `messages: { value: { MsgEnablePackage: {} } }`
	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, where, c.lightFields(ctx))
		err := c.query(ctx, q, nil, &result)
		return result.GetTransactions, err
	})
}

// GetPackageRejectTransactions fetches every MsgRejectPackage this chain has
// seen — a parked package an approver, its own creator, or a live package's
// owner explicitly dropped rather than left to expire.
func (c *Client) GetPackageRejectTransactions(ctx context.Context, need int) ([]Transaction, error) {
	const where = `messages: { value: { MsgRejectPackage: {} } }`
	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, where, c.lightFields(ctx))
		err := c.query(ctx, q, nil, &result)
		return result.GetTransactions, err
	})
}

// GetPackageLifecycleTransactions fetches every AddPackage, EnablePackage and
// RejectPackage transaction naming pkgPath — the full submission history for
// one package path, including redeploys parked while an earlier submission
// at the same path was still pending (see keeper_inert.go's redeploy case).
func (c *Client) GetPackageLifecycleTransactions(ctx context.Context, pkgPath string, need int) ([]Transaction, error) {
	e := gqlEscape(pkgPath)
	where := fmt.Sprintf(`_or: [
					{ messages: { value: { MsgAddPackage: { package: { path: { eq: "%s" } } } } } }
					{ messages: { value: { MsgEnablePackage: { pkg_path: { eq: "%s" } } } } }
					{ messages: { value: { MsgRejectPackage: { pkg_path: { eq: "%s" } } } } }
				]`, e, e, e)
	return c.recentTransactionsWindowed(ctx, need, where, func() ([]Transaction, error) {
		var result struct {
			GetTransactions []Transaction `json:"getTransactions"`
		}
		q := fmt.Sprintf(`{
		getTransactions(
			where: { %s }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, where, c.lightFields(ctx))
		err := c.query(ctx, q, nil, &result)
		return result.GetTransactions, err
	})
}
