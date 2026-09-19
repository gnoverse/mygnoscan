package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/store"
)

// fakeChainNode serves the four endpoints probeNode reads. An empty string for
// any of them makes that one fail, which is how the per-call degrade path gets
// covered.
type fakeChainNode struct {
	status         string
	consensusState string
	netInfo        string
	mempool        string
}

func (f fakeChainNode) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{
			"/status":              f.status,
			"/consensus_state":     f.consensusState,
			"/net_info":            f.netInfo,
			"/num_unconfirmed_txs": f.mempool,
		}[r.URL.Path]
		if body == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func statusJSON(height string, catchingUp bool) string {
	return `{"jsonrpc":"2.0","result":{"node_info":{"network":"gnoland-1"},"sync_info":{
		"latest_block_height":"` + height + `","latest_block_time":"2026-09-19T16:35:05Z",
		"catching_up":` + map[bool]string{true: "true", false: "false"}[catchingUp] + `}}}`
}

func consensusJSON(hrs, startTime string) string {
	return `{"jsonrpc":"2.0","result":{"round_state":{
		"height/round/step":"` + hrs + `","start_time":"` + startTime + `","height_vote_set":{}}}}`
}

// healthyNode is a node behaving normally: the round started a moment ago,
// which is what makes every heartbeat check pass without a baseline.
func healthyNode() fakeChainNode {
	return fakeChainNode{
		status:         statusJSON("168419", false),
		consensusState: consensusJSON("168420/0/1", time.Now().UTC().Add(-2*time.Second).Format(time.RFC3339)),
		netInfo:        `{"jsonrpc":"2.0","result":{"listening":true,"n_peers":"13"}}`,
		mempool:        `{"jsonrpc":"2.0","result":{"n_txs":"0","total":"0"}}`,
	}
}

// wedgedNode reproduces staging as it actually was: proposed a block, entered
// prevote, and never advanced. `/status` answers, the height is plausible, and
// three transactions sit in a mempool that will never drain.
func wedgedNode() fakeChainNode {
	return fakeChainNode{
		status:         statusJSON("546040", false),
		consensusState: consensusJSON("546041/0/4", "2026-07-10T13:11:36Z"),
		netInfo:        `{"jsonrpc":"2.0","result":{"listening":true,"n_peers":"0"}}`,
		mempool:        `{"jsonrpc":"2.0","result":{"n_txs":"3","total":"3"}}`,
	}
}

