package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moul/mygnoscan/pkg/syncer"
)

type sanityPayload struct {
	Sync     map[string]syncer.NetworkHealth `json:"sync"`
	Indexers map[string]struct {
		Active string   `json:"active"`
		Pool   []string `json:"pool"`
	} `json:"indexers"`
}

func sanityGet(t *testing.T, api *API, url string) sanityPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	api.HandleSanityOverview(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, rec.Code, rec.Body.String())
	}
	var got sanityPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got
}

// The failure this endpoint exists to surface: a chain that is alive and
// reachable while every sync pass against it fails.
func TestSanityOverviewReportsSyncFailures(t *testing.T) {
	api, _ := newTestAPI(t)
	reg := syncer.NewRegistry()
	reg.Record("alpha", errors.New(`indexer returned 422: Unknown type "MsgCreateSession"`))
	reg.Record("beta", nil)
	api.SetSyncHealth(reg)

	got := sanityGet(t, api, "/api/sanity/overview")

	alpha, ok := got.Sync["alpha"]
	if !ok {
		t.Fatal("alpha missing from the sync section")
	}
	if alpha.Healthy {
		t.Error("alpha reported healthy while its passes are failing")
	}
	if alpha.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", alpha.ConsecutiveFailures)
	}
	if alpha.LastError == "" {
		t.Error("no error text: a reader cannot act on 'something failed'")
	}
	if !got.Sync["beta"].Healthy {
		t.Error("beta should be healthy")
	}
}

// Selecting one network should not report the others' sync state: the page is
// showing that chain.
func TestSanityOverviewScopesSyncToTheSelectedNetwork(t *testing.T) {
	api, _ := newTestAPI(t)
	reg := syncer.NewRegistry()
	reg.Record("alpha", errors.New("boom"))
	reg.Record("beta", nil)
	api.SetSyncHealth(reg)

	got := sanityGet(t, api, "/api/sanity/overview?network=beta")
	if _, leaked := got.Sync["alpha"]; leaked {
		t.Error("alpha's sync state leaked into a beta-only request")
	}
	if !got.Sync["beta"].Healthy {
		t.Error("beta's own entry is missing")
	}
}

// A process with no sync loop, which is every test binary and any instance run
// with -sync=false, must not be reported as broken.
func TestSanityOverviewWithoutASyncLoop(t *testing.T) {
	api, _ := newTestAPI(t)

	rec := httptest.NewRecorder()
	api.HandleSanityOverview(rec, httptest.NewRequest(http.MethodGet, "/api/sanity/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// Absent, not present-and-unhealthy: nothing has been attempted, so there
	// is nothing to report.
	if body := rec.Body.String(); strings.Contains(body, `"sync":`) {
		t.Errorf("sync section present with no registry: %s", body)
	}
}
