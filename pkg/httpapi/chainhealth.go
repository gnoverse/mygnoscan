package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/store"
)

// Asking the node itself, instead of inferring its health from the indexer.
//
// Everything mygnoscan knew about whether a chain was up came from
// `livenessOf`, which reads the newest block *out of the indexer*. That
// conflates two independent failures into one word. When staging's indexer went
// to NXDOMAIN the page said "unreachable", while the node was answering
// perfectly and had been stuck mid-consensus for ten weeks. Those need
// different people to fix them and the page could not tell them apart.
//
// So this file asks the node four questions the indexer cannot answer, and the
// verdict below combines them with what the indexer said.
//
// The load-bearing one is `/consensus_state`. A node that has stopped still
// answers `/status` with a plausible height forever, which is why every "is it
// up" check written as a floor (`height >= 1`) passes on a chain frozen since
// July. `round_state.start_time` does not: it is refreshed every height on a
// healthy chain, so its age is seconds when things are fine and weeks when they
// are not, with no baseline to keep and no second sample to take.

// RoundStepType names, from tm2/pkg/bft/consensus/types/round_state.go. The
// wire value is a number, and a page that prints "step 4" has told the reader
// nothing.
var consensusStepNames = map[int]string{
	1: "NewHeight",
	2: "NewRound",
	3: "Propose",
	4: "Prevote",
	5: "PrevoteWait",
	6: "Precommit",
	7: "PrecommitWait",
	8: "Commit",
}

// wedgedRoundAge is how stale a consensus round has to be before the chain is
// called wedged rather than merely quiet.
//
// Deliberately the same 120s that livenessOf uses for IsAlive, so a chain
// cannot come out stale by one measure and healthy by the other. On a chain
// with ~3s blocks this is ~40 blocks of silence, which no ordinary hiccup
// reaches, and a genuinely wedged chain overshoots it by weeks rather than by
// seconds.
const wedgedRoundAge = 120 * time.Second

// nodeProbeTimeout bounds each of the four node calls. They are answered from
// local state, so a node that needs longer than this is itself the finding.
const nodeProbeTimeout = 6 * time.Second

// NodeState is the node's own account of itself, read over RPC.
type NodeState struct {
	Reachable bool `json:"reachable"`
	// RPC is the endpoint asked, and ChainID what it said it serves. Both are
	// reported rather than assumed: this probe deliberately talks to an
	// unverified endpoint (see probeNetworks), so the page has to be able to
	// show that the answer came from the right chain.
	RPC     string `json:"rpc,omitempty"`
	ChainID string `json:"chain_id,omitempty"`
	// Verified says whether this endpoint is also the one trusted for balances,
	// i.e. whether it passed VerifyRPCChains against the indexer.
	Verified bool `json:"verified"`
	// Height and BlockTime come from /status, and are the node's view rather
	// than the indexer's. They disagree exactly when one of the two is behind,
	// which is worth being able to see.
	Height     int64  `json:"height,omitempty"`
	BlockTime  string `json:"block_time,omitempty"`
	CatchingUp bool   `json:"catching_up"`

	// The live consensus round.
	RoundHeight    int64  `json:"round_height,omitempty"`
	Round          int    `json:"round,omitempty"`
	Step           int    `json:"step,omitempty"`
	StepName       string `json:"step_name,omitempty"`
	RoundStart     string `json:"round_start,omitempty"`
	RoundAgeSecond int    `json:"round_age_seconds,omitempty"`

	// Peers and MempoolTxs are the corroborating detail. A frozen chain still
	// accepts transactions it will never include, which is what a developer
	// experiences as "my deploy silently did nothing".
	Peers      int `json:"peers"`
	MempoolTxs int `json:"mempool_txs"`

	Error string `json:"error,omitempty"`
}

