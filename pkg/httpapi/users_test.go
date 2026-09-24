package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moul/mygnoscan/pkg/store"
)

func TestHandleUserSearchAnswersWithRegistrations(t *testing.T) {
	api, db := newTestAPI(t)
	for _, u := range []store.User{
		{Name: "moul", Address: "g1manfred47kzduec920z88wfr64ylksmdcedlf5"},
		{Name: "moulbot", Address: "g1bot"},
		{Name: "aeddi", Address: "g1aeddi"},
	} {
		if err := db.UpsertUser("alpha", u); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertPackage("alpha", "gno.land/r/moul/home", "home",
		"g1manfred47kzduec920z88wfr64ylksmdcedlf5", "TX1", 10, "", true, 1); err != nil {
		t.Fatal(err)
	}

	var resp usersResponse
	getJSON(t, api.HandleUserSearch, "/api/users/search?q=moul&network=alpha", &resp)

	if len(resp.Users) != 2 {
		t.Fatalf("got %+v, want the two moul* registrations", resp.Users)
	}
	if resp.Users[0].Name != "moul" {
		t.Errorf("first = %q, want the exact match", resp.Users[0].Name)
	}
	if resp.Users[0].Packages != 1 {
		t.Errorf("packages = %d, want 1", resp.Users[0].Packages)
	}
	// The curated registry knows this address, and the row carries the gloss
	// beside the registration rather than instead of it.
	if resp.Users[0].Label == "" {
		t.Error("label is empty, want the curated name for a known address")
	}
}

// A missing q is a client error, not an empty answer: the search box never
// sends one, so a request without it is a caller getting the contract wrong.
func TestHandleUserSearchRequiresAQuery(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandleUserSearch(rec, httptest.NewRequest(http.MethodGet, "/api/users/search", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The search box draws realms and packages as separate groups, so the response
// has to carry both. A flat cap let one namespace's realms fill it and leave the
// package group empty, which reads as "this namespace has no packages".
func TestSearchSeparatesRealmsFromPackages(t *testing.T) {
	api, db := newTestAPI(t)
	// Twelve realms, all newer than the packages, which is the shape that used
	// to push every package out of a flat twenty-row limit.
	for i := 0; i < 12; i++ {
		path := "gno.land/r/moul/realm" + string(rune('a'+i))
		if err := db.UpsertPackage("alpha", path, "realm", "g1moul", "TXR"+path, 100+i, "", true, 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		path := "gno.land/p/moul/lib" + string(rune('a'+i))
		if err := db.UpsertPackage("alpha", path, "lib", "g1moul", "TXP"+path, 10+i, "", false, 1); err != nil {
			t.Fatal(err)
		}
	}

	var rows []store.PackageInfo
	getJSON(t, api.HandleSearch, "/api/search?q=moul&network=alpha", &rows)

	realms, pkgs := 0, 0
	for _, r := range rows {
		if r.IsRealm {
			realms++
		} else {
			pkgs++
		}
	}
	if pkgs != 3 {
		t.Errorf("packages = %d, want all 3: the realms must not crowd them out", pkgs)
	}
	if realms != 10 {
		t.Errorf("realms = %d, want the per-kind cap of 10", realms)
	}
	// Realms first, so the thing a reader can open leads the response.
	if len(rows) == 0 || !rows[0].IsRealm {
		t.Errorf("first row = %+v, want a realm", rows)
	}
}

// The registry names an address that the deploy-dominance heuristic gets wrong,
// and both are "derived", so precedence cannot settle it. The registry wins
// because it is what the chain records rather than what this repo inferred.
func TestHandleLabelsPrefersTheRegistryOverDeployDominance(t *testing.T) {
	api, db := newTestAPI(t)
	const genesis = "g1genesisdeployer0000000000000000000"
	// Enough packages under one namespace for DerivedAddressLabels to name the
	// deployer after it, which is exactly the inference that is wrong for the
	// genesis account on mainnet.
	for i := 0; i < 6; i++ {
		path := "gno.land/r/onbloc/pkg" + string(rune('a'+i))
		if err := db.UpsertPackage("alpha", path, "pkg", genesis, "TX"+path, 10+i, "", true, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertUser("alpha", store.User{Name: "gnoland", Address: genesis}); err != nil {
		t.Fatal(err)
	}

	var labels map[string]store.AddressLabel
	getJSON(t, api.HandleLabels, "/api/labels?network=alpha", &labels)

	got := labels[genesis]
	if got.Label != "@gnoland" {
		t.Fatalf("label = %+v, want the registered name, not the namespace it deployed", got)
	}
}

// An alias and a tombstone are not what an address is called now, so neither
// becomes its label -- even though both stay searchable.
func TestHandleLabelsSkipsAliasesAndTombstones(t *testing.T) {
	api, db := newTestAPI(t)
	for _, u := range []store.User{
		{Name: "old", Address: "g1a", Alias: true},
		{Name: "gone", Address: "g1b", Deleted: true},
		{Name: "live", Address: "g1c"},
	} {
		if err := db.UpsertUser("alpha", u); err != nil {
			t.Fatal(err)
		}
	}

	var labels map[string]store.AddressLabel
	getJSON(t, api.HandleLabels, "/api/labels?network=alpha", &labels)

	if _, ok := labels["g1a"]; ok {
		t.Error("an alias became a label; it is a previous name, not the current one")
	}
	if _, ok := labels["g1b"]; ok {
		t.Error("a deleted user became a label")
	}
	if labels["g1c"].Label != "@live" {
		t.Errorf("g1c = %+v, want @live", labels["g1c"])
	}
}
