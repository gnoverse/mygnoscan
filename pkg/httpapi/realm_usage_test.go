package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// /api/realm/usage/{path...} has to beat the /api/realm/{path...} wildcard it
// sits under, and its filters have to reach the aggregates and not only the
// feed. Both are things a unit test on the store cannot see.
func TestHandleRealmUsage(t *testing.T) {
	api, db := newTestAPI(t)
	const path = "gno.land/r/alpha/rumble"
	when := time.Now().UTC().Format(time.RFC3339Nano)
	if err := db.UpsertPackage("alpha", path, "rumble", "g1alice", "TXD", 100, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	calls := []struct {
		tx     string
		caller string
		fn     string
		ok     bool
	}{
		{"TX1", "g1alice", "Bid", true},
		{"TX2", "g1bob", "Bid", true},
		{"TX3", "g1bob", "Claim", false},
	}
	for i, c := range calls {
		if err := db.InsertCall("alpha", c.tx, 110+i, 0, when, c.caller, path, c.fn, c.ok); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name       string
		target     string
		wantMsgs   int
		wantUnique int
		wantRows   int
	}{
		{"no filter", "", 3, 2, 3},
		{"by function", "&func=Bid", 2, 2, 2},
		{"by caller", "&caller=g1bob", 2, 1, 2},
		{"failures only", "&status=fail", 1, 1, 1},
		{"paging does not move the aggregates", "&limit=1", 3, 2, 1},
		{"an unknown status is no filter at all", "&status=banana", 3, 2, 3},
		{"a window that predates every row keeps them", "&window=90d", 3, 2, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/realm/usage/r/alpha/rumble?network=alpha"+tt.target, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var got store.RealmUsage
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if got.Summary.Messages != tt.wantMsgs {
				t.Errorf("messages = %d, want %d", got.Summary.Messages, tt.wantMsgs)
			}
			if got.Summary.UniqueCallers != tt.wantUnique {
				t.Errorf("unique callers = %d, want %d", got.Summary.UniqueCallers, tt.wantUnique)
			}
			if len(got.Rows) != tt.wantRows {
				t.Errorf("rows = %d, want %d", len(got.Rows), tt.wantRows)
			}
		})
	}
}

// "usage" is a path element the realm wildcard would otherwise swallow.
func TestRealmUsageRouteBeatsTheWildcard(t *testing.T) {
	api, _ := newTestAPI(t)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/realm/usage/r/alpha/nope?network=alpha", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// Both handlers 404 an unknown path, so the body is what tells them apart:
	// HandleRealm says "package not found: gno.land/usage/r/alpha/nope".
	if body := rec.Body.String(); rec.Code == 404 && !strings.Contains(body, "gno.land/r/alpha/nope") {
		t.Errorf("the wildcard answered instead of the usage route: %s", body)
	}
}
