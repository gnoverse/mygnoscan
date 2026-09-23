package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moul/mygnoscan/pkg/store"
)

func seedDirectory(t *testing.T, db *store.DB) {
	t.Helper()
	rows := []struct {
		path    string
		isRealm bool
	}{
		{"gno.land/r/gnoswap/gns", true},
		{"gno.land/r/gnoswap/router", true},
		{"gno.land/p/gnoswap/uint256", false},
		{"gno.land/r/moul/home", true},
		{"gno.land/p/nt/ufmt/v0", false},
	}
	for i, r := range rows {
		name := r.path[len(r.path)-3:]
		if err := db.UpsertPackage("alpha", r.path, name, "g1creator", "tx"+name,
			100+i, "2026-09-01T00:00:00Z", r.isRealm, 1); err != nil {
			t.Fatal(err)
		}
	}
	// One package with a symbol count, so the symbols sort has something to
	// order by and the column has something to show.
	if err := db.ReplaceSymbols("alpha", "gno.land/p/nt/ufmt/v0", "k", "", []store.SymbolRow{
		{Kind: "func", Name: "Sprintf", Exported: true},
		{Kind: "func", Name: "Println", Exported: true},
		{Kind: "func", Name: "Errorf", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func listPaths(t *testing.T, api *API, query string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	api.HandlePackages(rec, httptest.NewRequest("GET", "/api/packages?"+query, nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/packages?%s = %d: %s", query, rec.Code, rec.Body.String())
	}
	var body struct {
		Items []store.PackageInfo `json:"items"`
		Total int                 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, it.Path)
	}
	return out
}

// The existing routes have to keep answering what they always answered. A
// facet parameter widens the directory; its absence must not.
func TestPackageRoutesKeepTheirOldMeaning(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	rec := httptest.NewRecorder()
	api.HandleRealms(rec, httptest.NewRequest("GET", "/api/realms?network=alpha", nil))
	var realms struct {
		Items []store.PackageInfo `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &realms)
	if len(realms.Items) != 3 {
		t.Fatalf("/api/realms returned %d rows, want the 3 realms", len(realms.Items))
	}

	if got := listPaths(t, api, "network=alpha"); len(got) != 2 {
		t.Fatalf("/api/packages with no parameters returned %d rows, want the 2 pure packages: %v", len(got), got)
	}
}

// /api/realms pins its kind. A query string must not be able to turn it into a
// list of pure packages, which is the shape of bug a shared handler invites.
func TestRealmsRouteIgnoresTheKindParameter(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	rec := httptest.NewRecorder()
	api.HandleRealms(rec, httptest.NewRequest("GET", "/api/realms?network=alpha&kind=pure", nil))
	var body struct {
		Items []store.PackageInfo `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != 3 {
		t.Fatalf("returned %d rows, want the 3 realms regardless of ?kind=", len(body.Items))
	}
	for _, it := range body.Items {
		if !it.IsRealm {
			t.Fatalf("%s is not a realm", it.Path)
		}
	}
}

func TestPackagesKindFacet(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	for _, tt := range []struct {
		query string
		want  int
	}{
		{"network=alpha&kind=all", 5},
		{"network=alpha&kind=realm", 3},
		{"network=alpha&kind=pure", 2},
	} {
		if got := listPaths(t, api, tt.query); len(got) != tt.want {
			t.Errorf("%s returned %d rows, want %d: %v", tt.query, len(got), tt.want, got)
		}
	}
}

// An unknown kind is refused rather than quietly meaning "everything". stdlib
// is the specific one worth refusing: it is a real facet that has not shipped,
// so answering it with the full directory would be a lie with a plausible name.
func TestPackagesRefusesAnUnknownKind(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)
	for _, kind := range []string{"stdlib", "REALM", "nonsense"} {
		rec := httptest.NewRecorder()
		api.HandlePackages(rec, httptest.NewRequest("GET", "/api/packages?network=alpha&kind="+kind, nil))
		if rec.Code != 400 {
			t.Errorf("kind=%s returned %d, want 400", kind, rec.Code)
		}
	}
}

func TestPackagesNamespaceFacet(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	got := listPaths(t, api, "network=alpha&namespace=gnoswap")
	if len(got) != 3 {
		t.Fatalf("namespace=gnoswap returned %d rows, want 3: %v", len(got), got)
	}
	// Both halves of the namespace, not just the realms: a namespace is an
	// owner, and its libraries are as much theirs as its realms.
	var realms, pure int
	for _, p := range got {
		if p == "gno.land/p/gnoswap/uint256" {
			pure++
		} else {
			realms++
		}
	}
	if realms != 2 || pure != 1 {
		t.Fatalf("namespace=gnoswap split %d realms / %d pure, want 2/1", realms, pure)
	}

	// And the two facets compose.
	if got := listPaths(t, api, "network=alpha&namespace=gnoswap&kind=pure"); len(got) != 1 {
		t.Fatalf("namespace=gnoswap&kind=pure returned %v, want one row", got)
	}
	if got := listPaths(t, api, "network=alpha&namespace=nobody"); len(got) != 0 {
		t.Fatalf("an unknown namespace returned %v, want nothing", got)
	}
}

// Sorting by what a package declares is the only ordering that ranks the pure
// half at all: gas, calls and unique users are zero for every library by
// construction.
func TestPackagesSortBySymbols(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	got := listPaths(t, api, "network=alpha&kind=all&sort=symbols")
	if len(got) == 0 {
		t.Fatal("no rows")
	}
	if got[0] != "gno.land/p/nt/ufmt/v0" {
		t.Fatalf("first row by symbols is %q, want the only package with any", got[0])
	}
}

func TestPackageInfoCarriesItsSymbolCount(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	rec := httptest.NewRecorder()
	api.HandlePackages(rec, httptest.NewRequest("GET", "/api/packages?network=alpha&kind=all", nil))
	var body struct {
		Items []store.PackageInfo `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	found := false
	for _, it := range body.Items {
		if it.Path == "gno.land/p/nt/ufmt/v0" {
			if it.Symbols != 3 {
				t.Fatalf("symbols = %d, want 3", it.Symbols)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the indexed package is missing from the listing")
	}
}

func TestPackageFacetsEndpoint(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)

	rec := httptest.NewRecorder()
	api.HandlePackageFacets(rec, httptest.NewRequest("GET", "/api/packages/facets?network=alpha", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Kinds      store.KindFacet        `json:"kinds"`
		Namespaces []store.NamespaceFacet `json:"namespaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kinds.All != 5 || body.Kinds.Realm != 3 || body.Kinds.Pure != 2 {
		t.Fatalf("kinds = %+v", body.Kinds)
	}
	if len(body.Namespaces) != 3 || body.Namespaces[0].Namespace != "gnoswap" {
		t.Fatalf("namespaces = %+v, want the biggest first", body.Namespaces)
	}
}

// An empty directory must answer with an empty array rather than a JSON null,
// or every caller has to guard before iterating and the one that forgets throws.
func TestPackageFacetsAnswersEmptyRatherThanNull(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandlePackageFacets(rec, httptest.NewRequest("GET", "/api/packages/facets?network=alpha", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !jsonHasEmptyArray(rec.Body.String(), "namespaces") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPackageFacetsRoutesAreRegistered(t *testing.T) {
	api, db := newTestAPI(t)
	seedDirectory(t, db)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{
		"/api/packages/facets?network=alpha",
		"/api/packages?network=alpha&kind=all",
		"/api/packages?network=alpha&namespace=gnoswap",
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
	}
}

// Every sort key the SQL understands has to be understood by the merge too.
//
// Three were not, and the failure is silent: the all-networks view asked for
// "most gas", each network's rows came back sorted by gas, and the merge then
// re-sorted the lot by block time. The per-network view was right the whole
// time, which is exactly why nobody noticed.
//
// Asserted against sortMergedPackages directly rather than through the handler,
// because the merge path fans out over indexer clients and a test API has none.
func TestEverySortKeyIsHonouredByTheMerge(t *testing.T) {
	// Block time runs opposite to every metric, so a key that falls through to
	// the default ordering produces exactly the reverse of what was asked for.
	rows := []store.PackageInfo{
		{Path: "a", BlockTime: "2026-09-03T00:00:00Z", BlockHeight: 3, Calls: 1, Importers: 1, Imports: 1, UniqueUsers: 1, GasUsed: 1, StorageDeposit: 1, Symbols: 1},
		{Path: "b", BlockTime: "2026-09-02T00:00:00Z", BlockHeight: 2, Calls: 2, Importers: 2, Imports: 2, UniqueUsers: 2, GasUsed: 2, StorageDeposit: 2, Symbols: 2},
		{Path: "c", BlockTime: "2026-09-01T00:00:00Z", BlockHeight: 1, Calls: 3, Importers: 3, Imports: 3, UniqueUsers: 3, GasUsed: 3, StorageDeposit: 3, Symbols: 3},
	}
	metric := map[string]func(store.PackageInfo) int{
		"calls":     func(p store.PackageInfo) int { return p.Calls },
		"importers": func(p store.PackageInfo) int { return p.Importers },
		"imports":   func(p store.PackageInfo) int { return p.Imports },
		"users":     func(p store.PackageInfo) int { return p.UniqueUsers },
		"gas":       func(p store.PackageInfo) int { return p.GasUsed },
		"storage":   func(p store.PackageInfo) int { return p.StorageDeposit },
		"symbols":   func(p store.PackageInfo) int { return p.Symbols },
	}
	for key, read := range metric {
		t.Run(key, func(t *testing.T) {
			got := append([]store.PackageInfo(nil), rows...)
			sortMergedPackages(got, key)
			for i := 1; i < len(got); i++ {
				if read(got[i]) > read(got[i-1]) {
					t.Fatalf("sort=%s left %s (%d) after %s (%d)",
						key, got[i].Path, read(got[i]), got[i-1].Path, read(got[i-1]))
				}
			}
		})
	}
}