func TestParseRoundStep(t *testing.T) {
	tests := []struct {
		in          string
		height      int64
		round, step int
	}{
		{"546041/0/4", 546041, 0, 4},
		{"168420/0/1", 168420, 0, 1},
		{"100/12/6", 100, 12, 6},
		{"", 0, 0, 0},
		{"nonsense", 0, 0, 0},
		{"1/2", 0, 0, 0},
	}
	for _, tt := range tests {
		h, r, s := parseRoundStep(tt.in)
		if h != tt.height || r != tt.round || s != tt.step {
			t.Errorf("parseRoundStep(%q) = %d/%d/%d, want %d/%d/%d", tt.in, h, r, s, tt.height, tt.round, tt.step)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tt := range []struct {
		in   int
		want string
	}{{5, "5s"}, {59, "59s"}, {60, "1m"}, {3599, "59m"}, {3600, "1h"}, {86399, "23h"}, {86400, "1d"}, {6134400, "71d"}} {
		if got := humanDuration(tt.in); got != tt.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestProbeNodeReadsTheNodeNotTheIndexer is the point of the whole file: the
// wedged chain's own report, which nothing in mygnoscan previously asked for.
func TestProbeNodeReadsTheNodeNotTheIndexer(t *testing.T) {
	srv := wedgedNode().server(t)

	n := probeNode(context.Background(), srv.URL)

	if !n.Reachable {
		t.Fatalf("node reported unreachable: %q", n.Error)
	}
	if n.Height != 546040 {
		t.Errorf("height = %d, want 546040", n.Height)
	}
	if n.RoundHeight != 546041 || n.Round != 0 || n.Step != 4 {
		t.Errorf("round = %d/%d/%d, want 546041/0/4", n.RoundHeight, n.Round, n.Step)
	}
	if n.StepName != "Prevote" {
		t.Errorf("step name = %q, want Prevote", n.StepName)
	}
	if n.Peers != 0 {
		t.Errorf("peers = %d, want 0", n.Peers)
	}
	if n.MempoolTxs != 3 {
		t.Errorf("mempool = %d, want the 3 transactions that will never be included", n.MempoolTxs)
	}
	// Ten weeks and counting. This is the field that needs no baseline and no
	// second sample, which is why it catches what a height floor cannot.
	if n.RoundAgeSecond < 60*86400 {
		t.Errorf("round age = %ds, want a very large number for a round that started in July", n.RoundAgeSecond)
	}
}

// A node that answers /status but nothing else still reports what it can. A
// proxy that blocks one path should not cost the whole probe.
func TestProbeNodeDegradesPerCall(t *testing.T) {
	node := healthyNode()
	node.netInfo = ""
	node.mempool = ""
	srv := node.server(t)

	n := probeNode(context.Background(), srv.URL)

	if !n.Reachable || n.Height != 168419 {
		t.Fatalf("status should still have been read: %+v", n)
	}
	if n.RoundHeight != 168420 {
		t.Errorf("consensus state should still have been read, got round height %d", n.RoundHeight)
	}
	if n.Peers != 0 || n.MempoolTxs != 0 {
		t.Errorf("peers/mempool = %d/%d, want the zero values for calls that failed", n.Peers, n.MempoolTxs)
	}
}

func TestProbeNodeWithoutAnEndpoint(t *testing.T) {
	n := probeNode(context.Background(), "")
	if n.Reachable || n.Error == "" {
		t.Errorf("probe = %+v, want unreachable with a reason", n)
	}
}

// TestProbeNodeReportsTheChainItClaims covers the trade this probe makes. It
// talks to an unverified endpoint on purpose, so the identity of whatever
// answered has to travel with the answer rather than being assumed.
func TestProbeNodeReportsTheChainItClaims(t *testing.T) {
	srv := healthyNode().server(t)

	n := probeNode(context.Background(), srv.URL)

	if n.ChainID != "gnoland-1" {
		t.Errorf("chain id = %q, want the one the node reported", n.ChainID)
	}
	if n.RPC != srv.URL {
		t.Errorf("rpc = %q, want the endpoint that was asked", n.RPC)
	}
	if n.Verified {
		t.Error("verified = true, but probeNode alone cannot know that")
	}
}

// A network whose indexer is dead must still be diagnosable, which is the
// whole point: rpcURLFor withholds an endpoint until the *indexer* has vouched
// for it, so routing this probe through it would blind the page to exactly the
// case it exists for.
func TestAnUnverifiedRPCIsStillProbed(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := wedgedNode().server(t)
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}

	if api.rpcURLFor("alpha") != "" {
		t.Fatal("precondition: this endpoint should not be verified")
	}

	nodes, diags := api.probeNetworks(context.Background(), api.networks, nil)
	if !nodes["alpha"].Reachable {
		t.Fatalf("node = %+v, want it probed despite being unverified", nodes["alpha"])
	}
	if nodes["alpha"].Verified {
		t.Error("verified = true, want the response to admit the endpoint is unvouched")
	}
	if diags["alpha"].State != DiagWedged {
		t.Errorf("diagnosis = %+v, want the wedge reported", diags["alpha"])
	}
}

// TestDiagnose is the truth table. Each row names a different person as the
// one who has to act, which is why they are not allowed to collapse into one
// another.
func TestDiagnose(t *testing.T) {
	alive := store.SanityLiveness{Reachable: true, IsAlive: true, ChainHeight: 168419, LastBlockTime: "2026-09-19T16:35:05Z"}
	indexerDown := store.SanityLiveness{}
	staleButReachable := store.SanityLiveness{Reachable: true, IsAlive: false, SecondsSinceBlock: 400, LastBlockTime: "2026-09-19T16:00:00Z"}

	healthy := NodeState{Reachable: true, Height: 168419, Peers: 13, RoundAgeSecond: 2}
	wedged := NodeState{
		Reachable: true, Height: 546040, Peers: 0, MempoolTxs: 3,
		RoundHeight: 546041, Round: 0, Step: 4, StepName: "Prevote",
		RoundStart: "2026-07-10T13:11:36Z", RoundAgeSecond: 6134400,
	}

	tests := []struct {
		name        string
		node        NodeState
		live        store.SanityLiveness
		wantState   string
		wantHealthy bool
		wantIn      string
	}{
		{
			name: "both halves working", node: healthy, live: alive,
			wantState: DiagAlive, wantHealthy: true, wantIn: "producing blocks",
		},
		{
			name: "the chain stopped, and the node says where",
			node: wedged, live: store.SanityLiveness{Reachable: true},
			wantState: DiagWedged, wantIn: "546041/0/4 (Prevote)",
		},
		{
			// The staging case exactly: the indexer is gone AND the chain is
			// wedged. The chain has to win, because that is the fact a reader
			// whose deploy is not landing needs.
			name: "a wedged chain whose indexer is also gone reports the wedge",
			node: wedged, live: indexerDown,
			wantState: DiagWedged, wantIn: "will never be included",
		},
		{
			// What the page used to call "unreachable".
			name: "a healthy chain with a dead indexer blames the indexer",
			node: healthy, live: indexerDown,
			wantState: DiagIndexerDown, wantIn: "our indexer is not answering",
		},
		{
			name: "a node we cannot reach is not a chain that stopped",
			node: NodeState{Error: "dial tcp: connection refused"}, live: alive,
			wantState: DiagNodeUnreachable, wantIn: "connection refused",
		},
		{
			name: "neither half answering says so rather than guessing",
			node: NodeState{Error: "no verified RPC endpoint"}, live: indexerDown,
			wantState: DiagUnknown, wantIn: "neither the node nor the indexer",
		},
		{
			name: "a syncing node is not a broken one",
			node: NodeState{Reachable: true, Height: 100, CatchingUp: true}, live: alive,
			wantState: DiagSyncing, wantIn: "catching up",
		},
		{
			// A young round means consensus is moving, so this is quiet rather
			// than wedged, and the verdict says only what was observed.
			name: "quiet but advancing is stale, not wedged",
			node: healthy, live: staleButReachable,
			wantState: DiagStale, wantIn: "consensus is still advancing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diagnose(tt.node, tt.live)
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
			if got.Healthy != tt.wantHealthy {
				t.Errorf("healthy = %v, want %v", got.Healthy, tt.wantHealthy)
			}
			if tt.wantIn != "" && !strings.Contains(got.Detail, tt.wantIn) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.wantIn)
			}
		})
	}
}

