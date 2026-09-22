package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// newShotAPI wires an API against a stub capture service and records what the
// upstream was actually asked for, which is the only thing worth asserting: the
// proxy's whole job is turning a chain-chosen package path into a safe URL.
func newShotAPI(t *testing.T, upstreamHandler http.HandlerFunc) (*API, *string) {
	t.Helper()
	var asked string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		if upstreamHandler != nil {
			upstreamHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		w.Write([]byte("RIFF....WEBP"))
	}))
	t.Cleanup(up.Close)

	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{
		{ID: "alpha", GnowebURL: "https://gno.land"},
		{ID: "nogweb"},
	}
	db.SetConfiguredNetworks(nets)
	api := NewAPI(db, map[string]*indexer.Client{}, nets, analyzer.NewAnalyzer(db))
	api.SetShotUpstream(up.URL)
	return api, &asked
}

func TestShotsAreOffUntilAnUpstreamIsConfigured(t *testing.T) {
	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{{ID: "alpha", GnowebURL: "https://gno.land"}}
	db.SetConfiguredNetworks(nets)
	api := NewAPI(db, map[string]*indexer.Client{}, nets, analyzer.NewAnalyzer(db))

	if api.ShotsEnabled() {
		t.Fatal("shots are enabled with no upstream configured")
	}
	rec := httptest.NewRecorder()
	api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?network=alpha&path=gno.land/r/gov/dao", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The package path comes from the chain and can hold anything a deployer typed.
// It has to reach the upstream as a path and nothing else: a "?" in it must not
// become a query parameter, and "../" must not climb out of the host.
func TestShotBuildsTheUpstreamURLFromAChainChosenPath(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantCode int
		wantURL  string // the url= parameter handed to the capture service
	}{
		{
			name:     "plain realm",
			query:    "network=alpha&path=gno.land/r/gov/dao",
			wantCode: http.StatusOK,
			wantURL:  "https://gno.land/r/gov/dao",
		},
		{
			name:     "path without the gno.land prefix",
			query:    "network=alpha&path=r/gov/dao",
			wantCode: http.StatusOK,
			wantURL:  "https://gno.land/r/gov/dao",
		},
		{
			name:     "a question mark stays in the path",
			query:    "network=alpha&path=" + url.QueryEscape("gno.land/r/x/a?b=c"),
			wantCode: http.StatusOK,
			wantURL:  "https://gno.land/r/x/a%3Fb=c",
		},
		{
			name:     "traversal is refused",
			query:    "network=alpha&path=" + url.QueryEscape("gno.land/r/../../etc"),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "a network with no gnoweb has no pictures",
			query:    "network=nogweb&path=gno.land/r/gov/dao",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "an unknown network has no pictures",
			query:    "network=nope&path=gno.land/r/gov/dao",
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, asked := newShotAPI(t, nil)
			rec := httptest.NewRecorder()
			api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?"+tt.query, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantURL == "" {
				return
			}
			q, err := url.ParseQuery(strings.SplitN(*asked, "?", 2)[1])
			if err != nil {
				t.Fatal(err)
			}
			if got := q.Get("url"); got != tt.wantURL {
				t.Fatalf("upstream url = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

// mode and size are a closed set. Passing them through would let a page ask the
// capture service for anything at all, and the service is the expensive half.
func TestShotValidatesModeAndSize(t *testing.T) {
	tests := []struct {
		query    string
		wantCode int
		wantMode string
		wantSize string
		wantDPR  string
	}{
		{"network=alpha&path=r/x/y", http.StatusOK, "render", "thumb", "2"},
		{"network=alpha&path=r/x/y&mode=page&size=og&dpr=1", http.StatusOK, "page", "og", "1"},
		{"network=alpha&path=r/x/y&size=hero&dpr=7", http.StatusOK, "render", "hero", "2"},
		{"network=alpha&path=r/x/y&mode=whatever", http.StatusBadRequest, "", "", ""},
		{"network=alpha&path=r/x/y&size=enormous", http.StatusBadRequest, "", "", ""},
		{"network=alpha&path=r/x/y&size=avatar", http.StatusBadRequest, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			api, asked := newShotAPI(t, nil)
			rec := httptest.NewRecorder()
			api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?"+tt.query, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode != http.StatusOK {
				return
			}
			q, _ := url.ParseQuery(strings.SplitN(*asked, "?", 2)[1])
			if q.Get("mode") != tt.wantMode || q.Get("size") != tt.wantSize || q.Get("dpr") != tt.wantDPR {
				t.Fatalf("upstream got mode=%q size=%q dpr=%q, want %q %q %q",
					q.Get("mode"), q.Get("size"), q.Get("dpr"), tt.wantMode, tt.wantSize, tt.wantDPR)
			}
		})
	}
}

// v is the caller's cache key, not the proxy's. It has to survive the hop,
// because it is the only thing that makes the response safe to mark immutable.
func TestShotPassesTheCacheKeyThrough(t *testing.T) {
	api, asked := newShotAPI(t, nil)
	rec := httptest.NewRecorder()
	api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?network=alpha&path=r/x/y&v=12345", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	q, _ := url.ParseQuery(strings.SplitN(*asked, "?", 2)[1])
	if q.Get("v") != "12345" {
		t.Fatalf("v = %q, want 12345", q.Get("v"))
	}
}

func TestShotForwardsTheHeadersAConsumerNeeds(t *testing.T) {
	api, _ := newShotAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"abc-thumb@2x"`)
		w.Header().Set("X-Gnoshot-Matched", "directory")
		w.Header().Set("X-Gnoshot-Truncated", "1")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("image"))
	})
	rec := httptest.NewRecorder()
	api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?network=alpha&path=r/x/y", nil))
	for k, want := range map[string]string{
		"Content-Type":        "image/webp",
		"Cache-Control":       "public, max-age=31536000, immutable",
		"ETag":                `"abc-thumb@2x"`,
		"X-Gnoshot-Matched":   "directory",
		"X-Gnoshot-Truncated": "1",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if rec.Body.String() != "image" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// A capture service that is down is a decoration that is missing, not an
// explorer that is broken. A 500 here would make a listing look like a failure
// over a picture.
func TestShotAnswers503WhenTheCaptureServiceIsDown(t *testing.T) {
	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{{ID: "alpha", GnowebURL: "https://gno.land"}}
	db.SetConfiguredNetworks(nets)
	api := NewAPI(db, map[string]*indexer.Client{}, nets, analyzer.NewAnalyzer(db))
	// A port nothing is listening on, and a literal address so no DNS is
	// involved in a unit test.
	api.SetShotUpstream("http://127.0.0.1:1")

	rec := httptest.NewRecorder()
	api.HandleShot(rec, httptest.NewRequest("GET", "/api/shot?network=alpha&path=r/x/y", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: an outage must not be cached", got)
	}
}

func TestShotMetaProxiesTheManifest(t *testing.T) {
	api, asked := newShotAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"matched":"realm","truncated":true}`))
	})
	rec := httptest.NewRecorder()
	api.HandleShotMeta(rec, httptest.NewRequest("GET", "/api/shot/meta?network=alpha&path=r/gov/dao", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.HasPrefix(*asked, "/meta?") {
		t.Fatalf("upstream path = %q, want /meta", *asked)
	}
	if !strings.Contains(rec.Body.String(), `"truncated":true`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// The default network is the first one that can actually be photographed, not
// just the first one configured.
func TestShotDefaultsToTheFirstNetworkWithAGnoweb(t *testing.T) {
	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{{ID: "nogweb"}, {ID: "alpha", GnowebURL: "https://gno.land"}}
	db.SetConfiguredNetworks(nets)
	api := NewAPI(db, map[string]*indexer.Client{}, nets, analyzer.NewAnalyzer(db))
	if got := defaultShotNetwork(api); got != "alpha" {
		t.Fatalf("defaultShotNetwork = %q, want alpha", got)
	}
}
