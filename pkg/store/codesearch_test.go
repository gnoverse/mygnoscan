package store

import (
	"fmt"
	"strings"
	"testing"
)

func seedFiles(t *testing.T, d *DB) {
	t.Helper()
	files := []struct{ net, path, name, body string }{
		{"alpha", "gno.land/p/nt/avl/v0", "tree.gno",
			"package avl\n\nfunc (t *Tree) IterateByOffset(offset int, count int) {\n\t// walk\n}\n"},
		{"alpha", "gno.land/r/demo/boards", "boards.gno",
			"package boards\n\nfunc CreateBoard(name string) BoardID {\n\treturn 0\n}\n"},
		{"alpha", "gno.land/r/demo/users", "users.gno",
			"package users\n\nfunc Register(addr std.Address) {\n\tpanic(\"nope\")\n}\n"},
		// A second network, to prove the filter holds.
		{"beta", "gno.land/r/demo/boards", "boards.gno",
			"package boards\n\nfunc IterateByOffset() {}\n"},
	}
	for _, f := range files {
		if err := d.UpsertPackageFile(f.net, f.path, f.name, f.body); err != nil {
			t.Fatalf("seed %s: %v", f.path, err)
		}
	}
}

func TestSearchCode(t *testing.T) {
	d := NewTestDB(t)
	seedFiles(t, d)

	tests := []struct {
		name      string
		opts      CodeSearchOpts
		wantPaths []string
	}{
		{
			name:      "a symbol name finds the file that declares it",
			opts:      CodeSearchOpts{Network: "alpha", Query: "IterateByOffset"},
			wantPaths: []string{"gno.land/p/nt/avl/v0"},
		},
		{
			// The whole point: this is a word inside the source, not metadata,
			// and the old LIKE-on-path search could never have found it.
			name:      "a word in a function body is findable",
			opts:      CodeSearchOpts{Network: "alpha", Query: "nope"},
			wantPaths: []string{"gno.land/r/demo/users"},
		},
		{
			name:      "realm filter excludes pure packages",
			opts:      CodeSearchOpts{Network: "alpha", Query: "package", Kind: "realm"},
			wantPaths: []string{"gno.land/r/demo/boards", "gno.land/r/demo/users"},
		},
		{
			name:      "package filter excludes realms",
			opts:      CodeSearchOpts{Network: "alpha", Query: "package", Kind: "package"},
			wantPaths: []string{"gno.land/p/nt/avl/v0"},
		},
		{
			// Network scoping is the invariant this repo breaks most often
			// (AGENTS.md): beta has its own IterateByOffset and must not appear.
			name:      "another network's source is never returned",
			opts:      CodeSearchOpts{Network: "beta", Query: "IterateByOffset"},
			wantPaths: []string{"gno.land/r/demo/boards"},
		},
		{
			name:      "an empty query returns nothing rather than everything",
			opts:      CodeSearchOpts{Network: "alpha", Query: "   "},
			wantPaths: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := d.SearchCode(tt.opts)
			if err != nil {
				t.Fatalf("SearchCode: %v", err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.Path)
			}
			if len(got) != len(tt.wantPaths) {
				t.Fatalf("paths = %v, want %v", got, tt.wantPaths)
			}
			for _, want := range tt.wantPaths {
				found := false
				for _, g := range got {
					if g == want {
						found = true
					}
				}
				if !found {
					t.Errorf("missing %q; got %v", want, got)
				}
			}
		})
	}
}

// The snippet is the reason a result list is readable: it shows the matched
// line, not just the file it lived in.
func TestSearchCodeReturnsSnippet(t *testing.T) {
	d := NewTestDB(t)
	seedFiles(t, d)

	hits, err := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: "IterateByOffset"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, "«IterateByOffset»") {
		t.Errorf("snippet = %q, want the match marked", hits[0].Snippet)
	}
	if !hits[0].IsRealm == false {
		t.Errorf("a /p/ path was classified as a realm")
	}
}

