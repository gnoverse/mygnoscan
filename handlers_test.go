package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestAPI builds an API over a real temp database with no indexer clients.
//
// Every handler covered here reads from storage, so an absent client is the
// point rather than a limitation: it proves these endpoints answer without the
// indexer, which is what keeps them working when it is slow or down.
func newTestAPI(t *testing.T) (*API, *DB) {
	t.Helper()

	db := newTestDB(t)
	nets := []NetworkConfig{{ID: "alpha"}, {ID: "beta"}}
	db.SetConfiguredNetworks(nets)

	return NewAPI(db, map[string]*IndexerClient{}, nets, NewAnalyzer(db)), db
}

// seedActivity writes a small, coherent slice of activity for one network.
// (syncer_test.go already has a seedNetwork with a different shape.)
func seedActivity(t *testing.T, db *DB, network string, calls, realms, sends int) {
	t.Helper()

	when := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	for i := 0; i < calls; i++ {
		if err := db.InsertCall(network, fmt.Sprintf("%s-call-%d", network, i), 100+i, when,
			fmt.Sprintf("g1caller%d", i%3), "gno.land/r/demo/board", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	for i := 0; i < realms; i++ {
		if err := db.UpsertPackage(network, fmt.Sprintf("gno.land/r/%s/pkg%d", network, i), "pkg",
			"g1creator", fmt.Sprintf("%s-dep-%d", network, i), 200+i, when, true, 2); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}
	for i := 0; i < sends; i++ {
		if err := db.InsertBankSend(network, fmt.Sprintf("%s-send-%d", network, i), 300+i, when,
			"g1from", "g1to", "1000ugnot", true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}
}

func get(t *testing.T, h http.HandlerFunc, target string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", target, nil))
	return rec, rec.Body.Bytes()
}

// The storage-backed handlers, exercised end to end over HTTP. None had a test.
func TestStorageBackedHandlers(t *testing.T) {
	api, db := newTestAPI(t)
	seedActivity(t, db, "alpha", 5, 3, 2)
	seedActivity(t, db, "beta", 2, 1, 1)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		target  string
		// check receives the decoded body; nil means "any valid JSON will do".
		check func(t *testing.T, body []byte)
	}{
		{
			name:    "stats scoped to one network",
			handler: api.HandleStats,
			target:  "/api/stats?network=alpha",
			check: func(t *testing.T, body []byte) {
				var s Stats
				mustJSON(t, body, &s)
				if s.TotalCalls != 5 {
					t.Errorf("calls = %d, want 5", s.TotalCalls)
				}
				if s.TotalRealms != 3 {
					t.Errorf("realms = %d, want 3", s.TotalRealms)
				}
			},
		},
		{
			name:    "stats across all configured networks",
			handler: api.HandleStats,
			target:  "/api/stats",
			check: func(t *testing.T, body []byte) {
				var s Stats
				mustJSON(t, body, &s)
				if s.TotalCalls != 7 {
					t.Errorf("calls = %d, want 5+2", s.TotalCalls)
				}
			},
		},
		{
			name:    "accounts carry their network",
			handler: api.HandleAccounts,
			target:  "/api/accounts",
			check: func(t *testing.T, body []byte) {
				var accounts []AccountInfo
				mustJSON(t, body, &accounts)
				if len(accounts) == 0 {
					t.Fatal("no accounts")
				}
				for _, a := range accounts {
					if a.Network == "" {
						t.Errorf("account %s has no network — the row dot cannot render", a.Address)
					}
				}
			},
		},
		{
			name:    "realms list",
			handler: api.HandleRealms,
			target:  "/api/realms?limit=10&network=alpha",
			check: func(t *testing.T, body []byte) {
				var page struct {
					Items []PackageInfo `json:"items"`
					Total int           `json:"total"`
				}
				mustJSON(t, body, &page)
				if page.Total != 3 {
					t.Errorf("total = %d, want 3", page.Total)
				}
				for _, r := range page.Items {
					if r.Network != "alpha" {
						t.Errorf("realm from %q leaked into the alpha view", r.Network)
					}
				}
			},
		},
		{
			name:    "validators come from storage, no client needed",
			handler: api.HandleValidators,
			target:  "/api/validators",
			check: func(t *testing.T, body []byte) {
				var regs []ValoperRegistration
				mustJSON(t, body, &regs) // empty is correct; it must not 500
			},
		},
		{
			name:    "tokens",
			handler: api.HandleTokens,
			target:  "/api/tokens",
			check:   nil,
		},
		{
			name:    "analytics",
			handler: api.HandleAnalytics,
			target:  "/api/analytics?network=alpha",
			check:   nil,
		},
		{
			name:    "bank stats",
			handler: api.HandleBankStats,
			target:  "/api/bankstats?network=alpha",
			check:   nil,
		},
		{
			name:    "gas",
			handler: api.HandleGas,
			target:  "/api/gas?network=alpha",
			check:   nil,
		},
		{
			name:    "transactions time series",
			handler: api.HandleTimeSeriesTransactions,
			target:  "/api/timeseries/transactions?days=7&granularity=daily&network=alpha",
			check:   nil,
		},
		{
			name:    "gas time series",
			handler: api.HandleTimeSeriesGas,
			target:  "/api/timeseries/gas?days=7&granularity=daily&network=alpha",
			check:   nil,
		},
		{
			name:    "callers time series",
			handler: api.HandleTimeSeriesCallers,
			target:  "/api/timeseries/callers?days=7&granularity=daily&network=alpha",
			check:   nil,
		},
		{
			name:    "packages time series",
			handler: api.HandleTimeSeriesPackages,
			target:  "/api/timeseries/packages?days=7&granularity=daily&network=alpha",
			check:   nil,
		},
		{
			name:    "active addresses time series",
			handler: api.HandleTimeSeriesActiveAddresses,
			target:  "/api/timeseries/active-addresses?days=7&granularity=daily&network=alpha",
			check:   nil,
		},
		{
			name:    "search",
			handler: api.HandleSearch,
			target:  "/api/search?q=pkg&network=alpha",
			check:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := get(t, tt.handler, tt.target)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !json.Valid(body) {
				t.Fatalf("body is not valid JSON: %s", body)
			}
			if tt.check != nil {
				tt.check(t, body)
			}
		})
	}
}

// Handlers that need a live chain must say so rather than answering from an
// arbitrary one. This is the #86 class of bug, pinned.
func TestHandlersThatRequireANetwork(t *testing.T) {
	api, _ := newTestAPI(t)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		target  string
		want    int
	}{
		{
			// A height identifies a different block on every chain.
			name:    "a block height needs a network",
			handler: api.HandleBlock,
			target:  "/api/block/100",
			want:    http.StatusBadRequest,
		},
		{
			// Storage figures are denominated amounts; blending chains is the
			// category error #86 is about.
			name:    "storage figures need a network",
			handler: api.HandleStorage,
			target:  "/api/storage/r/demo/boards",
			want:    http.StatusBadRequest,
		},
		{
			name:    "an unconfigured network is not found",
			handler: api.HandleBlock,
			target:  "/api/block/100?network=nope",
			want:    http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := get(t, tt.handler, tt.target)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.want, body)
			}
			var e struct {
				Error string `json:"error"`
			}
			mustJSON(t, body, &e)
			if e.Error == "" {
				t.Error("rejection carries no message for the caller to act on")
			}
		})
	}
}

