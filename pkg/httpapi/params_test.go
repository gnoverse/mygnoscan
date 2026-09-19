package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNode serves the three endpoints the parameters page reads: abci_query for
// params, and plain GETs for /status and /consensus_params.
//
// params maps a key to the JSON-encoded value the chain would return. A key
// absent from the map answers with empty Data, which is what an unset key
// actually looks like on chain.
type fakeNode struct {
	params    map[string]string
	status    string
	consensus string
	// paramErr makes one key answer with an abci error, to cover a key that
	// fails while its neighbours succeed.
	paramErr string
}

func (f fakeNode) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			if f.status == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			io.WriteString(w, f.status)
			return
		case "/consensus_params":
			if f.consensus == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			io.WriteString(w, f.consensus)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct{ Path string } `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode abci request: %v", err)
		}
		key := strings.TrimPrefix(req.Params.Path, "params/")
		if key == req.Params.Path {
			// The prefix is the whole point: a bare key would answer blank on a
			// real node, and a test that let it through would not notice.
			t.Errorf("param query path = %q, want a params/ prefix", req.Params.Path)
		}
		base := map[string]any{}
		if key == f.paramErr {
			base["Error"] = map[string]any{"@type": "/std.UnknownRequestError"}
		} else if v, ok := f.params[key]; ok {
			base["Data"] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"response": map[string]any{"ResponseBase": base}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mainnetNode mirrors what gno.land answered on 2026-09-19, trimmed to the
// fields this page reads. It is the fixture the whole file leans on, so that a
// change in the chain's response shape fails here rather than in production.
func mainnetNode() fakeNode {
	return fakeNode{
		params: map[string]string{
			"vm:p:code_submission_policy": `"inert"`,
			"vm:p:pkg_approvers":          `["g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704"]`,
			"vm:p:run_submitters":         `["g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m","g1manfred47kzduec920z88wfr64ylksmdcedlf5"]`,
			"vm:p:storage_price":          `"100ugnot"`,
			"vm:p:default_deposit":        `"100000000ugnot"`,
			"bank:p:restricted_denoms":    `[]`,
			"auth:p:unrestricted_addrs":   `["g1ugke9x9ylrlex0lxcgw7eu0mdftvcrgglru0l0"]`,
			"node:p:halt_height":          `"162200"`,
			"node:p:halt_min_version":     `""`,
		},
		status: `{"jsonrpc":"2.0","result":{
			"node_info":{"network":"gnoland-1","version":"v1.0.0-rc.0","moniker":"gno-core-rpc-public",
				"other":{"tx_index":"off"}},
			"sync_info":{"latest_block_height":"168419","latest_block_time":"2026-09-19T16:35:05Z","catching_up":false}}}`,
		consensus: `{"jsonrpc":"2.0","result":{"block_height":"168419","consensus_params":{
			"Block":{"MaxTxBytes":"1000000","MaxDataBytes":"2000000","MaxBlockBytes":"0","MaxGas":"3000000000","TimeIotaMS":"100"},
			"Validator":{"PubKeyTypeURLs":["/tm.PubKeyEd25519"]}}}}`,
	}
}

// TestFetchParamClassifiesTheAnswer is the load-bearing test of this file.
//
// Unset and empty are different configurations with different consequences, and
// the chain reports them as two shapes of the same successful response. Getting
// this wrong would have the page claim "no approvers are configured, every
// parked package is frozen" about a chain that simply has never had the key.
func TestFetchParamClassifiesTheAnswer(t *testing.T) {
	node := fakeNode{
		params: map[string]string{
			"vm:p:code_submission_policy": `"inert"`,
			"bank:p:restricted_denoms":    `[]`,
			"node:p:halt_min_version":     `""`,
		},
		paramErr: "gov:p:nonsense",
	}
	srv := node.server(t)

	tests := []struct {
		name      string
		key       string
		wantRaw   string
		wantState string
	}{
		{"a set string", "vm:p:code_submission_policy", `"inert"`, paramStateSet},
		{"an empty list is set, not unset", "bank:p:restricted_denoms", `[]`, paramStateEmpty},
		{"an empty string is set, not unset", "node:p:halt_min_version", `""`, paramStateEmpty},
		{"a key the chain has never held", "vm:p:halt_height", "", paramStateUnset},
		{"a key the chain rejects", "gov:p:nonsense", "", paramStateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, state, err := fetchParam(context.Background(), srv.URL, tt.key)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q (err %v)", state, tt.wantState, err)
			}
			if raw != tt.wantRaw {
				t.Errorf("raw = %q, want %q", raw, tt.wantRaw)
			}
			if (err != nil) != (tt.wantState == paramStateError) {
				t.Errorf("err = %v, want an error only for the error state", err)
			}
		})
	}
}

func TestDecodeParam(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantValue string
		wantList  []string
	}{
		{"a quoted string loses its quotes", `"inert"`, "inert", nil},
		{"a numeric string stays a string", `"162200"`, "162200", nil},
		{"a bare number decodes too", `162200`, "162200", nil},
		{"a list", `["g1a","g1b"]`, "", []string{"g1a", "g1b"}},
		{"an empty list", `[]`, "", []string{}},
		{"an empty string", `""`, "", nil},
		{"something undecodable keeps raw and gains nothing", `{not json`, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ChainParam{Raw: tt.raw}
			decodeParam(&p)
			if p.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", p.Value, tt.wantValue)
			}
			if len(p.List) != len(tt.wantList) {
				t.Fatalf("list = %v, want %v", p.List, tt.wantList)
			}
			for i := range tt.wantList {
				if p.List[i] != tt.wantList[i] {
					t.Errorf("list[%d] = %q, want %q", i, p.List[i], tt.wantList[i])
				}
			}
			if p.Raw != tt.raw {
				t.Errorf("raw was modified: %q", p.Raw)
			}
		})
	}
}

// TestAnnotateParam covers the two remarks that need state the key itself does
// not carry. The halt case is the one that matters: mainnet has sat at
// halt_height 162200 with the chain well past it, which reads as an armed halt
// unless the page says otherwise.
func TestAnnotateParam(t *testing.T) {
	tests := []struct {
		name     string
		param    ChainParam
		height   int64
		wantNote string
	}{
		{
			name:     "a halt height already passed",
			param:    ChainParam{Key: "node:p:halt_height", State: paramStateSet, Value: "162200"},
			height:   168419,
			wantNote: "6,219 blocks in the past: this halt has already been passed",
		},
		{
			name:     "a halt height still ahead",
			param:    ChainParam{Key: "node:p:halt_height", State: paramStateSet, Value: "170000"},
			height:   168419,
			wantNote: "1,581 blocks away",
		},
		{
			name:     "a halt height reached exactly",
			param:    ChainParam{Key: "node:p:halt_height", State: paramStateSet, Value: "168419"},
			height:   168419,
			wantNote: "the chain is at the halt height now",
		},
		{
			name:   "an unset halt height earns no remark",
			param:  ChainParam{Key: "node:p:halt_height", State: paramStateUnset},
			height: 168419,
		},
		{
			name:   "no remark without a height to compare against",
			param:  ChainParam{Key: "node:p:halt_height", State: paramStateSet, Value: "162200"},
			height: 0,
		},
		{
			name:     "an empty approver list freezes the queue",
			param:    ChainParam{Key: "vm:p:pkg_approvers", State: paramStateEmpty},
			wantNote: "no approver is configured, so every parked package is frozen",
		},
		{
			name:  "a populated approver list earns no remark",
			param: ChainParam{Key: "vm:p:pkg_approvers", State: paramStateSet, List: []string{"g1a"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.param
			annotateParam(&p, tt.height)
			if p.Note != tt.wantNote {
				t.Errorf("note = %q, want %q", p.Note, tt.wantNote)
			}
		})
	}
}

func TestFmtThousands(t *testing.T) {
	for _, tt := range []struct {
		in   int64
		want string
	}{{0, "0"}, {12, "12"}, {999, "999"}, {1000, "1,000"}, {6219, "6,219"}, {168419, "168,419"}, {1234567, "1,234,567"}} {
		if got := fmtThousands(tt.in); got != tt.want {
			t.Errorf("fmtThousands(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestHandleParameters walks the whole endpoint against a node that answers
// like mainnet did, and checks the values a reader would act on.
func TestHandleParameters(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := mainnetNode().server(t)
	api.setRPCVerified("alpha", srv.URL)

	var resp ParametersResponse
	getJSON(t, api.HandleParameters, "/api/params?network=alpha", &resp)

	if resp.Network != "alpha" {
		t.Errorf("network = %q, want alpha", resp.Network)
	}
	if resp.Identity.ChainID != "gnoland-1" || resp.Identity.LatestHeight != 168419 {
		t.Errorf("identity = %+v, want chain gnoland-1 at 168419", resp.Identity)
	}
	if resp.Identity.TxIndex != "off" {
		t.Errorf("tx_index = %q, want off", resp.Identity.TxIndex)
	}
	if resp.Identity.RPC != srv.URL {
		t.Errorf("rpc = %q, want the verified endpoint %q", resp.Identity.RPC, srv.URL)
	}
	if resp.Consensus == nil || resp.Consensus.MaxGas != "3000000000" {
		t.Fatalf("consensus = %+v, want MaxGas 3000000000", resp.Consensus)
	}
	if len(resp.Consensus.PubKeyTypes) != 1 {
		t.Errorf("pubkey types = %v, want one entry", resp.Consensus.PubKeyTypes)
	}

	byKey := map[string]ChainParam{}
	for _, g := range resp.Groups {
		if g.Title == "" || g.Explain == "" {
			t.Errorf("group %+v is missing its title or explanation", g)
		}
		for _, p := range g.Params {
			if p.Explain == "" {
				t.Errorf("param %s has no explanation", p.Key)
			}
			byKey[p.Key] = p
		}
	}
	if len(byKey) != 9 {
		t.Fatalf("got %d params, want the 9 in the catalogue", len(byKey))
	}

	policy := byKey["vm:p:code_submission_policy"]
	if policy.Value != "inert" || policy.Raw != `"inert"` {
		t.Errorf("policy = %+v, want value inert alongside its raw form", policy)
	}
	approvers := byKey["vm:p:pkg_approvers"]
	if len(approvers.List) != 1 || approvers.List[0] != "g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704" {
		t.Errorf("approvers = %+v, want the single oracle address", approvers)
	}
	if approvers.Note != "" {
		t.Errorf("approvers note = %q, want none while the list is populated", approvers.Note)
	}
	denoms := byKey["bank:p:restricted_denoms"]
	if denoms.State != paramStateEmpty {
		t.Errorf("restricted denoms state = %q, want empty (the transfer lock is lifted)", denoms.State)
	}
	halt := byKey["node:p:halt_height"]
	if halt.Note != "6,219 blocks in the past: this halt has already been passed" {
		t.Errorf("halt note = %q, want the past-height remark", halt.Note)
	}
	floor := byKey["node:p:halt_min_version"]
	if floor.State != paramStateEmpty {
		t.Errorf("halt_min_version state = %q, want empty rather than unset", floor.State)
	}
}

// TestHandleParametersNeverBlendsNetworks pins the single-network rule: an
// absent network resolves to one chain and the response names it, instead of
// answering for "all" the way most endpoints do. A blended params page would be
// one chain's configuration wearing another's name.
func TestHandleParametersNeverBlendsNetworks(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := mainnetNode().server(t)
	api.setRPCVerified("alpha", srv.URL)

	for _, url := range []string{"/api/params", "/api/params?network=all"} {
		var resp ParametersResponse
		getJSON(t, api.HandleParameters, url, &resp)
		if resp.Network != "alpha" {
			t.Errorf("GET %s: network = %q, want the first configured network named explicitly", url, resp.Network)
		}
		if len(resp.Groups) == 0 {
			t.Errorf("GET %s: no groups returned", url)
		}
	}
}

// TestHandleParametersWithoutAVerifiedRPC checks the endpoint says why it is
// empty rather than answering with a blank page. An unverified RPC may serve a
// different chain, and this page exists to say what *this* chain is configured
// to do.
func TestHandleParametersWithoutAVerifiedRPC(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp ParametersResponse
	getJSON(t, api.HandleParameters, "/api/params?network=alpha", &resp)

	if resp.Identity.Error == "" {
		t.Error("identity error = empty, want an explanation of why nothing was read")
	}
	if len(resp.Groups) != 0 {
		t.Errorf("groups = %d, want none without a verified endpoint", len(resp.Groups))
	}
}

// TestHandleParametersSurvivesOneBadKey covers the degrade path: a node that
// rejects one query still renders every other value. A page that failed whole
// would be useless exactly when a chain is in an odd state, which is when
// someone opens it.
func TestHandleParametersSurvivesOneBadKey(t *testing.T) {
	api, _ := newTestAPI(t)
	node := mainnetNode()
	node.paramErr = "vm:p:storage_price"
	srv := node.server(t)
	api.setRPCVerified("alpha", srv.URL)

	var resp ParametersResponse
	getJSON(t, api.HandleParameters, "/api/params?network=alpha", &resp)

	var broken, ok int
	for _, g := range resp.Groups {
		for _, p := range g.Params {
			if p.State == paramStateError {
				broken++
				if p.Error == "" {
					t.Errorf("param %s failed without saying why", p.Key)
				}
				continue
			}
			ok++
		}
	}
	if broken != 1 {
		t.Errorf("broken params = %d, want exactly the one that was rejected", broken)
	}
	if ok != 8 {
		t.Errorf("readable params = %d, want the other 8 unaffected", ok)
	}
}

// TestHandleParametersWithADeadNode checks that a node that answers nothing
// still produces a page shaped like a page, with the failures named per
// section instead of a 500.
func TestHandleParametersWithADeadNode(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := fakeNode{}.server(t)
	api.setRPCVerified("alpha", srv.URL)

	var resp ParametersResponse
	getJSON(t, api.HandleParameters, "/api/params?network=alpha", &resp)

	if resp.Identity.Error == "" {
		t.Error("identity error = empty, want the /status failure reported")
	}
	if resp.Consensus == nil || resp.Consensus.Error == "" {
		t.Error("consensus error = empty, want the /consensus_params failure reported")
	}
	// Every param answers with empty Data here, which is a successful "unset",
	// not a failure. That distinction is the file's whole subject.
	for _, g := range resp.Groups {
		for _, p := range g.Params {
			if p.State != paramStateUnset {
				t.Errorf("param %s state = %q, want unset", p.Key, p.State)
			}
		}
	}
}
