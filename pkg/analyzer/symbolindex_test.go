package analyzer

import (
	"context"
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

const treeSource = `// Package tree is a sorted map.
package tree

// Size is the fixed node size.
const Size = 32

// Tree is an AVL tree.
type Tree struct{ root *node }

// Get returns the value at key.
func (t *Tree) Get(key string) any { return nil }

// IterateByOffset walks from an offset.
func IterateByOffset(o int, n int) {}

func unexportedHelper() {}
`

func indexTestSetup(t *testing.T) (*Analyzer, *store.DB) {
	t.Helper()
	db := store.NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})
	return NewAnalyzer(db), db
}

func TestFlattenSymbolsLiftsMethodsToTheirOwnRows(t *testing.T) {
	ps := ExtractSymbols([]indexer.MemFile{{Name: "tree.gno", Body: treeSource}})
	rows := FlattenSymbols(ps)

	got := map[string]store.SymbolRow{}
	for _, r := range rows {
		key := r.Kind + ":" + r.Recv + ":" + r.Name
		got[key] = r
	}
	for _, want := range []string{
		"const::Size",
		"type::Tree",
		"method:Tree:Get",
		"func::IterateByOffset",
		"func::unexportedHelper",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %q; got %v", want, keysOf(got))
		}
	}
	// A method is only reachable through its type, and the receiver is what
	// makes its name unambiguous.
	if got["method:Tree:Get"].Recv != "Tree" {
		t.Fatal("Get lost its receiver")
	}
	if !got["func::IterateByOffset"].Exported {
		t.Fatal("IterateByOffset is not marked exported")
	}
	if got["func::unexportedHelper"].Exported {
		t.Fatal("unexportedHelper is marked exported")
	}
	// Unexported declarations are indexed too: they are real, and worth
	// finding when you are reading the source.
	if got["func::unexportedHelper"].Name == "" {
		t.Fatal("unexported symbols were dropped")
	}
}

func keysOf(m map[string]store.SymbolRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The key is what makes a corpus pass cheap, so it has to move with the source
// and stay put otherwise. It also has to be assembled the same way in Go as in
// SQL, which TestSourceKeyMatchesTheSQLExpression is about.
func TestSourceKeyOf(t *testing.T) {
	a := []indexer.MemFile{{Name: "a.gno", Body: "package a"}, {Name: "b.gno", Body: "package a\n"}}
	// Deterministic across two separately built slices, not across two calls
	// with the same variable: the key is compared against one SQL built
	// somewhere else, so what matters is that equal inputs give equal output,
	// not that the function is pure.
	same := []indexer.MemFile{{Name: "a.gno", Body: "package a"}, {Name: "b.gno", Body: "package a\n"}}
	if SourceKeyOf("tx1", 10, a) != SourceKeyOf("tx1", 10, same) {
		t.Fatal("equal inputs gave different keys")
	}
	// A redeploy is a new transaction, which is what actually changes a body.
	if SourceKeyOf("tx1", 10, a) == SourceKeyOf("tx2", 11, a) {
		t.Fatal("a redeploy did not move the key")
	}
	// A file added mid-sync, at the same deploy, still has to be noticed.
	c := append(append([]indexer.MemFile{}, a...), indexer.MemFile{Name: "c.gno", Body: "x"})
	if SourceKeyOf("tx1", 10, a) == SourceKeyOf("tx1", 10, c) {
		t.Fatal("an added file did not move the key")
	}
	// And so does a body that grew without the file count changing.
	d := []indexer.MemFile{{Name: "a.gno", Body: "package a"}, {Name: "b.gno", Body: "package a\n// more"}}
	if SourceKeyOf("tx1", 10, a) == SourceKeyOf("tx1", 10, d) {
		t.Fatal("a longer body did not move the key")
	}
}

// The Go side and the SQL side are the same string assembled in two languages,
// and a disagreement would silently re-index the whole corpus on every pass.
//
// The specific trap is LENGTH(): on a TEXT column SQLite counts characters and
// Go counts bytes, so any package with a non-ASCII byte in it would never stop
// looking stale. This is that case.
func TestSourceKeyMatchesTheSQLExpression(t *testing.T) {
	an, db := indexTestSetup(t)
	const body = "package a\n// \u00e9\u00e9\u00e9 and \u4e16\u754c\n"
	if err := db.UpsertPackageFile("alpha", "gno.land/p/x/u", "u.gno", body); err != nil {
		t.Fatal(err)
	}
	files := []indexer.MemFile{{Name: "u.gno", Body: body}}

	fromSQL, err := db.PackageSourceKey("alpha", "gno.land/p/x/u")
	if err != nil {
		t.Fatal(err)
	}
	// No packages row, so the SQL side coalesces the deploy to ("",-1).
	if got := SourceKeyOf("", -1, files); got != fromSQL {
		t.Fatalf("Go key %q != SQL key %q", got, fromSQL)
	}

	// And the pass must agree: index once, then find nothing stale.
	if _, err := an.IndexPackageSymbols("alpha", "gno.land/p/x/u", files); err != nil {
		t.Fatal(err)
	}
	cands, err := db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("a freshly indexed package with non-ASCII source still reads as stale: %+v", cands)
	}
}

