package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/moul/mygnoscan/pkg/gnoaddr"
	"github.com/moul/mygnoscan/pkg/store"
)

// /api/pulse — what the chain did inside a window, in one request.
//
// The home page needs five ranked lists and two sets of counters to paint one
// screen. As five endpoints that is five round trips for a single view, on a
// page whose whole job is to be the fast answer; as one it is one cache entry
// and one SWR paint. The lists are independent of each other, so nothing here
// is a join across them — it is one handler because the *reader* asks one
// question, not because the data is coupled.

// pulseWindow is one selectable span.
//
// Offered as a fixed list rather than an arbitrary duration because each one is
// a cache key: a free-form ?since= would give every visitor their own entry and
// the cache would never hit. The five cover the questions people actually have,
// and 7d is one click away because deploys are sparse — mainnet ran between 0
// and 44 of them a day over the week to 2026-09-23, so the two deploy-driven
// panels are empty on most 24h windows and full on every 7d one.
type pulseWindow struct {
	Key   string
	Label string
	D     time.Duration
}

var pulseWindows = []pulseWindow{
	{"1h", "last hour", time.Hour},
	{"6h", "last 6 hours", 6 * time.Hour},
	{"24h", "last 24 hours", 24 * time.Hour},
	{"7d", "last 7 days", 7 * 24 * time.Hour},
	{"30d", "last 30 days", 30 * 24 * time.Hour},
}

// defaultPulseWindow is what an unnamed window means. A day is the shortest
// span in which every panel on the page has something in it on a normal day.
const defaultPulseWindow = "24h"

// resolvePulseWindow never errors. An unknown key falls back to the default the
// same way an unknown directory sort does: a hand-edited URL should land on
// something sensible rather than on a 400, and the response says which window it
// actually used so the caller is never guessing.
func resolvePulseWindow(key string) pulseWindow {
	for _, w := range pulseWindows {
		if w.Key == key {
			return w
		}
	}
	for _, w := range pulseWindows {
		if w.Key == defaultPulseWindow {
			return w
		}
	}
	return pulseWindows[0]
}

const (
	pulseLimitDefault = 10
	pulseLimitMax     = 50
)

func pulseLimit(r *http.Request) int {
	v, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || v <= 0 {
		return pulseLimitDefault
	}
	return min(v, pulseLimitMax)
}

// pulseWindowMeta is the window the response was actually computed over.
//
// Spelled out rather than left implicit because this endpoint is cached and may
// be served stale: "last hour" painted from a fifteen-minute-old entry is the
// hour that ended fifteen minutes ago, and a reader can only tell if the
// boundaries are on the page.
type pulseWindowMeta struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Seconds   int    `json:"seconds"`
	Since     string `json:"since"`
	Until     string `json:"until"`
	PrevSince string `json:"prev_since"`
	// Options lets the frontend build the selector from the server's list
	// instead of keeping a second copy of it that can drift.
	Options []pulseWindowOption `json:"options"`
}

type pulseWindowOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// pulseFlow is a transfer with both ends resolved.
//
// FromPath and ToPath are the answer to "is this a person or a realm", which is
// the first thing anyone asks of a transfer list and which no column of the
// database holds: a package's account is a hash of its path, so the only way
// back is to derive every known path forward (see pkg/gnoaddr.Reverse). Absent
// means "not a package this explorer knows", which on a synced chain reads as a
// human — the frontend says it that way round rather than claiming certainty.
type pulseFlow struct {
	store.HotFlow
	FromPath    string `json:"from_path,omitempty"`
	FromDeposit bool   `json:"from_deposit,omitempty"`
	ToPath      string `json:"to_path,omitempty"`
	ToDeposit   bool   `json:"to_deposit,omitempty"`
}

type pulseResponse struct {
	Network string          `json:"network,omitempty"`
	Window  pulseWindowMeta `json:"window"`
	// Current and Prev are the same shape over adjacent equal windows, so a
	// caller can compute a delta for any field without a table of which ones
	// are comparable.
	Current store.PulseCounts `json:"current"`
	Prev    store.PulseCounts `json:"prev"`
	HasPrev bool              `json:"has_prev"`

	HotRealms []store.HotRealm `json:"hot_realms"`
	HotTokens []store.HotToken `json:"hot_tokens"`
	HotFlows  []pulseFlow      `json:"hot_flows"`
	HotDevs   []store.HotDev   `json:"hot_devs"`
	HotLibs   []store.HotLib   `json:"hot_libs"`
}

func (a *API) HandlePulse(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	win := resolvePulseWindow(r.URL.Query().Get("window"))

	now := time.Now().UTC()
	since := now.Add(-win.D)
	prevSince := since.Add(-win.D)

	pulse, err := a.db.GetPulse(store.PulseParams{
		Network:   network,
		Since:     since.Format(time.RFC3339),
		PrevSince: prevSince.Format(time.RFC3339),
		Limit:     pulseLimit(r),
	})
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Built once per request over every path this chain knows: a few hundred
	// paths, two hashes each. Not cached beyond the response cache in front of
	// this handler, because a map that outlives a deploy would stop resolving
	// exactly the realms a reader came to look at.
	paths, err := a.db.PackagePaths(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	rev := gnoaddr.NewReverse(paths)

	flows := make([]pulseFlow, 0, len(pulse.HotFlows))
	for _, f := range pulse.HotFlows {
		pf := pulseFlow{HotFlow: f}
		if o, ok := rev.Lookup(f.From); ok {
			pf.FromPath, pf.FromDeposit = o.Path, o.Deposit
		}
		if o, ok := rev.Lookup(f.To); ok {
			pf.ToPath, pf.ToDeposit = o.Path, o.Deposit
		}
		flows = append(flows, pf)
	}

	options := make([]pulseWindowOption, 0, len(pulseWindows))
	for _, o := range pulseWindows {
		options = append(options, pulseWindowOption{Key: o.Key, Label: o.Label})
	}

	JSONResponse(w, pulseResponse{
		Network: network,
		Window: pulseWindowMeta{
			Key:       win.Key,
			Label:     win.Label,
			Seconds:   int(win.D / time.Second),
			Since:     since.Format(time.RFC3339),
			Until:     now.Format(time.RFC3339),
			PrevSince: prevSince.Format(time.RFC3339),
			Options:   options,
		},
		Current:   pulse.Window,
		Prev:      pulse.Prev,
		HasPrev:   pulse.HasPrev,
		HotRealms: pulse.HotRealms,
		HotTokens: pulse.HotTokens,
		HotFlows:  flows,
		HotDevs:   pulse.HotDevs,
		HotLibs:   pulse.HotLibs,
	})
}
