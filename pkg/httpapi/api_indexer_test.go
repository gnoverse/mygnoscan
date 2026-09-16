package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// The handlers that reach the indexer, driven by two fake chains.
//
// These are the endpoints the merged views are built from, and every ordering
// bug this project has had lived here rather than in the query layer: a page
// that looks fine on one network and silently drops the other.
//
// alpha is the older, higher-numbered chain; beta is newer but numbers its
// blocks far lower — the shape that makes height a dangerous tiebreaker.
func newIndexerAPI(t *testing.T) (*API, *indexer.Fake, *indexer.Fake) {
	t.Helper()

	alpha, alphaClient := indexer.NewFake(t)
	alpha.ChainID = "alpha-1"
	alpha.SeedChain(3_100_000, 25)

	beta, betaClient := indexer.NewFake(t)
	beta.ChainID = "beta-1"
	beta.SeedChain(400_000, 25)
	beta.Redate("2026-09-01T00:00:00Z")

	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}}
	db.SetConfiguredNetworks(nets)

	clients := map[string]*indexer.Client{"alpha": alphaClient, "beta": betaClient}
	return NewAPI(db, clients, nets, analyzer.NewAnalyzer(db)), alpha, beta
}

// serve routes through the real mux rather than calling a handler directly.
//
// A handler invoked directly receives a request with no path values, so
// `{hash}` and `{height}` arrive empty and it rejects its own input. The route
// pattern is part of the endpoint's behaviour.
func serve(t *testing.T, api *API, target string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	return rec, rec.Body.Bytes()
}

func TestIndexerBackedHandlers(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	tests := []struct {
		name   string
		target string
		check  func(t *testing.T, body []byte)
	}{
		{
			name:   "txs from one network",
			target: "/api/txs?network=alpha&limit=10",
			check: func(t *testing.T, body []byte) {
				items, total := page(t, body)
				if len(items) != 10 {
					t.Errorf("got %d items, want the requested 10", len(items))
				}
				if total == 0 {
					t.Error("total is 0 with rows present")
				}
			},
		},
		{
			name:   "txs merged across networks carry their network",
			target: "/api/txs?limit=20",
			check: func(t *testing.T, body []byte) {
				items, _ := page(t, body)
				for _, it := range items {
					if str(it["network"]) == "" {
						t.Fatalf("merged row has no network, so it cannot be attributed: %v", it)
					}
				}
			},
		},
		{
			name:   "blocks merged across networks",
			target: "/api/blocks?limit=20",
		},
		{
			name:   "one block on a named network",
			target: "/api/block/3100005?network=alpha",
			check: func(t *testing.T, body []byte) {
				var b map[string]any
				mustJSON(t, body, &b)
				if b["block"] == nil && b["height"] == nil {
					t.Errorf("no block in the response: %s", body)
				}
			},
		},
		{
			name:   "all events",
			target: "/api/allevents?limit=10",
		},
		{
			name:   "address activity",
			target: "/api/address/g1caller0?network=alpha",
		},
		{
			name:   "govdao",
			target: "/api/govdao",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := serve(t, api, tt.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, body)
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

// The failure the ordering exists to prevent, through the real handler rather
// than the predicate alone: a page-sized limit must not amount to picking a
// chain.
func TestMergedTxsKeepEveryChain(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	rec, body := serve(t, api, "/api/txs?limit=30")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}

	items, _ := page(t, body)
	seen := map[string]int{}
	for _, it := range items {
		seen[str(it["network"])]++
	}
	if len(seen) < 2 {
		t.Fatalf("a 30-row page covers only %v; beta's newer rows were crowded out by alpha's larger heights", seen)
	}
	if got := str(items[0]["network"]); got != "beta" {
		t.Errorf("first row is from %q, want beta, which holds the most recent timestamps", got)
	}
}

// A network whose indexer is down must not take the merged page with it. This
// is the per-network circuit breaker seen from the outside.
func TestMergedViewsSurviveOneDeadNetwork(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)
	alpha.Status = http.StatusInternalServerError

	for _, tc := range []struct{ name, target string }{
		{"txs", "/api/txs?limit=20"},
		{"blocks", "/api/blocks?limit=20"},
		{"allevents", "/api/allevents?limit=20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := serve(t, api, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("one dead network returned %d for the whole page: %s", rec.Code, body)
			}
			items, _ := page(t, body)
			if len(items) == 0 {
				t.Fatal("the healthy network returned nothing")
			}
			for _, it := range items {
				if str(it["network"]) == "alpha" {
					t.Errorf("rows from the dead network appeared: %v", it)
				}
			}
		})
	}
}

