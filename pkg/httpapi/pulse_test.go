package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/gnoaddr"
	"github.com/moul/mygnoscan/pkg/web"
)

func TestResolvePulseWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
		dur  time.Duration
	}{
		{"an offered key is used", "1h", "1h", time.Hour},
		{"days are offered too", "7d", "7d", 7 * 24 * time.Hour},
		{"an empty key is the default", "", "24h", 24 * time.Hour},
		// A hand-edited URL should land somewhere sensible rather than on a
		// 400, the same rule the directory sort follows.
		{"an unknown key falls back", "42y", "24h", 24 * time.Hour},
		{"a duration that is not offered falls back", "90m", "24h", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePulseWindow(tc.key)
			if got.Key != tc.want || got.D != tc.dur {
				t.Errorf("resolvePulseWindow(%q) = %q/%v, want %q/%v", tc.key, got.Key, got.D, tc.want, tc.dur)
			}
		})
	}
}

func TestPulseEndpoint(t *testing.T) {
	api, db := newTestAPI(t)

	realm := "gno.land/r/alpha/pool"
	when := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)

	if err := db.UpsertPackage("alpha", realm, "pool", "g1dev", "deploy", 10, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.InsertPackageSubmission("alpha", "deploy", 0, realm, "pool", "g1dev", 10, when, true, 1, true); err != nil {
		t.Fatalf("InsertPackageSubmission: %v", err)
	}
	if err := db.InsertCall("alpha", "call-1", 11, 0, when, "g1human", realm, "Swap", true); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	// The realm pays a human out of its banker account, which is the case the
	// whole reverse derivation exists for.
	if err := db.InsertBankSend("alpha", "payout", 12, when,
		gnoaddr.Derive(realm), "g1human", "4000000ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
	// Outside every offered window.
	if err := db.InsertBankSend("alpha", "ancient", 1, old, "g1human", "g1other", "9ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}

	rec, body := get(t, api.HandlePulse, "/api/pulse?network=alpha&window=24h")
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, body)
	}
	var resp pulseResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}

	if resp.Window.Key != "24h" || resp.Window.Seconds != 86400 {
		t.Errorf("window = %+v, want the 24h one", resp.Window)
	}
	// The boundaries are on the page because this endpoint is cached and may be
	// served stale: without them "last 24 hours" cannot be checked.
	if resp.Window.Since == "" || resp.Window.Until == "" || resp.Window.PrevSince == "" {
		t.Errorf("window boundaries incomplete: %+v", resp.Window)
	}
	if len(resp.Window.Options) != len(pulseWindows) {
		t.Errorf("window options = %d, want the server's whole list", len(resp.Window.Options))
	}
	if resp.Current.Calls != 1 || resp.Current.Sends != 1 {
		t.Errorf("current counts = %+v, want 1 call and 1 send (the 40-day-old send is out)", resp.Current)
	}
	if !resp.HasPrev {
		t.Error("HasPrev = false")
	}

	if len(resp.HotRealms) != 1 || resp.HotRealms[0].Path != realm {
		t.Errorf("hot realms = %+v, want the one called realm", resp.HotRealms)
	}

	var payout *pulseFlow
	for i := range resp.HotFlows {
		if resp.HotFlows[i].TxHash == "payout" {
			payout = &resp.HotFlows[i]
		}
	}
	if payout == nil {
		t.Fatalf("the payout is missing from %d flows", len(resp.HotFlows))
	}
	if payout.FromPath != realm {
		t.Errorf("flow from_path = %q, want %q: a realm's banker must resolve back to its path",
			payout.FromPath, realm)
	}
	if payout.FromDeposit {
		t.Error("the banker account was reported as the storage deposit account")
	}
	// The other end belongs to no package, which is what "a human" means here.
	// Claiming otherwise is the one answer worse than saying nothing.
	if payout.ToPath != "" {
		t.Errorf("flow to_path = %q, want empty for an address no package owns", payout.ToPath)
	}
	if !payout.Native || payout.Value != 4000000 {
		t.Errorf("flow = %+v, want a native 4000000 ugnot move", payout.HotFlow)
	}
}

// An unknown window is not an error, and neither is a chain with nothing in it:
// both have to render, and a nil slice would throw in the frontend's `for ... of`.
func TestPulseEmptyChainReturnsEmptyLists(t *testing.T) {
	api, _ := newTestAPI(t)

	rec, body := get(t, api.HandlePulse, "/api/pulse?network=beta&window=nonsense")
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"hot_realms", "hot_tokens", "hot_flows", "hot_devs", "hot_libs"} {
		if string(raw[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, raw[key])
		}
	}
	var resp pulseResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window.Key != defaultPulseWindow {
		t.Errorf("window = %q, want the default for an unknown key", resp.Window.Key)
	}
}

func TestPulseLimitIsBounded(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", pulseLimitDefault},
		{"?limit=0", pulseLimitDefault},
		{"?limit=abc", pulseLimitDefault},
		{"?limit=5", 5},
		// Unbounded is not an option: each list is a sort over a range scan,
		// and the limit is on five of them at once.
		{"?limit=9999", pulseLimitMax},
	} {
		req := httptest.NewRequest("GET", "/api/pulse"+tc.query, nil)
		if got := pulseLimit(req); got != tc.want {
			t.Errorf("pulseLimit(%q) = %d, want %d", tc.query, got, tc.want)
		}
	}
}

// The window list is described twice: once here, as the keys the endpoint
// accepts, and once in the frontend, as the keys it offers before it has an
// answer to read the labels off. Left to drift, the frontend would send a key
// the server silently replaces with the default, and the selector would show an
// active button for a window nobody is looking at.
//
// Same reason TestRailMatchesNavTable exists, and the same fix: pin the two.
func TestHomeWindowsMatchTheServer(t *testing.T) {
	index, err := web.Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	const marker = "const HOME_WINDOWS = ["
	i := strings.Index(string(index), marker)
	if i < 0 {
		t.Fatal("HOME_WINDOWS is gone from the frontend, or was renamed")
	}
	rest := string(index)[i+len(marker):]
	j := strings.Index(rest, "]")
	if j < 0 {
		t.Fatal("HOME_WINDOWS is not a single-line literal any more")
	}

	var fromFrontend []string
	for _, part := range strings.Split(rest[:j], ",") {
		if k := strings.Trim(strings.TrimSpace(part), "'\""); k != "" {
			fromFrontend = append(fromFrontend, k)
		}
	}
	var fromServer []string
	for _, w := range pulseWindows {
		fromServer = append(fromServer, w.Key)
	}
	if !slices.Equal(fromFrontend, fromServer) {
		t.Errorf("HOME_WINDOWS = %v, pulseWindows = %v", fromFrontend, fromServer)
	}

	// The frontend also names the default, because it has to pick one before
	// any response has told it what the server's is.
	if !strings.Contains(string(index), `const HOME_WINDOW_DEFAULT = '`+defaultPulseWindow+`'`) {
		t.Errorf("the frontend's HOME_WINDOW_DEFAULT is not %q", defaultPulseWindow)
	}
}
