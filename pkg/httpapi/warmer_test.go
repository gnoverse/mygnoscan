package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The warmer fills the response cache under the keys a browser produces, so
// the reader that arrives afterwards is served without waiting.
//
// This is the whole point of the type: /api/govdao/overview measured 24.8s to
// 51.1s cold and 27ms warm against production, and nothing kept it warm.
func TestWarmerMakesTheNextReaderAHit(t *testing.T) {
	var calls atomic.Int32
	cache := NewResponseCache(time.Hour)
	handler := WithResponseCache(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))

	wm := &Warmer{handler: handler, plan: []string{"/api/govdao/overview"}, interval: time.Hour}
	wm.Pass(context.Background())
	if n := calls.Load(); n != 1 {
		t.Fatalf("warm pass ran the handler %d times, want 1", n)
	}

	// What a browser sends.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/govdao/overview", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache = %q after a warm pass, want HIT", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("handler ran %d times, want 1: the reader recomputed what the warmer had already computed", n)
	}
}

// The SPA sends ?network=all by default. That is the same request as no
// network at all, and the warmer warms the one spelling for both.
func TestWarmerCoversTheSPAsDefaultNetwork(t *testing.T) {
	var calls atomic.Int32
	cache := NewResponseCache(time.Hour)
	handler := WithResponseCache(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))

	wm := &Warmer{handler: handler, plan: []string{"/api/govdao/overview"}, interval: time.Hour}
	wm.Pass(context.Background())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/govdao/overview?network=all", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache = %q for ?network=all, want HIT", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("handler ran %d times, want 1", n)
	}
}

// The plan names each target once, and adds only the network-scoped ones per
// network. A plan that grew by every target times every network would turn one
// pass into minutes of upstream traffic.
func TestWarmPlan(t *testing.T) {
	base := WarmPlan(nil)
	if len(base) != len(warmTargets) {
		t.Errorf("plan for no networks has %d targets, want %d", len(base), len(warmTargets))
	}
	withNets := WarmPlan([]string{"mainnet", "pearl"})
	want := len(warmTargets) + 2*len(networkScopedWarmTargets)
	if len(withNets) != want {
		t.Errorf("plan for two networks has %d targets, want %d", len(withNets), want)
	}
	var found bool
	for _, p := range withNets {
		if p == "/api/govdao/overview?network=mainnet" {
			found = true
		}
		if strings.Contains(p, "network=all") {
			t.Errorf("plan contains %q: network=all is canonicalized away and warming it is a duplicate", p)
		}
	}
	if !found {
		t.Error("plan is missing /api/govdao/overview?network=mainnet")
	}
}

func TestParseWarmNetworks(t *testing.T) {
	configured := []string{"mainnet", "pearl", "staging"}
	for _, tt := range []struct {
		raw  string
		want int
	}{
		{"", 0},
		{"all", 3},
		{"mainnet", 1},
		{"mainnet, pearl", 2},
		{" , ", 0},
	} {
		if got := ParseWarmNetworks(tt.raw, configured); len(got) != tt.want {
			t.Errorf("ParseWarmNetworks(%q) = %v, want %d entries", tt.raw, got, tt.want)
		}
	}
}
