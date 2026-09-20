package httpapi

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// The balance sweeper, and the two views it makes possible.
//
// /api/accounts used to fill every row's balance with one RPC call per row, 20
// at a time, on every cold request, against a single node. That is the 7.8s the
// perf pass measured and handed off rather than fixed, and it is also why there
// was no rich list: ranking by balance needs every balance, and gno gives you
// neither an enumeration of accounts nor a batch balance query.
//
// Moving the fetch off the read path solves both at once. The read path becomes
// a join against a local table, the RPC traffic becomes one bounded background
// sweep instead of a burst per visitor, and the ranking falls out for free.

const (
	// BalanceSweepInterval is how often a sweep runs. Slower than the rollups:
	// a balance is not a number anyone watches tick, and every pass is real
	// traffic against someone else's node.
	BalanceSweepInterval = 10 * time.Minute

	// balanceSweepBatch bounds one pass. The sweeper orders never-fetched
	// addresses first and then oldest-first, so a cold cache fills in over
	// several passes rather than one pass trying the whole chain, and a warm
	// one refreshes round-robin.
	balanceSweepBatch = 400

	// balanceSweepConcurrency is how many requests are in flight at once. This
	// runs against a public node that nothing else is paying for, so it is
	// deliberately below the 20 the old read path used: nobody is waiting on
	// this one.
	balanceSweepConcurrency = 8
)

// SweepBalances refreshes one batch of cached balances per network.
//
// Errors are logged and skipped per address rather than failing the pass: one
// unreachable address must not stop the other 399, and a network whose RPC is
// down simply keeps the balances it already had, which is what the fetched_at
// on each row is for.
func (a *API) SweepBalances(ctx context.Context) {
	for _, n := range a.networks {
		rpcURL := a.rpcURLFor(n.ID)
		if rpcURL == "" {
			// Unverified RPC means an endpoint that might serve another chain's
			// balances. Withholding a figure costs a blank; trusting a wrong
			// one costs a number nobody can see is wrong. See rpcURLFor.
			continue
		}
		addresses, err := a.db.KnownAddresses(n.ID, balanceSweepBatch)
		if err != nil {
			log.Printf("[%s] balance sweep: %v", n.ID, err)
			continue
		}
		if len(addresses) == 0 {
			continue
		}

		height := 0
		if _, h, err := rpcStatus(ctx, rpcURL); err == nil {
			height = h
		}

		rows := make([]store.BalanceRow, len(addresses))
		sem := make(chan struct{}, balanceSweepConcurrency)
		var wg sync.WaitGroup
		for i, addr := range addresses {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, addr string) {
				defer wg.Done()
				defer func() { <-sem }()
				rows[i] = store.BalanceRow{
					Address: addr,
					Amount:  fetchBalance(ctx, addr, rpcURL),
					Height:  height,
				}
			}(i, addr)
		}
		wg.Wait()

		// An address with no coins answers with an empty string, which is a
		// real answer and worth caching: without it the sweeper would retry the
		// same empty accounts forever and never reach the rest.
		if err := a.db.UpsertBalances(n.ID, rows); err != nil {
			log.Printf("[%s] balance sweep write: %v", n.ID, err)
		}
	}
}

// RunBalanceSweeper sweeps on a timer until the context is cancelled.
func (a *API) RunBalanceSweeper(ctx context.Context) {
	a.SweepBalances(ctx)
	ticker := time.NewTicker(BalanceSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.SweepBalances(ctx)
		}
	}
}

// fillAccountBalancesFromCache joins the cached balances onto a page of rows.
//
// One query for the page instead of one request per row. A row with nothing
// cached keeps an empty balance, which the frontend renders as unknown rather
// than as zero: those are different claims and only one of them is safe to
// make about money.
func (a *API) fillAccountBalancesFromCache(network string, accounts []store.AccountInfo) {
	if network == "" || len(accounts) == 0 {
		return
	}
	addrs := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		addrs = append(addrs, acc.Address)
	}
	cached, err := a.db.BalancesFor(network, addrs)
	if err != nil {
		log.Printf("[%s] balance lookup: %v", network, err)
		return
	}
	for i := range accounts {
		if b, ok := cached[accounts[i].Address]; ok {
			accounts[i].Balance = b.Amount
		}
	}
}

// richListEntry is one ranked row, with the rank resolved server-side so a
// paged request past the first page still numbers correctly.
type richListEntry struct {
	Rank int `json:"rank"`
	store.BalanceRow
}

type richListResponse struct {
	Network  string                `json:"network"`
	Entries  []richListEntry       `json:"entries"`
	Coverage store.BalanceCoverage `json:"coverage"`
}

// HandleRichList ranks addresses by balance.
//
// Single network, resolved rather than refused, like the other endpoints that
// cannot blend: a balance is denominated per chain and one column holding two
// chains' ugnot would belong to neither.
//
// The response always carries `coverage`, and the page always prints it.
// Mintscan can say it ranks a chain's accounts because Cosmos can enumerate
// them; gno cannot, so this ranks *the addresses it has seen and swept* and has
// to say so rather than implying otherwise.
func (a *API) HandleRichList(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	if network == "" {
		jsonError(w, "no network configured", 404)
		return
	}
	limit := clampParam(r, "limit", 100, 500)
	offset := clampParam(r, "offset", 1, 100000) - 1

	rows, err := a.db.RichList(network, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	cov, err := a.db.BalanceCoverage(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	entries := make([]richListEntry, len(rows))
	for i, row := range rows {
		entries[i] = richListEntry{Rank: offset + i + 1, BalanceRow: row}
	}
	JSONResponse(w, richListResponse{Network: network, Entries: entries, Coverage: cov})
}

// HandleAccountPopulation serves the summary bar.
func (a *API) HandleAccountPopulation(w http.ResponseWriter, r *http.Request) {
	pop, err := a.db.AccountPopulation(a.networkParam(r))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, pop)
}