// A single-network request against a dead indexer is a different case: there is
// no healthy half to fall back on, so it must say so rather than serve an empty
// page that reads as "this chain has no transactions".
func TestSingleNetworkReportsItsIndexerFailure(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)
	alpha.Status = http.StatusInternalServerError

	rec, body := serve(t, api, "/api/txs?network=alpha&limit=10")
	if rec.Code == http.StatusOK {
		t.Fatalf("a dead indexer was reported as an empty chain: %s", body)
	}
}

func TestUnknownNetworkOnIndexerHandlers(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	for _, tc := range []struct{ name, target string }{
		{"txs", "/api/txs?network=nope"},
		{"tx", "/api/tx/abc?network=nope"},
		{"block", "/api/block/1?network=nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := serve(t, api, tc.target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (body %s)", rec.Code, body)
			}
		})
	}
}

// A hash lives on at most one chain, so an unfiltered lookup asks all of them
// and takes whichever answers.
func TestTxLookupFindsTheRightChain(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	t.Run("a hash on beta is found without naming beta", func(t *testing.T) {
		rec, body := serve(t, api, "/api/tx/tx-call-400005")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, body)
		}
		var tx map[string]any
		mustJSON(t, body, &tx)
		if got := str(tx["network"]); got != "beta" {
			t.Errorf("network = %q, want beta", got)
		}
	})

	t.Run("a hash on no chain is a 404", func(t *testing.T) {
		rec, _ := serve(t, api, "/api/tx/definitely-not-a-hash")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// Looking up transactions must not take the other chains offline.
//
// A hash lives on exactly one chain, so an unfiltered lookup asks every network
// and expects all but one to answer "no". Those answers were being charged to
// the per-network breaker, so browsing three transactions in a row marked every
// other chain unreachable for a minute and collapsed every merged page onto the
// one chain that happened to hold them. Reproduced in production before fixing:
// /api/blocks went from three networks to one.
func TestLookupMissesDoNotOpenTheBreaker(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	// Well past the breaker threshold, all resolving on beta and therefore
	// missing on alpha every time.
	for _, h := range []string{"tx-call-400001", "tx-call-400002", "tx-call-400003", "tx-call-400004"} {
		if rec, body := serve(t, api, "/api/tx/"+h); rec.Code != http.StatusOK {
			t.Fatalf("lookup of %s: status %d, body %s", h, rec.Code, body)
		}
	}

	if api.health.shouldSkip("alpha") {
		t.Fatal("alpha was marked unreachable for answering that it does not have those hashes")
	}

	rec, body := serve(t, api, "/api/blocks?limit=30")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}
	items, _ := page(t, body)
	seen := map[string]bool{}
	for _, it := range items {
		seen[str(it["network"])] = true
	}
	if !seen["alpha"] {
		t.Errorf("alpha vanished from the merged page after transaction lookups; networks present: %v", seen)
	}
}

// The mirror image: a network that genuinely fails must still be skipped, or
// the fix above would have disabled the breaker rather than corrected it.
func TestRealFailuresStillOpenTheBreaker(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)
	alpha.Status = http.StatusInternalServerError

	for i := 0; i < breakerThreshold; i++ {
		serve(t, api, "/api/blocks?limit=10")
	}
	if !api.health.shouldSkip("alpha") {
		t.Error("a network returning 500s was never marked unreachable")
	}
}