// TestDiagnoseWedgeThreshold pins the boundary. A round a little older than a
// couple of blocks must not be called wedged, or every ordinary hiccup pages
// someone.
func TestDiagnoseWedgeThreshold(t *testing.T) {
	live := store.SanityLiveness{Reachable: true, IsAlive: true}
	for _, tt := range []struct {
		age  int
		want string
	}{
		{0, DiagAlive},
		{int(wedgedRoundAge.Seconds()) - 1, DiagAlive},
		{int(wedgedRoundAge.Seconds()), DiagAlive},
		{int(wedgedRoundAge.Seconds()) + 1, DiagWedged},
	} {
		node := NodeState{Reachable: true, Height: 1, RoundAgeSecond: tt.age}
		if got := diagnose(node, live); got.State != tt.want {
			t.Errorf("round age %ds: state = %q, want %q", tt.age, got.State, tt.want)
		}
	}
}

// TestBucketTicksAnchorsOnNow is the trap this function exists to avoid. A grid
// built from the newest block shows a full strip for a chain that stopped an
// hour ago, which is exactly the wrong answer.
func TestBucketTicksAnchorsOnNow(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	cell := 10 * time.Second
	cells := 6 // a one-minute grid

	// Three blocks, all in the oldest two cells, then nothing for 40 seconds.
	ticks := []store.BlockTick{
		{Height: 1, Time: now.Add(-58 * time.Second).Format(time.RFC3339), Txs: 2},
		{Height: 2, Time: now.Add(-55 * time.Second).Format(time.RFC3339), Txs: 0},
		{Height: 3, Time: now.Add(-45 * time.Second).Format(time.RFC3339), Txs: 1},
	}

	got := bucketTicks(ticks, now, cell, cells)

	if len(got) != cells {
		t.Fatalf("got %d cells, want %d", len(got), cells)
	}
	if got[0].Blocks != 2 || got[0].Txs != 2 {
		t.Errorf("cell 0 = %+v, want 2 blocks and 2 txs", got[0])
	}
	if got[1].Blocks != 1 {
		t.Errorf("cell 1 = %+v, want 1 block", got[1])
	}
	for i := 2; i < cells; i++ {
		if got[i].Blocks != 0 {
			t.Errorf("cell %d = %+v, want the silence after the last block", i, got[i])
		}
	}
	// Every cell carries its own left edge, so a gap can be dated without
	// counting backwards from the end of the strip.
	if got[0].Start != now.Add(-60*time.Second).Format(time.RFC3339) {
		t.Errorf("cell 0 start = %q, want the grid to begin one window before now", got[0].Start)
	}
}