// Chain diagnosis states. Each names who has to act, which is the whole reason
// for splitting them.
const (
	DiagAlive           = "alive"            // nothing to do
	DiagSyncing         = "syncing"          // the node is catching up, wait
	DiagWedged          = "wedged"           // the chain stopped, a node operator
	DiagStale           = "stale"            // no recent blocks, cause not established
	DiagIndexerDown     = "indexer_down"     // the chain is fine, our data source is not
	DiagNodeUnreachable = "node_unreachable" // we cannot ask, so we do not know
	DiagUnknown         = "unknown"          // neither source answered
)

// ChainDiagnosis is the verdict, and a sentence explaining it.
//
// Detail is assembled here rather than in the frontend because it needs the
// numbers: "wedged at 546041/0/4 (Prevote) since 2026-07-10, 71d" is the
// finding, and "unreachable" is what the page used to say instead.
type ChainDiagnosis struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
	// Healthy is the single boolean a badge or a header dot reads, so no caller
	// has to keep its own list of which states are bad.
	Healthy bool `json:"healthy"`
}

// networkConfig looks up a configured network by ID.
//
// The single-network path needs the real entry and not a synthesised one: a
// NetworkConfig built from just an ID carries no RPC, so the probe would ask
// nothing and report every chain as unconfigured.
func (a *API) networkConfig(id string) (config.NetworkConfig, bool) {
	for _, n := range a.networks {
		if n.ID == id {
			return n, true
		}
	}
	return config.NetworkConfig{}, false
}

