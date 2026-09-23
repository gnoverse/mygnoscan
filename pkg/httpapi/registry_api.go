package httpapi

import (
	"net/http"
	"sort"
	"strings"

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
	for _, app := range a.registry.Apps {
		if !seen[app.Category] {
			seen[app.Category] = true
			cats = append(cats, app.Category)
		}
		paths = append(paths, app.Path)
	}
	sort.Strings(cats)
	resp := appsResponse{Categories: cats, Apps: a.registry.Apps, Tokens: len(a.registry.Tokens)}

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
