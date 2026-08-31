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

// The accounts leaderboard takes paging and sort controls.
//
// It used to return a fixed top 100 with none, which #10 flagged as the reason
// it could not back a real "rich list": there was no way to page past the first
// hundred or to rank by anything but total activity.
func TestAccountsPagingAndSort(t *testing.T) {
	api, db := newTestAPI(t)

	const when = "2026-08-01T00:00:00Z"
	for i := 0; i < 12; i++ {
		addr := fmt.Sprintf("g1caller%02d", i)
		// Descending calls, ascending sends: the two orders disagree, so a sort
		// that is ignored cannot pass by coincidence.
		for c := 0; c <= 12-i; c++ {
			if err := db.InsertCall("alpha", fmt.Sprintf("c-%d-%d", i, c), 100+c, when,
				addr, "gno.land/r/demo/boards", "Post", true); err != nil {
				t.Fatalf("InsertCall: %v", err)
			}
		}
		for sIdx := 0; sIdx <= i; sIdx++ {
			if err := db.InsertBankSend("alpha", fmt.Sprintf("s-%d-%d", i, sIdx), 200+sIdx, when,
				addr, "g1recv", "1ugnot", true); err != nil {
				t.Fatalf("InsertBankSend: %v", err)
			}
		}
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	get := func(t *testing.T, target string) []AccountInfo {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var accounts []AccountInfo
		mustJSON(t, rec.Body.Bytes(), &accounts)
		return accounts
	}

	t.Run("limit is honoured", func(t *testing.T) {
		if got := get(t, "/api/accounts?limit=5"); len(got) != 5 {
			t.Errorf("got %d accounts for limit=5", len(got))
		}
	})

	t.Run("offset pages past the first rows", func(t *testing.T) {
		first := get(t, "/api/accounts?limit=3")
		next := get(t, "/api/accounts?limit=3&offset=3")
		if len(first) != 3 || len(next) != 3 {
			t.Fatalf("page sizes = %d and %d", len(first), len(next))
		}
		for _, a := range first {
			for _, b := range next {
				if a.Address == b.Address {
					t.Errorf("%s appears on both pages", a.Address)
				}
			}
		}
	})

	t.Run("sort changes the order", func(t *testing.T) {
		byCalls := get(t, "/api/accounts?limit=1&sort=calls")
		bySends := get(t, "/api/accounts?limit=1&sort=sends")
		if len(byCalls) == 0 || len(bySends) == 0 {
			t.Fatal("empty result")
		}
		if byCalls[0].Address == bySends[0].Address {
			t.Errorf("sort=calls and sort=sends both lead with %s; the parameter is ignored",
				byCalls[0].Address)
		}
	})

	t.Run("an absent limit keeps the previous default", func(t *testing.T) {
		// Twelve callers plus g1recv, which is an active account by virtue of
		// receiving — the accounts view counts both sides of a send.
		if got := get(t, "/api/accounts"); len(got) != 13 {
			t.Errorf("got %d accounts, want all 13 under the default of %d", len(got), defaultAccounts)
		}
	})
}

// The watchlist digest: counts for the things a reader follows, plus how much
// has happened since they last reviewed each one.
//
// Answered entirely from stored rows. A watchlist is checked often and covers
// things the reader already cares about, so it must not cost an indexer
// round-trip per item.
func TestWatchEndpoint(t *testing.T) {
	api, db := newTestAPI(t)

	const when = "2026-08-01T00:00:00Z"
	const realm = "gno.land/r/demo/boards"
	if err := db.UpsertPackage("alpha", realm, "boards", "g1creator", "deploy", 100, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := db.InsertCall("alpha", fmt.Sprintf("c%d", i), 200+i, when, "g1watched", realm, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	get := func(t *testing.T, target string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		mustJSON(t, rec.Body.Bytes(), &out)
		return out
	}

	t.Run("a realm reports its activity", func(t *testing.T) {
		out := get(t, "/api/watch?realm="+realm)
		realms, _ := out["realms"].([]any)
		if len(realms) != 1 {
			t.Fatalf("got %d realms, want 1", len(realms))
		}
		r := realms[0].(map[string]any)
		if r["calls"].(float64) != 10 {
			t.Errorf("calls = %v, want 10", r["calls"])
		}
		if r["exists"] != true {
			t.Error("a deployed realm reported as not existing")
		}
	})

	t.Run("new_since counts only activity above the acknowledged height", func(t *testing.T) {
		out := get(t, "/api/watch?realm="+realm+"@205")
		r := out["realms"].([]any)[0].(map[string]any)
		// Calls sit at heights 200..209, so four are above 205.
		if r["new_since"].(float64) != 4 {
			t.Errorf("new_since = %v, want 4", r["new_since"])
		}
	})

	t.Run("an acknowledged height at the tip reports nothing new", func(t *testing.T) {
		out := get(t, "/api/watch?realm="+realm+"@209")
		r := out["realms"].([]any)[0].(map[string]any)
		if r["new_since"].(float64) != 0 {
			t.Errorf("new_since = %v, want 0", r["new_since"])
		}
	})

	t.Run("a watched path absent from this chain is reported, not hidden", func(t *testing.T) {
		// Showing zeros silently would read as "idle" rather than "not here".
		out := get(t, "/api/watch?realm=gno.land/r/demo/nothing")
		r := out["realms"].([]any)[0].(map[string]any)
		if r["exists"] != false {
			t.Error("an unknown realm reported as existing")
		}
	})

	t.Run("an address reports across every table it appears in", func(t *testing.T) {
		out := get(t, "/api/watch?address=g1watched")
		addrs, _ := out["addresses"].([]any)
		if len(addrs) != 1 {
			t.Fatalf("got %d addresses, want 1", len(addrs))
		}
		if addrs[0].(map[string]any)["calls"].(float64) != 10 {
			t.Errorf("calls = %v, want 10", addrs[0].(map[string]any)["calls"])
		}
	})

	t.Run("an empty watchlist is an empty digest, not an error", func(t *testing.T) {
		out := get(t, "/api/watch")
		if out["realms"] == nil || out["addresses"] == nil {
			t.Errorf("missing keys on an empty watchlist: %v", out)
		}
	})
}

// A type-filtered transaction list is served from storage, not the indexer.
//
// The indexer has no index for message type, so asking it for deploys walks the
// chain until it finds enough — measured at 12s for a 50-row page on sapphire,
// against 0.5s unfiltered, past the client deadline. The syncer already writes
// one row per message into a per-type table, which answers the same question
// with a real offset.
func TestFilteredTransactionsFromStorage(t *testing.T) {
	api, db := newTestAPI(t)

	const when = "2026-08-01T00:00:00Z"
	for i := 0; i < 30; i++ {
		if err := db.InsertCall("alpha", fmt.Sprintf("call-%d", i), 1000+i, when,
			"g1caller", "gno.land/r/demo/boards", "Post", i%5 != 0); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	for i := 0; i < 7; i++ {
		if err := db.UpsertPackage("alpha", fmt.Sprintf("gno.land/r/demo/pkg%d", i), "pkg",
			"g1deployer", fmt.Sprintf("deploy-%d", i), 2000+i, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}
	for i := 0; i < 12; i++ {
		if err := db.InsertBankSend("alpha", fmt.Sprintf("send-%d", i), 3000+i, when,
			"g1from", "g1to", "5ugnot", true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	page := func(t *testing.T, target string) (items []map[string]any, total int, fromStorage bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Items       []map[string]any `json:"items"`
			Total       int              `json:"total"`
			FromStorage bool             `json:"from_storage"`
		}
		mustJSON(t, rec.Body.Bytes(), &out)
		return out.Items, out.Total, out.FromStorage
	}

	t.Run("deploys come from storage with a real total", func(t *testing.T) {
		items, total, storage := page(t, "/api/txs?type=MsgAddPackage&limit=5&network=alpha")
		if !storage {
			t.Error("a type filter went to the indexer")
		}
		if total != 7 {
			t.Errorf("total = %d, want 7 — the count must cover the chain, not the page", total)
		}
		if len(items) != 5 {
			t.Errorf("got %d rows for limit=5", len(items))
		}
		for _, it := range items {
			if it["type"] != "MsgAddPackage" {
				t.Errorf("a %v leaked into a deploy-filtered page", it["type"])
			}
		}
	})

	t.Run("offset pages properly rather than re-slicing a window", func(t *testing.T) {
		first, _, _ := page(t, "/api/txs?type=BankMsgSend&limit=5&network=alpha")
		second, _, _ := page(t, "/api/txs?type=BankMsgSend&limit=5&offset=5&network=alpha")
		if len(first) != 5 || len(second) != 5 {
			t.Fatalf("page sizes = %d and %d", len(first), len(second))
		}
		for _, a := range first {
			for _, b := range second {
				if a["hash"] == b["hash"] {
					t.Errorf("%v appears on both pages", a["hash"])
				}
			}
		}
	})

	t.Run("the status filter narrows the total too", func(t *testing.T) {
		_, all, _ := page(t, "/api/txs?type=MsgCall&limit=1&network=alpha")
		_, failed, _ := page(t, "/api/txs?type=MsgCall&success=false&limit=1&network=alpha")
		if all != 30 {
			t.Errorf("unfiltered total = %d, want 30", all)
		}
		// Every fifth call was seeded as a failure.
		if failed != 6 {
			t.Errorf("failed total = %d, want 6", failed)
		}
	})

	t.Run("deploys have a success flag despite the table lacking one", func(t *testing.T) {
		// packages is keyed by path and holds current state, not per-deploy
		// outcome, so the flag comes from the transaction row and defaults to
		// success when that row has not been backfilled.
		items, _, _ := page(t, "/api/txs?type=MsgAddPackage&limit=1&network=alpha")
		if len(items) == 0 {
			t.Fatal("no deploys")
		}
		if _, ok := items[0]["success"]; !ok {
			t.Error("a deploy row carries no success flag")
		}
	})
}
