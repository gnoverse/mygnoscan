package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Chain parameters: what the chain is currently configured to do.
//
// Everything here is read live from one node. None of it is synced, none of it
// is in SQLite, and none of it can be answered from the indexer: params live in
// the ABCI store and the consensus limits live in the node's own config.
//
// Why a page and not a lookup: on gno.land the params *are* the interesting
// state. Whether a deploy parks or goes live, whether GNOT can move, whether a
// halt is armed and whether the version floor next to it is satisfiable are all
// single keys, and reading any of them today means knowing both the key and the
// query shape.
//
// ⚠️ The chain cannot enumerate its own params. There is no "list every key"
// query, so paramCatalog below is curated by hand and will go stale when a new
// key ships. That is a known limitation, stated on the page rather than hidden:
// an absent key here means nobody has added it, not that the chain lacks it.

// paramState classifies what the chain said about a key.
//
// The three-way split matters and is easy to get wrong. A bare `vm:p:<key>`
// query returns empty data with no error whether the key is unset or the query
// was malformed, which is why every read here goes through the `params/`
// prefix: with it, empty data means genuinely unset, a JSON `""` or `[]` means
// set-but-empty, and a malformed key is an error instead of a silent blank.
//
// Conflating unset with empty would be a real misreport. `pkg_approvers` empty
// freezes every parked package on the chain; `pkg_approvers` unset does not
// exist as a configuration and would mean something else entirely.
const (
	paramStateSet   = "set"
	paramStateEmpty = "empty"
	paramStateUnset = "unset"
	paramStateError = "error"
)

// ChainParam is one params key as the chain answers it, with the raw response
// kept alongside the decoded one.
//
// Raw is deliberately not dropped once Value is filled. A params page that only
// shows a prettified value is a page nobody can cite: the raw string is what a
// reader pastes into a proposal or a bug report, and it is the only form that
// survives a decoding bug here.
type ChainParam struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Explain string `json:"explain"`
	State   string `json:"state"`
	// Raw is the chain's response verbatim, still JSON-encoded.
	Raw string `json:"raw,omitempty"`
	// Value is the decoded scalar, List the decoded array. At most one is set.
	Value string   `json:"value,omitempty"`
	List  []string `json:"list,omitempty"`
	// Note is a contextual remark computed against other live state, e.g. a
	// halt height read against the current height. Never a restatement of the
	// value.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// ParamGroup is one titled section of the page.
type ParamGroup struct {
	Title   string       `json:"title"`
	Explain string       `json:"explain"`
	Params  []ChainParam `json:"params"`
}

// ChainIdentity is what the node says about itself. Read from /status, which
// every tm2 node serves without an index.
type ChainIdentity struct {
	Network         string `json:"network"`
	ChainID         string `json:"chain_id,omitempty"`
	NodeVersion     string `json:"node_version,omitempty"`
	NodeMoniker     string `json:"node_moniker,omitempty"`
	TxIndex         string `json:"tx_index,omitempty"`
	LatestHeight    int64  `json:"latest_height,omitempty"`
	LatestBlockTime string `json:"latest_block_time,omitempty"`
	CatchingUp      bool   `json:"catching_up"`
	RPC             string `json:"rpc,omitempty"`
	Indexer         string `json:"indexer,omitempty"`
	Error           string `json:"error,omitempty"`
}

