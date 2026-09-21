package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type activeResponse struct {
	Network string   `json:"network"`
	Window  string   `json:"window"`
	Paths   []string `json:"paths"`
}

func TestHandleGraphActive(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	tests := []struct {
		name    string
		url     string
		window  string
		network string
		want    int
	}{
		// The fixture's packages and calls are all 48h old, so a 24h window
		// keeps nothing and an all-time one keeps the two called realms.
		{"all time names every called path", "/api/graph/active?network=alpha", "all", "alpha", 2},
		{"an explicit all is the same", "/api/graph/active?network=alpha&window=all", "all", "alpha", 2},
		{"a short window excludes everything older", "/api/graph/active?network=alpha&window=24h", "24h", "alpha", 0},
		{"90d reaches back past the fixture", "/api/graph/active?network=alpha&window=90d", "90d", "alpha", 3},
		// Unlike the contracts map, an absent network is the union rather than
		// a silent fallback to the first chain: this returns paths, which
		// carry no per-chain quantity to be mixed up. Still 2, because beta's
		// only called path is one alpha already has, and a union dedups it.
		{"no network is the union over every chain", "/api/graph/active", "all", "", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got activeResponse
			getJSON(t, api.HandleGraphActive, tt.url, &got)
			if got.Window != tt.window {
				t.Errorf("window = %q, want %q", got.Window, tt.window)
			}
			if got.Network != tt.network {
				t.Errorf("network = %q, want %q", got.Network, tt.network)
			}
			if len(got.Paths) != tt.want {
				t.Errorf("paths = %v, want %d of them", got.Paths, tt.want)
			}
		})
	}
}

// A window name the UI never offers is a typo, and answering it as all-time
// would produce a plausible set for the wrong period.
func TestHandleGraphActiveRejectsUnknownWindow(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	rec := httptest.NewRecorder()
	api.HandleGraphActive(rec, httptest.NewRequest(http.MethodGet, "/api/graph/active?window=1w", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}
