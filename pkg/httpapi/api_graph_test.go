package httpapi

import (
	"encoding/json"
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

// The reverse dependency graph's depth cap, at the layer the frontend talks to.
//
// The funnel on the deps tab is drawn from one hop, so the handler's default
// has to be one hop: a default that walked the closure would draw somebody
// else's dependents above the subject and no control on the page would say so.
// ?depth=0 is the escape hatch for a caller that wants the whole closure, and
// it has to keep working because that is what this endpoint always returned.
func TestHandleDepsReverseDepth(t *testing.T) {
	api, db := newTestAPI(t)

	// util <- lib <- app: two hops from util to app, on one network.
	for i, p := range []string{"gno.land/p/demo/util", "gno.land/p/demo/lib", "gno.land/r/demo/app"} {
		if err := db.UpsertPackage("alpha", p, "n", "g1creator", "TX", 10+i,
			"2026-08-01T00:00:00Z", i == 2, 1); err != nil {
			t.Fatalf("upsert %s: %v", p, err)
		}
	}
	for _, e := range [][2]string{
		{"gno.land/p/demo/lib", "gno.land/p/demo/util"},
		{"gno.land/r/demo/app", "gno.land/p/demo/lib"},
	} {
		if err := db.SetDependencies("alpha", e[0], []string{e[1]}); err != nil {
			t.Fatalf("deps %v: %v", e, err)
		}
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name      string
		url       string
		wantApp   bool
		wantCode  int
		wantLibAt bool // lib carries its own dependents rather than an empty list
	}{
		{
			name: "the default stops at one hop",
			url:  "/api/deps/p/demo/util?network=alpha&dir=dependents",
		},
		{
			name: "an explicit depth of one is the same",
			url:  "/api/deps/p/demo/util?network=alpha&dir=dependents&depth=1",
		},
		{
			name:      "depth zero still walks the whole closure",
			url:       "/api/deps/p/demo/util?network=alpha&dir=dependents&depth=0",
			wantApp:   true,
			wantLibAt: true,
		},
		{
			name:      "depth two reaches app",
			url:       "/api/deps/p/demo/util?network=alpha&dir=dependents&depth=2",
			wantApp:   true,
			wantLibAt: true,
		},
		{
			name:     "a negative depth is a typo, not unbounded",
			url:      "/api/deps/p/demo/util?network=alpha&dir=dependents&depth=-1",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "a non-numeric depth is rejected rather than ignored",
			url:      "/api/deps/p/demo/util?network=alpha&dir=dependents&depth=deep",
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.url, nil))

			want := tt.wantCode
			if want == 0 {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, want, rec.Body.String())
			}
			if want != http.StatusOK {
				return
			}

			var graph map[string][]string
			if err := json.Unmarshal(rec.Body.Bytes(), &graph); err != nil {
				t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
			}
			if got := graph["gno.land/p/demo/util"]; len(got) != 1 || got[0] != "gno.land/p/demo/lib" {
				t.Errorf("util dependents = %v, want [gno.land/p/demo/lib]", got)
			}
			// lib is always present: it is what util's one edge points at, and
			// a node an edge points at has to exist or the funnel cannot draw
			// the edge.
			if _, ok := graph["gno.land/p/demo/lib"]; !ok {
				t.Errorf("lib is missing from the graph: %v", graph)
			}
			if got := len(graph["gno.land/p/demo/lib"]) > 0; got != tt.wantLibAt {
				t.Errorf("lib expanded = %v, want %v: %v", got, tt.wantLibAt, graph)
			}
			if _, ok := graph["gno.land/r/demo/app"]; ok != tt.wantApp {
				t.Errorf("app present = %v, want %v: %v", ok, tt.wantApp, graph)
			}
		})
	}
}
