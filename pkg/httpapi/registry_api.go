package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/registry"
	"github.com/moul/mygnoscan/pkg/store"
)

// Serving the curated registry, merged with what the chain proves.
//
// /api/labels used to return only derived labels, and the curated names lived
// in a const inside the frontend. That split meant the two could not be ranked
// against each other, and that contributing a name meant editing JavaScript.
// Both now come from here.

// labelPrecedence orders the provenance kinds, strongest first.
//
// Curated wins over derived, which is worth saying because the obvious argument
// runs the other way: a derived label is proved and live, a curated one is a
// human's say-so and can rot. The reason it still loses is the registry's own
// rule, that a curated entry is only ever added for something that *cannot* be
// derived. An address carrying both therefore means a person looked at the
// derived name and decided a better one was needed, and overriding that with
// the machine's answer throws away the more informative label: "@jaekwon" is a
// person, "@jaekwon" derived from a namespace is a coincidence, and the derived
// name for a genesis deployer is whichever namespace it happened to deploy most
// of.
//
// Declared loses to both because it is attacker-controlled: anyone may call
// UpdateDescription on r/gnops/valopers and claim any name. Inferred is last
// because it is this repo guessing, and it always carries its evidence.
var labelPrecedence = map[string]int{
	registry.KindCurated:  4,
	registry.KindDerived:  3,
	registry.KindDeclared: 2,
	registry.KindInferred: 1,
}

// mergeLabels overlays the curated registry onto the derived labels.
//
// Both sides are kept in the merged value's Why, so a reader who hovers a
// curated name can still see what the chain independently says about it. A
// merge that dropped the loser would be throwing away the corroboration.
func mergeLabels(derived map[string]store.AddressLabel, curated map[string]registry.Entry) map[string]store.AddressLabel {
	out := make(map[string]store.AddressLabel, len(derived)+len(curated))
	for addr, l := range derived {
		out[addr] = l
	}
	for addr, e := range curated {
		candidate := store.AddressLabel{Label: e.Label, Kind: e.Kind, Why: e.Why}
		if e.Checked != "" {
			candidate.Why = appendChecked(candidate.Why, e.Checked)
		}
		existing, ok := out[addr]
		if !ok {
			out[addr] = candidate
			continue
		}
		winner, loser := candidate, existing
		if labelPrecedence[existing.Kind] > labelPrecedence[candidate.Kind] {
			winner, loser = existing, candidate
		}
		if loser.Label != "" && loser.Label != winner.Label {
			winner.Why = strings.TrimSpace(winner.Why) + "\nalso " + loser.Kind + ": " + loser.Label
			if loser.Why != "" {
				winner.Why += " (" + loser.Why + ")"
			}
		}
		out[addr] = winner
	}
	return out
}

// appendChecked dates a claim inline, so the age travels with the evidence
// rather than needing another field on the wire.
func appendChecked(why, checked string) string {
	if why == "" {
		return "measured " + checked
	}
	return why + " (measured " + checked + ")"
}

func (a *API) HandleLabels(w http.ResponseWriter, r *http.Request) {
	derived, err := a.db.DerivedAddressLabels(a.networkParam(r))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, mergeLabels(derived, a.registry.Addresses))
}

type appsResponse struct {
	Categories []string       `json:"categories"`
	Apps       []registry.App `json:"apps"`
	Tokens     int            `json:"tokens"`

	// Everything below is what the chain says, and it is present only when a
	// single network was asked for. The same path is a different deployment on
	// each chain, so a blended call count would be an invented figure.
	Network string `json:"network,omitempty"`
	Window  string `json:"window,omitempty"`
	// Stats is keyed by path. An entry the selected chain has never seen is
	// absent rather than zeroed: "not deployed here" and "deployed and unused"
	// are different facts and the card draws them differently.
	Stats map[string]store.AppStat `json:"stats,omitempty"`
	// Candidates are the busiest realms this directory does not mention, and
	// Realms is how many exist. Together they are the page's own admission
	// that a curated list is always behind the chain.
	Candidates []store.AppCandidate `json:"candidates,omitempty"`
	Realms     int                  `json:"realms,omitempty"`

	// Awesome is a three-number summary of the community list, carried here so
	// the directory's own introduction can point at it with a figure rather
	// than a vague nudge, without this page paying for the whole snapshot. The
	// snapshot itself is a separate request, made when a reader opens the tab.
	Awesome *awesomeSummary `json:"awesome,omitempty"`
}