// An empty database must produce empty results, not errors and not nulls: the
// frontend iterates these.
func TestHandlersOnAnEmptyDatabase(t *testing.T) {
	api, _ := newTestAPI(t)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		target  string
	}{
		{"accounts", api.HandleAccounts, "/api/accounts"},
		{"tokens", api.HandleTokens, "/api/tokens"},
		{"validators", api.HandleValidators, "/api/validators"},
		{"realms", api.HandleRealms, "/api/realms?limit=10"},
		{"stats", api.HandleStats, "/api/stats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := get(t, tc.handler, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d on an empty database, body = %s", rec.Code, body)
			}
			if string(body) == "null\n" || string(body) == "null" {
				t.Error("returned null; the frontend iterates this and would throw")
			}
		})
	}
}

func mustJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// The handlers left over after the storage-backed and indexer-backed sweeps.
// All of these read from the database and were reachable in production without
// ever having been exercised by a test.
func TestRemainingStorageHandlers(t *testing.T) {
	api, db := newTestAPI(t)
	seedActivity(t, db, "alpha", 4, 2, 1)
	seedActivity(t, db, "beta", 1, 1, 1)

	// seedActivity writes realms; /api/packages lists the non-realms, so it
	// needs one of those to have anything to show.
	if err := db.UpsertPackage("alpha", "gno.land/p/alpha/lib", "lib", "g1creator",
		"alpha-lib", 400, "2026-08-01T00:00:00Z", false, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name   string
		target string
		check  func(t *testing.T, body []byte)
	}{
		{
			name:   "packages list",
			target: "/api/packages?limit=10",
			check: func(t *testing.T, body []byte) {
				var p struct {
					Items []PackageInfo `json:"items"`
					Total int           `json:"total"`
				}
				mustJSON(t, body, &p)
				if p.Total == 0 {
					t.Error("no packages")
				}
				for _, r := range p.Items {
					if r.Network == "" {
						t.Errorf("package %s carries no network", r.Path)
					}
				}
			},
		},
		{
			name:   "one realm's detail",
			target: "/api/realm/r/alpha/pkg0?network=alpha",
			check: func(t *testing.T, body []byte) {
				var d map[string]any
				mustJSON(t, body, &d)
				if d["path"] == nil && d["package"] == nil {
					t.Errorf("no package in the response: %s", body)
				}
			},
		},
		{
			name:   "dependency graph",
			target: "/api/deps/r/alpha/pkg0?network=alpha",
		},
		{
			name:   "reverse dependency graph",
			target: "/api/deps/r/alpha/pkg0?network=alpha&dir=dependents",
		},
		{
			name:   "storage time series",
			target: "/api/timeseries/storage?days=7&network=alpha",
		},
		{
			name:   "storage realms selector",
			target: "/api/timeseries/storage/realms?days=7&network=alpha",
		},
		{
			name:   "health time series",
			target: "/api/timeseries/health?days=7&network=alpha",
		},
		{
			name:   "sanity overview for one network",
			target: "/api/sanity/overview?network=alpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", tt.target, nil))
			body := rec.Body.Bytes()

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, body)
			}
			if !json.Valid(body) {
				t.Fatalf("body is not valid JSON: %s", body)
			}
			if string(body) == "null\n" || string(body) == "null" {
				t.Error("returned null; the frontend iterates this and would throw")
			}
			if tt.check != nil {
				tt.check(t, body)
			}
		})
	}
}

