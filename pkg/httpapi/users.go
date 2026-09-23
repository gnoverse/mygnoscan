package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/moul/mygnoscan/pkg/store"
)

// The user registry, served for the search box and the address page.
//
// Separate from /api/search rather than folded into it, for the same reason
// assets and symbols are: that endpoint answers with a flat array of packages
// and every caller reads it as one. A user is not a package -- 67 of mainnet's
// 78 registrations on 2026-09-23 have never deployed anything, so folding them
// in would mean inventing a package row for a name that has none.
//
// Distinct from /api/namespaces, which answers a narrower question with a
// different method: that one resolves the handful of names that *do* own
// packages, one vm/qeval per name against the live chain, for the ownership
// labels the frontend paints on a path. This is the registry itself.

// usersResponse is the search box's user group.
type usersResponse struct {
	Network string    `json:"network"`
	Users   []userRow `json:"users"`
}

// userRow is one registration, with whatever else is known about the address
// merged in.
type userRow struct {
	store.User
	// Label is the curated name for the address, when the registry file has
	// one. Shown beside the registration rather than instead of it: the
	// registered name is what the chain says, and the curated one is the
	// human-readable gloss on it (`@jaekwon` the person, not the namespace).
	Label string `json:"label,omitempty"`
}

// userSearchLimit bounds the group. The search popup shows four or five
// sections at once, so a group that can fill the viewport on its own is a group
// that hides the others.
const userSearchLimit = 6

// HandleUserSearch answers the search box's user group.
func (a *API) HandleUserSearch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonError(w, "missing q parameter", 400)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = userSearchLimit
	}
	found, err := a.db.SearchUsers(network, q, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	rows := make([]userRow, len(found))
	for i, u := range found {
		row := userRow{User: u}
		if e, ok := a.registry.Addresses[u.Address]; ok {
			row.Label = e.Label
		}
		rows[i] = row
	}
	JSONResponse(w, usersResponse{Network: network, Users: rows})
}
