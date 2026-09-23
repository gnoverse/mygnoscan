package gnoaddr

import "testing"

// The index is only worth anything if a hit is a proof. These check the three
// ways it could lie: a wrong path, the wrong one of a package's two accounts,
// and a human's address wearing a realm's name.
func TestReverse(t *testing.T) {
	paths := []string{
		"gno.land/r/gnoland/wugnot",
		"gno.land/p/demo/avl",
		// A run path: Derive resolves this to the caller's own address, which
		// belongs to a person and not to the package.
		"gno.land/e/g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5/run",
		"",
		"strings",
	}
	r := NewReverse(paths)

	for _, tc := range []struct {
		name    string
		addr    string
		want    Owner
		wantHit bool
	}{
		{
			name:    "banker account resolves to its package",
			addr:    Derive("gno.land/r/gnoland/wugnot"),
			want:    Owner{Path: "gno.land/r/gnoland/wugnot"},
			wantHit: true,
		},
		{
			name:    "storage deposit account is marked as one",
			addr:    DeriveStorageDeposit("gno.land/r/gnoland/wugnot"),
			want:    Owner{Path: "gno.land/r/gnoland/wugnot", Deposit: true},
			wantHit: true,
		},
		{
			name:    "a pure package is indexed too",
			addr:    Derive("gno.land/p/demo/avl"),
			want:    Owner{Path: "gno.land/p/demo/avl"},
			wantHit: true,
		},
		{
			name: "the caller behind a run path is not labelled a realm",
			addr: "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5",
		},
		{
			name: "an address no package owns is a miss",
			addr: "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
		},
		{
			name: "the empty address is a miss, not a panic",
			addr: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := r.Lookup(tc.addr)
			if ok != tc.wantHit {
				t.Fatalf("Lookup(%q) hit = %v, want %v (got %+v)", tc.addr, ok, tc.wantHit, got)
			}
			if got != tc.want {
				t.Errorf("Lookup(%q) = %+v, want %+v", tc.addr, got, tc.want)
			}
		})
	}

	// Two accounts per indexable path, and the three unindexable ones
	// contribute nothing.
	if r.Len() != 4 {
		t.Errorf("Len() = %d, want 4", r.Len())
	}
}

func TestReverseNilIsSafe(t *testing.T) {
	var r *Reverse
	if _, ok := r.Lookup("g1manfred47kzduec920z88wfr64ylksmdcedlf5"); ok {
		t.Error("a nil index answered a lookup")
	}
	if r.Len() != 0 {
		t.Errorf("nil Len() = %d, want 0", r.Len())
	}
}