// probeNode asks the node its four questions, in parallel.
//
// Failures are per-call: a node behind a proxy that blocks /net_info still
// yields its consensus state. Only /status failing marks the node unreachable,
// because that is the one every tm2 node serves.
func probeNode(ctx context.Context, rpcURL string) NodeState {
	if rpcURL == "" {
		return NodeState{Error: "no RPC endpoint is configured for this network"}
	}
	ctx, cancel := context.WithTimeout(ctx, nodeProbeTimeout)
	defer cancel()

	var (
		n  = NodeState{RPC: rpcURL}
		wg sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var status struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
			SyncInfo struct {
				LatestBlockHeight string `json:"latest_block_height"`
				LatestBlockTime   string `json:"latest_block_time"`
				CatchingUp        bool   `json:"catching_up"`
			} `json:"sync_info"`
		}
		if err := fetchRPCJSON(ctx, rpcURL, "/status", &status); err != nil {
			n.Error = err.Error()
			return
		}
		n.Reachable = true
		n.ChainID = status.NodeInfo.Network
		n.BlockTime = status.SyncInfo.LatestBlockTime
		n.CatchingUp = status.SyncInfo.CatchingUp
		n.Height, _ = strconv.ParseInt(status.SyncInfo.LatestBlockHeight, 10, 64)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var cs struct {
			RoundState struct {
				HeightRoundStep string `json:"height/round/step"`
				StartTime       string `json:"start_time"`
			} `json:"round_state"`
		}
		if err := fetchRPCJSON(ctx, rpcURL, "/consensus_state", &cs); err != nil {
			return
		}
		n.RoundHeight, n.Round, n.Step = parseRoundStep(cs.RoundState.HeightRoundStep)
		n.StepName = consensusStepNames[n.Step]
		n.RoundStart = cs.RoundState.StartTime
		if t, err := time.Parse(time.RFC3339, cs.RoundState.StartTime); err == nil {
			// Clamped at zero. A node a second or two ahead of our clock reports
			// a round that started in the future, and "-2s" on the page reads as
			// a bug in the explorer rather than as the rounding it is. Measured
			// against pearl, which runs a couple of seconds fast.
			age := int(time.Since(t).Seconds())
			if age < 0 {
				age = 0
			}
			n.RoundAgeSecond = age
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var info struct {
			NPeers json.Number `json:"n_peers"`
		}
		if err := fetchRPCJSON(ctx, rpcURL, "/net_info", &info); err != nil {
			return
		}
		if v, err := info.NPeers.Int64(); err == nil {
			n.Peers = int(v)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var mem struct {
			NTxs json.Number `json:"n_txs"`
		}
		if err := fetchRPCJSON(ctx, rpcURL, "/num_unconfirmed_txs", &mem); err != nil {
			return
		}
		if v, err := mem.NTxs.Int64(); err == nil {
			n.MempoolTxs = int(v)
		}
	}()

	wg.Wait()
	return n
}

// parseRoundStep splits tm2's "H/R/S" field. Anything unparseable yields zeros,
// which every caller reads as "no round information" rather than as height 0.
func parseRoundStep(s string) (height int64, round, step int) {
	var h, r, st int64
	if n, _ := fmt.Sscanf(s, "%d/%d/%d", &h, &r, &st); n != 3 {
		return 0, 0, 0
	}
	return h, int(r), int(st)
}

// diagnose combines what the node said with what the indexer said.
//
// The order of the cases is the point. "The node cannot be reached" has to come
// before any judgement about the chain, and "the chain is wedged" has to come
// before "the indexer is down", because a reader whose deploy is not landing
// needs the first fact and not the second.
func diagnose(node NodeState, live store.SanityLiveness) ChainDiagnosis {
	switch {
	case !node.Reachable && !live.Reachable:
		detail := "neither the node nor the indexer answered"
		if node.Error != "" {
			detail += ": " + node.Error
		}
		return ChainDiagnosis{State: DiagUnknown, Detail: detail}

	case !node.Reachable:
		// The indexer is answering, so there is still history to read. Say the
		// node is the missing half rather than calling the chain down.
		detail := "the node is not answering, so live chain state is unavailable"
		if node.Error != "" {
			detail += ": " + node.Error
		}
		return ChainDiagnosis{State: DiagNodeUnreachable, Detail: detail}

	case node.CatchingUp:
		return ChainDiagnosis{
			State:  DiagSyncing,
			Detail: fmt.Sprintf("the node is catching up, currently at height %d", node.Height),
		}

	case node.RoundAgeSecond > int(wedgedRoundAge.Seconds()):
		// The finding, stated in full. A round that has not advanced names the
		// step it stopped on, and the step says whether this is a vote-gathering
		// failure or a dead consensus goroutine.
		step := node.StepName
		if step == "" {
			step = "step " + strconv.Itoa(node.Step)
		}
		detail := fmt.Sprintf("consensus has not advanced for %s: stuck at %d/%d/%d (%s) since %s",
			humanDuration(node.RoundAgeSecond), node.RoundHeight, node.Round, node.Step, step, node.RoundStart)
		if node.MempoolTxs > 0 {
			detail += fmt.Sprintf(", with %d transaction(s) queued that will never be included", node.MempoolTxs)
		}
		if node.Peers == 0 {
			detail += ". The node has no peers"
		}
		return ChainDiagnosis{State: DiagWedged, Detail: detail}

	case !live.Reachable:
		// The chain is fine and our own data source is not. This is the case
		// that used to be reported as the chain being unreachable.
		return ChainDiagnosis{
			State: DiagIndexerDown,
			Detail: fmt.Sprintf("the chain is producing blocks (height %d), but our indexer is not answering, "+
				"so history and search are stale", node.Height),
		}

	case !live.IsAlive && live.LastBlockTime != "":
		// No recent block, but the round is young, so this is not a wedge. Say
		// what is observed and stop: a quiet chain and one that is about to
		// stop look the same from here.
		return ChainDiagnosis{
			State:  DiagStale,
			Detail: fmt.Sprintf("no block for %s, though consensus is still advancing", humanDuration(live.SecondsSinceBlock)),
		}

	default:
		return ChainDiagnosis{
			State:   DiagAlive,
			Healthy: true,
			Detail:  fmt.Sprintf("producing blocks at height %d, %d peers", node.Height, node.Peers),
		}
	}
}

// humanDuration renders a gap in the largest unit that keeps it readable. Ten
// weeks in seconds is a number nobody parses, and "71d" is the finding.
func humanDuration(seconds int) string {
	switch {
	case seconds < 60:
		return strconv.Itoa(seconds) + "s"
	case seconds < 3600:
		return strconv.Itoa(seconds/60) + "m"
	case seconds < 86400:
		return strconv.Itoa(seconds/3600) + "h"
	default:
		return strconv.Itoa(seconds/86400) + "d"
	}
}

// probeNetworks probes every named network's node concurrently and returns the
// node state and verdict per network.
//
// **It asks the configured RPC, not the verified one**, which is the opposite
// of what every other RPC caller here does and is the whole reason this works.
// rpcURLFor withholds an endpoint until VerifyRPCChains has confirmed it serves
// the same chain as the indexer, and that verification reads block 1 *from the
// indexer*. A network whose indexer is gone can therefore never have a verified
// RPC, so the one case this page exists for would be the one case it could not
// diagnose: staging, whose node answers fine and whose indexer is NXDOMAIN.
//
// Withholding is right for balances, where an unverified endpoint could serve a
// number from another chain and nobody would see it. It is wrong here, because
// the node's own identity is part of what is being reported: NodeState carries
// the chain ID it claims and whether the endpoint is verified, so a mismatch
// shows up on the page instead of being silently trusted.
//
// live carries what the indexer already said, keyed by network; a network
// missing from it is treated as an unreachable indexer, which is what an
// absent entry means at every call site.
func (a *API) probeNetworks(ctx context.Context, networks []config.NetworkConfig, live map[string]store.SanityLiveness) (map[string]NodeState, map[string]ChainDiagnosis) {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		nodes = make(map[string]NodeState, len(networks))
		diags = make(map[string]ChainDiagnosis, len(networks))
	)
	for _, n := range networks {
		wg.Add(1)
		go func(cfg config.NetworkConfig) {
			defer wg.Done()
			rpcs := cfg.RPCs()
			var url string
			if len(rpcs) > 0 {
				url = rpcs[0]
			}
			state := probeNode(ctx, url)
			state.Verified = url != "" && a.rpcVerified(cfg.ID) == url
			mu.Lock()
			defer mu.Unlock()
			nodes[cfg.ID] = state
			diags[cfg.ID] = diagnose(state, live[cfg.ID])
		}(n)
	}
	wg.Wait()
	return nodes, diags
}

