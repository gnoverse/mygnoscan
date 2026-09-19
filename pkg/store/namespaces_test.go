package store

import (
	"reflect"
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// Namespaces lists what could have an owner, which is the input to resolving
// one. Address-shaped namespaces are excluded because they are their own
// owner — asking a registry about gno.land/r/g1abc…/foo has no meaning.
func TestNamespaces(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	const when = "2026-08-01T00:00:00Z"
	paths := []string{
		"gno.land/r/onbloc/foo",
		"gno.land/r/onbloc/bar",   // same namespace, listed once
		"gno.land/p/moul/x/thing", // p/ counts too
		"gno.land/r/g1abcdefghijklmnopqrstuvwxyz0123456789/private",
	}
	for i, p := range paths {
		if err := db.UpsertPackage("alpha", p, "pkg", "g1creator", "tx"+p, 100+i, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}

	got, err := db.Namespaces("alpha")
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	want := []string{"moul", "onbloc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Namespaces() = %v, want %v (deduplicated, address-namespaces excluded)", got, want)
	}
}

// A retired network's packages must not contribute namespaces to a live one.
func TestNamespacesAreScoped(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	const when = "2026-08-01T00:00:00Z"
	if err := db.UpsertPackage("alpha", "gno.land/r/live/a", "pkg", "g1c", "t1", 1, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertPackage("retired", "gno.land/r/gone/a", "pkg", "g1c", "t2", 2, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	got, err := db.Namespaces("")
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"live"}) {
		t.Errorf("Namespaces() = %v, want [live]; a retired chain's namespaces must not appear", got)
	}
}
