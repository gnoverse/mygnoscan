package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/moul/mygnoscan/pkg/registry"
	"github.com/moul/mygnoscan/pkg/store"
)

// TestMergeLabelsPrecedence pins the ranking, which is the one judgement call
// in this whole change. See labelPrecedence for why curated beats derived.
func TestMergeLabelsPrecedence(t *testing.T) {
	const addr = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"

	tests := []struct {
		name      string
		derived   map[string]store.AddressLabel
		curated   map[string]registry.Entry
		wantLabel string
		wantKind  string
	}{
		{
			name:      "only derived",
			derived:   map[string]store.AddressLabel{addr: {Label: "@moul", Kind: "derived", Why: "sole deployer"}},
			wantLabel: "@moul", wantKind: "derived",
		},
		{
			name:      "only curated",
			curated:   map[string]registry.Entry{addr: {Label: "@moul", Kind: registry.KindCurated}},
			wantLabel: "@moul", wantKind: registry.KindCurated,
		},
		{
			name:      "curated beats derived",
			derived:   map[string]store.AddressLabel{addr: {Label: "@somenamespace", Kind: "derived", Why: "sole deployer"}},
			curated:   map[string]registry.Entry{addr: {Label: "@moul", Kind: registry.KindCurated}},
			wantLabel: "@moul", wantKind: registry.KindCurated,
		},
		{
			// A heuristic must never override a proof.
			name:      "derived beats inferred",
			derived:   map[string]store.AddressLabel{addr: {Label: "@gnoswap", Kind: "derived", Why: "sole deployer"}},
			curated:   map[string]registry.Entry{addr: {Label: "@a_bot", Kind: registry.KindInferred, Why: "many sends"}},
			wantLabel: "@gnoswap", wantKind: "derived",
		},
		{
			// Nor may a name the address gave itself.
			name:      "derived beats declared",
			derived:   map[string]store.AddressLabel{addr: {Label: "@gnoswap", Kind: "derived", Why: "sole deployer"}},
			curated:   map[string]registry.Entry{addr: {Label: "@totally_the_foundation", Kind: registry.KindDeclared, Why: "valoper moniker"}},
			wantLabel: "@gnoswap", wantKind: "derived",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeLabels(tt.derived, tt.curated)[addr]
			if got.Label != tt.wantLabel || got.Kind != tt.wantKind {
				t.Errorf("label = %+v, want %s (%s)", got, tt.wantLabel, tt.wantKind)
			}
		})
	}
}

// The loser survives in the winner's evidence. A merge that dropped it would
// throw away the corroboration, which is the part that makes a name checkable.
func TestMergeLabelsKeepsTheLosingClaim(t *testing.T) {
	const addr = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
	got := mergeLabels(
		map[string]store.AddressLabel{addr: {Label: "@somenamespace", Kind: "derived", Why: "sole deployer of 12 packages"}},
		map[string]registry.Entry{addr: {Label: "@moul", Kind: registry.KindCurated}},
	)[addr]

	if !strings.Contains(got.Why, "also derived: @somenamespace") {
		t.Errorf("why = %q, want the derived claim carried alongside", got.Why)
	}
	if !strings.Contains(got.Why, "sole deployer of 12 packages") {
		t.Errorf("why = %q, want the derived evidence kept too", got.Why)
	}
}

// A measurement date travels with the evidence, so a reader can weigh its age
// without another field on the wire.
func TestMergeLabelsDatesAMeasurement(t *testing.T) {
	const addr = "g18qhq2fl54lszhmxeyqlvxnwjzc3xpu4nnakclp"
	got := mergeLabels(nil, map[string]registry.Entry{
		addr: {Label: "@faucet", Kind: registry.KindInferred, Why: "3683 sends", Checked: "2026-08-28"},
	})[addr]

	if !strings.Contains(got.Why, "measured 2026-08-28") {
		t.Errorf("why = %q, want the check date included", got.Why)
	}
}