// The staleness check is the thing that has to read no source, or a pass every
// ten minutes is hundreds of megabytes of I/O to discover nothing moved.
func TestSymbolIndexCandidatesFindsOnlyWhatMoved(t *testing.T) {
	an, db := indexTestSetup(t)
	for _, p := range []string{"a", "b"} {
		if err := db.UpsertPackageFile("alpha", "gno.land/p/x/"+p, "x.gno", treeSource); err != nil {
			t.Fatal(err)
		}
	}
	cands, err := db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("an unindexed corpus gave %d candidates, want 2", len(cands))
	}
	if _, err := an.RefreshSymbolIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	cands, err = db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("an indexed corpus still reads as stale: %+v", cands)
	}

	// One package changes; only that one comes back.
	if err := db.UpsertPackageFile("alpha", "gno.land/p/x/b", "x.gno", treeSource+"\nfunc More() {}\n"); err != nil {
		t.Fatal(err)
	}
	cands, err = db.SymbolIndexCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Path != "gno.land/p/x/b" {
		t.Fatalf("candidates = %+v, want only gno.land/p/x/b", cands)
	}
}

func TestIndexPackageSymbolsSkipsUnchangedSource(t *testing.T) {
	an, db := indexTestSetup(t)
	const path = "gno.land/p/x/tree"
	files := []indexer.MemFile{{Name: "tree.gno", Body: treeSource}}
	if err := db.UpsertPackageFile("alpha", path, "tree.gno", treeSource); err != nil {
		t.Fatal(err)
	}

	wrote, err := an.IndexPackageSymbols("alpha", path, files)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first index wrote nothing")
	}
	wrote, err = an.IndexPackageSymbols("alpha", path, files)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("unchanged source was re-indexed")
	}

	changedBody := treeSource + "\nfunc Added() {}\n"
	if err := db.UpsertPackageFile("alpha", path, "tree.gno", changedBody); err != nil {
		t.Fatal(err)
	}
	wrote, err = an.IndexPackageSymbols("alpha", path,
		[]indexer.MemFile{{Name: "tree.gno", Body: changedBody}})
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("changed source was not re-indexed")
	}
	hits, err := db.SearchSymbols("alpha", "Added", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("the new declaration is not searchable: %d hits", len(hits))
	}
}

// A package whose source is not in the database is not indexable, and saying so
// by writing an empty symbol table would be worse than doing nothing: the
// source key would record a package that has none.
func TestIndexPackageSymbolsWithNoStoredSourceIsANoop(t *testing.T) {
	an, db := indexTestSetup(t)
	wrote, err := an.IndexPackageSymbols("alpha", "gno.land/p/x/nothing",
		[]indexer.MemFile{{Name: "x.gno", Body: treeSource}})
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("indexed a package with no stored source")
	}
	if st, _ := db.SymbolIndexStatus(); st.Packages != 0 {
		t.Fatalf("status = %+v, want nothing indexed", st)
	}
}

func TestRefreshSymbolIndexWalksTheCorpus(t *testing.T) {
	an, db := indexTestSetup(t)
	if err := db.UpsertPackageFile("alpha", "gno.land/p/x/tree", "tree.gno", treeSource); err != nil {
		t.Fatal(err)
	}

	res, err := an.RefreshSymbolIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 1 || res.Errors != 0 {
		t.Fatalf("first pass = %s", res)
	}

	// A second pass over an unchanged corpus must do no writing and read no
	// source. That is what makes running this on a timer uninteresting.
	res, err = an.RefreshSymbolIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 0 {
		t.Fatalf("second pass re-indexed %d packages", res.Indexed)
	}
	if res.Scanned != 0 {
		t.Fatalf("second pass found %d stale packages, want 0", res.Scanned)
	}

	hits, err := db.SearchSymbols("alpha", "IterateByOffset", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Path != "gno.land/p/x/tree" || hits[0].Kind != "func" {
		t.Fatalf("hit = %+v", hits[0])
	}
	if hits[0].Doc == "" || hits[0].Signature == "" {
		t.Fatalf("hit lost its doc or signature: %+v", hits[0])
	}
}

// Cancellable, because it runs on a ticker beside the syncer and a shutdown
// should not wait for a full corpus walk.
func TestRefreshSymbolIndexStopsWhenCancelled(t *testing.T) {
	an, db := indexTestSetup(t)
	for _, p := range []string{"a", "b", "c"} {
		if err := db.UpsertPackageFile("alpha", "gno.land/p/x/"+p, "x.gno", treeSource); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := an.RefreshSymbolIndex(ctx)
	if err == nil {
		t.Fatal("a cancelled pass returned no error")
	}
	if res.Indexed > 1 {
		t.Fatalf("indexed %d packages after cancellation", res.Indexed)
	}
}
