package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/moul/mygnoscan/pkg/store"
)

// Code search: grep across every .gno file the chain holds.
//
// This is the one thing an explorer can offer that no node can. gno stores
// source on chain, so the corpus is complete and already in our database; the
// chain itself has no query that reads it. Until now the honest way to answer
// "has anyone written this before" was to clone tx-exports and grep.

// CodeSearchResponse is one query's answer.
type CodeSearchResponse struct {
	Query   string          `json:"query"`
	Network string          `json:"network"`
	Hits    []store.CodeHit `json:"hits"`
	Count   int             `json:"count"`
	// Indexed is how many files the index holds for this network. It is on
	// every response so "no results" can be told apart from "nothing indexed
	// yet", which look identical and mean opposite things.
	Indexed int  `json:"indexed"`
	Capped  bool `json:"capped"`
	MaxHits int  `json:"max_hits"`
}

func (a *API) HandleCodeSearch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}

	hits, err := a.db.SearchCode(store.CodeSearchOpts{
		Network: network,
		Query:   q,
		Kind:    r.URL.Query().Get("kind"),
		Limit:   limit,
	})
	if err != nil {
		// A query the reader wrote wrong is their problem to fix and needs the
		// message; anything else is ours and must not leak SQL.
		var bad *store.BadQueryError
		if errors.As(err, &bad) {
			jsonError(w, "that is not a valid search query: "+bad.Error(), 400)
			return
		}
		jsonError(w, "search failed", 500)
		return
	}

	indexed, _ := a.db.CodeIndexSize(network)
	resp := CodeSearchResponse{
		Query:   q,
		Network: network,
		Hits:    hits,
		Count:   len(hits),
		Indexed: indexed,
		Capped:  len(hits) >= limit,
		MaxHits: limit,
	}
	if resp.Hits == nil {
		resp.Hits = []store.CodeHit{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
