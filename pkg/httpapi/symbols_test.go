package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/moul/mygnoscan/pkg/store"
)

func seedSymbols(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.ReplaceSymbols("alpha", "gno.land/p/nt/avl/v0", "fp", "", []store.SymbolRow{
		{Kind: "func", Name: "IterateByOffset", Signature: "func IterateByOffset(o, n int)",
			Doc: "IterateByOffset walks from an offset. It stops after n.", Exported: true, File: "avl.gno", Line: 12},
		{Kind: "type", Name: "Tree", Signature: "type Tree struct{...}", Exported: true},
		{Kind: "method", Recv: "Tree", Name: "Get", Signature: "func (t *Tree) Get(k string) any", Exported: true},
		{Kind: "func", Name: "iterInternal", Exported: false},
	}); err != nil {
		t.Fatal(err)
	}
}

func decodeSymbols(t *testing.T, rec *httptest.ResponseRecorder) symbolSearchResponse {
	t.Helper()
	var out symbolSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out
}

func TestSymbolSearchFindsADeclarationByName(t *testing.T) {
	api, db := newTestAPI(t)
	seedSymbols(t, db)

	rec := httptest.NewRecorder()
	api.HandleSymbolSearch(rec, httptest.NewRequest("GET", "/api/symbols/search?network=alpha&q=IterateByOffset", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeSymbols(t, rec)
	if len(got.Symbols) != 1 {
		t.Fatalf("got %d symbols, want 1", len(got.Symbols))
	}
	s := got.Symbols[0]
	if s.Path != "gno.land/p/nt/avl/v0" || s.Kind != "func" {
		t.Fatalf("hit = %+v", s)
	}
	// The row has to be renderable without a second request.
	if s.Signature == "" || s.Doc == "" || s.Display != "IterateByOffset" {
		t.Fatalf("hit is missing what a row needs: %+v", s)
	}
}

// A method's bare name collides with every other type's method of the same
// name. The receiver is what makes the row readable.
func TestSymbolSearchQualifiesMethods(t *testing.T) {
	api, db := newTestAPI(t)
	seedSymbols(t, db)

	rec := httptest.NewRecorder()
	api.HandleSymbolSearch(rec, httptest.NewRequest("GET", "/api/symbols/search?network=alpha&q=Get", nil))
	got := decodeSymbols(t, rec)
	if len(got.Symbols) != 1 {
		t.Fatalf("got %d symbols, want 1", len(got.Symbols))
	}
	if got.Symbols[0].Display != "Tree.Get" {
		t.Fatalf("display = %q, want Tree.Get", got.Symbols[0].Display)
	}
	if got.Symbols[0].Recv != "Tree" {
		t.Fatalf("recv = %q", got.Symbols[0].Recv)
	}
}

func TestSymbolSearchRequiresAQuery(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandleSymbolSearch(rec, httptest.NewRequest("GET", "/api/symbols/search", nil))
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSymbolSearchAnswersEmptyRatherThanNull(t *testing.T) {
	// A JSON null here means every caller has to guard before iterating, and
	// the one that forgets throws in the search box.
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandleSymbolSearch(rec, httptest.NewRequest("GET", "/api/symbols/search?q=nothingmatchesthis", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); !jsonHasEmptyArray(got, "symbols") {
		t.Fatalf("body = %s, want an empty symbols array", got)
	}
}

func jsonHasEmptyArray(body, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	return string(m[key]) == "[]"
}

func TestSymbolSearchEscapesTheQuery(t *testing.T) {
	api, db := newTestAPI(t)
	seedSymbols(t, db)
	rec := httptest.NewRecorder()
	api.HandleSymbolSearch(rec, httptest.NewRequest("GET",
		"/api/symbols/search?network=alpha&q="+url.QueryEscape("%"), nil))
	got := decodeSymbols(t, rec)
	if len(got.Symbols) != 0 {
		t.Fatalf("a wildcard query returned %d symbols", len(got.Symbols))
	}
}

func TestSymbolIndexStatusReportsCoverage(t *testing.T) {
	api, db := newTestAPI(t)
	seedSymbols(t, db)
	rec := httptest.NewRecorder()
	api.HandleSymbolIndexStatus(rec, httptest.NewRequest("GET", "/api/symbols/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var st store.SymbolIndexStats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Packages != 1 || st.Symbols != 4 {
		t.Fatalf("status = %+v", st)
	}
}

// The docs of a Go declaration start with a summary sentence, and a search row
// has space for exactly that.
func TestDocSummary(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Iterate walks the tree. It stops at n.", "Iterate walks the tree."},
		{"Iterate walks the tree.\nMore below.", "Iterate walks the tree."},
		{"No terminator here", "No terminator here"},
		// A period inside a path is not the end of a sentence, which is the
		// case a naive split on "." gets wrong on almost every gno doc comment.
		{"Imports gno.land/p/moul/txlink and nothing else", "Imports gno.land/p/moul/txlink and nothing else"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := docSummary(tt.in); got != tt.want {
			t.Errorf("docSummary(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSymbolSearchRoutesAreRegistered(t *testing.T) {
	api, db := newTestAPI(t)
	seedSymbols(t, db)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/api/symbols/search?q=Tree", "/api/symbols/status"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
	}
}
