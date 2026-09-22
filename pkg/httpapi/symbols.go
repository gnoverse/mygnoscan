package httpapi

import (
	"net/http"
	"strconv"
)

// Symbol search.
//
// /api/search answers with packages: it matches a path, a name or a creator,
// which cannot find a package by what it declares. `IterateByOffset` is a real
// thing somebody types into the box, and until this existed the only page that
// knew about it was the docs tab of the one package that has it, reachable only
// by already knowing the answer.
//
// A second endpoint rather than a third group inside /api/search, for the same
// reason /api/assets/search is its own: the search response is an array of
// packages and every caller treats it as one. Widening it into an object would
// break them all to save one request the frontend already makes in parallel.

type symbolSearchResponse struct {
	Query   string      `json:"query"`
	Symbols []symbolHit `json:"symbols"`
}

// symbolHit is the store's row plus the route the frontend should send a click
// to, so the row does not have to reconstruct a URL from a path.
type symbolHit struct {
	Network   string `json:"network,omitempty"`
	Path      string `json:"path"`
	IsRealm   bool   `json:"is_realm"`
	Kind      string `json:"kind"`
	Recv      string `json:"recv,omitempty"`
	Name      string `json:"name"`
	Display   string `json:"display"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
	Exported  bool   `json:"exported"`
}

// docSummary is the first sentence of a doc comment, which is the part that
// fits on one row. Go's own convention puts the summary there.
func docSummary(doc string) string {
	for i := 0; i < len(doc); i++ {
		if doc[i] == '\n' {
			return doc[:i]
		}
		// A period followed by a space or end of string. Not a bare period:
		// "gno.land/p/moul" is not the end of a sentence.
		if doc[i] == '.' && (i+1 == len(doc) || doc[i+1] == ' ') {
			return doc[:i+1]
		}
	}
	return doc
}

const maxDocSummary = 160

// HandleSymbolSearch answers the search box's symbol group.
func (a *API) HandleSymbolSearch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := r.URL.Query().Get("q")
	if q == "" {
		jsonError(w, "missing q parameter", 400)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hits, err := a.db.SearchSymbols(network, q, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	out := make([]symbolHit, 0, len(hits))
	for _, h := range hits {
		display := h.Name
		if h.Recv != "" {
			// The receiver is what makes a method's name unambiguous: three
			// packages declare String, and so do four types inside one of them.
			display = h.Recv + "." + h.Name
		}
		doc := docSummary(h.Doc)
		if len(doc) > maxDocSummary {
			doc = doc[:maxDocSummary] + "…"
		}
		out = append(out, symbolHit{
			Network: h.Network, Path: h.Path, IsRealm: h.IsRealm,
			Kind: h.Kind, Recv: h.Recv, Name: h.Name, Display: display,
			Signature: h.Signature, Doc: doc, File: h.File, Line: h.Line,
			Exported: h.Exported,
		})
	}
	JSONResponse(w, symbolSearchResponse{Query: q, Symbols: out})
}

// HandleSymbolIndexStatus reports what the index covers, for the sanity page
// and for anybody wondering why a search found nothing.
func (a *API) HandleSymbolIndexStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.db.SymbolIndexStatus()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, st)
}
