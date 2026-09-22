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

// The fingerprint is what makes a corpus walk cheap, so it has to move with the
// source and stay put otherwise. Over bodies rather than over a deploy height,
// because a resync rewrites rows at the same height.
func TestSymbolFingerprint(t *testing.T) {
	a := []indexer.MemFile{{Name: "a.gno", Body: "package a"}, {Name: "b.gno", Body: "package a\n"}}
	if SymbolFingerprint(a) != SymbolFingerprint(a) {
		t.Fatal("not deterministic")
	}
	b := []indexer.MemFile{{Name: "a.gno", Body: "package a"}, {Name: "b.gno", Body: "package a\n// x"}}
	if SymbolFingerprint(a) == SymbolFingerprint(b) {
		t.Fatal("a changed body did not move the fingerprint")
	}
	// A file added with empty content still changes the package.
	c := append(append([]indexer.MemFile{}, a...), indexer.MemFile{Name: "c.gno", Body: ""})
	if SymbolFingerprint(a) == SymbolFingerprint(c) {
		t.Fatal("an added file did not move the fingerprint")
	}
	// The separator has to make "ab"+"" different from "a"+"b", or two
	// packages with the same concatenated bytes collide.
	x := []indexer.MemFile{{Name: "f", Body: "ab"}}
	y := []indexer.MemFile{{Name: "f", Body: "a"}, {Name: "g", Body: "b"}}
	if SymbolFingerprint(x) == SymbolFingerprint(y) {
		t.Fatal("concatenation collision")
	}
}

func TestIndexPackageSymbolsSkipsUnchangedSource(t *testing.T) {
	an, db := indexTestSetup(t)
	files := []indexer.MemFile{{Name: "tree.gno", Body: treeSource}}

	wrote, err := an.IndexPackageSymbols("alpha", "gno.land/p/x/tree", files)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first index wrote nothing")
	}
	wrote, err = an.IndexPackageSymbols("alpha", "gno.land/p/x/tree", files)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("unchanged source was re-indexed")
	}

	changed := []indexer.MemFile{{Name: "tree.gno", Body: treeSource + "\nfunc Added() {}\n"}}
	wrote, err = an.IndexPackageSymbols("alpha", "gno.land/p/x/tree", changed)
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

	// A second pass over an unchanged corpus must do no writing at all. That
	// is what makes running this on a timer cheap enough to be uninteresting.
	res, err = an.RefreshSymbolIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 0 {
		t.Fatalf("second pass re-indexed %d packages", res.Indexed)
	}
	if res.Scanned != 1 {
		t.Fatalf("second pass scanned %d, want 1", res.Scanned)
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
	if res.Scanned > 1 {
		t.Fatalf("scanned %d packages after cancellation", res.Scanned)
	}
}