func TestBucketTicksIgnoresOutOfRangeAndUnparseable(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	ticks := []store.BlockTick{
		{Height: 1, Time: now.Add(-10 * time.Minute).Format(time.RFC3339)}, // older than the grid
		{Height: 2, Time: now.Add(time.Hour).Format(time.RFC3339)},         // in the future
		{Height: 3, Time: "not a timestamp"},
		{Height: 4, Time: now.Add(-5 * time.Second).Format(time.RFC3339)}, // the only one that counts
	}

	got := bucketTicks(ticks, now, 10*time.Second, 6)

	total := 0
	for _, c := range got {
		total += c.Blocks
	}
	if total != 1 {
		t.Errorf("counted %d blocks, want only the one inside the window", total)
	}
}

func TestHandleHeartbeat(t *testing.T) {
	api, db := newTestAPI(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := db.UpsertBlock("alpha", 100+i, now.Add(-time.Duration(i)*8*time.Second).Format(time.RFC3339), 0, i); err != nil {
			t.Fatalf("upsert block: %v", err)
		}
	}

	var resp heartbeatResponse
	getJSON(t, api.HandleHeartbeat, "/api/health/heartbeat", &resp)

	if resp.Window != "5m" || resp.Cell != 10 {
		t.Errorf("window/cell = %s/%d, want the 5m default at 10s cells", resp.Window, resp.Cell)
	}
	// Every configured network gets a row, including the one with no blocks:
	// an absent row reads as "no such chain" rather than "nothing synced".
	if len(resp.Rows) != 2 {
		t.Fatalf("got %d rows, want one per configured network", len(resp.Rows))
	}
	byNet := map[string]HeartbeatRow{}
	for _, row := range resp.Rows {
		if len(row.Cells) != heartbeatCells {
			t.Errorf("%s: %d cells, want %d", row.Network, len(row.Cells), heartbeatCells)
		}
		byNet[row.Network] = row
	}
	if byNet["alpha"].LastBlock != 104 {
		t.Errorf("alpha last block = %d, want 104", byNet["alpha"].LastBlock)
	}
	if byNet["beta"].LastBlock != 0 || byNet["beta"].LastTime != "" {
		t.Errorf("beta = %+v, want an empty strip rather than a missing row", byNet["beta"])
	}

	total := 0
	for _, c := range byNet["alpha"].Cells {
		total += c.Blocks
	}
	if total != 5 {
		t.Errorf("alpha counted %d blocks, want 5", total)
	}
}

