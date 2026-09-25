package achievements

import (
	"strings"
	"testing"
)

// The catalog is data, and every one of these is a property that breaks
// something specific and quietly when it stops holding.
func TestCatalogInvariants(t *testing.T) {
	seen := map[string]bool{}
	groups := map[Group]bool{}
	for _, g := range GroupOrder {
		groups[g] = true
	}

	for _, d := range Catalog {
		t.Run(d.Slug, func(t *testing.T) {
			// A duplicate slug is a silently lost badge: the second definition
			// overwrites the first in the table's primary key and in bySlug.
			if seen[d.Slug] {
				t.Fatalf("duplicate slug %q", d.Slug)
			}
			seen[d.Slug] = true

			if d.Slug != strings.ToLower(d.Slug) || strings.ContainsAny(d.Slug, " _") {
				t.Errorf("slug %q is not kebab-case; it ends up in a URL", d.Slug)
			}
			for _, f := range []struct{ name, val string }{
				{"Name", d.Name}, {"Emoji", d.Emoji}, {"What", d.What}, {"How", d.How},
			} {
				if strings.TrimSpace(f.val) == "" {
					t.Errorf("%s is empty", f.name)
				}
			}
			if !groups[d.Group] {
				t.Errorf("group %q is not in GroupOrder, so this badge renders in no bucket", d.Group)
			}
			if GroupLabel[d.Group] == "" {
				t.Errorf("group %q has no label", d.Group)
			}

			// SQL and Live are the two ways a badge can be decided, and it has
			// to be exactly one. Neither means a badge nobody can ever earn;
			// both means the rollup and the live read would disagree about the
			// same slug, and the one that wrote last would win.
			switch {
			case d.SQL == "" && !d.Live:
				t.Error("no SQL and not marked Live: nothing can ever award this")
			case d.SQL != "" && d.Live:
				t.Error("both SQL and Live: two sources for one badge")
			}

			if d.SQL != "" {
				// Every table here is network-scoped (AGENTS.md's first
				// invariant). A definition that forgets the filter merges two
				// chains' histories, which for a badge means awarding it on
				// mainnet for something done on a testnet.
				if !strings.Contains(d.SQL, "@net") {
					t.Error("SQL does not bind @net, so it is not network-scoped")
				}
				// The four columns the rollup selects out of it, by name.
				for _, col := range []string{"address", "block_height", "block_time", "tx_hash"} {
					if !strings.Contains(d.SQL, col) {
						t.Errorf("SQL never mentions %q, which the rollup selects by name", col)
					}
				}
			}
		})
	}
}

func TestLookupAndIndexed(t *testing.T) {
	if Lookup("first-realm") == nil {
		t.Error("Lookup missed a slug that is in the catalog")
	}
	if Lookup("no-such-badge") != nil {
		t.Error("Lookup invented a badge")
	}
	indexed := Indexed()
	if len(indexed) == 0 || len(indexed) >= len(Catalog) {
		t.Errorf("Indexed returned %d of %d definitions; it should be every one carrying SQL, and session-used carries none",
			len(indexed), len(Catalog))
	}
	for _, d := range indexed {
		if d.SQL == "" {
			t.Errorf("%s has no SQL but Indexed returned it", d.Slug)
		}
	}
}
