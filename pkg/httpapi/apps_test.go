package httpapi

import (
	"testing"
)

// The layering is the design, so these are the tests that matter: which source
// wins for which field, what removes an entry, and what the ranking does when
// the two halves of the list are scored in incomparable units.

func TestFirstSentence(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{
			name: "cuts at the sentence, not at a character count",
			in:   "The official blog, rendered on chain. Posts are published by an admin.",
			want: "The official blog, rendered on chain.",
		},
		{
			name: "a one-sentence doc survives whole",
			in:   "A central place for all gno.land faucets.",
			want: "A central place for all gno.land faucets.",
		},
		{
			name: "newlines and runs of spaces collapse",
			in:   "Package blog is\n   the official blog. And more.",
			want: "Package blog is the official blog.",
		},
		{
			name: "a doc with no sentence end is trimmed with an ellipsis rather than mid-word rubbish",
			in:   "x" + repeat("y", 400),
			want: "x" + repeat("y", 179) + "…",
		},
		{name: "empty stays empty", in: "   ", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstSentence(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}

// Two live deployments of the same idea is the normal state of a chain nobody
// can delete from. Ranking them as peers sends people to last year's version
// with a call count that makes it look current.
func TestCollapseSupersededFoldsTheOlderGeneration(t *testing.T) {
	v1 := &AppCard{Path: "gno.land/r/x/kourt", Name: "Kourt", Calls: 900}
	v3 := &AppCard{Path: "gno.land/r/x/kourtv3", Name: "Kourt v3", Calls: 10,
		Supersedes: []string{"gno.land/r/x/kourt"}}
	other := &AppCard{Path: "gno.land/r/x/other", Name: "Other"}

	got := collapseSuperseded([]*AppCard{v1, v3, other})

	if len(got) != 2 {
		t.Fatalf("got %d cards, want the older generation folded away: %+v", len(got), got)
	}
	var kourt *AppCard
	for i := range got {
		if got[i].Name == "Kourt v3" {
			kourt = &got[i]
		}
		if got[i].Name == "Kourt" {
			t.Error("the superseded generation is still ranked as a peer")
		}
	}
	if kourt == nil || len(kourt.Previous) != 1 || kourt.Previous[0].Name != "Kourt" {
		t.Fatalf("the old one was dropped rather than carried: %+v", kourt)
	}
	// Carried, not deleted: it is still on the chain and somebody may hold a
	// position in it, so a hub that pretended it was gone would be lying about
	// state a reader can check.
	if kourt.Previous[0].Calls != 900 {
		t.Error("the folded card lost its figures")
	}
}

// A three-generation chain must stay flat: a reader wants "and the ones
// before", not a tree.
func TestCollapseDoesNotNest(t *testing.T) {
	v1 := &AppCard{Path: "gno.land/r/x/a", Name: "A"}
	v2 := &AppCard{Path: "gno.land/r/x/b", Name: "B", Supersedes: []string{"gno.land/r/x/a"}}
	v3 := &AppCard{Path: "gno.land/r/x/c", Name: "C", Supersedes: []string{"gno.land/r/x/b"}}

	got := collapseSuperseded([]*AppCard{v1, v2, v3})

	if len(got) != 1 || got[0].Name != "C" {
		t.Fatalf("got %+v, want only the newest", got)
	}
	for _, p := range got[0].Previous {
		if len(p.Previous) != 0 {
			t.Errorf("%s carries its own Previous, so the page would nest", p.Name)
		}
	}
}

// The ranking exists because the two halves of this list are scored in
// incomparable units: a realm has callers, and a browser extension never will.
func TestRankApps(t *testing.T) {
	busy := &AppCard{Name: "busy realm", Via: viaDiscovered, Score: 500}
	quiet := &AppCard{Name: "quiet realm", Via: viaDiscovered, Score: 11}
	wallet := &AppCard{Name: "a wallet", Via: viaCommunity}
	listed := &AppCard{Name: "listed, unused", Via: viaPinned}

	cards := []*AppCard{quiet, wallet, busy, listed}
	rankApps(cards)

	got := []string{}
	for _, c := range cards {
		got = append(got, c.Name)
	}
	want := []string{"busy realm", "a wallet", "listed, unused", "quiet realm"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// The claim, spelled out: a realm people genuinely use beats anything a
	// human merely listed, and a listed entry beats a realm nobody opens.
	//
	// Floored rather than promoted, which is a correction. Ranking listed
	// entries to the top put Boards2, which nobody had called, above the
	// busiest realm on the chain. Being listed means included despite having no
	// metrics; it does not mean important.
}

// The whole stack, through the handler: which layer wins for which field.
//
// A name a human wrote, a name the community wrote and a name the realm's own
// source carries are three different claims. The page shows which is which, so
// the order they override in is load-bearing rather than cosmetic.
func TestAppsHubLayersTheSources(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp appsHubResponse
	getJSON(t, api.HandleAppsHub, "/api/apps?network=alpha", &resp)

	if len(resp.Apps) == 0 {
		t.Fatal("no apps assembled")
	}
	by := map[string]AppCard{}
	for _, a := range resp.Apps {
		if a.Path != "" {
			by[a.Path] = a
		}
	}

	// In apps.json, so this repo's own sentence wins over everything.
	blog, ok := by["gno.land/r/gnoland/blog"]
	if !ok {
		t.Fatal("the blog is not in the assembled list")
	}
	if blog.NameFrom != fromCurated || blog.Name != "gno.land blog" {
		t.Errorf("blog name = %q from %q, want the curated one", blog.Name, blog.NameFrom)
	}
	if blog.DescriptionFrom != fromCurated {
		t.Errorf("blog description is from %q, want curated to win", blog.DescriptionFrom)
	}

	// Every card says where each field came from, because a reader who cannot
	// tell a vouched-for sentence from a generated one has to trust both.
	for _, a := range resp.Apps {
		switch a.NameFrom {
		case fromCurated, fromCommunity, fromChain, fromPath:
		default:
			t.Errorf("%s has name_from %q", a.Name, a.NameFrom)
		}
		if a.Description != "" {
			switch a.DescriptionFrom {
			case fromCurated, fromCommunity, fromChain:
			default:
				t.Errorf("%s has a description from %q", a.Name, a.DescriptionFrom)
			}
		}
		switch a.Via {
		case viaDiscovered, viaPinned, viaCommunity:
		default:
			t.Errorf("%s got here via %q", a.Name, a.Via)
		}
	}

	// The community list supplies what is not on a chain at all, which is most
	// of what it holds and all of what an indexer is blind to.
	offChain := 0
	for _, a := range resp.Apps {
		if a.Path == "" {
			offChain++
			if a.Website == "" {
				t.Errorf("%s is off chain with nowhere to go", a.Name)
			}
		}
	}
	if offChain == 0 {
		t.Error("no off-chain apps reached the list, so the community layer is not wired")
	}
}

// Without a network there is nothing to rank and nothing honest to count, and
// the off-chain half is still worth serving: someone asking whether gno.land
// has a wallet should not have to pick a chain first, and the wallet is not on
// one anyway.
func TestAppsHubWithoutANetworkKeepsTheOffChainHalf(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp appsHubResponse
	getJSON(t, api.HandleAppsHub, "/api/apps", &resp)

	if resp.Discovered != 0 {
		t.Errorf("discovered %d apps with no chain selected", resp.Discovered)
	}
	if resp.OffChain == 0 || len(resp.Apps) == 0 {
		t.Fatal("the off-chain apps went away with the network")
	}
	for _, a := range resp.Apps {
		if a.CallsWindow != 0 || a.Score != 0 {
			t.Errorf("%s carries chain figures with no chain selected", a.Name)
		}
	}
}

// The skip list is applied *and* served. A page that quietly dropped an entry
// would be indistinguishable from one that lost it, and an unexplained removal
// is indistinguishable from censorship.
func TestAppsHubServesItsOwnSkipList(t *testing.T) {
	api, _ := newTestAPI(t)

	var resp appsHubResponse
	getJSON(t, api.HandleAppsHub, "/api/apps?network=alpha", &resp)

	if resp.Moderation == nil {
		t.Fatal("the skip list is not served, so the page cannot show it")
	}
	skipped := map[string]bool{}
	for _, s := range resp.Moderation {
		if s.Why == "" {
			t.Errorf("%s is skipped with no reason", s.Path)
		}
		skipped[s.Path] = true
	}
	for _, a := range resp.Apps {
		if a.Path != "" && skipped[a.Path] {
			t.Errorf("%s is on the skip list and in the grid", a.Path)
		}
	}
}

// The derived name is allowed to be poor. It is not allowed to be ambiguous:
// the last segment alone produced `position`, `staker`, `gns` and two separate
// cards both called `staker` on mainnet, which tells a reader nothing and tells
// them it twice.
func TestNameFromPath(t *testing.T) {
	for _, tt := range []struct {
		in, want, why string
	}{
		{"gno.land/r/gnoswap/v1/position", "gnoswap/position",
			"a version names a generation, never a project"},
		{"gno.land/r/gnoswap/v1/staker", "gnoswap/staker",
			"and the namespace is what tells two stakers apart"},
		{"gno.land/r/gnoland/blog", "gnoland/blog", "the ordinary case"},
		{"gno.land/r/moul/config/v0", "moul/config", "trailing version dropped"},
		{"gno.land/r/g1leu8d2vsplhehcfkjg50mwgdpxdkt8tztu95wr/kourtv3", "kourtv3",
			"40 characters of address is noise to a reader"},
		{"gno.land/r/demo/v0", "demo",
			"a generation is not a name; two generations of one realm collapse here and supersedes tells them apart"},
		{"gno.land/r/x/v1", "x", "and the namespace survives alone"},
		{"gno.land/r/a/b/c/d", "c/d", "two segments is the most a card has room for"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := nameFromPath(tt.in); got != tt.want {
				t.Errorf("got %q, want %q (%s)", got, tt.want, tt.why)
			}
		})
	}
}
