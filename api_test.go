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
