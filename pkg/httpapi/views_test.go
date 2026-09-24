package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// The counter has to sit outside the response cache, and this is the test that
// says why: a cached answer never reaches the handler, so a counter deeper in
// the stack would count the first reader of a realm and miss everyone after.
// The more a realm was read, the less it would appear to be read.
func TestViewsAreCountedOnCacheHitsToo(t *testing.T) {
	_, db := newTestAPI(t)
	v := NewViewCounter(db)

	handlerCalls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"gno.land/r/x/a"}`))
	})
	cache := NewResponseCache(time.Minute)
	stack := WithRealmViews(v, WithResponseCache(cache, inner))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/realm/r/x/a?network=alpha", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		stack.ServeHTTP(httptest.NewRecorder(), req)
	}
	v.Flush()

	got, err := db.RealmViewsFor("alpha", []string{"gno.land/r/x/a"}, "")
	if err != nil {
		t.Fatalf("RealmViewsFor: %v", err)
	}
	if got["gno.land/r/x/a"] != 3 {
		t.Errorf("views = %d, want 3: every read counts, not only the ones that miss the cache",
			got["gno.land/r/x/a"])
	}
	if handlerCalls >= 3 {
		t.Skip("the cache did not serve a hit, so this test proved nothing")
	}
}

// A page open is one open. The realm page fires several requests and counting
// all of them would rank a realm by how many tabs it has.
func TestOnlyTheRealmDetailCounts(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"/api/realm/r/x/a", true},
		{"/api/realm/r/x/a/v2", true},
		{"/api/realm/usage/r/x/a", false},
		{"/api/realm/defi/r/x/a", false},
		{"/api/realm/cousage/r/x/a", false},
		{"/api/realms", false},
		{"/api/realm/", false},
	} {
		if got := isRealmDetailPath(tt.path); got != tt.want {
			t.Errorf("%s: counted=%v, want %v", tt.path, got, tt.want)
		}
	}
}

// The readers that are not readers. The warmer is the one that would have gone
// unnoticed: it replays real request paths through the real handler stack, so
// it would vote for whatever it warms.
func TestRobotsDoNotVote(t *testing.T) {
	for _, ua := range []string{
		"mygnoscan-warmer", "Googlebot/2.1", "curl/8.4.0", "Go-http-client/1.1",
		"HeadlessChrome/120", "", "python-requests/2.31",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/realm/r/x/a", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if !isRobot(req) {
			t.Errorf("%q counted as a reader", ua)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/realm/r/x/a", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/131 Safari/537.36")
	if isRobot(req) {
		t.Error("a browser was dropped as a robot")
	}
}

// Flushing has to survive a restart, because a deploy restarts this process
// several times a day and dropping the buffer each time would bias the counts
// toward whatever nobody was reading at deploy time.
func TestRunFlushesOnShutdown(t *testing.T) {
	_, db := newTestAPI(t)
	v := NewViewCounter(db)
	v.Record("alpha", "gno.land/r/x/b", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { v.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	got, _ := db.RealmViewsFor("alpha", []string{"gno.land/r/x/b"}, "")
	if got["gno.land/r/x/b"] != 1 {
		t.Error("the buffer was dropped on shutdown")
	}
}

// Yesterday's reads must not fall inside a 24h window just because the sum is
// taken over whole days.
func TestViewsSinceBuckets(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if got := store.ViewsSince("24h", now); got != "2026-09-24" {
		t.Errorf("24h -> %q", got)
	}
	if got := store.ViewsSince("", now); got != "" {
		t.Errorf("no window -> %q, want every day", got)
	}
}
