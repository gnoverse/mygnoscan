package indexer

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The endpoint pool, which exists because of a failure that did not look like
// one.
//
// gno.land's mainnet indexer sat at block 785 while the chain was at 36,000. It
// answered every query promptly and correctly for the 785 blocks it knew about,
// so nothing about it read as broken: no errors, no timeouts, no wrong chain.
// The explorer reported a stalled chain as a healthy one for hours.
//
// A pool that fails over only on error would have stayed on it forever. So the
// pool is ordered by how far along each endpoint is, and these tests pin that
// rather than merely pinning "falls back when one is down".
func TestPoolPrefersTheEndpointFurthestAlong(t *testing.T) {
	stale, staleClient := NewFake(t)
	stale.SeedChain(1, 785)
	fresh, freshClient := NewFake(t)
	fresh.SeedChain(1, 36000)
	_, _ = staleClient, freshClient

	// Stale is listed first, so picking it would be the natural default.
	c := NewClient(stale.URL, fresh.URL)
	c.selectEndpoint(context.Background())

	if got := c.activeURL(); got != fresh.URL {
		t.Errorf("pool chose %q; want the endpoint at 36000, not the one stuck at 785", got)
	}
	tip, err := c.LatestBlockHeight(context.Background())
	if err != nil {
		t.Fatalf("LatestBlockHeight: %v", err)
	}
	if tip < 36000 {
		t.Errorf("tip = %d; the pool is still reading from the stalled endpoint", tip)
	}
}

// The error-driven half: an endpoint that is simply down is skipped.
func TestPoolFailsOverFromADeadEndpoint(t *testing.T) {
	dead, _ := NewFake(t)
	dead.Status = http.StatusInternalServerError
	live, _ := NewFake(t)
	live.SeedChain(1, 100)

	c := NewClient(dead.URL, live.URL)
	tip, err := c.LatestBlockHeight(context.Background())
	if err != nil {
		t.Fatalf("a dead first endpoint took the whole pool down: %v", err)
	}
	if tip == 0 {
		t.Error("failed over but read nothing")
	}
}

// A pool member serving a different chain must never be selected, however
// healthy or far along it is.
//
// This is the failure mode the pool could introduce that the single-endpoint
// code could not: splicing two chains together under one network. A fast,
// healthy, wrong chain is the worst member a pool can have — mainnet is
// `gnoland-1` and the old testnet was `gnoland1`, one hyphen apart.
func TestPoolRefusesAnEndpointOnAnotherChain(t *testing.T) {
	ours, _ := NewFake(t)
	ours.ChainID = "gnoland-1"
	ours.SeedChain(1, 500)

	// Further along, and wrong.
	other, _ := NewFake(t)
	other.ChainID = "gnoland1"
	other.SeedChain(1, 90000)

	c := NewClient(ours.URL, other.URL)
	c.selectEndpoint(context.Background())

	if got := c.activeURL(); got != ours.URL {
		t.Errorf("pool chose %q, an endpoint serving a different chain, because it was further along", got)
	}
}

