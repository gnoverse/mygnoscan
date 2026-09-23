package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/moul/mygnoscan/pkg/glossary"
)

// The server's root package loads the glossary at init; a test binary for this
// package does not link that, so the shipped file is loaded here from disk. It
// is the same bytes either way, which is the property the package exists for.
func loadRealGlossary(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("../../docs/glossary.md")
	if err != nil {
		t.Fatalf("read docs/glossary.md: %v", err)
	}
	prev := glossary.Default
	glossary.MustLoad(raw)
	t.Cleanup(func() { glossary.Default = prev })
}

func TestHandleGlossary(t *testing.T) {
	loadRealGlossary(t)
	api, _ := newTestAPI(t)

	var resp glossaryResponse
	getJSON(t, api.HandleGlossary, "/api/glossary", &resp)

	if resp.Version == "" {
		t.Error("no version: consumers stamp their own responses with it")
	}
	if resp.Count != len(resp.Terms) || resp.Count != len(resp.Order) {
		t.Errorf("count %d, %d terms, %d in order: all three describe the same table",
			resp.Count, len(resp.Terms), len(resp.Order))
	}
	for _, term := range resp.Order {
		if _, ok := resp.Terms[term]; !ok {
			t.Errorf("order names %q, which is not in terms", term)
		}
	}

	parked, ok := resp.Terms["parked"]
	if !ok {
		t.Fatal("no entry for parked, the word every inert package should be described with")
	}
	if parked.Gloss == "" {
		t.Error("parked has an empty gloss")
	}
	if len(parked.Also) != 1 || parked.Also[0] != "inert" {
		t.Errorf("parked.Also = %v, want [inert] so a renderer can gloss it on first use", parked.Also)
	}
}

// The words mean the same thing on every chain. Taking a network parameter
// would invite a caller to believe they might not.
func TestGlossaryIgnoresNetwork(t *testing.T) {
	loadRealGlossary(t)
	api, _ := newTestAPI(t)

	var a, b glossaryResponse
	getJSON(t, api.HandleGlossary, "/api/glossary?network=alpha", &a)
	getJSON(t, api.HandleGlossary, "/api/glossary?network=beta", &b)

	if a.Version != b.Version || len(a.Terms) != len(b.Terms) {
		t.Errorf("two networks got different glossaries: %d vs %d terms", len(a.Terms), len(b.Terms))
	}
}

// A binary that did not load one says so, rather than serving an empty table
// that reads as "this product defines nothing".
func TestGlossaryUnloadedIsAnErrorNotAnEmptyTable(t *testing.T) {
	prev := glossary.Default
	glossary.Default = nil
	t.Cleanup(func() { glossary.Default = prev })

	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.HandleGlossary(rec, httptest.NewRequest(http.MethodGet, "/api/glossary", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