// awesomeSummary is the headline of the cross-check: how big the community list
// is, and how far the two lists are from agreeing with each other.
type awesomeSummary struct {
	Entries int `json:"entries"`
	// MissingFromAwesome counts directory entries the community list does not
	// name. It is the number the contribute call to action is built on.
	MissingFromAwesome int `json:"missing_from_awesome"`
	// MissingFromDirectory counts realms they name and we do not describe.
	MissingFromDirectory int    `json:"missing_from_directory"`
	Synced               string `json:"synced"`
	AgeDays              int    `json:"age_days"`
	Source               string `json:"source"`
}

// appsDefaultWindow is what "recently" means on the directory when the reader
// has not said. 30d rather than 7d because an app can be perfectly healthy and
// quiet for a week, and a directory that calls it dead on that basis is worse
// than one with no numbers at all.
const appsDefaultWindow = "30d"

// appsCandidateLimit bounds the suggestion queue. It is a worklist, not a
// second directory: a hundred rows of unlabelled paths is the listing page,
// which already exists.
const appsCandidateLimit = 12

// HandleApps serves the curated directory.
//
// Network-independent on purpose: an app is a description of what a realm is
// for, which does not change between chains even where the deployment does.
// Whether it is actually deployed on the selected chain is a question the realm
// page answers, and the page links through to it.
func (a *API) HandleApps(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	cats := []string{}
	paths := make([]string, 0, len(a.registry.Apps))
	described := make([]registry.App, 0, len(a.registry.Apps))
	for _, app := range a.registry.Apps {
		// A relation-only entry ("v3 replaces v2") is a fact about two deploys,
		// not a directory entry: it has no name and describes nothing. /apps
		// uses it to fold one card into another; this endpoint is the curated
		// *directory*, so it has nothing to say about it.
		if app.Description == "" {
			continue
		}
		described = append(described, app)
		if !seen[app.Category] {
			seen[app.Category] = true
			cats = append(cats, app.Category)
		}
		paths = append(paths, app.Path)
	}
	sort.Strings(cats)
	resp := appsResponse{Categories: cats, Apps: described, Tokens: len(a.registry.Tokens)}
	if aw := a.registry.Awesome; aw != nil {
		resp.Awesome = &awesomeSummary{
			Entries:              aw.Count(),
			MissingFromAwesome:   len(a.registry.MissingFromAwesome()),
			MissingFromDirectory: len(a.registry.MissingFromDirectory()),
			Synced:               aw.Synced,
			AgeDays:              aw.SyncedAge(time.Now()),
			Source:               aw.Source,
		}
	}

	// The directory itself answers for every chain; its usage figures cannot.
	// Rather than 400 the whole page the way the graph endpoints do, the
	// description survives and only the numbers drop out, because a reader who
	// came to find out what Boards2 is should get an answer without first
	// picking a chain.
	network := a.networkParam(r)
	if network != "" {
		window := r.URL.Query().Get("window")
		if _, ok := usageWindows[window]; !ok {
			window = appsDefaultWindow
		}
		since := usageWindowCutoff(window)
		resp.Network, resp.Window = network, window

		stats, err := a.db.AppStats(network, paths, since)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		resp.Stats = stats

		candidates, err := a.db.AppCandidates(network, paths, since, appsCandidateLimit)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		resp.Candidates = candidates

		if n, err := a.db.CountRealms(network); err == nil {
			resp.Realms = n
		}
	}
	JSONResponse(w, resp)
}

