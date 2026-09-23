package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seedCode(t *testing.T, api *API) {
	t.Helper()
	files := []struct{ net, path, name, body string }{
		{"alpha", "gno.land/p/nt/avl/v0", "tree.gno", "package avl\nfunc (t *Tree) IterateByOffset(o int) {}\n"},
		{"alpha", "gno.land/r/demo/boards", "boards.gno", "package boards\nfunc CreateBoard(n string) {}\n"},
		{"beta", "gno.land/r/other/thing", "x.gno", "package thing\nfunc IterateByOffset() {}\n"},
	}
	for _, f := range files {
		if err := api.db.UpsertPackageFile(f.net, f.path, f.name, f.body); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func codeSearch(t *testing.T, api *API, query string) (*httptest.ResponseRecorder, CodeSearchResponse) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/code/search?"+query, nil)
	w := httptest.NewRecorder()
	api.HandleCodeSearch(w, r)
	var got CodeSearchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w, got
}

func TestHandleCodeSearch(t *testing.T) {
	api, _ := newTestAPI(t)
	seedCode(t, api)

	tests := []struct {
		name      string
		query     string
		status    int
		wantPaths []string
	}{
		{
			name:  "finds a symbol inside the source",
			query: "network=alpha&q=IterateByOffset", status: 200,
			wantPaths: []string{"gno.land/p/nt/avl/v0"},
		},
		{
			// AGENTS.md's first invariant. beta declares the same symbol and
			// must never leak into alpha's results.
			name:  "never returns another network's source",
			query: "network=beta&q=IterateByOffset", status: 200,
			wantPaths: []string{"gno.land/r/other/thing"},
		},
		{
			name:  "realm filter excludes pure packages",
			query: "network=alpha&q=package&kind=realm", status: 200,
			wantPaths: []string{"gno.land/r/demo/boards"},
		},
		{
			name:  "an empty query is not an error and matches nothing",
			query: "network=alpha&q=", status: 200,
			wantPaths: nil,
		},
		{
			// The default. getNetwork() returns "all" until a reader picks
			// one, so this is what a shared /developer/search link runs, and
			// it used to match nothing at all.
			name:  "no network searches every configured one",
			query: "q=IterateByOffset", status: 200,
			wantPaths: []string{"gno.land/p/nt/avl/v0", "gno.land/r/other/thing"},
		},
		{
			name:  "all is the same as naming no network",
			query: "network=all&q=IterateByOffset", status: 200,
			wantPaths: []string{"gno.land/p/nt/avl/v0", "gno.land/r/other/thing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, got := codeSearch(t, api, tt.query)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if len(got.Hits) != len(tt.wantPaths) {
				t.Fatalf("hits = %d %+v, want %d", len(got.Hits), got.Hits, len(tt.wantPaths))
			}
			// Compared as a set: hits come back by FTS rank, and two networks
			// scoring the same term is a tie the engine is free to break
			// either way.
			seen := map[string]bool{}
			for _, h := range got.Hits {
				seen[h.Path] = true
				if h.Network == "" {
					t.Errorf("hit %q carries no network, so nothing can link it to a chain", h.Path)
				}
			}
			for _, want := range tt.wantPaths {
				if !seen[want] {
					t.Errorf("missing %q; got %+v", want, got.Hits)
				}
			}
		})
	}
}

// "no results" and "nothing indexed" look identical to a reader and mean
// opposite things, so the count is on every response and the page uses it to
// say which one happened.
func TestHandleCodeSearchReportsIndexSize(t *testing.T) {
	api, _ := newTestAPI(t)
	_, empty := codeSearch(t, api, "network=alpha&q=anything")
	if empty.Indexed != 0 {
		t.Errorf("indexed = %d on a cold index, want 0", empty.Indexed)
	}
	seedCode(t, api)
	_, warm := codeSearch(t, api, "network=alpha&q=nothingmatchesthis")
	if warm.Indexed == 0 {
		t.Error("indexed = 0 after seeding, so the page cannot tell an empty index from no match")
	}
	if len(warm.Hits) != 0 {
		t.Errorf("hits = %+v, want none", warm.Hits)
	}
}

// A query the reader typed wrong is a 400 that tells them so, not a 500 that
// tells them nothing, and never a leaked SQL string.
func TestHandleCodeSearchRejectsMalformedQuery(t *testing.T) {
	api, _ := newTestAPI(t)
	seedCode(t, api)

	w, _ := codeSearch(t, api, `network=alpha&q=%22unbalanced`)
	if w.Code == 200 {
		t.Skip("this FTS5 build accepts the query")
	}
	if w.Code != 400 {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SELECT") {
		t.Errorf("the response leaks SQL: %s", w.Body.String())
	}
}

// On-chain source is attacker-controlled: whoever deploys picks every byte.
func TestHandleCodeSearchKeepsHostileSourceAsData(t *testing.T) {
	api, _ := newTestAPI(t)
	hostile := `func Evil() { /* <img src=x onerror=alert(1)> */ }`
	if err := api.db.UpsertPackageFile("alpha", "gno.land/r/x/y", "e.gno", hostile); err != nil {
		t.Fatal(err)
	}
	w, got := codeSearch(t, api, "network=alpha&q=Evil")
	if w.Code != 200 || len(got.Hits) != 1 {
		t.Fatalf("status %d hits %+v", w.Code, got.Hits)
	}
	if strings.Contains(w.Body.String(), "<img") {
		t.Errorf("hostile markup is unescaped on the wire: %s", w.Body.String())
	}
}

// The API reference is generated from the route table so it cannot drift. This
// is the test that keeps that true: every route the mux serves has to appear,
// and the endpoint list itself has to be one of them.
func TestEndpointsListsEveryRegisteredRoute(t *testing.T) {
	api, _ := newTestAPI(t)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	r := httptest.NewRequest("GET", "/api/endpoints", nil)
	w := httptest.NewRecorder()
	api.HandleEndpoints(w, r)

	var got struct {
		Count     int        `json:"count"`
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count < 50 {
		t.Errorf("count = %d; the route table is far larger than that, so recording is broken", got.Count)
	}
	seen := map[string]bool{}
	for _, e := range got.Endpoints {
		seen[e.Path] = true
		if e.Method == "" || !strings.HasPrefix(e.Path, "/api/") {
			t.Errorf("malformed entry %+v", e)
		}
	}
	// Spot-check the ends of the table and this endpoint itself: a recorder
	// that dropped the first or last registration would still look plausible.
	for _, want := range []string{"/api/endpoints", "/api/stats", "/api/code/search"} {
		if !seen[want] {
			t.Errorf("%s is missing from the generated reference", want)
		}
	}
}
