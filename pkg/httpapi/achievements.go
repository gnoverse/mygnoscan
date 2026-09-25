package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/moul/mygnoscan/pkg/achievements"
	"github.com/moul/mygnoscan/pkg/store"
)

// The achievements API: the catalog, who holds each badge, what one address
// holds, and the people directory that filters on all of it.
//
// Every response carries the catalog entry, not just the slug. The badge's
// `how` line is the reason this feature exists (see pkg/achievements), and a
// payload that made the frontend hold its own copy of the wording would put the
// two out of step the first time one is edited.

// achievementView is one catalog entry as the API serves it: the definition,
// plus the two things only the database knows.
type achievementView struct {
	achievements.Def

	// Holders is how many addresses on this network hold it. Zero is a real
	// answer and is shown as one: a badge nobody has yet is the interesting
	// end of the list, not a gap.
	Holders int `json:"holders"`

	// GroupLabel saves every consumer from carrying the group-to-words map.
	GroupLabel string `json:"group_label"`
}

func (a *API) achievementViews(network string) ([]achievementView, error) {
	counts, err := a.db.AchievementCounts(network)
	if err != nil {
		return nil, err
	}
	out := make([]achievementView, 0, len(achievements.Catalog))
	for _, def := range achievements.Catalog {
		out = append(out, achievementView{
			Def:        def,
			Holders:    counts[def.Slug],
			GroupLabel: achievements.GroupLabel[def.Group],
		})
	}
	return out, nil
}

// HandleAchievements serves the whole catalog with holder counts.
func (a *API) HandleAchievements(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	views, err := a.achievementViews(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	groups := make([]map[string]any, 0, len(achievements.GroupOrder))
	for _, g := range achievements.GroupOrder {
		groups = append(groups, map[string]any{"group": g, "label": achievements.GroupLabel[g]})
	}
	JSONResponse(w, map[string]any{
		"network":      network,
		"groups":       groups,
		"achievements": views,
		// Badges are rebuilt on a timer, so the page can say how fresh they
		// are rather than implying a live read.
		"computed_at": a.db.AchievementsComputedAt(),
	})
}

// HandleAchievement serves one badge and who holds it, earliest first.
func (a *API) HandleAchievement(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	def := achievements.Lookup(slug)
	if def == nil {
		jsonError(w, "unknown achievement: "+slug, 404)
		return
	}
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	holders, total, err := a.db.AchievementHolders(network, slug, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, map[string]any{
		"network":     network,
		"achievement": achievementView{Def: *def, Holders: total, GroupLabel: achievements.GroupLabel[def.Group]},
		"holders":     holders,
		"total":       total,
		"limit":       len(holders),
		"offset":      offset,
		"computed_at": a.db.AchievementsComputedAt(),
	})
}

// HandleAddressAchievements serves one address's badges, against the full
// catalog so the page can show what is still locked.
//
// The locked half is the half that teaches. A grid of only what somebody has
// already done is a trophy case; a grid that also says "you have never wrapped
// ugnot, and here is the command" is the thing that makes them try it.
func (a *API) HandleAddressAchievements(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("addr")
	network := a.networkParam(r)

	unlocked, err := a.db.AddressAchievements(network, addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	byslug := make(map[string]store.Unlock, len(unlocked))
	for _, u := range unlocked {
		byslug[u.Slug] = u
	}

	// session-used is the one badge no index can answer: a session signs as its
	// master, so the only surviving trace is the Sequence on a live grant. Read
	// it here, best-effort, exactly the way the address page reads the grants
	// themselves — an unreachable RPC or a chain without the feature leaves the
	// badge locked rather than taking the page down.
	if network != "" {
		if sessions, ok := fetchSessions(r.Context(), addr, a.rpcURLFor(network)); ok {
			for _, s := range sessions {
				if s.Sequence > 0 {
					byslug["session-used"] = store.Unlock{Slug: "session-used"}
					break
				}
			}
		}
	}

	counts, err := a.db.AchievementCounts(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	type entry struct {
		achievementView
		Unlocked bool `json:"unlocked"`
		Height   int  `json:"block_height,omitempty"`
		// Omitted rather than zero-valued when locked: a badge showing a block
		// time it never earned is worse than one showing nothing.
		Time   string `json:"block_time,omitempty"`
		TxHash string `json:"tx_hash,omitempty"`
	}

	out := make([]entry, 0, len(achievements.Catalog))
	earned := 0
	for _, def := range achievements.Catalog {
		e := entry{achievementView: achievementView{
			Def: def, Holders: counts[def.Slug], GroupLabel: achievements.GroupLabel[def.Group],
		}}
		if u, ok := byslug[def.Slug]; ok {
			e.Unlocked, e.Height, e.Time, e.TxHash = true, u.Height, u.Time, u.TxHash
			earned++
		}
		out = append(out, e)
	}

	JSONResponse(w, map[string]any{
		"address":      addr,
		"network":      network,
		"achievements": out,
		"earned":       earned,
		"total":        len(achievements.Catalog),
		"computed_at":  a.db.AchievementsComputedAt(),
	})
}

// HandleDirectoryPeople serves the people directory: every address that has done
// something, ranked by how much of the catalog it has covered, narrowable by
// badge.
func (a *API) HandleDirectoryPeople(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// `has` is repeatable and also accepts one comma-separated value, because
	// both shapes turn up: the frontend builds one per chip, a person typing a
	// URL writes the list.
	var has []string
	for _, v := range q["has"] {
		for _, slug := range strings.Split(v, ",") {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			// Validated against the catalog rather than passed through. An
			// unknown slug would otherwise match nothing and read as "nobody
			// has done this", which is a different and wrong answer from
			// "there is no such badge".
			if achievements.Lookup(slug) == nil {
				jsonError(w, "unknown achievement: "+slug, 400)
				return
			}
			has = append(has, slug)
		}
	}

	people, total, err := a.db.DirectoryPeople(store.PeopleQuery{
		Network:   a.networkParam(r),
		Q:         strings.TrimSpace(q.Get("q")),
		Has:       has,
		NamedOnly: q.Get("named") == "1",
		Sort:      q.Get("sort"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	JSONResponse(w, map[string]any{
		"network":     a.networkParam(r),
		"people":      people,
		"total":       total,
		"limit":       len(people),
		"offset":      offset,
		"has":         has,
		"computed_at": a.db.AchievementsComputedAt(),
	})
}
