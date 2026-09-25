package httpapi

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/registry"
	"github.com/moul/mygnoscan/pkg/store"
)

// The app hub, assembled from three layers.
//
// The chain proposes: every realm people actually use, ranked, with a
// screenshot and the realm's own doc comment as its description. Curation
// corrects, with three small levers and no large one:
//
//	pin      something the ranking misses, because it just shipped or is quiet
//	enrich   a better name, a real sentence, the website, a category
//	skip     something that should not be here at all (moderation.toml)
//
// And awesome-gno, the community's own list, is a second source of enrichment
// and the only source for apps that are not on a chain at all: a wallet, an
// explorer, a playground. Those have no path and no call count, and a hub that
// could not show them would be answering "what is built on gno.land" with the
// subset it happens to be able to index.
//
// Every field says where it came from. That is the part worth keeping: a
// sentence a human wrote and a sentence the realm's own source carries are both
// worth showing and are not the same claim, and a reader who cannot tell which
// is which has to trust both equally or neither.

// Provenance of an assembled field.
const (
	fromCurated   = "curated"   // this repo's apps.json
	fromCommunity = "community" // awesome-gno
	fromChain     = "chain"     // the realm's own package doc comment
	fromReadme    = "readme"    // the realm's own README
	fromPath      = "path"      // derived from the path, the last resort
)

// Why an entry is in the list at all.
const (
	viaDiscovered = "discovered" // the chain ranked it here
	viaPinned     = "pinned"     // a human put it here despite the ranking
	viaCommunity  = "community"  // awesome-gno lists it and it is not on a chain
)

// AppCard is one entry, after the layers have been merged.
type AppCard struct {
	// Path is the realm. Empty for an app that is not on a chain, which is
	// most of what the community list holds.
	Path string `json:"path,omitempty"`
	Name string `json:"name"`
	// Description is one line. The card shows this and nothing longer.
	Description string `json:"description,omitempty"`
	// Website is the app's own front end.
	//
	// It leads, and the realm is the second link. Somebody sent here to look at
	// what is built on gno.land wants the thing itself; the realm page is what
	// they want next, and only if they are the kind of person who wants it.
	Website  string `json:"website,omitempty"`
	Category string `json:"category,omitempty"`

	NameFrom        string `json:"name_from"`
	DescriptionFrom string `json:"description_from,omitempty"`
	// Checked dates a curated description, and travels with it.
	//
	// The registry requires one on any blurb naming a fact that can change
	// without the entry changing, and the old dense table printed it. That
	// table is gone, so the date moved into the provenance tooltip rather than
	// quietly ceasing to exist: the rule that made contributors write it is
	// only worth anything if a reader can still see the answer.
	Checked     string `json:"checked,omitempty"`
	WebsiteFrom string `json:"website_from,omitempty"`
	Via         string `json:"via"`

	// The chain's own account of it, absent for an app that is not on one.
	Calls         int    `json:"calls,omitempty"`
	Callers       int    `json:"callers,omitempty"`
	CallsWindow   int    `json:"calls_window,omitempty"`
	CallersWindow int    `json:"callers_window,omitempty"`
	LastCall      string `json:"last_call,omitempty"`
	DeployedAt    string `json:"deployed_at,omitempty"`
	Score         int    `json:"score,omitempty"`

	// Supersedes names the older generations this entry replaces, and Previous
	// carries them, so the page can offer them without ranking them.
	Supersedes []string  `json:"supersedes,omitempty"`
	Previous   []AppCard `json:"previous,omitempty"`
	// Covers names the realms this app is made of, and Parts carries them.
	//
	// Not the same fold as Previous, and the card says so differently: a
	// previous generation is somewhere else to go, a part is somewhere you
	// already are.
	Covers []string  `json:"covers,omitempty"`
	Parts  []AppCard `json:"parts,omitempty"`
	// FoldedRealms is how many realms the figures above cover, this one
	// included. Present only when something was folded, so a reader can tell a
	// card's own traffic from an app's.
	FoldedRealms int `json:"folded_realms,omitempty"`
	// CommunityURL is where awesome-gno points, when that is not the website:
	// usually the source repository.
	CommunityURL string `json:"community_url,omitempty"`
}