// The heartbeat strip: one row per network, one cell per interval, filled when
// a block landed in it.
//
// A status light says alive or not. A strip says *cadence*, which is the thing
// that degrades before it fails: a chain slowing down looks different from one
// that is fine and different again from one that stopped, and only the third
// of those trips any threshold. Mintscan runs the same view across 37 chains
// and it is the fastest read on that whole site.
//
// Sourced from the local `blocks` table rather than from a node, so it costs
// one indexed query per network and shows the same history after a reload. The
// consequence is honest and worth knowing: a network mygnoscan is not
// successfully syncing has an empty strip even when its chain is fine, which is
// why the strip sits beside the diagnosis rather than replacing it.

const (
	// heartbeatCells is how many intervals a strip shows. 30 fits on one line
	// at a readable cell size.
	heartbeatCells = 30
	// heartbeatMaxWindow bounds the query. At the widest cell size this is
	// still one indexed range scan per network.
	heartbeatMaxWindow = 6 * time.Hour
	// heartbeatRowCap guards the per-network row count, for a fast chain on a
	// wide window.
	heartbeatRowCap = 20000
)

// heartbeatWindows are the cell sizes the UI offers, by name.
var heartbeatWindows = map[string]time.Duration{
	"5m":  10 * time.Second,
	"30m": time.Minute,
	"3h":  6 * time.Minute,
}

