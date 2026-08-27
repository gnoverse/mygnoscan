package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
