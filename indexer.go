package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type IndexerClient struct {
	url    string
	client *http.Client
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
			Timeout: 30 * time.Second,
		},
	}
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
	Index       int          `json:"index"`
	Hash        string       `json:"hash"`
	Success     bool         `json:"success"`
	BlockHeight int          `json:"block_height"`
	GasWanted   int          `json:"gas_wanted"`
	GasUsed     int          `json:"gas_used"`
	GasFee      *Coin        `json:"gas_fee"`
	Memo        string       `json:"memo"`
	Messages    []TxMessage  `json:"messages"`
	Response    *TxResponse  `json:"response"`
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
	Typename string          `json:"__typename"`
	Type     string          `json:"type,omitempty"`
	PkgPath  string          `json:"pkg_path,omitempty"`
	Attrs    []EventAttr     `json:"attrs,omitempty"`
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
		}
	}
`

// GetAllPackages fetches all MsgAddPackage transactions.
func (c *IndexerClient) GetAllPackages(ctx context.Context) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { messages: { value: { MsgAddPackage: {} } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, txFields)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetRecentTransactions fetches transactions from the last N blocks.
func (c *IndexerClient) GetRecentTransactions(ctx context.Context, blockRange int) ([]Transaction, error) {
	if blockRange <= 0 {
		blockRange = 1000
	}
	latest, err := c.LatestBlockHeight(ctx)
	if err != nil {
		return nil, err
	}
	fromHeight := latest - blockRange
	if fromHeight < 0 {
		fromHeight = 0
	}
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { block_height: { gt: %d } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, fromHeight, txFieldsLight)
	err = c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
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
	}`, pkgPath, txFieldsLight)
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
	}`, hash, txFields)
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
				_or: {
					messages: {
						value: {
							_or: {
								MsgCall: { caller: { eq: "%s" } }
								MsgAddPackage: { creator: { eq: "%s" } }
								MsgRun: { caller: { eq: "%s" } }
								BankMsgSend: {
									_or: {
										from_address: { eq: "%s" }
										to_address: { eq: "%s" }
									}
								}
							}
						}
					}
				}
			}
			order: { heightAndIndex: DESC }
		) { %s }
	}`, addr, addr, addr, addr, addr, txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetMsgRunTransactions fetches all MsgRun transactions.
func (c *IndexerClient) GetMsgRunTransactions(ctx context.Context) ([]Transaction, error) {
	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: { messages: { value: { MsgRun: {} } } }
			order: { heightAndIndex: DESC }
		) { %s }
	}`, txFields)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
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
	fromHeight := latest - limit
	if fromHeight < 0 {
		fromHeight = 0
	}

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

// GetBlock fetches a single block by height.
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
		return nil, fmt.Errorf("block not found: %d", height)
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
	}`, pkgPath, funcName, txFieldsLight)
	err := c.query(ctx, q, nil, &result)
	return result.GetTransactions, err
}

// GetRecentTransactionsWithEvents fetches recent transactions that have GnoEvents.
func (c *IndexerClient) GetRecentTransactionsWithEvents(ctx context.Context) ([]Transaction, error) {
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
	}`, pkgPath, txFieldsLight)
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
