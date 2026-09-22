package store

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

func symbolTestDB(t *testing.T) *DB {
	t.Helper()
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}})
	return db
}

func TestReplaceSymbolsRoundTrip(t *testing.T) {
	db := symbolTestDB(t)

	if k, err := db.SymbolSourceKey("alpha", "gno.land/p/x/y"); err != nil || k != "" {
		t.Fatalf("SymbolSourceKey on an unindexed package = (%q,%v), want (\"\",nil)", k, err)
	}

	rows := []SymbolRow{
		{Kind: "func", Name: "IterateByOffset", Signature: "func IterateByOffset(o int)", Exported: true, File: "avl.gno", Line: 12},
		{Kind: "type", Name: "Tree", Signature: "type Tree struct{...}", Exported: true},
		{Kind: "method", Recv: "Tree", Name: "Get", Signature: "func (t *Tree) Get(k string) any", Exported: true},
		{Kind: "func", Name: "iterate", Signature: "func iterate()", Exported: false},
	}
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp1", rows); err != nil {
		t.Fatal(err)
	}
	if k, _ := db.SymbolSourceKey("alpha", "gno.land/p/x/y"); k != "fp1" {
		t.Fatalf("source key = %q, want fp1", k)
	}
	st, err := db.SymbolIndexStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Packages != 1 || st.Symbols != 4 {
		t.Fatalf("status = %+v, want 1 package and 4 symbols", st)
	}
}

