package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
