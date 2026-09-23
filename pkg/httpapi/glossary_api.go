package httpapi

import (
	"net/http"

	"github.com/moul/mygnoscan/pkg/glossary"
)

// glossaryResponse is the whole endpoint.
//
// Keyed by term rather than a list, because every consumer's question is "what
// does this word mean" and none of them wants to scan. Order carries the
// document's own ordering alongside it, so a page rendering the full table does
// not sort it into a different one than docs/glossary.md shows.
type glossaryResponse struct {
	Version string                    `json:"version"`
	Count   int                       `json:"count"`
	Order   []string                  `json:"order"`
	Terms   map[string]glossary.Entry `json:"terms"`
}

// HandleGlossary serves docs/glossary.md, parsed.
//
// No network parameter: the words mean the same thing on every chain, and
// taking one would invite a caller to believe otherwise.
func (a *API) HandleGlossary(w http.ResponseWriter, r *http.Request) {
	g := glossary.Get()
	if g == nil {
		// Only reachable from a test binary that did not load it: the server's
		// root package parses at init and panics on a bad file, so a running
		// server either has a valid glossary or is not running. Saying which is
		// better than serving an empty table that reads as "no terms defined".
		http.Error(w, `{"error":"the glossary was not loaded into this binary"}`, http.StatusServiceUnavailable)
		return
	}
	JSONResponse(w, glossaryResponse{
		Version: g.Version,
		Count:   len(g.Order),
		Order:   g.Order,
		Terms:   g.Terms,
	})
}
