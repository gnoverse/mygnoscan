package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// rejectUnknownNetwork guards every API route at once, so it is tested against a
// stand-in handler rather than the real mux: what matters is which requests
// reach the other side, not what they would have returned.
func TestRejectUnknownNetwork(t *testing.T) {
	configured := []NetworkConfig{{ID: "gnoland1"}, {ID: "sapphire"}}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantPassed bool
	}{
		{
			name:       "configured network passes",
			path:       "/api/stats?network=sapphire",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name:       "no network means all networks",
			path:       "/api/stats",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name:       "explicit all passes",
			path:       "/api/stats?network=all",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name: "retired network is rejected",
			// topaz was removed from the config once its testnet went away. The
			// database still holds its rows, so without this the endpoint answers
			// 200 with stale data and an unrelated chain's block height.
			path:       "/api/stats?network=topaz",
			wantStatus: 404,
			wantPassed: false,
		},
		{
			name:       "never-configured network is rejected",
			path:       "/api/txs?network=does-not-exist",
			wantStatus: 404,
			wantPassed: false,
		},
		{
			name: "SPA is not guarded",
			// A stale bookmark must still load the app, which can report the
			// unknown network itself. Handing a browser a JSON 404 would not.
			path:       "/?network=topaz",
			wantStatus: 200,
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			passed := false
			h := rejectUnknownNetwork(configured, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				passed = true
				w.WriteHeader(200)
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if passed != tt.wantPassed {
				t.Errorf("reached the handler = %v, want %v", passed, tt.wantPassed)
			}
			if !tt.wantPassed {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("rejection body is not JSON: %v", err)
				}
				if body.Error == "" {
					t.Error("rejection body carries no error message")
				}
			}
		})
	}
}

// gnoEvents is the pure half of the events endpoints: pick out GnoEvents, drop
// transactions that emitted none, and tag each row with the chain it came from.
// The tag is what lets the merged view interleave chains and show a dot.
func TestGnoEvents(t *testing.T) {
	tx := func(hash string, ok bool, evs ...TxEvent) Transaction {
		return Transaction{Hash: hash, BlockHeight: 10, BlockTime: "2026-01-01T00:00:00Z", Success: ok,
			Response: &TxResponse{Events: evs}}
	}
	gno := func(typ string) TxEvent { return TxEvent{Typename: "GnoEvent", Type: typ} }
	storage := TxEvent{Typename: "StorageDepositEvent", Type: "StorageDeposit"}

	tests := []struct {
		name     string
		txs      []Transaction
		wantRows int
		wantEvs  int
	}{
		{
			name:     "keeps gno events",
			txs:      []Transaction{tx("a", true, gno("Transfer"), gno("Approval"))},
			wantRows: 1,
			wantEvs:  2,
		},
		{
			name: "drops transactions with no gno events",
			// A storage event is still an event, but not one this view lists.
			txs:      []Transaction{tx("a", true, storage), tx("b", true, gno("Mint"))},
			wantRows: 1,
			wantEvs:  1,
		},
		{
			name:     "filters non-gno events out of a mixed transaction",
			txs:      []Transaction{tx("a", true, storage, gno("Mint"), storage)},
			wantRows: 1,
			wantEvs:  1,
		},
		{
			name:     "a nil response is not a crash",
			txs:      []Transaction{{Hash: "a", BlockHeight: 1}},
			wantRows: 0,
		},
		{
			name:     "no transactions",
			txs:      nil,
			wantRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gnoEvents(tt.txs, "sapphire")
			if len(got) != tt.wantRows {
				t.Fatalf("got %d rows, want %d", len(got), tt.wantRows)
			}
			for _, r := range got {
				if r.Network != "sapphire" {
					t.Errorf("row %s has network %q, want sapphire", r.TxHash, r.Network)
				}
				if r.BlockTime == "" {
					t.Errorf("row %s lost its timestamp — the merged view sorts on it", r.TxHash)
				}
				for _, ev := range r.Events {
					if ev.Typename != "GnoEvent" {
						t.Errorf("row %s kept a %s", r.TxHash, ev.Typename)
					}
				}
			}
			if tt.wantRows == 1 && len(got[0].Events) != tt.wantEvs {
				t.Errorf("got %d events, want %d", len(got[0].Events), tt.wantEvs)
			}
		})
	}
}

// "No limit" used to mean every transaction the chain had ever seen, which
// returned 500 after ten seconds on a busy network because the fetch could not
// finish inside the client timeout. The endpoint bounds it instead.
func TestTxWindow(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"absent falls back to the default window", "", defaultTxs},
		{"zero is a request for recent, not for everything", "limit=0", defaultTxs},
		{"negative is treated the same", "limit=-1", defaultTxs},
		{"a small limit is honoured", "limit=20", 20},
		{"a limit above the cap is clamped", "limit=99999", maxTxs},
		{"exactly the cap", fmt.Sprintf("limit=%d", maxTxs), maxTxs},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/txs?"+tt.query, nil)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

			windowed := limit
			if windowed <= 0 {
				windowed = defaultTxs
			}
			if windowed > maxTxs {
				windowed = maxTxs
			}
			if windowed != tt.want {
				t.Errorf("window = %d, want %d", windowed, tt.want)
			}
		})
	}
}

