package registry

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The fixture is a trimmed copy of the real README, kept because the generator
// needs the network and is therefore never run by CI: without it the parser has
// no coverage at all. Every line in it is there for a case that bit.
func fixture(t *testing.T) []AwesomeSection {
	t.Helper()
	md, err := os.ReadFile("testdata/awesome-readme.md")
	if err != nil {
		t.Fatal(err)
	}
	secs, err := ParseAwesome(string(md))
	if err != nil {
		t.Fatal(err)
	}
	return secs
}

func TestParseAwesomeSections(t *testing.T) {
	secs := fixture(t)
	// Contents and Contributing are navigation and instructions. The table of
	// contents in particular would otherwise parse as a section full of anchor
	// links, which look exactly like entries.
	if len(secs) != 2 {
		t.Fatalf("got %d sections, want 2 (Contents and Contributing must be skipped): %+v", len(secs), secs)
	}
	if secs[0].Title != "Apps" || secs[0].Slug != "apps" {
		t.Errorf("first section is %q/%q", secs[0].Title, secs[0].Slug)
	}
	if secs[0].Note != "Apps developed by the gno.land team." {
		t.Errorf("note is %q", secs[0].Note)
	}
	if secs[1].Slug != "archive" {
		t.Errorf("second section is %q", secs[1].Slug)
	}
}

func TestParseAwesomeEntries(t *testing.T) {
	secs := fixture(t)
	byName := map[string]AwesomeEntry{}
	for _, s := range secs {
		for _, e := range s.Entries {
			byName[e.Name] = e
		}
	}

	for _, tt := range []struct {
		name string
		url  string
		desc string
		path string
		why  string
	}{
		{
			name: "Gno Playground", url: "https://play.gno.land/",
			desc: "An online Gno editor.",
			why:  "a gno.land subdomain that is not a realm path must not become one",
		},
		{
			name: "r/docs", url: "https://staging.gno.land/r/docs/home",
			desc: "The on-chain documentation realm.", path: "gno.land/r/docs/home",
			why: "the list links staging as readily as mainnet; the realm is the same realm",
		},
		{
			name: "Gno Debugger", url: "https://gno.land/r/gnoland/blog:p/gno-debugger",
			desc: "A debugger packaged with the GnoVM.",
			why:  "a render argument names a page of a realm; attributing it would file the debugger under the blog and print the blog's call count beside it",
		},
		{
			name: "Kourt", url: "https://kourt.xyz",
			desc: "Fact-staking courts. Source, realm.",
			path: "gno.land/r/g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6/kourt",
			why:  "the realm is named in an inline link, not in the entry's own destination",
		},
		{
			name: "legacy Bounties (deprecated)", url: "https://github.com/gnolang/bounties",
			desc: "Legacy official bounty board.",
			why:  "parentheses inside the link text must not end the link",
		},
		{
			name: "Discord", url: "https://discord.com/invite/gnoland",
			why: "an entry with no description is still an entry",
		},
		{
			name: "The Portal Loop",
			desc: "The rolling testnet that used to serve the gno.land homepage.",
			why:  "a retired thing has no URL left to point at, and dropping it would hide that the list mentions it",
		},
		{
			name: "tx-exports", url: "https://github.com/gnolang/tx-exports",
			desc: "Archived transaction data, one directory per network.",
			why:  "backticks are markup, and the frontend renders text into a DOM node",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, ok := byName[tt.name]
			if !ok {
				t.Fatalf("entry missing (%s)", tt.why)
			}
			if e.URL != tt.url {
				t.Errorf("url = %q, want %q (%s)", e.URL, tt.url, tt.why)
			}
			if e.Description != tt.desc {
				t.Errorf("description = %q, want %q (%s)", e.Description, tt.desc, tt.why)
			}
			if e.Path != tt.path {
				t.Errorf("path = %q, want %q (%s)", e.Path, tt.path, tt.why)
			}
		})
	}

	// The links are lifted out of the prose rather than left in it, because the
	// frontend builds DOM and would otherwise print `[Source](...)` verbatim.
	kourt := byName["Kourt"]
	if len(kourt.Links) != 2 || kourt.Links[1].Text != "realm" {
		t.Errorf("kourt links = %+v, want Source and realm", kourt.Links)
	}
}

func TestParseAwesomeRejectsAShapeChange(t *testing.T) {
	for _, tt := range []struct {
		name string
		md   string
	}{
		{"no sections at all", "# Title\n\nsome prose and no headings.\n"},
		{"a heading with nothing under it", "## Apps\n\n_a note._\n\n## Tools\n\n- [x](https://x.example)\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Silence is the failure mode that matters: a README that grew a
			// new bullet syntax would otherwise emit an empty section and the
			// page would report that the community deleted a category.
			if _, err := ParseAwesome(tt.md); err == nil {
				t.Fatal("parsed without error, want a failure loud enough to stop `make awesome`")
			}
		})
	}
}

func TestShippedAwesomeSnapshot(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	aw := reg.Awesome
	if aw == nil {
		t.Fatal("no awesome snapshot embedded")
	}
	if aw.Count() < 40 {
		t.Errorf("snapshot holds %d entries, which is too few to be the real list", aw.Count())
	}
	if !strings.HasPrefix(aw.Source, "https://github.com/gnoverse/awesome-gno") {
		t.Errorf("source = %q", aw.Source)
	}
	for _, s := range aw.Sections {
		if s.Slug == "archive" && !s.Archived {
			t.Error("the archive section is not marked archived, so retired entries would render as live")
		}
		if s.Slug != "archive" && s.Archived {
			t.Errorf("%q is marked archived", s.Slug)
		}
	}
	// A path with a render argument in it means the page rule leaked into the
	// data, and a card would show one realm's traffic under another's name.
	for _, p := range aw.Paths() {
		if strings.Contains(p, ":") {
			t.Errorf("path %q names a page, not a realm", p)
		}
	}
}

func TestSyncedAge(t *testing.T) {
	now := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		synced string
		want   int
	}{
		{"2026-09-30", 0},
		{"2026-09-23", 7},
		{"not a date", -1},
	} {
		a := &Awesome{Synced: tt.synced}
		if got := a.SyncedAge(now); got != tt.want {
			t.Errorf("SyncedAge(%q) = %d, want %d", tt.synced, got, tt.want)
		}
	}
}
