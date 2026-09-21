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
}

// HandleApps serves the curated directory.
//
// Network-independent on purpose: an app is a description of what a realm is
// for, which does not change between chains even where the deployment does.
// Whether it is actually deployed on the selected chain is a question the realm
// page answers, and the page links through to it.
func (a *API) HandleApps(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	cats := []string{}
	for _, app := range a.registry.Apps {
		if !seen[app.Category] {
			seen[app.Category] = true
			cats = append(cats, app.Category)
		}
	}
	sort.Strings(cats)
	JSONResponse(w, appsResponse{Categories: cats, Apps: a.registry.Apps, Tokens: len(a.registry.Tokens)})
}