// A declaration removed from the source has to disappear from the index. An
// upsert-per-row would leave it there forever, and search would keep offering a
// function nobody can call.
func TestReplaceSymbolsDropsWhatIsGone(t *testing.T) {
	db := symbolTestDB(t)
	path := "gno.land/r/x/app"

	if err := db.ReplaceSymbols("alpha", path, "fp1", []SymbolRow{
		{Kind: "func", Name: "Bid", Exported: true},
		{Kind: "func", Name: "Withdraw", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSymbols("alpha", path, "fp2", []SymbolRow{
		{Kind: "func", Name: "Bid", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := db.SearchSymbols("alpha", "Withdraw", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a removed declaration is still searchable: %+v", hits)
	}
	if st, _ := db.SymbolIndexStatus(); st.Symbols != 1 {
		t.Fatalf("symbols = %d, want 1", st.Symbols)
	}
}

// Two types in one package can both declare String, and so can a top-level
// function. Without recv in the primary key one silently replaces the other.
func TestMethodsOnDifferentTypesAreDifferentSymbols(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp", []SymbolRow{
		{Kind: "method", Recv: "Tree", Name: "String", Signature: "func (t Tree) String() string", Exported: true},
		{Kind: "method", Recv: "Node", Name: "String", Signature: "func (n Node) String() string", Exported: true},
		{Kind: "func", Name: "String", Signature: "func String() string", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchSymbols("alpha", "String", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("found %d symbols named String, want 3: %+v", len(hits), hits)
	}
}

func TestSearchSymbolsRanksPrefixesFirst(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp", []SymbolRow{
		{Kind: "func", Name: "unmarshalIterState", Exported: false},
		{Kind: "func", Name: "IterateByOffset", Exported: true},
		{Kind: "func", Name: "Iterate", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchSymbols("alpha", "Iter", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	// Somebody typing "Iter" means a symbol that starts with it. Burying
	// Iterate under unmarshalIterState is the ranking this exists to avoid.
	if hits[0].Name != "Iterate" {
		t.Fatalf("first hit = %q, want Iterate", hits[0].Name)
	}
	if hits[len(hits)-1].Name != "unmarshalIterState" {
		t.Fatalf("last hit = %q, want the substring-only match last", hits[len(hits)-1].Name)
	}
}

// Everything is network-scoped, and a symbol is no exception: the same path on
// two chains is two packages with two different sources.
func TestSearchSymbolsIsNetworkScoped(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/r/x/app", "fp", []SymbolRow{
		{Kind: "func", Name: "OnlyOnAlpha", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSymbols("beta", "gno.land/r/x/app", "fp", []SymbolRow{
		{Kind: "func", Name: "OnlyOnBeta", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		network string
		q       string
		want    int
	}{
		{"alpha", "OnlyOnAlpha", 1},
		{"alpha", "OnlyOnBeta", 0},
		{"beta", "OnlyOnBeta", 1},
		{"", "OnlyOn", 2}, // unscoped is every chain, deliberately
	}
	for _, tt := range tests {
		hits, err := db.SearchSymbols(tt.network, tt.q, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != tt.want {
			t.Fatalf("SearchSymbols(%q,%q) = %d hits, want %d", tt.network, tt.q, len(hits), tt.want)
		}
	}
}

// A search box is a place where anybody can type anything, and LIKE has
// wildcards of its own. "%" must not mean "every symbol on the chain".
func TestSearchSymbolsEscapesWildcards(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp", []SymbolRow{
		{Kind: "func", Name: "Alpha", Exported: true},
		{Kind: "func", Name: "Beta", Exported: true},
		{Kind: "func", Name: "A_B", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		q    string
		want int
	}{
		{"%", 0},     // not every symbol
		{"_", 1},     // the literal underscore in A_B, not every character
		{"A_B", 1},   // and an underscore in a longer query is literal too
		{"Alpha", 1}, // a normal query still works
	}
	for _, tt := range tests {
		hits, err := db.SearchSymbols("alpha", tt.q, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != tt.want {
			t.Fatalf("SearchSymbols(%q) = %d hits, want %d", tt.q, len(hits), tt.want)
		}
	}
}

func TestSearchSymbolsEmptyQueryFindsNothing(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp", []SymbolRow{
		{Kind: "func", Name: "Anything", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "   "} {
		hits, err := db.SearchSymbols("alpha", q, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Fatalf("SearchSymbols(%q) returned %d hits", q, len(hits))
		}
	}
}

func TestDeleteSymbolIndex(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", "fp", []SymbolRow{
		{Kind: "func", Name: "Gone", Exported: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSymbolIndex("alpha", "gno.land/p/x/y"); err != nil {
		t.Fatal(err)
	}
	if k, _ := db.SymbolSourceKey("alpha", "gno.land/p/x/y"); k != "" {
		t.Fatalf("source key survived the delete: %q", k)
	}
	if st, _ := db.SymbolIndexStatus(); st.Packages != 0 || st.Symbols != 0 {
		t.Fatalf("status = %+v, want empty", st)
	}
}

// A package whose source has gone away must lose its symbols too, or search
// keeps offering declarations from a package that no longer has any.
func TestOrphanedSymbolIndexes(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.UpsertPackageFile("alpha", "gno.land/p/x/live", "x.gno", "package x"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"gno.land/p/x/live", "gno.land/p/x/gone"} {
		if err := db.ReplaceSymbols("alpha", p, "k", []SymbolRow{{Kind: "func", Name: "F", Exported: true}}); err != nil {
			t.Fatal(err)
		}
	}
	orphans, err := db.OrphanedSymbolIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].Path != "gno.land/p/x/gone" {
		t.Fatalf("orphans = %+v, want only gno.land/p/x/gone", orphans)
	}
}

// The candidates query is the one that has to read no source. Assert on what it
// selects rather than on timing, which would be flaky and would pass for the
// wrong reason whenever the corpus happened to be small.
func TestSymbolIndexCandidatesIgnoresAnIndexedPackage(t *testing.T) {
	db := symbolTestDB(t)
	if err := db.UpsertPackageFile("alpha", "gno.land/p/x/y", "x.gno", "package x"); err != nil {
		t.Fatal(err)
	}
	cands, err := db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v, want 1", cands)
	}
	if err := db.ReplaceSymbols("alpha", "gno.land/p/x/y", cands[0].SourceKey, nil); err != nil {
		t.Fatal(err)
	}
	cands, err = db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates after indexing = %+v, want none", cands)
	}
	// A package with no declarations at all is still indexed: without the
	// index row it would be re-parsed on every pass forever.
	if k, _ := db.SymbolSourceKey("alpha", "gno.land/p/x/y"); k == "" {
		t.Fatal("a package with zero symbols recorded no source key")
	}
}