// Two entries with the same label must not merge into one: identical labels are
// expected (four addresses are @gnoswap_bot) and they are different accounts.
func TestMergeLabelsKeepsAddressesDistinct(t *testing.T) {
	merged := mergeLabels(nil, map[string]registry.Entry{
		"g1v73cacldctfky0thu5xuuhmy5zs0mnd8vxyl2w": {Label: "@gnoswap_bot", Kind: registry.KindInferred, Why: "7189 sends"},
		"g1vmmklhc7eyxgr3x8ll0yr5wupv8lkxv4xhd8l0": {Label: "@gnoswap_bot", Kind: registry.KindInferred, Why: "6410 sends"},
	})
	if len(merged) != 2 {
		t.Errorf("got %d labels, want both addresses kept", len(merged))
	}
}

func TestHandleLabelsServesTheRegistry(t *testing.T) {
	api, _ := newTestAPI(t)

	var labels map[string]store.AddressLabel
	getJSON(t, api.HandleLabels, "/api/labels", &labels)

	oracle, ok := labels["g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704"]
	if !ok {
		t.Fatal("the gpao oracle is missing from /api/labels")
	}
	if oracle.Label != "@gpao_oracle" || oracle.Kind != registry.KindCurated {
		t.Errorf("oracle = %+v, want a curated @gpao_oracle", oracle)
	}
}

func TestHandleApps(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp appsResponse
	getJSON(t, api.HandleApps, "/api/registry/apps", &resp)

	if len(resp.Apps) == 0 {
		t.Fatal("no apps returned")
	}
	if len(resp.Categories) == 0 {
		t.Error("no categories returned")
	}
	for _, app := range resp.Apps {
		if app.Description == "" {
			// A directory entry that does not say what the thing does is a link
			// list, and /realms is already that.
			t.Errorf("%s has no description", app.Path)
		}
	}
}

func TestHandleAwesome(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp awesomeResponse
	getJSON(t, api.HandleAwesome, "/api/registry/awesome", &resp)

	if resp.Entries < 40 {
		t.Fatalf("got %d entries, which is too few to be the real list", resp.Entries)
	}
	if len(resp.Commit) != 40 {
		t.Errorf("commit = %q, want the full sha the snapshot was read at", resp.Commit)
	}
	// Both directions of the cross-check are the point of the endpoint. The two
	// lists overlap in a handful of entries out of eighty, so a zero on either
	// side means the matching broke rather than that the lists agree.
	if len(resp.MissingFromAwesome) == 0 {
		t.Error("no directory entries reported as absent from the community list")
	}
	if len(resp.InDirectory) == 0 {
		t.Error("no overlap found at all, which means the name and path matching is broken")
	}
	for _, app := range resp.MissingFromAwesome {
		if _, ok := resp.InDirectory[app.Path]; ok {
			// Being in both answers at once is the one contradiction this
			// endpoint can produce, and a page built on it would print the
			// same realm under "listed" and "not listed".
			t.Errorf("%s is reported both matched and missing", app.Path)
		}
	}
}

// Without a network there are no numbers, and the list is still worth serving:
// someone who came to find out whether gno.land has a VS Code extension should
// not have to pick a chain first, and the extension is not on a chain anyway.
func TestHandleAwesomeWithoutANetworkStillServesTheList(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp awesomeResponse
	getJSON(t, api.HandleAwesome, "/api/registry/awesome", &resp)

	if len(resp.Apps) == 0 {
		t.Fatal("no apps")
	}
	if resp.Network != "" || resp.Stats != nil {
		t.Errorf("network = %q, stats = %v, want both absent", resp.Network, resp.Stats)
	}
}