// HeartbeatCell is one interval of one network's strip.
type HeartbeatCell struct {
	// Start is the cell's left edge, so a reader can hover a gap and know when
	// it was rather than counting cells back from now.
	Start  string `json:"start"`
	Blocks int    `json:"blocks"`
	Txs    int    `json:"txs"`
}

// HeartbeatRow is one network's strip.
type HeartbeatRow struct {
	Network string          `json:"network"`
	Cells   []HeartbeatCell `json:"cells"`
	// LastBlock is the newest block in the window, or empty when the window
	// holds none. An empty strip plus no last block is "nothing synced here",
	// which is a different statement from "this chain stopped".
	LastBlock int    `json:"last_block,omitempty"`
	LastTime  string `json:"last_time,omitempty"`
	Error     string `json:"error,omitempty"`
}

type heartbeatResponse struct {
	Window string         `json:"window"`
	Cell   int            `json:"cell_seconds"`
	Rows   []HeartbeatRow `json:"rows"`
}

// bucketTicks lays blocks onto a fixed grid ending now.
//
// The grid is built from `now` rather than from the newest block on purpose: a
// chain that stopped an hour ago must show an hour of empty cells, and a grid
// anchored to its last block would show a full strip and look healthy.
func bucketTicks(ticks []store.BlockTick, now time.Time, cell time.Duration, cells int) []HeartbeatCell {
	// No rounding of the grid edges. Truncating `start` to the second pushed it
	// fractionally earlier, which put a block stamped at `now` at index `cells`
	// and dropped it: on a healthy chain the newest cell then read as empty,
	// which is the one cell a reader looks at.
	start := now.Add(-cell * time.Duration(cells))
	out := make([]HeartbeatCell, cells)
	for i := range out {
		out[i] = HeartbeatCell{Start: start.Add(cell * time.Duration(i)).UTC().Format(time.RFC3339)}
	}
	for _, t := range ticks {
		parsed, err := time.Parse(time.RFC3339, t.Time)
		if err != nil {
			continue
		}
		idx := int(parsed.Sub(start) / cell)
		switch {
		case idx < 0:
			continue // older than the grid
		case idx >= cells:
			// At or just past `now`: the chain moved while this response was
			// being assembled, or the node's clock is a shade ahead. Within one
			// cell that belongs in the newest cell; beyond it, the timestamp is
			// wrong and counting it would invent activity.
			if parsed.Sub(now) >= cell {
				continue
			}
			idx = cells - 1
		}
		out[idx].Blocks++
		out[idx].Txs += t.Txs
	}
	return out
}

// HandleHeartbeat serves the strip for every configured network.
//
// Always every network, never just the selected one: the point of the view is
// the comparison, and a chain is most interesting here precisely when it is not
// the one you were looking at.
func (a *API) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "5m"
	}
	cell, ok := heartbeatWindows[name]
	if !ok {
		http.Error(w, "unknown window: use 5m, 30m or 3h", http.StatusBadRequest)
		return
	}
	span := cell * time.Duration(heartbeatCells)
	if span > heartbeatMaxWindow {
		span = heartbeatMaxWindow
	}

	now := time.Now().UTC()
	since := now.Add(-span)
	resp := heartbeatResponse{Window: name, Cell: int(cell.Seconds())}
	for _, n := range a.networks {
		row := HeartbeatRow{Network: n.ID}
		ticks, err := a.db.RecentBlockTimes(n.ID, since, heartbeatRowCap)
		if err != nil {
			row.Error = err.Error()
			row.Cells = bucketTicks(nil, now, cell, heartbeatCells)
			resp.Rows = append(resp.Rows, row)
			continue
		}
		row.Cells = bucketTicks(ticks, now, cell, heartbeatCells)
		if len(ticks) > 0 {
			last := ticks[len(ticks)-1]
			row.LastBlock, row.LastTime = last.Height, last.Time
		}
		resp.Rows = append(resp.Rows, row)
	}
	JSONResponse(w, resp)
}