// The community list, and the gap between it and ours.
//
// awesome-gno is the other curated answer to "what is being built on gno.land",
// and it holds the part this explorer is structurally blind to: a wallet, an
// editor extension, a language server and a workshop are not realms, so no
// amount of indexing will ever surface them. Serving it here is not a mirror
// for its own sake; it is what makes /apps a complete answer to the question
// people actually arrive with.
//
// It is served from the vendored snapshot rather than fetched from GitHub per
// request. See pkg/registry/awesome.go for why, and `make awesome` for how it
// is refreshed.

type awesomeResponse struct {
	Source       string `json:"source"`
	Readme       string `json:"readme"`
	Contributing string `json:"contributing"`
	Commit       string `json:"commit"`
	Synced       string `json:"synced"`
	// AgeDays is computed rather than left to the browser, so the page says the
	// same thing to a reader whose clock is wrong.
	AgeDays int `json:"age_days"`
	Entries int `json:"entries"`

	// Apps is what the page draws: the entries with a page you can open, which
	// is the only kind there is anything to photograph. Everything else on the
	// list is real and is counted in Others rather than rendered, because a
	// grid of pictures with grey holes where the SDKs are reads as broken.
	Apps   []registry.AwesomeEntry `json:"apps"`
	Others []registry.AwesomeGroup `json:"others"`

	// The cross-check, in both directions. Neither is a promotion: one is the
	// list of realms we describe that the community has not named, the other
	// the realms they name that we have not described.
	MissingFromAwesome   []registry.App                 `json:"missing_from_awesome"`
	MissingFromDirectory []registry.AwesomeRef          `json:"missing_from_directory"`
	InDirectory          map[string]registry.AwesomeRef `json:"in_directory"`
	// DirectoryByName is the same overlap keyed the other way, by the community
	// list's own spelling, so an entry in the grid can say "this one is
	// described next door" without the browser re-deriving the match.
	DirectoryByName map[string]string `json:"directory_by_name"`

	// Chain figures, present only when a single network was asked for, for the
	// same reason as on /api/registry/apps: the same path is a different
	// deployment per chain and a blended count is an invented one.
	Network string                   `json:"network,omitempty"`
	Window  string                   `json:"window,omitempty"`
	Stats   map[string]store.AppStat `json:"stats,omitempty"`
}

// HandleAwesome serves the vendored awesome-gno snapshot, cross-checked against
// the directory.
func (a *API) HandleAwesome(w http.ResponseWriter, r *http.Request) {
	aw := a.registry.Awesome
	if aw == nil {
		jsonError(w, "no awesome snapshot", 500)
		return
	}
	resp := awesomeResponse{
		Source:               aw.Source,
		Readme:               aw.Readme,
		Contributing:         aw.Contrib,
		Commit:               aw.Commit,
		Synced:               aw.Synced,
		AgeDays:              aw.SyncedAge(time.Now()),
		Entries:              aw.Count(),
		Apps:                 aw.Apps(),
		Others:               aw.Others(),
		MissingFromAwesome:   a.registry.MissingFromAwesome(),
		MissingFromDirectory: a.registry.MissingFromDirectory(),
		InDirectory:          a.registry.AwesomeInDirectory(),
		DirectoryByName:      a.registry.DirectoryByAwesomeName(),
	}

	network := a.networkParam(r)
	if network == "" {
		JSONResponse(w, resp)
		return
	}
	window := r.URL.Query().Get("window")
	if _, ok := usageWindows[window]; !ok {
		window = appsDefaultWindow
	}
	resp.Network, resp.Window = network, window

	// One query for both sides of the page: the realms the community list names,
	// and the directory entries it does not. The second set is what makes the
	// invitation rankable, because "this realm has 4,000 calls and nobody has
	// added it to the community list" is an argument and "please contribute" is
	// not.
	seen := map[string]bool{}
	paths := []string{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, e := range aw.Apps() {
		add(e.Path)
	}
	for _, app := range resp.MissingFromAwesome {
		add(app.Path)
	}
	stats, err := a.db.AppStats(network, paths, usageWindowCutoff(window))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	resp.Stats = stats
	JSONResponse(w, resp)
}
