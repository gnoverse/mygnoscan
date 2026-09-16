package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetGnockpitCache clears the package-level cache so a test starts from a
// known state regardless of what earlier tests left behind.
func resetGnockpitCache(t *testing.T) {
	t.Helper()
	gnockpitCache.mu.Lock()
	gnockpitCache.validators = nil
	gnockpitCache.fetched = time.Time{}
	gnockpitCache.mu.Unlock()
}

func useGnockpitFake(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	old := gnockpitURL
	gnockpitURL = srv.URL
	t.Cleanup(func() { gnockpitURL = old })
}

func TestFetchGnockpitMonikers(t *testing.T) {
	resetGnockpitCache(t)
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"validators":[
			{"name":"berty-val-01","address":"g1l983yy3kpmapyzcfy53y5charfxupa5czjalea"},
			{"name":"","address":"g1noname"}
		]}`))
	})

	got := FetchGnockpitMonikers(context.Background())
	if got["g1l983yy3kpmapyzcfy53y5charfxupa5czjalea"] != "berty-val-01" {
		t.Errorf("moniker = %q, want berty-val-01", got["g1l983yy3kpmapyzcfy53y5charfxupa5czjalea"])
	}
	// A validator gnockpit reports with no name yet must not produce a blank
	// label — that would read as "found but nameless" rather than "unknown".
	if _, ok := got["g1noname"]; ok {
		t.Error("a validator with an empty name should not produce a map entry")
	}
}

func TestFetchGnockpitMonikersUnreachable(t *testing.T) {
	resetGnockpitCache(t)
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	got := FetchGnockpitMonikers(context.Background())
	if got != nil {
		t.Errorf("monikers = %+v, want nil with nothing cached yet and gnockpit down", got)
	}
}

// A refresh that fails must not erase what the previous successful one
// found — gnockpit having one bad moment should cost freshness, not every
// moniker on the page.
func TestFetchGnockpitMonikersServesStaleOnFailedRefresh(t *testing.T) {
	resetGnockpitCache(t)
	calls := 0
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"validators":[{"name":"first","address":"g1a"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	first := FetchGnockpitMonikers(context.Background())
	if first["g1a"] != "first" {
		t.Fatalf("first fetch = %+v, want g1a=first", first)
	}

	// Force the cache to look stale so the next call actually re-fetches
	// (and hits the now-failing response) instead of serving the cache.
	gnockpitCache.mu.Lock()
	gnockpitCache.fetched = time.Now().Add(-gnockpitCacheTTL - time.Second)
	gnockpitCache.mu.Unlock()

	second := FetchGnockpitMonikers(context.Background())
	if second["g1a"] != "first" {
		t.Errorf("after a failed refresh = %+v, want the stale map preserved (g1a=first)", second)
	}
	if calls != 2 {
		t.Errorf("server got %d requests, want exactly 2 (one per fetch attempt)", calls)
	}
}

func TestFetchGnockpitValidators(t *testing.T) {
	resetGnockpitCache(t)
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"validators":[
			{"name":"berty-val-01","address":"g1l983yy3kpmapyzcfy53y5charfxupa5czjalea","voting_power":"60","spof":false,"missed_100":0,"missed_24h":2,"avg_block_ms":3351}
		]}`))
	})

	got := FetchGnockpitValidators(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d validators, want 1", len(got))
	}
	v := got[0]
	if v.VotingPower != "60" || v.Missed24h != 2 || v.AvgBlockMs != 3351 || v.SPOF {
		t.Errorf("validator = %+v, want voting_power=60 missed_24h=2 avg_block_ms=3351 spof=false", v)
	}
}

func TestFetchGnockpitMonikersCachesWithinTTL(t *testing.T) {
	resetGnockpitCache(t)
	calls := 0
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"validators":[{"name":"first","address":"g1a"}]}`))
	})

	FetchGnockpitMonikers(context.Background())
	FetchGnockpitMonikers(context.Background())
	FetchGnockpitMonikers(context.Background())

	if calls != 1 {
		t.Errorf("server got %d requests across 3 calls within the TTL, want 1", calls)
	}
}
