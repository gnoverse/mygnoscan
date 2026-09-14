package main

import (
	"context"
	"net/http"
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
	stale, staleClient := newFakeIndexer(t)
	stale.seedChain(1, 785)
	fresh, freshClient := newFakeIndexer(t)
	fresh.seedChain(1, 36000)
	_, _ = staleClient, freshClient

	// Stale is listed first, so picking it would be the natural default.
	c := NewIndexerClient(stale.URL, fresh.URL)
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
	dead, _ := newFakeIndexer(t)
	dead.status = http.StatusInternalServerError
	live, _ := newFakeIndexer(t)
	live.seedChain(1, 100)

	c := NewIndexerClient(dead.URL, live.URL)
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
	ours, _ := newFakeIndexer(t)
	ours.chainID = "gnoland-1"
	ours.seedChain(1, 500)

	// Further along, and wrong.
	other, _ := newFakeIndexer(t)
	other.chainID = "gnoland1"
	other.seedChain(1, 90000)

	c := NewIndexerClient(ours.URL, other.URL)
	c.selectEndpoint(context.Background())

	if got := c.activeURL(); got != ours.URL {
		t.Errorf("pool chose %q, an endpoint serving a different chain, because it was further along", got)
	}
}

// A single endpoint must behave exactly as before: no probing, no failover.
func TestSingleEndpointIsUnchanged(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(1, 10)

	if _, err := c.LatestBlockHeight(context.Background()); err != nil {
		t.Fatalf("LatestBlockHeight: %v", err)
	}
	for _, q := range f.askedQueries() {
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