// The live feed is an SSE stream, so it must set the streaming headers and stay
// open rather than answering and closing like a normal endpoint.
func TestLiveFeedHandlerOpensAStream(t *testing.T) {
	handler := liveFeedHandler()

	req := httptest.NewRequest("GET", "/api/live?network=nonexistent", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler(rec, req.WithContext(ctx))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after its request was cancelled")
	}

	// A subscription naming a network with no feed is accepted but silent: the
	// connection stays open and delivers nothing, rather than erroring at a
	// browser that cannot do anything useful with the error.
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache: a cached SSE stream never updates", got)
	}
}

// No list endpoint may answer `null`.
//
// Go marshals a nil slice as `null`, and every list view in the frontend
// iterates what it gets, so a query that legitimately matched nothing rendered
// as a broken page rather than an empty one. /api/search did this on production
// for any query with no hits.
//
// Most of these only ever looked correct because the chain they were tried
// against happened to have data, so this runs them all against an empty
// database, where every one of them matches nothing.
func TestNoListEndpointAnswersNull(t *testing.T) {
	api, _ := newTestAPI(t)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	targets := []string{
		"/api/search?q=nothing-matches-this",
		"/api/accounts",
		"/api/tokens",
		"/api/validators",
		"/api/realms?limit=10",
		"/api/packages?limit=10",
		"/api/deps/r/demo/nothing",
		"/api/timeseries/transactions?days=7",
		"/api/timeseries/packages?days=7",
		"/api/timeseries/callers?days=7",
		"/api/timeseries/gas?days=7",
		"/api/timeseries/active-addresses?days=7",
		"/api/timeseries/health?days=7",
		"/api/timeseries/storage?days=7",
		"/api/timeseries/storage/realms?days=7",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
			body := strings.TrimSpace(rec.Body.String())

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, body)
			}
			if body == "null" {
				t.Error("answered null; the frontend iterates this and would throw")
			}
			if !json.Valid([]byte(body)) {
				t.Fatalf("body is not valid JSON: %s", body)
			}
		})
	}
}

// The normalization itself, including the cases it must leave alone.
func TestEmptyNotNull(t *testing.T) {
	var nilSlice []string
	var nilMap map[string]int
	type payload struct{ N int }

	tests := []struct {
		name string
		in   any
		want string
	}{
		{"a nil slice becomes an empty array", nilSlice, "[]"},
		{"a nil map becomes an empty object", nilMap, "{}"},
		{"an empty slice is unchanged", []string{}, "[]"},
		{"a populated slice is unchanged", []int{1, 2}, "[1,2]"},
		{"a populated map is unchanged", map[string]int{"a": 1}, `{"a":1}`},
		{"a struct is unchanged", payload{N: 3}, `{"N":3}`},
		{"a pointer is unchanged", &payload{N: 4}, `{"N":4}`},
		// A nil pointer genuinely is null: there is no empty value to stand in
		// for a missing object, and the handlers that return one 404 instead.
		{"a nil pointer stays null", (*payload)(nil), "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(emptyNotNull(tt.in))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