// A single endpoint must behave exactly as before: no probing, no failover.
func TestSingleEndpointIsUnchanged(t *testing.T) {
	f, c := NewFake(t)
	f.SeedChain(1, 10)

	if _, err := c.LatestBlockHeight(context.Background()); err != nil {
		t.Fatalf("LatestBlockHeight: %v", err)
	}
	for _, q := range f.AskedQueries() {
		if containsAll(q, "latestBlockHeight", "getBlocks") {
			t.Errorf("a single-endpoint client sent a pool probe: %s", q)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// An indexer that predates the inert-package message types must still serve
// every other query.
//
// Those types are selected inside the *shared* transaction field set, so an
// indexer that does not define them rejects every transaction query, not just
// the package one. That is how a single unsupported type took a whole chain
// offline in production:
//
//	[pearl] sync error: sync packages: indexer returned 422 Unprocessable
//	Entity: Unknown type "MsgEnablePackage"
//
// The rows pearl had already synced stayed put, so nothing looked wrong until a
// schema migration dropped those tables and the resync could not refill them —
// the chain went from 83,163 transactions to 624 with no way back.
func TestAnOlderIndexerStillSyncs(t *testing.T) {
	f, c := NewFake(t)
	f.NoInertTypes = true
	f.SeedChain(1, 40)

	txs, err := c.GetRecentTransactionsPage(context.Background(), 10)
	if err != nil {
		t.Fatalf("an indexer without the inert types rejected an ordinary transaction query: %v", err)
	}
	if len(txs) == 0 {
		t.Fatal("no transactions returned")
	}

	// The fragments must be gone from the wire, not merely tolerated: the
	// server rejects the query outright, so leaving them in cannot work.
	for _, q := range f.AskedQueries() {
		if strings.Contains(q, "MsgEnablePackage") && !isCapabilityProbe(q) {
			t.Errorf("still asking an indexer that does not define it for MsgEnablePackage:\n%s", q)
		}
	}
}

// A newer indexer keeps the full selection — the trim must be conditional, not
// a blanket removal that quietly stops recording inert-package lifecycle on the
// chains that do support it.
func TestANewerIndexerKeepsTheInertTypes(t *testing.T) {
	f, c := NewFake(t)
	f.SeedChain(1, 40)

	if _, err := c.GetRecentTransactionsPage(context.Background(), 10); err != nil {
		t.Fatalf("GetRecentTransactionsPage: %v", err)
	}
	var asked bool
	for _, q := range f.AskedQueries() {
		if strings.Contains(q, "MsgEnablePackage") && !isCapabilityProbe(q) {
			asked = true
		}
	}
	if !asked {
		t.Error("the inert-package types were dropped from a query to an indexer that supports them")
	}
}

// The shape every live gno.land indexer has today, and the one a single probe
// standing for both groups gets wrong: MsgEnablePackage defined,
// MsgCreateSession not.
//
// Before the groups were probed separately this failed every transaction query
// with a 422, which on a running instance means package sync stops and the
// explorer quietly keeps serving the contracts it already knew about.
func TestAnIndexerWithInertTypesButNoSessionTypes(t *testing.T) {
	f, c := NewFake(t)
	f.NoSessionTypes = true
	f.SeedChain(1, 40)

	txs, err := c.GetRecentTransactionsPage(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetRecentTransactionsPage: %v", err)
	}
	if len(txs) == 0 {
		t.Fatal("no transactions: the query this indexer can answer returned nothing")
	}

	var keptInert, keptSessions bool
	for _, q := range f.AskedQueries() {
		if isCapabilityProbe(q) {
			continue
		}
		if strings.Contains(q, "MsgEnablePackage") {
			keptInert = true
		}
		if strings.Contains(q, "MsgCreateSession") {
			keptSessions = true
		}
	}
	if !keptInert {
		t.Error("dropped the inert-package types from an indexer that defines them")
	}
	if keptSessions {
		t.Error("kept the session types for an indexer that does not define them")
	}
}

// Each capability is asked once, not per query: a probe on every call would
// double the request count of every sync pass.
//
// Two probes, not one, because there are two independently shipped fragment
// groups to ask about. What matters is that the number does not grow with the
// number of queries.
func TestCapabilitiesAreProbedOncePerType(t *testing.T) {
	f, c := NewFake(t)
	f.SeedChain(1, 40)

	for i := 0; i < 5; i++ {
		if _, err := c.GetRecentTransactionsPage(context.Background(), 5); err != nil {
			t.Fatalf("GetRecentTransactionsPage: %v", err)
		}
	}
	probes := map[string]int{}
	for _, q := range f.AskedQueries() {
		if isCapabilityProbe(q) {
			probes[q]++
		}
	}
	if len(probes) != 2 {
		t.Errorf("probed %d distinct types, want 2 (inert lifecycle and sessions): %v", len(probes), probes)
	}
	for q, n := range probes {
		if n != 1 {
			t.Errorf("sent %d probes for %q across 5 queries, want 1", n, q)
		}
	}
}

// isCapabilityProbe identifies the schema probe specifically.
//
// Matching on "__type" alone does not work: every transaction query selects
// `__typename` on its message values, which contains it. The first version of
// these tests did exactly that and reported six probes for one, while claiming
// the fragments had been dropped from queries that were carrying them.
func isCapabilityProbe(q string) bool {
	return strings.Contains(q, `__type(name:`)
}