// firstSentence trims a doc comment to the one line a card has room for.
//
// A package doc is written for `gno doc` and often runs to paragraphs, but its
// first sentence is a summary by convention, the same convention Go's own
// tooling leans on. Cutting at the first period followed by a space rather than
// at a character count keeps it a sentence: a hard truncation produces "The
// official blog, rendered on ch" and a card that looks broken.
func firstSentence(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	// A doc that is one sentence with no trailing space still ends in a period.
	if strings.HasSuffix(s, ".") {
		return s
	}
	if len(s) > 180 {
		return strings.TrimSpace(s[:180]) + "…"
	}
	return s
}

// versionSegment matches a trailing generation marker: v0, v1, v23.
var versionSegment = regexp.MustCompile(`^v[0-9]+$`)

// nameFromPath is the last resort, and it is a poor one on purpose: it is what
// a card looks like when nobody has said anything about a realm and the realm
// says nothing about itself, which is exactly the case a contributor should be
// able to spot and fix.
//
// It is still not allowed to be *ambiguous*. The last segment alone produced
// `position`, `staker`, `gns` and two cards both called `staker` on mainnet,
// which tells a reader nothing and tells them it twice. A version segment is
// dropped, because `v0` names a generation and never a project, and the
// namespace is kept, because `gnoswap/position` is a thing somebody can
// recognise and `position` is not.
func nameFromPath(path string) string {
	p := strings.Trim(strings.TrimPrefix(path, "gno.land/"), "/")
	// Drop the kind prefix: every entry here is r/, so it distinguishes nothing.
	if i := strings.Index(p, "/"); i >= 0 && (p[:i] == "r" || p[:i] == "p") {
		p = p[i+1:]
	}
	parts := strings.Split(p, "/")
	// A version belongs to the realm's history, not its name, and it is not
	// always the last segment: gnoswap deploys as `gnoswap/v1/position`, so
	// dropping only a trailing one left `v1/position`. Every version segment
	// goes, and if that leaves nothing the last original segment comes back,
	// because a card with a blank name is worse than one called `v0`.
	kept := parts[:0:0]
	for _, seg := range parts {
		if !versionSegment.MatchString(seg) {
			kept = append(kept, seg)
		}
	}
	if len(kept) > 0 {
		parts = kept
	} else {
		parts = parts[len(parts)-1:]
	}
	// An address namespace is 40 characters of noise to a reader, so a realm
	// deployed under one is named by what follows it.
	if len(parts) > 1 && bech32Namespace.MatchString(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// bech32Namespace matches a realm deployed under a raw address rather than a
// registered username.
var bech32Namespace = regexp.MustCompile(`^g1[0-9a-z]{38}$`)

type appsHubResponse struct {
	Network string    `json:"network,omitempty"`
	Window  string    `json:"window,omitempty"`
	Apps    []AppCard `json:"apps"`

	// Counts, so the page can say what it is and is not showing without the
	// reader having to take its word for the shape of the list.
	Discovered int `json:"discovered"`
	Pinned     int `json:"pinned"`
	OffChain   int `json:"off_chain"`
	Skipped    int `json:"skipped"`
	Realms     int `json:"realms,omitempty"`

	// Moderation is served, not just applied. A skip list nobody can read is
	// indistinguishable from a page that quietly lost an entry.
	Moderation []registry.Skip `json:"moderation"`

	Categories []string        `json:"categories"`
	Awesome    *awesomeSummary `json:"awesome,omitempty"`
}

// appsHubLimit bounds discovery. Generous, because the grid is paged by the
// reader's scroll and a hub that stops at twenty is a hub that hides the long
// tail it exists to surface.
const appsHubLimit = 60

// HandleAppsHub serves the assembled directory.
func (a *API) HandleAppsHub(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	window := r.URL.Query().Get("window")
	if _, ok := usageWindows[window]; !ok {
		window = appsDefaultWindow
	}
	resp := appsHubResponse{Network: network, Window: window}

	skips := a.registry.Moderation.Skipped()
	resp.Moderation = a.registry.Moderation.Skips

	// Layer 1: the chain proposes.
	byPath := map[string]*AppCard{}
	order := []*AppCard{}
	if network != "" {
		found, err := a.db.DiscoverApps(network, usageWindowCutoff(window), appsHubLimit)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		for _, d := range found {
			if _, cut := skips[d.Path]; cut {
				resp.Skipped++
				continue
			}
			c := &AppCard{
				Path: d.Path, Name: nameFromPath(d.Path), NameFrom: fromPath,
				Via:           viaDiscovered,
				Calls:         d.Calls,
				Callers:       d.Callers,
				CallsWindow:   d.CallsWindow,
				CallersWindow: d.CallersWindow,
				LastCall:      d.LastCall,
				DeployedAt:    d.DeployedAt,
				Score:         d.Score,
			}
			if doc := firstSentence(d.PackageDoc); doc != "" {
				c.Description, c.DescriptionFrom = doc, fromChain
			}
			byPath[d.Path] = c
			order = append(order, c)
			resp.Discovered++
		}
		if n, err := a.db.CountRealms(network); err == nil {
			resp.Realms = n
		}
	}

	// Layer 2: everything a human has described is in, traffic or not.
	//
	// Being in apps.json *is* the vouch. An entry somebody wrote a sentence for
	// and that the ranking cannot see, because it just shipped or is quiet by
	// nature, would otherwise vanish from the page it was written for, which is
	// the most surprising thing this design could do to a contributor.
	pinned := []string{}
	for _, app := range a.registry.Apps {
		if _, cut := skips[app.Path]; cut {
			resp.Skipped++
			continue
		}
		if _, have := byPath[app.Path]; have {
			continue
		}
		c := &AppCard{Path: app.Path, Name: nameFromPath(app.Path), NameFrom: fromPath, Via: viaPinned}
		byPath[app.Path] = c
		order = append(order, c)
		pinned = append(pinned, app.Path)
		resp.Pinned++
	}
	if network != "" && len(pinned) > 0 {
		// A pin has no traffic by definition, so discovery never read its doc.
		if docs, err := a.db.PackageDocs(network, pinned); err == nil {
			for p, doc := range docs {
				if c := byPath[p]; c != nil && c.Description == "" {
					c.Description, c.DescriptionFrom = firstSentence(doc), fromChain
				}
			}
		}
		if stats, err := a.db.AppStats(network, pinned, usageWindowCutoff(window)); err == nil {
			for p, s := range stats {
				if c := byPath[p]; c != nil {
					c.Calls, c.Callers, c.CallsWindow = s.Calls, s.Callers, s.CallsWindow
					c.LastCall, c.DeployedAt = s.LastCall, s.DeployedAt
				}
			}
		}
	}

	// Layer 2b: a README, for the realms that carry one and no doc comment.
	//
	// Worth its own pass because on mainnet it is the difference between a
	// described grid and a mostly blank one: most realms have no package
	// comment, and several of the busiest ship a README that opens with exactly
	// the sentence a card wants.
	if network != "" {
		want := []string{}
		for p, c := range byPath {
			if c.Description == "" {
				want = append(want, p)
			}
		}
		if readmes, err := a.db.PackageReadmes(network, want); err == nil {
			for p, lead := range readmes {
				if c := byPath[p]; c != nil && c.Description == "" {
					c.Description, c.DescriptionFrom = firstSentence(lead), fromReadme
				}
			}
		}
	}

	// Layer 3: the community list enriches the realms it names.
	//
	// Only the realms, here. The off-chain half of that list is appended after
	// curation instead, because the name it has to be matched against is
	// usually the curated one: awesome-gno's `Gnoswap` and our `GnoSwap` are
	// one DEX, and at this point in the pass the card is still called
	// `gnoswap/router` after its path.
	a.enrichFromAwesome(byPath, &order, &resp, skips)

	// Layer 4: this repo's own curation wins over everything, because it is the
	// only layer where somebody looked at this page and said "that is wrong".
	for _, app := range a.registry.Apps {
		c := byPath[app.Path]
		if c == nil {
			continue
		}
		if app.Name != "" {
			c.Name, c.NameFrom = app.Name, fromCurated
		}
		if app.Description != "" {
			c.Description, c.DescriptionFrom = firstSentence(app.Description), fromCurated
			c.Checked = app.Checked
		}
		if app.URL != "" {
			c.Website, c.WebsiteFrom = app.URL, fromCurated
		}
		if app.Category != "" {
			c.Category = app.Category
		}
		c.Supersedes = app.Supersedes
		c.Covers = app.Covers
	}

	// Layer 5: the apps that are not on a chain at all.
	//
	// A wallet, an editor extension, an explorer: there is nothing to index and
	// nothing to count, and a hub that could not show them would be answering
	// "what is built on gno.land" with the subset it happens to be able to see.
	a.appendOffChain(&order, &resp)

	// Fold before ranking, not after. A folded card is ranked on what the whole
	// app does, and GnoSwap's router alone is a fraction of that.
	kept := collapseSuperseded(order)
	kept = foldCovered(kept)
	a.refoldStats(kept, network, window)
	rankApps(kept)
	resp.Apps = materialize(kept)
	resp.Categories = categoriesOf(resp.Apps)
	if aw := a.registry.Awesome; aw != nil {
		resp.Awesome = &awesomeSummary{
			Entries: aw.Count(), Synced: aw.Synced,
			AgeDays: aw.SyncedAge(time.Now()), Source: aw.Source,
		}
	}
	JSONResponse(w, resp)
}

// enrichFromAwesome overlays the community list onto the realms it names.
func (a *API) enrichFromAwesome(byPath map[string]*AppCard, order *[]*AppCard, resp *appsHubResponse, skips map[string]string) {
	aw := a.registry.Awesome
	if aw == nil {
		return
	}
	for _, e := range aw.Apps() {
		if e.Path == "" {
			continue
		}
		if _, cut := skips[e.Path]; cut {
			continue
		}
		c := byPath[e.Path]
		if c == nil {
			// Listed by the community, on a chain, and the ranking did not
			// reach it. Being vouched for in public is a reason to show it.
			c = &AppCard{Path: e.Path, Name: nameFromPath(e.Path), NameFrom: fromPath, Via: viaCommunity}
			byPath[e.Path] = c
			*order = append(*order, c)
			resp.OffChain++
		}
		applyAwesome(c, e)
	}
}

// appendOffChain adds the community entries that are not on a chain, and merges
// the ones that are already here under another spelling.
//
// The merge is the correction worth recording. awesome-gno lists `Gnoswap` with
// a website and no realm path; this directory curates `GnoSwap` at
// gno.land/r/gnoswap/router. Nothing joined them, so the hub drew the DEX twice,
// once with 1,253 calls and once as an off-chain entry with none, and a reader
// had no way to tell that the second one was the same product with a lowercase
// s.
//
// Matched on the name with case and punctuation removed, which is deliberately
// the narrowest rule that fixes it. Anything looser (a prefix, a website host,
// an edit distance) starts merging two projects that merely sound alike, and
// this page's whole claim is that it does not guess silently.
func (a *API) appendOffChain(order *[]*AppCard, resp *appsHubResponse) {
	aw := a.registry.Awesome
	if aw == nil {
		return
	}
	byName := map[string]*AppCard{}
	for _, c := range *order {
		if k := normalizeAppName(c.Name); k != "" {
			if _, taken := byName[k]; !taken {
				byName[k] = c
			}
		}
	}
	for _, e := range aw.Apps() {
		if e.Path != "" || e.Site == "" {
			continue
		}
		if c := byName[normalizeAppName(e.Name)]; c != nil {
			// Already on the page as a realm. The community entry still has
			// something to give: usually the website, which a realm cannot
			// know about itself, and the source repository.
			applyAwesome(c, e)
			continue
		}
		c := &AppCard{Via: viaCommunity}
		applyAwesome(c, e)
		*order = append(*order, c)
		byName[normalizeAppName(c.Name)] = c
		resp.OffChain++
	}
}

// normalizeAppName reduces a display name to what two spellings of the same
// project have in common: letters and digits, lowercased.
func normalizeAppName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func applyAwesome(c *AppCard, e registry.AwesomeEntry) {
	if e.Name != "" && (c.NameFrom == fromPath || c.NameFrom == "") {
		c.Name, c.NameFrom = e.Name, fromCommunity
	}
	if e.Description != "" && (c.DescriptionFrom == "" || c.DescriptionFrom == fromPath) {
		c.Description, c.DescriptionFrom = firstSentence(e.Description), fromCommunity
	}
	if e.Site != "" && c.Website == "" {
		c.Website, c.WebsiteFrom = e.Site, fromCommunity
	}
	if e.URL != "" && e.URL != c.Website {
		c.CommunityURL = e.URL
	}
}

// collapseSuperseded folds an older generation into the entry that replaces it.
//
// Two live deployments of the same idea is the normal state of a chain nobody
// can delete from, and a directory that ranks them as peers sends people to
// last year's version, which is worse than not listing it: it is listing it
// with a call count that makes it look current. Kourt is the worked example,
// with v1 and v3 both live and both busy.
//
// Folded rather than dropped. The old one is still on the chain, somebody may
// hold a position in it, and a hub that pretended it was gone would be lying
// about state a reader can check.
func collapseSuperseded(order []*AppCard) []*AppCard {
	replaced := map[string]*AppCard{}
	for _, c := range order {
		for _, old := range c.Supersedes {
			replaced[old] = c
		}
	}
	// Two passes, and the second one is why.
	//
	// Folding while building the output loses every fold whose newer card came
	// first in the ranking: that card has already been copied into the result
	// by value, so appending to the pointer's Previous changes nothing anybody
	// will see. On mainnet that silently dropped the bubblerumble and gnomi/pad
	// chains while Kourt v3, which happened to rank *below* its predecessor,
	// worked. A bug that depends on sort order is one that looks fixed.
	// Follow the chain to the generation that survived, rather than to the one
	// that merely replaced this one. bubblerumble is four deep: 4 replaces 3,
	// 3 replaces 2 and 1, and hanging 1 and 2 off card 3 hangs them off a card
	// that is not in the output, so two generations disappear from a page whose
	// entire promise about the older ones is that it does not pretend they are
	// gone.
	survivor := func(c *AppCard) *AppCard {
		seen := 0
		for {
			newer := replaced[c.Path]
			if newer == nil || newer == c {
				return c
			}
			c = newer
			if seen++; seen > len(order) {
				// A cycle in the data. Stop rather than spin; validation
				// rejects the obvious form of it (an entry superseding itself)
				// and this covers the rest.
				return c
			}
		}
	}
	for _, c := range order {
		if newer := replaced[c.Path]; newer != nil && c.Path != "" {
			// Carried without its own Previous, so a four-generation chain does
			// not nest: the reader wants "and the ones before", flat.
			old := *c
			old.Previous = nil
			into := survivor(newer)
			into.Previous = append(into.Previous, old)
		}
	}
	out := make([]*AppCard, 0, len(order))
	for _, c := range order {
		if newer := replaced[c.Path]; newer != nil && c.Path != "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// foldCovered folds a realm into the card of the app it is part of.
//
// The other half of "one app, one card", and the half that is about the
// present rather than the past. A chain shows an app as the several realms it
// was deployed as: GnoSwap is a router, a token, positions, a staker, an NFT
// and a governance staker, all busy, all current, all one DEX. Discovery ranks
// realms, so it ranked six of them as peers, five under names taken from their
// paths, and told a visitor there were six DEXes. The same is true of
// governance, where r/sys/params and r/sys/users are the things GovDAO writes
// to rather than three separate products.
//
// The parts are carried, not dropped, and each keeps its own link: they are
// real realms with real state, and somebody who came looking for
// r/gnoswap/position has to be able to reach it.
func foldCovered(cards []*AppCard) []*AppCard {
	parentOf := map[string]*AppCard{}
	for _, parent := range cards {
		for _, pat := range parent.Covers {
			for _, c := range cards {
				switch {
				case c == parent || c.Path == "":
				case len(c.Covers) > 0:
					// A card that covers is never itself a part. Two apps
					// claiming each other would otherwise fold the page flat.
				case parentOf[c.Path] != nil:
					// First claim wins, so two overlapping prefixes are
					// stable rather than dependent on map order.
				case coversPath(pat, c.Path):
					parentOf[c.Path] = parent
				}
			}
		}
	}
	if len(parentOf) == 0 {
		return cards
	}
	out := make([]*AppCard, 0, len(cards))
	for _, c := range cards {
		if parent := parentOf[c.Path]; parent != nil {
			part := *c
			// Flat, like Previous: the reader wants "and the realms it is made
			// of", not a tree.
			part.Parts, part.Previous = nil, nil
			parent.Parts = append(parent.Parts, part)
			continue
		}
		out = append(out, c)
	}
	return out
}

// refoldStats re-reads a folded card's figures over every realm it folded.
//
// One app is one card, so it has to be one set of numbers too, and a card that
// folded five realms and then reported the traffic of one of them was quietly
// the wrong claim in both directions: it under-counts the app, and it ranks it
// below realms it dwarfs.
//
// Both kinds of fold count, and the second one is the correction. A part is
// obviously the same app. A *superseded generation* is the same app as well,
// just at an earlier date: bubblerumble4 shipped this morning with 348 calls
// while the pools on bubblerumble3 carry 4,160, and a card that showed only the
// new one said the game was three days old and barely played.
//
// Re-read rather than summed, because calls add up and callers do not: the same
// people use GnoSwap's router and its staker, and they follow a game from one
// generation to the next. AppFamilyStat answers both exactly with one query,
// and the score is recomputed from the window with discovery's own weights, so
// a folded card is ranked against the others on the same scale.
func (a *API) refoldStats(cards []*AppCard, network, window string) {
	if network == "" {
		return
	}
	for _, c := range cards {
		if len(c.Parts) == 0 && len(c.Previous) == 0 {
			continue
		}
		paths := make([]string, 0, len(c.Parts)+len(c.Previous)+1)
		add := func(p string) {
			if p != "" {
				paths = append(paths, p)
			}
		}
		add(c.Path)
		for _, p := range c.Parts {
			add(p.Path)
		}
		for _, p := range c.Previous {
			add(p.Path)
		}
		fam, err := a.db.AppFamilyStat(network, paths, usageWindowCutoff(window))
		if err != nil {
			continue
		}
		c.Calls, c.Callers = fam.Calls, fam.Callers
		c.CallsWindow, c.CallersWindow = fam.CallsWindow, fam.CallersWindow
		if fam.LastCall > c.LastCall {
			c.LastCall = fam.LastCall
		}
		c.Score = fam.CallersWindow*store.ScoreCallerWeight + fam.CallsWindow*store.ScoreCallWeight
		// What the figures now cover, so the card can say so rather than
		// letting a reader assume one realm.
		c.FoldedRealms = len(paths)
	}
}

// coversPath reports whether a `covers` pattern names this path. A trailing
// `/*` is a prefix; anything else is exact.
func coversPath(pattern, path string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(path, prefix+"/")
	}
	return pattern == path
}

func materialize(cards []*AppCard) []AppCard {
	out := make([]AppCard, 0, len(cards))
	for _, c := range cards {
		out = append(out, *c)
	}
	return out
}

// scoreVouched is the rank a human's say-so is worth.
//
// The second tuning knob, and it exists because the two halves of this list are
// scored in incomparable units. A realm has callers; Adena has none, because a
// browser extension is not a realm and never will be. Ranking purely on chain
// activity therefore buries every off-chain app below the last realm with two
// calls, which is the opposite of useful: the wallet is the single most likely
// thing a visitor came here to find.
//
// So being vouched for in public is itself a signal, and this is what it is
// worth in caller-equivalents. Set so a listed app outranks a realm nobody much
// uses and loses to one people genuinely do, which is the ordering a reader
// would produce by hand. Tune it here and nowhere else.
const scoreVouched = 5 * 10 // five callers' worth, at ScoreCallerWeight

// rankApps orders the merged list.
//
// One rule: score, with anything a human listed floored rather than boosted.
//
// Floored, not promoted, and that is the correction worth recording. Ranking a
// listed entry to the top first put Boards2, which nobody had called, above the
// busiest realm on the chain. Being listed means *included* despite having no
// metrics; it does not mean important. A realm people genuinely use still wins,
// and the floor only keeps a vouched-for app from sinking below the last realm
// with two calls.
func rankApps(cards []*AppCard) {
	rank := func(c *AppCard) int {
		s := c.Score
		if c.Via != viaDiscovered || c.NameFrom == fromCurated {
			if s < scoreVouched {
				s = scoreVouched
			}
		}
		return s
	}
	sort.SliceStable(cards, func(i, j int) bool {
		ri, rj := rank(cards[i]), rank(cards[j])
		if ri != rj {
			return ri > rj
		}
		// Stable within a tie on the name, so two runs of the same data draw
		// the same page and a screenshot diff means something.
		return cards[i].Name < cards[j].Name
	})
}

func categoriesOf(apps []AppCard) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, a := range apps {
		if a.Category != "" && !seen[a.Category] {
			seen[a.Category] = true
			out = append(out, a.Category)
		}
	}
	sort.Strings(out)
	return out
}