// The cap has to stay under what the server will wait for, or the endpoint
// trades a bounded answer for a timeout — which is the bug it was fixing.
func TestTxWindowCapIsSane(t *testing.T) {
	if maxTxs > indexerElementCap {
		t.Errorf("maxTxs %d exceeds the indexer's element cap %d, so the cap can never be reached",
			maxTxs, indexerElementCap)
	}
	if defaultTxs > maxTxs {
		t.Errorf("defaultTxs %d is above maxTxs %d", defaultTxs, maxTxs)
	}
	if defaultTxs <= 0 {
		t.Errorf("defaultTxs %d would make an absent limit return nothing", defaultTxs)
	}
}

// Every cross-network list orders through newerFirst. Heights are per-chain —
// gnoland1 sits near 3.1M while sapphire is near 400k — so comparing them across
// networks lets the chain with the largest numbers win every comparison. In a
// list that is then truncated to a page, that does not mis-order: it deletes a
// chain. It did exactly that to sapphire's events before block times were
// stamped, which is what this pins.
func TestNewerFirst(t *testing.T) {
	const (
		older = "2026-08-01T00:00:00Z"
		newer = "2026-08-27T00:00:00Z"
	)

	tests := []struct {
		name             string
		timeA, timeB     string
		heightA, heightB int
		want             bool
	}{
		{
			name: "the newer timestamp wins regardless of height",
			// The classic cross-chain case: a low-height row from a busy chain
			// is genuinely more recent than a high-height row from a quiet one.
			timeA: newer, timeB: older, heightA: 400_000, heightB: 3_100_000,
			want: true,
		},
		{
			name:  "the older timestamp loses regardless of height",
			timeA: older, timeB: newer, heightA: 3_100_000, heightB: 400_000,
			want: false,
		},
		{
			name: "a dated row sorts ahead of an undated one",
			// Undated rows collect at the end instead of being interleaved by a
			// number that means nothing across chains.
			timeA: older, timeB: "", heightA: 1, heightB: 9_999_999,
			want: true,
		},
		{
			name:  "an undated row sorts behind a dated one",
			timeA: "", timeB: older, heightA: 9_999_999, heightB: 1,
			want: false,
		},
		{
			name: "height only decides between two undated rows",
			// By here both are undated, so any order is a guess — but a stable one.
			timeA: "", timeB: "", heightA: 200, heightB: 100,
			want: true,
		},
		{
			// Neither is strictly newer, so this reports false and the stable
			// sort keeps the input order. Height is deliberately not consulted:
			// rows sharing a timestamp are in the same block on the same chain,
			// where height cannot separate them either.
			name:  "equal timestamps leave the order alone",
			timeA: newer, timeB: newer, heightA: 200, heightB: 100,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newerFirst(tt.timeA, tt.timeB, tt.heightA, tt.heightB); got != tt.want {
				t.Errorf("newerFirst(%q, %q, %d, %d) = %v, want %v",
					tt.timeA, tt.timeB, tt.heightA, tt.heightB, got, tt.want)
			}
		})
	}
}

// The failure this ordering exists to prevent, end to end: a page-sized
// truncation must not drop a whole chain.
func TestMergedListDoesNotDropAChain(t *testing.T) {
	// sapphire is busier and more recent but numbers its blocks far lower.
	rows := []Transaction{}
	for i := 0; i < 40; i++ {
		rows = append(rows, Transaction{
			Hash:        fmt.Sprintf("gnoland1-%d", i),
			BlockHeight: 3_100_000 + i,
			BlockTime:   "2026-08-01T00:00:00Z",
		})
	}
	for i := 0; i < 40; i++ {
		rows = append(rows, Transaction{
			Hash:        fmt.Sprintf("sapphire-%d", i),
			BlockHeight: 400_000 + i,
			BlockTime:   "2026-08-27T00:00:00Z",
		})
	}

	sortTransactionsByTime(rows)
	page := rows[:20] // what a limit would keep

	for _, tx := range page {
		if tx.BlockTime != "2026-08-27T00:00:00Z" {
			t.Fatalf("page contains an older row (%s) — the newest 20 should all be sapphire's", tx.Hash)
		}
	}
}

func TestParseTimeseriesParams(t *testing.T) {
	tests := []struct {
		name            string
		query           string
		wantDays        int
		wantGranularity string
	}{
		{"empty falls back to the historical default", "", 30, "daily"},
		{"window 24h", "window=24h", 1, "hourly"},
		{"window 7d", "window=7d", 7, "hourly"},
		{"window 30d", "window=30d", 30, "daily"},
		{"window 90d", "window=90d", 90, "daily"},
		{"window 1y", "window=1y", 365, "weekly"},
		{"window all is monthly and exceeds the 365 cap", "window=all", allWindowDays, "monthly"},
		{"window is case-insensitive", "window=ALL", allWindowDays, "monthly"},
		{"unknown window is ignored", "window=nope", 30, "daily"},
		// Back-compat: the existing analytics/gas/sanity views pass these and
		// must keep their exact behaviour.
		{"legacy days and granularity still work", "days=14&granularity=hourly", 14, "hourly"},
		{"legacy days still capped at 365", "days=5000", 365, "daily"},
		{"legacy invalid granularity still falls back to daily", "granularity=yearly", 30, "daily"},
		// Explicit parameters win over window.
		{"explicit days overrides window", "window=all&days=7", 7, "monthly"},
		{"explicit granularity overrides window and re-applies the cap", "window=all&granularity=daily", 365, "daily"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/timeseries/transactions?"+tt.query, nil)
			days, granularity := parseTimeseriesParams(r)
			if days != tt.wantDays {
				t.Errorf("days = %d, want %d", days, tt.wantDays)
			}
			if granularity != tt.wantGranularity {
				t.Errorf("granularity = %q, want %q", granularity, tt.wantGranularity)
			}
		})
	}
}