// ConsensusLimits is /consensus_params, which is node config rather than
// governance: it cannot be changed by a proposal, only by a release.
type ConsensusLimits struct {
	BlockHeight   string   `json:"block_height,omitempty"`
	MaxTxBytes    string   `json:"max_tx_bytes,omitempty"`
	MaxDataBytes  string   `json:"max_data_bytes,omitempty"`
	MaxBlockBytes string   `json:"max_block_bytes,omitempty"`
	MaxGas        string   `json:"max_gas,omitempty"`
	TimeIotaMS    string   `json:"time_iota_ms,omitempty"`
	PubKeyTypes   []string `json:"pubkey_types,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// ParametersResponse is the whole page in one payload.
type ParametersResponse struct {
	Network   string           `json:"network"`
	Identity  ChainIdentity    `json:"identity"`
	Groups    []ParamGroup     `json:"groups"`
	Consensus *ConsensusLimits `json:"consensus,omitempty"`
	FetchedAt string           `json:"fetched_at"`
}

// paramSpec is one catalogued key: what to query, what to call it, and what it
// gates. The explanations are the point of the page as much as the values are.
type paramSpec struct {
	key     string
	label   string
	explain string
}

type paramGroupSpec struct {
	title   string
	explain string
	specs   []paramSpec
}

// paramCatalog is every key this page knows how to read, grouped the way a
// reader asks about them rather than by owning module.
//
// Every key below was queried against gno.land mainnet on 2026-09-19 and
// answered. A key that answers "unset" is still worth listing: "no halt is
// armed" is an answer, and an absent row would not be.
var paramCatalog = []paramGroupSpec{
	{
		title:   "deploy policy",
		explain: "whether code that lands on this chain becomes callable, and who decides.",
		specs: []paramSpec{
			{
				key:     "vm:p:code_submission_policy",
				label:   "code submission policy",
				explain: `"inert" parks every new package until an approver enables it. Unset means a package is live the moment it is deployed.`,
			},
			{
				key:     "vm:p:pkg_approvers",
				label:   "package approvers",
				explain: "the addresses allowed to send MsgEnablePackage. Clearing this list is the only on-chain brake on deployment.",
			},
			{
				key:     "vm:p:run_submitters",
				label:   "MsgRun submitters",
				explain: "the addresses allowed to send MsgRun. An empty list means the gate is off and anyone may run arbitrary code.",
			},
			{
				key:     "vm:p:storage_price",
				label:   "storage price",
				explain: "locked per byte a realm writes. Refundable when the bytes are freed.",
			},
			{
				key:     "vm:p:default_deposit",
				label:   "default storage deposit",
				explain: "the per-message ceiling used when a transaction omits max_deposit. It is a ceiling, not an opt-out, and unlike the gas fee it is refundable.",
			},
		},
	},
	{
		title:   "transfer restrictions",
		explain: "whether GNOT can move, and for whom. Two independent mechanisms: a denom list and a per-account exemption.",
		specs: []paramSpec{
			{
				key:     "bank:p:restricted_denoms",
				label:   "restricted denoms",
				explain: "denoms that cannot be transferred. An empty list means the transfer lock is lifted for everyone.",
			},
			{
				key:     "auth:p:unrestricted_addrs",
				label:   "unrestricted addresses",
				explain: "accounts exempt from the transfer lock. Independent of the denom list, so these stay exempt whatever it says.",
			},
		},
	},
	{
		title:   "halt and upgrade",
		explain: "the coordinated-stop mechanism. Both keys are read together: a version floor only applies at a halt.",
		specs: []paramSpec{
			{
				key:     "node:p:halt_height",
				label:   "halt height",
				explain: "the height at which nodes stop producing blocks. Nothing clears it once passed.",
			},
			{
				key:     "node:p:halt_min_version",
				label:   "halt minimum version",
				explain: "the version a node must report to start past the halt height. A floor no running build satisfies locks the fleet out at its next restart.",
			},
		},
	},
}

// paramFetchTimeout bounds one node round trip. Several of these run in
// parallel per request and the whole page is behind the response cache, so the
// budget is per key rather than per page.
const paramFetchTimeout = 8 * time.Second

// maxParamConcurrency bounds the fan-out at one node. The catalogue is small
// and the node answers params from local state, but a page load should still
// not open a dozen sockets at once against a public RPC.
const maxParamConcurrency = 6

// fetchParam reads one key through the `params/` prefix and classifies the
// answer. See paramState for why the prefix is not optional.
func fetchParam(ctx context.Context, rpcURL, key string) (raw, state string, err error) {
	raw, err = fetchABCIQuery(ctx, rpcURL, "params/"+key, "")
	switch {
	case err != nil:
		return "", paramStateError, err
	case raw == "":
		return "", paramStateUnset, nil
	case raw == `""` || raw == "[]":
		return raw, paramStateEmpty, nil
	default:
		return raw, paramStateSet, nil
	}
}

// decodeParam fills Value or List from Raw.
//
// Params are stored JSON-encoded, so a string arrives with its quotes and a
// list arrives as an array. Anything that does not decode keeps Raw and gets no
// Value: showing the undecoded form is honest, inventing a decoding is not.
func decodeParam(p *ChainParam) {
	if p.Raw == "" {
		return
	}
	var s string
	if err := json.Unmarshal([]byte(p.Raw), &s); err == nil {
		p.Value = s
		return
	}
	var list []string
	if err := json.Unmarshal([]byte(p.Raw), &list); err == nil {
		p.List = list
		return
	}
	var num json.Number
	if err := json.Unmarshal([]byte(p.Raw), &num); err == nil {
		p.Value = num.String()
	}
}

// annotateParam adds the remark that needs state from outside the key itself.
//
// Only two keys earn one, and both are cases where the value alone reads as
// fine and the context is what makes it a problem.
func annotateParam(p *ChainParam, latestHeight int64) {
	switch p.Key {
	case "node:p:halt_height":
		if p.State != paramStateSet || latestHeight <= 0 {
			return
		}
		h, err := strconv.ParseInt(p.Value, 10, 64)
		if err != nil {
			return
		}
		switch {
		case h < latestHeight:
			p.Note = fmt.Sprintf("%s blocks in the past: this halt has already been passed", fmtThousands(latestHeight-h))
		case h == latestHeight:
			p.Note = "the chain is at the halt height now"
		default:
			p.Note = fmt.Sprintf("%s blocks away", fmtThousands(h-latestHeight))
		}
	case "vm:p:pkg_approvers":
		if p.State == paramStateEmpty {
			p.Note = "no approver is configured, so every parked package is frozen"
		}
	}
}

// fmtThousands groups a count for prose. The frontend formats its own numbers;
// this is only for the Note strings assembled here.
func fmtThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	lead := len(s) % 3
	if lead > 0 {
		out = append(out, s[:lead]...)
	}
	for i := lead; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}

// fetchRPCJSON does a plain GET against a node endpoint and decodes `result`
// into out. Used for /status and /consensus_params, which are ordinary RPC
// methods rather than ABCI queries.
func fetchRPCJSON(ctx context.Context, rpcURL, path string, out any) error {
	if rpcURL == "" {
		return fmt.Errorf("no verified RPC endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rpcURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: paramFetchTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("rpc error: %s", envelope.Error.Message)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("rpc returned no result")
	}
	return json.Unmarshal(envelope.Result, out)
}

// fetchChainIdentity reads /status.
//
// tm2's /status carries no earliest-block fields, so genesis time is not
// available here and is deliberately not guessed at from block 1: that would be
// a second round trip to say something the page does not need.
func fetchChainIdentity(ctx context.Context, rpcURL string) ChainIdentity {
	var status struct {
		NodeInfo struct {
			Network string `json:"network"`
			Version string `json:"version"`
			Moniker string `json:"moniker"`
			Other   struct {
				TxIndex string `json:"tx_index"`
			} `json:"other"`
		} `json:"node_info"`
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
			LatestBlockTime   string `json:"latest_block_time"`
			CatchingUp        bool   `json:"catching_up"`
		} `json:"sync_info"`
	}
	id := ChainIdentity{}
	if err := fetchRPCJSON(ctx, rpcURL, "/status", &status); err != nil {
		id.Error = err.Error()
		return id
	}
	id.ChainID = status.NodeInfo.Network
	id.NodeVersion = status.NodeInfo.Version
	id.NodeMoniker = status.NodeInfo.Moniker
	id.TxIndex = status.NodeInfo.Other.TxIndex
	id.LatestBlockTime = status.SyncInfo.LatestBlockTime
	id.CatchingUp = status.SyncInfo.CatchingUp
	if h, err := strconv.ParseInt(status.SyncInfo.LatestBlockHeight, 10, 64); err == nil {
		id.LatestHeight = h
	}
	return id
}

// fetchConsensusLimits reads /consensus_params.
func fetchConsensusLimits(ctx context.Context, rpcURL string) *ConsensusLimits {
	var raw struct {
		BlockHeight     string `json:"block_height"`
		ConsensusParams struct {
			Block struct {
				MaxTxBytes    string `json:"MaxTxBytes"`
				MaxDataBytes  string `json:"MaxDataBytes"`
				MaxBlockBytes string `json:"MaxBlockBytes"`
				MaxGas        string `json:"MaxGas"`
				TimeIotaMS    string `json:"TimeIotaMS"`
			} `json:"Block"`
			Validator struct {
				PubKeyTypeURLs []string `json:"PubKeyTypeURLs"`
			} `json:"Validator"`
		} `json:"consensus_params"`
	}
	if err := fetchRPCJSON(ctx, rpcURL, "/consensus_params", &raw); err != nil {
		return &ConsensusLimits{Error: err.Error()}
	}
	return &ConsensusLimits{
		BlockHeight:   raw.BlockHeight,
		MaxTxBytes:    raw.ConsensusParams.Block.MaxTxBytes,
		MaxDataBytes:  raw.ConsensusParams.Block.MaxDataBytes,
		MaxBlockBytes: raw.ConsensusParams.Block.MaxBlockBytes,
		MaxGas:        raw.ConsensusParams.Block.MaxGas,
		TimeIotaMS:    raw.ConsensusParams.Block.TimeIotaMS,
		PubKeyTypes:   raw.ConsensusParams.Validator.PubKeyTypeURLs,
	}
}

// buildParameters reads the whole catalogue plus the node's own state.
//
// Every read is independent, so one failing key degrades to an error row rather
// than taking the page down. A chain that has dropped a key, or a node behind a
// proxy that rejects one query path, still renders everything else.
func buildParameters(ctx context.Context, rpcURL string) ([]ParamGroup, ChainIdentity, *ConsensusLimits) {
	var (
		wg       sync.WaitGroup
		identity ChainIdentity
		limits   *ConsensusLimits
	)
	wg.Add(2)
	go func() { defer wg.Done(); identity = fetchChainIdentity(ctx, rpcURL) }()
	go func() { defer wg.Done(); limits = fetchConsensusLimits(ctx, rpcURL) }()

	groups := make([]ParamGroup, len(paramCatalog))
	sem := make(chan struct{}, maxParamConcurrency)
	for gi, gspec := range paramCatalog {
		groups[gi] = ParamGroup{
			Title:   gspec.title,
			Explain: gspec.explain,
			Params:  make([]ChainParam, len(gspec.specs)),
		}
		for pi, spec := range gspec.specs {
			groups[gi].Params[pi] = ChainParam{Key: spec.key, Label: spec.label, Explain: spec.explain}
			wg.Add(1)
			sem <- struct{}{}
			go func(p *ChainParam) {
				defer wg.Done()
				defer func() { <-sem }()
				raw, state, err := fetchParam(ctx, rpcURL, p.Key)
				p.Raw, p.State = raw, state
				if err != nil {
					p.Error = err.Error()
					return
				}
				decodeParam(p)
			}(&groups[gi].Params[pi])
		}
	}
	wg.Wait()

	// Annotation runs after the wait because it reads the height the identity
	// fetch produced, which is a different goroutine's result.
	for gi := range groups {
		for pi := range groups[gi].Params {
			annotateParam(&groups[gi].Params[pi], identity.LatestHeight)
		}
	}
	return groups, identity, limits
}

// HandleParameters serves the chain configuration for one network.
//
// Single-network only, for the same reason /api/contracts/* is: there is no
// such thing as the code submission policy of three chains at once, and a
// blended answer would be one chain's configuration wearing another's name.
func (a *API) HandleParameters(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	if network == "" {
		jsonError(w, "no network configured", 404)
		return
	}
	rpcURL := a.rpcURLFor(network)
	if rpcURL == "" {
		// Unverified is not "probably fine": the whole point of this page is
		// that the values are this chain's. See rpcURLFor.
		JSONResponse(w, ParametersResponse{
			Network:   network,
			Identity:  ChainIdentity{Network: network, Error: "no verified RPC endpoint for this network"},
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	groups, identity, limits := buildParameters(r.Context(), rpcURL)
	identity.Network = network
	identity.RPC = rpcURL
	if client := a.clientFor(network); client != nil {
		identity.Indexer = client.ActiveURL()
	}
	JSONResponse(w, ParametersResponse{
		Network:   network,
		Identity:  identity,
		Groups:    groups,
		Consensus: limits,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	})
}