// The summary on /api/registry/apps is what the directory's own introduction
// points at, so it has to agree with the endpoint it summarises. Two numbers
// drifting apart would have the page say "8 are missing" above a table of six.
func TestAppsSummaryAgreesWithTheAwesomeEndpoint(t *testing.T) {
	api, _ := newTestAPI(t)

	var apps appsResponse
	getJSON(t, api.HandleApps, "/api/registry/apps", &apps)
	var awesome awesomeResponse
	getJSON(t, api.HandleAwesome, "/api/registry/awesome", &awesome)

	if apps.Awesome == nil {
		t.Fatal("/api/registry/apps carries no awesome summary")
	}
	if apps.Awesome.Entries != awesome.Entries {
		t.Errorf("entries: apps says %d, awesome says %d", apps.Awesome.Entries, awesome.Entries)
	}
	if apps.Awesome.MissingFromAwesome != len(awesome.MissingFromAwesome) {
		t.Errorf("missing: apps says %d, awesome says %d",
			apps.Awesome.MissingFromAwesome, len(awesome.MissingFromAwesome))
	}
	if apps.Awesome.MissingFromDirectory != len(awesome.MissingFromDirectory) {
		t.Errorf("inbound: apps says %d, awesome says %d",
			apps.Awesome.MissingFromDirectory, len(awesome.MissingFromDirectory))
	}
}

// An app is exactly a thing with a page you can open, and the grid draws one
// card per app. Anything in here without a destination is a card the frontend
// cannot draw, and anything with a site but no provenance is an unattributed
// claim that a project lives at an address.
func TestAwesomeAppsAllHaveSomewhereToGo(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp awesomeResponse
	getJSON(t, api.HandleAwesome, "/api/registry/awesome", &resp)

	if len(resp.Apps) < 8 {
		t.Fatalf("got %d apps, which is too few to be the resolved list", len(resp.Apps))
	}
	for _, a := range resp.Apps {
		if a.Site == "" && a.Path == "" {
			t.Errorf("%s is listed as an app with neither a site nor a realm", a.Name)
		}
		if a.Site == "" {
			continue
		}
		if !strings.HasPrefix(a.Site, "http://") && !strings.HasPrefix(a.Site, "https://") {
			t.Errorf("%s has site %q", a.Name, a.Site)
		}
		switch a.SiteFrom {
		case registry.SiteListed, registry.SiteRepoHomepage:
		default:
			t.Errorf("%s has site_from %q", a.Name, a.SiteFrom)
		}
	}
	// The other half of the same statement: everything the grid does not draw
	// is counted, so the page can say what it is not showing.
	if len(resp.Others) == 0 {
		t.Error("nothing reported as unphotographable, which cannot be true of an awesome list")
	}
}

// The gate in front of the capture service. Exact strings, not hosts: a
// host-level check would let any page on a listed app's domain through, and the
// point of vendoring the list is that a human merged each of these.
func TestShotSiteRefusesAnythingNotInTheList(t *testing.T) {
	api, _ := newTestAPI(t)
	api.SetShotUpstream("http://127.0.0.1:1")

	var listed awesomeResponse
	getJSON(t, api.HandleAwesome, "/api/registry/awesome", &listed)
	var site string
	for _, a := range listed.Apps {
		if a.Site != "" {
			site = a.Site
			break
		}
	}
	if site == "" {
		t.Fatal("no site in the snapshot to test against")
	}

	for _, tt := range []struct {
		name string
		url  string
		want int
	}{
		{"a listed site reaches the proxy", site, http.StatusServiceUnavailable},
		{"another page on a listed host", site + "/something-else", http.StatusBadRequest},
		{"an unlisted host", "https://evil.example/", http.StatusBadRequest},
		{"a file url", "file:///etc/passwd", http.StatusBadRequest},
		{"nothing at all", "", http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/shot/site?url="+url.QueryEscape(tt.url), nil)
			w := httptest.NewRecorder()
			api.HandleShotSite(w, r)
			// The allowed one is answered 503 because the upstream in this test
			// is a closed port: reaching the proxy at all is what is being
			// asserted, and it is the only outcome the gate permits.
			if w.Code != tt.want {
				t.Errorf("got %d, want %d", w.Code, tt.want)
			}
		})
	}
}