func TestHandleHeartbeatRejectsAnUnknownWindow(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandleHeartbeat(rec, httptest.NewRequest(http.MethodGet, "/api/health/heartbeat?window=1y", nil))
	// Rejected rather than silently widened: a typo that quietly changes the
	// window produces a plausible picture of the wrong period.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestSanityOverviewCarriesTheNodeVerdict checks the wiring: the endpoint the
// page already reads now carries what the node said and what it means.
func TestSanityOverviewCarriesTheNodeVerdict(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := wedgedNode().server(t)
	// The RPC goes in the network *config*, not through setRPCVerified: the
	// probe deliberately asks an endpoint the indexer has not vouched for, so
	// a chain whose indexer is gone can still be diagnosed. See probeNetworks.
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}, {ID: "beta"}}

	var resp struct {
		Nodes     map[string]NodeState      `json:"nodes"`
		Diagnosis map[string]ChainDiagnosis `json:"diagnosis"`
	}
	getJSON(t, api.HandleSanityOverview, "/api/sanity/overview?network=alpha", &resp)

	if got := resp.Nodes["alpha"]; got.Step != 4 || got.MempoolTxs != 3 {
		t.Errorf("node = %+v, want the prevote step and the queued transactions", got)
	}
	d := resp.Diagnosis["alpha"]
	if d.State != DiagWedged || d.Healthy {
		t.Errorf("diagnosis = %+v, want an unhealthy wedged verdict", d)
	}
	if !strings.Contains(d.Detail, "Prevote") {
		t.Errorf("detail = %q, want the step named", d.Detail)
	}
}

// Without an RPC there is no node to ask, and the endpoint must still answer.
// This is the harness's own configuration, so it is also the path the e2e suite
// exercises.
func TestSanityOverviewWithoutAnRPC(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp struct {
		Diagnosis map[string]ChainDiagnosis `json:"diagnosis"`
	}
	getJSON(t, api.HandleSanityOverview, "/api/sanity/overview?network=alpha", &resp)

	d := resp.Diagnosis["alpha"]
	if d.State != DiagUnknown && d.State != DiagNodeUnreachable {
		t.Errorf("state = %q, want one of the cannot-ask verdicts", d.State)
	}
	if d.Detail == "" {
		t.Error("detail = empty, want a reason")
	}
}

func TestSanityOverviewAllNetworksProbesEach(t *testing.T) {
	api, _ := newTestAPI(t)
	alphaSrv := healthyNode().server(t)
	betaSrv := wedgedNode().server(t)
	api.networks = []config.NetworkConfig{
		{ID: "alpha", RPCURL: alphaSrv.URL},
		{ID: "beta", RPCURL: betaSrv.URL},
	}

	var resp struct {
		Diagnosis map[string]ChainDiagnosis `json:"diagnosis"`
	}
	getJSON(t, api.HandleSanityOverview, "/api/sanity/overview", &resp)

	if len(resp.Diagnosis) != 2 {
		t.Fatalf("got %d verdicts, want one per network: %+v", len(resp.Diagnosis), resp.Diagnosis)
	}
	if resp.Diagnosis["beta"].State != DiagWedged {
		t.Errorf("beta = %+v, want wedged", resp.Diagnosis["beta"])
	}
	// alpha's indexer is absent in this harness while its node is healthy,
	// which is precisely the split this change exists to report.
	if resp.Diagnosis["alpha"].State != DiagIndexerDown {
		t.Errorf("alpha = %+v, want the indexer blamed rather than the chain", resp.Diagnosis["alpha"])
	}
}

// A node whose clock runs ahead of ours reports a round that started in the
// future. Measured against pearl, which is a couple of seconds fast.
func TestProbeNodeClampsAFutureRound(t *testing.T) {
	node := healthyNode()
	node.consensusState = consensusJSON("100/0/1", time.Now().UTC().Add(30*time.Second).Format(time.RFC3339))
	srv := node.server(t)

	n := probeNode(context.Background(), srv.URL)

	if n.RoundAgeSecond != 0 {
		t.Errorf("round age = %d, want 0 rather than a negative age on the page", n.RoundAgeSecond)
	}
}
