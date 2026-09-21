package httpapi

import (
	"net/http"
	"strconv"

	"github.com/moul/mygnoscan/pkg/store"
)

// One validator, over time.
//
// /validators already renders the set: it derives proposers from recent blocks,
// draws a liveness sparkline per row, and joins gnockpit's power, missed-block
// and spof columns onto them. That table is a snapshot, and there was no way to
// ask about a single validator across the chain's history.
//
// ⚠️ Keyed on the **consensus** address, which is what proposes blocks and what
// gnockpit reports. The **operator** address that registers in r/gnops/valopers
// is a different key, and nothing on chain maps one to the other: that is the
// disjointness that made loadValMonikers never match a proposer. Verified
// against mainnet 2026-09-20, gnockpit's addresses and the interned proposer
// addresses agreed exactly, so the join used here does work.

// ValidatorRow is one validator, merged from the live set and local history.
type ValidatorRow struct {
	Address string `json:"address"`
	// Name is operator-supplied, by way of gnockpit. A claim, not a fact.
	Name        string `json:"name,omitempty"`
	VotingPower int64  `json:"voting_power"`
	SPOF        bool   `json:"spof"`
	Missed100   int    `json:"missed_100"`
	Missed24h   int    `json:"missed_24h"`
	AvgBlockMs  int    `json:"avg_block_ms"`
	// Blocks, Txs and Share are what this instance has synced, so they cover
	// the synced range rather than all of history.
	Blocks    int     `json:"blocks"`
	Txs       int     `json:"txs"`
	LastBlock int     `json:"last_height,omitempty"`
	LastTime  string  `json:"last_block_time,omitempty"`
	Share     float64 `json:"share"`
}

type validatorDetailResponse struct {
	Network   string                      `json:"network"`
	Validator ValidatorRow                `json:"validator"`
	InSet     bool                        `json:"in_set"`
	Blocks    []store.ProposedBlock       `json:"blocks"`
	Shares    []store.ValidatorSharePoint `json:"shares"`
}

// HandleValidator serves one validator by consensus address.
func (a *API) HandleValidator(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	if network == "" {
		jsonError(w, "no network configured", 404)
		return
	}
	addr := r.PathValue("addr")
	if addr == "" {
		jsonError(w, "no address given", 400)
		return
	}

	activity, err := a.db.ValidatorActivityAll(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	act, known := activity[addr]

	row := ValidatorRow{Address: addr}
	inSet := false
	for _, v := range FetchGnockpitValidators(r.Context()) {
		if v.Address != addr {
			continue
		}
		inSet = true
		row.Name, row.SPOF = v.Name, v.SPOF
		row.Missed100, row.Missed24h, row.AvgBlockMs = v.Missed100, v.Missed24h, v.AvgBlockMs
		row.VotingPower, _ = strconv.ParseInt(v.VotingPower, 10, 64)
	}
	if !known && !inSet {
		// Neither in the set nor ever seen proposing. An empty page would read
		// as an idle validator rather than as an address that is not one.
		jsonError(w, "no validator with that consensus address on this network", 404)
		return
	}

	total := 0
	for _, a := range activity {
		total += a.Blocks
	}
	row.Blocks, row.Txs = act.Blocks, act.Txs
	row.LastBlock, row.LastTime = act.LastH, act.Last
	if total > 0 {
		row.Share = float64(act.Blocks) / float64(total)
	}

	blocks, err := a.db.ValidatorBlocks(network, addr, 50)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	shares, err := a.db.ValidatorShareSeries(network, 30)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	JSONResponse(w, validatorDetailResponse{
		Network: network, Validator: row, InSet: inSet, Blocks: blocks, Shares: shares,
	})
}