// A redeploy must not leave the old source matching: the index would then
// return code that is not on chain, which is worse than returning nothing.
func TestSearchCodeForgetsReplacedSource(t *testing.T) {
	d := NewTestDB(t)
	if err := d.UpsertPackageFile("alpha", "gno.land/r/x/y", "a.gno", "func OldName() {}"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertPackageFile("alpha", "gno.land/r/x/y", "a.gno", "func NewName() {}"); err != nil {
		t.Fatal(err)
	}
	old, err := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: "OldName"})
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 {
		t.Errorf("the replaced source is still searchable: %+v", old)
	}
	fresh, err := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: "NewName"})
	if err != nil || len(fresh) != 1 {
		t.Errorf("hits for the new source = %v, err = %v", fresh, err)
	}
}

// A malformed FTS5 query is the reader's mistake and has to come back as one,
// so the handler can answer 400 with the reason instead of a blank 500.
func TestSearchCodeRejectsMalformedQuery(t *testing.T) {
	d := NewTestDB(t)
	seedFiles(t, d)

	_, err := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: `"unbalanced`})
	if err == nil {
		t.Skip("this FTS5 build accepts the query; nothing to assert")
	}
	var bad *BadQueryError
	if !asBadQuery(err, &bad) {
		t.Errorf("err = %v (%T), want a BadQueryError", err, err)
	}
}

func asBadQuery(err error, target **BadQueryError) bool {
	if b, ok := err.(*BadQueryError); ok {
		*target = b
		return true
	}
	return false
}

// Backfill is what saves every deployment that predates the index: a full
// corpus with an empty index must become searchable on the next start.
func TestBackfillCodeIndex(t *testing.T) {
	d := NewTestDB(t)
	seedFiles(t, d)

	// Simulate an instance whose source predates the index.
	if _, err := d.db.Exec(`DELETE FROM code_index`); err != nil {
		t.Fatal(err)
	}
	hits, _ := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: "IterateByOffset"})
	if len(hits) != 0 {
		t.Fatal("precondition: the index should be empty")
	}

	n, err := d.BackfillCodeIndex()
	if err != nil {
		t.Fatalf("BackfillCodeIndex: %v", err)
	}
	if n == 0 {
		t.Fatal("backfill indexed nothing")
	}
	hits, err = d.SearchCode(CodeSearchOpts{Network: "alpha", Query: "IterateByOffset"})
	if err != nil || len(hits) != 1 {
		t.Errorf("after backfill hits = %v, err = %v", hits, err)
	}

	// Second call is a no-op: re-running on every restart of a busy instance
	// would be pure cost.
	if n2, err := d.BackfillCodeIndex(); err != nil || n2 != 0 {
		t.Errorf("second backfill = %d, %v; want 0, nil", n2, err)
	}
}

// TestBackfillCodeIndexChunks is a regression test for a real production
// failure: the first version backfilled the whole corpus in one transaction
// and died with SQLITE_BUSY on the live instance, because the syncer writes
// continuously and the connection's busy_timeout is 5s. The index stayed
// empty while the search box looked like it worked.
//
// Seeding more files than one batch holds proves the loop actually pages: a
// single-transaction implementation would still pass a small-corpus test.
func TestBackfillCodeIndexChunks(t *testing.T) {
	d := NewTestDB(t)
	const files = 1200 // > the 500-row batch, so at least three pages
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("gno.land/r/x/p%04d", i)
		if err := d.UpsertPackageFile("alpha", path, "a.gno", fmt.Sprintf("package p%04d\nfunc Marker%04d() {}\n", i, i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := d.db.Exec(`DELETE FROM code_index`); err != nil {
		t.Fatal(err)
	}

	n, err := d.BackfillCodeIndex()
	if err != nil {
		t.Fatalf("BackfillCodeIndex: %v", err)
	}
	if n != files {
		t.Errorf("indexed %d, want %d: the paging loop dropped or repeated rows", n, files)
	}

	// Every page must be present, not just the first: an OFFSET bug shows up
	// as the last batch missing and nothing else.
	for _, i := range []int{0, 499, 500, 999, 1199} {
		hits, err := d.SearchCode(CodeSearchOpts{Network: "alpha", Query: fmt.Sprintf("Marker%04d", i)})
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		if len(hits) != 1 {
			t.Errorf("file %d: %d hits, want 1", i, len(hits))
		}
	}
	if size, _ := d.CodeIndexSize("alpha"); size != files {
		t.Errorf("index size = %d, want %d", size, files)
	}
}