// --- helpers ---------------------------------------------------------------

// page decodes the {items, total} envelope, falling back to a bare array.
func page(t *testing.T, body []byte) ([]map[string]any, int) {
	t.Helper()

	var p struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(body, &p); err == nil && p.Items != nil {
		return p.Items, p.Total
	}

	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return arr, len(arr)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// The per-package events endpoint, which reaches the indexer rather than
// storage. In all-networks mode it queries every chain and tags each row.
func TestPackageEventsHandler(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	for _, tc := range []struct{ name, target string }{
		{"one network", "/api/events/r/demo/boards?network=alpha&limit=5"},
		{"every network", "/api/events/r/demo/boards?limit=5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := serve(t, api, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, body)
			}

			rows, _ := page(t, body)
			for _, row := range rows {
				if str(row["network"]) == "" {
					t.Errorf("event row carries no network: %v", row)
				}
				if str(row["block_time"]) == "" {
					t.Errorf("event row lost its timestamp; the merged view sorts on it: %v", row)
				}
			}
		})
	}
}

// An unknown package is an empty list, not a failure: the events view for a
// realm nobody has called yet should render empty rather than error.
func TestPackageEventsOnAnUnknownPackage(t *testing.T) {
	api, _, _ := newIndexerAPI(t)

	rec, body := serve(t, api, "/api/events/r/demo/nothing-here?limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}
	if string(body) == "null\n" || string(body) == "null" {
		t.Error("returned null; the frontend iterates this and would throw")
	}
}

// A transaction hash reaches us in either of two encodings of the same 32
// bytes, and both must resolve.
//
// This explorer and the indexer use base64; gnoscan.io and Tendermint-style RPC
// print 64 hex characters. Someone comparing the two explorers pastes the hex
// form here and used to get a 404 for a transaction we were holding — the
// worst possible answer, because it reads as "this chain does not have it".
func TestTxLookupAcceptsEitherHashEncoding(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)

	// The fake seeds base64-looking hashes, so this exercises the real pair
	// taken from production: the hex form gnoscan.io showed for a mainnet
	// transaction, and the base64 form the indexer stores.
	const (
		hexHash = "7BA0A12FA4EB8A1AF51800700164EF18E024086B4DB6F7E1AF8664A22C6B0408"
		b64Hash = "e6ChL6Trihr1GABwAWTvGOAkCGtNtvfhr4ZkoixrBAg="
	)
	if got := normalizeTxHash(hexHash); got != b64Hash {
		t.Errorf("normalizeTxHash(hex) = %q, want %q", got, b64Hash)
	}

	for _, tc := range []struct{ name, in, want string }{
		{"base64 passes through", b64Hash, b64Hash},
		{"lowercase hex", strings.ToLower(hexHash), b64Hash},
		{"0x prefixed", "0x" + hexHash, b64Hash},
		{"surrounding space", "  " + hexHash + "  ", b64Hash},
		// Not a hash in either encoding: handed through for the indexer to
		// reject rather than guessed at.
		{"too short", "abc", "abc"},
		{"64 chars but not hex", strings.Repeat("z", 64), strings.Repeat("z", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeTxHash(tc.in); got != tc.want {
				t.Errorf("normalizeTxHash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// End to end through the real route: a transaction the chain holds under
	// its base64 hash must resolve when asked for by its hex spelling.
	tx := indexer.Call(3_100_007, "2026-08-01T00:00:00Z", "g1caller0", "gno.land/r/demo/boards", "Post")
	tx.Hash = b64Hash
	alpha.Add(tx)

	rec, body := serve(t, api, "/api/tx/"+hexHash)
	if rec.Code != http.StatusOK {
		t.Fatalf("hex hash lookup returned %d: %s", rec.Code, body)
	}
	var got map[string]any
	mustJSON(t, body, &got)
	if str(got["hash"]) != b64Hash {
		t.Errorf("resolved to hash %q, want the stored base64 form %q", str(got["hash"]), b64Hash)
	}
}
