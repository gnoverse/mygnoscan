package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moul/mygnoscan/pkg/store"
)

// muxGET routes through the real mux rather than calling a handler directly,
// because these endpoints read r.PathValue and a handler invoked on its own
// sees an empty path. It is also the only way to test that
// /api/realm/cousage/... does not get swallowed by /api/realm/{path...}.
func muxGET(t *testing.T, api *API, url string, out any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
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

// seedContractsAPI's alpha world: alice and carol called both one and two, bob
// called one only. So one's single partner is two, shared by the two addresses
// that used both, and bob still counts toward one's own caller total.
func TestHandleRealmCoUsage(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	var got store.RealmCoUsage
	muxGET(t, api, "/api/realm/cousage/r/ns/one?network=alpha", &got)

	if got.Path != "gno.land/r/ns/one" {
		t.Errorf("path = %q, want gno.land/r/ns/one", got.Path)
	}
	if got.Network != "alpha" || got.Window != "all" {
		t.Errorf("network/window = %q/%q, want alpha/all", got.Network, got.Window)
	}
	if got.Callers != 3 {
		t.Errorf("callers = %d, want 3 (alice, carol, bob)", got.Callers)
	}
	if len(got.Partners) != 1 {
		t.Fatalf("partners = %+v, want exactly one (r/ns/two)", got.Partners)
	}
	p := got.Partners[0]
	if p.Path != "gno.land/r/ns/two" || p.Shared != 2 || p.Callers != 2 {
		t.Errorf("partner = %+v, want gno.land/r/ns/two shared 2 of its own 2", p)
	}
}

// The same path is deployed on alpha and beta and alice called it on both.
// Blending them would report beta's realm as sharing callers with a contract
// beta has never seen.
func TestHandleRealmCoUsageIsPerNetwork(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	var got store.RealmCoUsage
	muxGET(t, api, "/api/realm/cousage/r/ns/one?network=beta", &got)

	if got.Network != "beta" {
		t.Fatalf("network = %q, want beta", got.Network)
	}
	if got.Callers != 1 {
		t.Errorf("callers = %d, want 1 (alice)", got.Callers)
	}
	if len(got.Partners) != 0 {
		t.Errorf("partners = %+v, want none on beta", got.Partners)
	}
}

// An absent network resolves to the first configured chain and says so, rather
// than blending every chain's callers. Same rule as the contracts map.
func TestHandleRealmCoUsageDefaultsToOneNetwork(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	var got store.RealmCoUsage
	muxGET(t, api, "/api/realm/cousage/r/ns/one", &got)
	if got.Network != "alpha" {
		t.Errorf("network = %q, want alpha (the first configured one)", got.Network)
	}
}

func TestHandleRealmCoUsageWindow(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	// The fixture's calls are 48 hours old, so a 24h window empties it. A
	// window that quietly widened to all time would look identical to a
	// working one on any realm with recent traffic, which is why the endpoint
	// rejects an unknown name instead of falling back.
	var got store.RealmCoUsage
	muxGET(t, api, "/api/realm/cousage/r/ns/one?network=alpha&window=24h", &got)
	if got.Window != "24h" {
		t.Errorf("window = %q, want 24h", got.Window)
	}
	if got.Callers != 0 || len(got.Partners) != 0 {
		t.Errorf("24h = %d callers, %+v partners; the fixture is 48h old so both should be empty",
			got.Callers, got.Partners)
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/realm/cousage/r/ns/one?network=alpha&window=fortnight", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown window: status %d, want 400", rec.Code)
	}
}

// Route precedence: the wildcard /api/realm/{path...} must not swallow this.
// Go 1.22 resolves it by specificity, and the realm handler answering here
// would return a package detail shaped nothing like a co-usage response.
func TestRealmCoUsageRouteBeatsRealmWildcard(t *testing.T) {
	api, db := newTestAPI(t)
	seedContractsAPI(t, db)

	rec := muxGET(t, api, "/api/realm/cousage/r/ns/one?network=alpha", nil)
	var probe map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := probe["partners"]; !ok {
		t.Fatalf("no partners key: the realm wildcard answered instead, body %s", rec.Body.String())
	}
	if _, ok := probe["num_files"]; ok {
		t.Error("num_files present: this is a package detail, so the wildcard won")
	}
}
