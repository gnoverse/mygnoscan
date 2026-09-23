package registry

import "testing"

func testCrossRegistry() *Registry {
	return &Registry{
		Apps: []App{
			{Path: "gno.land/r/gnoswap/router", Name: "GnoSwap", Category: "defi", Description: "A DEX."},
			{Path: "gno.land/r/g1abc/kourt", Name: "Kourt", Category: "content", Description: "Fact staking."},
			{Path: "gno.land/r/g1def/kourtv3", Name: "Kourt v3", Category: "content", Description: "The second generation."},
			{Path: "gno.land/r/sys/params", Name: "Params", Category: "governance", Description: "Chain parameters."},
		},
		Awesome: &Awesome{Sections: []AwesomeSection{
			{Title: "Community Apps", Slug: "community-apps", Entries: []AwesomeEntry{
				// Matched on name alone: awesome-gno points at the GitHub repo,
				// so there is no path to match on, and the spelling differs.
				{Name: "Gnoswap", URL: "https://github.com/gnoswap-labs/gnoswap"},
				// Matched on path, which is the provable kind of match.
				{Name: "Kourt", URL: "https://kourt.xyz", Path: "gno.land/r/g1abc/kourt"},
				// Names a realm the directory has never described.
				{Name: "Zenao", URL: "https://zenao.io", Path: "gno.land/r/zenao/home"},
			}},
			{Title: "Archive", Slug: "archive", Archived: true, Entries: []AwesomeEntry{
				{Name: "Old thing", Path: "gno.land/r/old/thing"},
			}},
		}},
	}
}

func TestMissingFromAwesome(t *testing.T) {
	got := testCrossRegistry().MissingFromAwesome()
	want := []string{"Kourt v3", "Params"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %v", len(got), want)
	}
	for i, a := range got {
		if a.Name != want[i] {
			t.Errorf("[%d] = %q, want %q", i, a.Name, want[i])
		}
	}
	// The one that matters: "Kourt v3" must not be swallowed by "Kourt".
	// awesome-gno's Kourt entry points at the v1 realm, and the two are
	// separate deployments with different economics, so a fuzzy match would
	// report a project as listed when nothing in either list mentions it.
}

func TestAwesomeInDirectory(t *testing.T) {
	got := testCrossRegistry().AwesomeInDirectory()
	if len(got) != 2 {
		t.Fatalf("got %+v, want two matches", got)
	}
	if ref, ok := got["gno.land/r/gnoswap/router"]; !ok || ref.Name != "Gnoswap" {
		t.Errorf("gnoswap matched to %+v; a name-only match is still a match", ref)
	}
	if ref := got["gno.land/r/g1abc/kourt"]; ref.Path != "gno.land/r/g1abc/kourt" {
		t.Errorf("kourt matched to %+v, want the path-proved match", ref)
	}
}

func TestMissingFromDirectory(t *testing.T) {
	got := testCrossRegistry().MissingFromDirectory()
	if len(got) != 1 || got[0].Name != "Zenao" {
		t.Fatalf("got %+v, want only Zenao", got)
	}
	// The archived entry is excluded on purpose: the community has retired it,
	// and importing it here would revive something its own maintainers buried.
}

func TestCrossCheckWithoutASnapshot(t *testing.T) {
	// A registry with no snapshot must return nothing rather than panic: the
	// handler asks for all three every request, and a nil map here would take
	// the whole /apps page down.
	r := &Registry{Apps: []App{{Path: "gno.land/r/x/y", Name: "X"}}}
	if got := r.MissingFromAwesome(); got != nil {
		t.Errorf("MissingFromAwesome = %+v", got)
	}
	if got := r.MissingFromDirectory(); got != nil {
		t.Errorf("MissingFromDirectory = %+v", got)
	}
	if got := r.AwesomeInDirectory(); len(got) != 0 {
		t.Errorf("AwesomeInDirectory = %+v", got)
	}
}
