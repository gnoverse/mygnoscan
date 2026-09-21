package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

func seedContractsAPI(t *testing.T, db *store.DB) {
	t.Helper()
	base := time.Now().UTC().Add(-48 * time.Hour)

	pkgs := []struct {
		network string
		path    string
		realm   bool
	}{
		{"alpha", "gno.land/r/ns/one", true},
		{"alpha", "gno.land/r/ns/two", true},
		{"alpha", "gno.land/p/lib/util", false},
		{"beta", "gno.land/r/ns/one", true},
	}
	for i, p := range pkgs {
		if err := db.UpsertPackage(p.network, p.path, "n", "g1creator", "TX", 10+i,
			base.Format(time.RFC3339), p.realm, 1); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	// alice and carol both use one and two, so that pair has two addresses in
	// common and survives the default min weight. bob uses one only and links
	// nothing. The beta row repeats a path and an address that exist on alpha,
	// which is what a cross-network leak would surface as.
	calls := []struct{ network, caller, path string }{
		{"alpha", "g1alice", "gno.land/r/ns/one"},
		{"alpha", "g1alice", "gno.land/r/ns/two"},
		{"alpha", "g1carol", "gno.land/r/ns/one"},
		{"alpha", "g1carol", "gno.land/r/ns/two"},
		{"alpha", "g1bob", "gno.land/r/ns/one"},
		{"beta", "g1alice", "gno.land/r/ns/one"},
	}
	for i, c := range calls {
		store.MustCall(t, db, c.network, "TXC"+string(rune('a'+i)), 100+i,
			base.Add(time.Duration(i)*time.Minute), c.caller, c.path, "Fn")
	}
	if err := db.SetDependencies("alpha", "gno.land/r/ns/one", []string{"gno.land/p/lib/util"}); err != nil {
		t.Fatalf("deps: %v", err)
	}
}

func getJSON(t *testing.T, h http.HandlerFunc, url string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", url, rec.Code, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v (body %s)", url, err, rec.Body.String())
		}
	}
	return rec
}

type mapResponse struct {
	Network string               `json:"network"`
	Window  string               `json:"window"`
	Nodes   []store.ContractNode `json:"nodes"`
}

type edgesResponse struct {
	Network string               `json:"network"`
	Kind    string               `json:"kind"`
	Edges   []store.ContractEdge `json:"edges"`
}

func TestHandleContractsMap(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	var got mapResponse
	getJSON(t, api.HandleContractsMap, "/api/contracts/map?network=alpha", &got)

	if got.Network != "alpha" {
		t.Errorf("network = %q, want alpha", got.Network)
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (beta must not leak in)", len(got.Nodes))
	}
	for _, n := range got.Nodes {
		if n.Namespace == "" {
			t.Errorf("node %s has no namespace, so it cannot be clustered or coloured", n.Path)
		}
	}
}

// A map that silently merges two chains is the failure mode the store
// invariant exists to prevent, and the handler is where it would be
// reintroduced: networkParam turns an absent network into "", which every
// other endpoint reads as "all networks".
func TestHandleContractsMapResolvesAllToOneNetwork(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	for _, url := range []string{"/api/contracts/map", "/api/contracts/map?network=all"} {
		var got mapResponse
		getJSON(t, api.HandleContractsMap, url, &got)
		if got.Network != "alpha" {
			t.Errorf("%s: network = %q, want the first configured network", url, got.Network)
		}
		if len(got.Nodes) != 3 {
			t.Errorf("%s: nodes = %d, want 3", url, len(got.Nodes))
		}
	}
}

func TestHandleContractsMapWindow(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	var all, day mapResponse
	getJSON(t, api.HandleContractsMap, "/api/contracts/map?network=alpha&window=all", &all)
	getJSON(t, api.HandleContractsMap, "/api/contracts/map?network=alpha&window=24h", &day)

	if day.Window != "24h" {
		t.Errorf("window echoed as %q, want 24h", day.Window)
	}
	if len(day.Nodes) != len(all.Nodes) {
		t.Errorf("window changed the node count (%d vs %d): it must narrow metrics only",
			len(day.Nodes), len(all.Nodes))
	}
	// Everything was seeded 48h ago, so a 24h window sees no calls at all.
	for _, n := range day.Nodes {
		if n.Calls != 0 {
			t.Errorf("%s: %d calls inside a 24h window over 48h-old data", n.Path, n.Calls)
		}
	}
}

func TestHandleContractsMapRejectsUnknownWindow(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	rec := httptest.NewRecorder()
	api.HandleContractsMap(rec, httptest.NewRequest(http.MethodGet, "/api/contracts/map?network=alpha&window=17y", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an unparsed window would silently mean all time", rec.Code)
	}
}

func TestHandleContractsEdges(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantKind  string
		wantCount int
	}{
		{"caller edges are the default", "/api/contracts/edges?network=alpha", "callers", 1},
		{"explicit callers", "/api/contracts/edges?network=alpha&kind=callers", "callers", 1},
		{"imports", "/api/contracts/edges?network=alpha&kind=imports", "imports", 1},
		{"min above every weight empties the map", "/api/contracts/edges?network=alpha&min=5", "callers", 0},
		{"min=1 admits the single-address overlaps too", "/api/contracts/edges?network=alpha&min=1", "callers", 1},
		{"limit truncates", "/api/contracts/edges?network=alpha&limit=0", "callers", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, db := newTestAPI(t)
			seedContractsAPI(t, db)

			var got edgesResponse
			getJSON(t, api.HandleContractsEdges, tt.url, &got)
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if len(got.Edges) != tt.wantCount {
				t.Errorf("edges = %d, want %d: %+v", len(got.Edges), tt.wantCount, got.Edges)
			}
		})
	}
}

func TestHandleContractsEdgesRejectsUnknownKind(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	rec := httptest.NewRecorder()
	api.HandleContractsEdges(rec, httptest.NewRequest(http.MethodGet, "/api/contracts/edges?network=alpha&kind=telepathy", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// A limit or fanout from the query string reaches a query that generates pairs.
// Left unclamped, one request can ask the server to build every pair on the
// chain and hold them all in memory.
func TestHandleContractsEdgesClampsBounds(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	for _, url := range []string{
		"/api/contracts/edges?network=alpha&limit=999999999",
		"/api/contracts/edges?network=alpha&maxFanout=999999999",
		"/api/contracts/edges?network=alpha&limit=-5&min=-5&maxFanout=-5",
	} {
		var got edgesResponse
		getJSON(t, api.HandleContractsEdges, url, &got)
		if len(got.Edges) > maxContractEdgeLimit {
			t.Errorf("%s: returned %d edges, over the %d cap", url, len(got.Edges), maxContractEdgeLimit)
		}
	}
}

func TestParseContractWindow(t *testing.T) {
	tests := []struct {
		in     string
		wantOK bool
		zero   bool
	}{
		{"", true, true},
		{"all", true, true},
		{"24h", true, false},
		{"7d", true, false},
		{"30d", true, false},
		// 90d was rejected until the activity filter needed the map's window
		// vocabulary to match the dashboards', which have always offered it.
		{"90d", true, false},
		{"1y", false, true},
		{"nonsense", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseContractWindow(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got.IsZero() != tt.zero {
				t.Errorf("zero = %v, want %v", got.IsZero(), tt.zero)
			}
		})
	}
}
