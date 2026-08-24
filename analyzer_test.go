package main

import (
	"reflect"
	"testing"
)

// ExtractImports feeds the dependency graph, which is what the realm/package
// views are drawn from. It needs no database, so it tests as a pure function.
func TestExtractImports(t *testing.T) {
	tests := []struct {
		name  string
		files []MemFile
		want  []string
	}{
		{
			name:  "no files",
			files: nil,
			want:  nil,
		},
		{
			name: "single import",
			files: []MemFile{
				{Name: "a.gno", Body: `import "gno.land/p/demo/avl"`},
			},
			want: []string{"gno.land/p/demo/avl"},
		},
		{
			name: "grouped import block",
			files: []MemFile{{Name: "a.gno", Body: `
				import (
					"std"
					"strings"
					"gno.land/p/demo/avl"
					"gno.land/r/demo/users"
				)
			`}},
			// Stdlib imports carry no gno.land prefix and are not dependencies
			// this graph tracks.
			want: []string{"gno.land/p/demo/avl", "gno.land/r/demo/users"},
		},
		{
			name: "duplicates collapse across files, first-seen order wins",
			files: []MemFile{
				{Name: "b.gno", Body: `import "gno.land/p/demo/ufmt"`},
				{Name: "a.gno", Body: `import "gno.land/p/demo/avl"`},
				{Name: "c.gno", Body: `import "gno.land/p/demo/ufmt"`},
			},
			want: []string{"gno.land/p/demo/ufmt", "gno.land/p/demo/avl"},
		},
		{
			name: "non-gno files are skipped",
			files: []MemFile{
				{Name: "README.md", Body: `see "gno.land/r/demo/boards"`},
				{Name: "gno.mod", Body: `require "gno.land/p/demo/avl"`},
				{Name: "a.gno", Body: `import "gno.land/p/demo/avl"`},
			},
			want: []string{"gno.land/p/demo/avl"},
		},
		{
			name: "test files are skipped",
			files: []MemFile{
				{Name: "a_test.gno", Body: `import "gno.land/p/demo/testutils"`},
				{Name: "a.gno", Body: `import "gno.land/p/demo/avl"`},
			},
			// A package's tests are not part of what it depends on at runtime.
			want: []string{"gno.land/p/demo/avl"},
		},
		{
			name: "only test files leaves nothing",
			files: []MemFile{
				{Name: "a_test.gno", Body: `import "gno.land/p/demo/testutils"`},
			},
			want: nil,
		},
		{
			name: "no gno.land imports",
			files: []MemFile{
				{Name: "a.gno", Body: `import ("std"; "strings")`},
			},
			want: nil,
		},
	}

	a := NewAnalyzer(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ExtractImports(tt.files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractImports() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Only real imports count. This test previously asserted the opposite — the old
// regex reported every quoted gno.land path, so a commented-out import and a
// path in a string literal both became dependency edges.
func TestExtractImportsIgnoresPathsThatAreNotImports(t *testing.T) {
	files := []MemFile{{Name: "a.gno", Body: `
		package main

		// import "gno.land/r/demo/commented"
		import "gno.land/p/demo/avl"

		// Cross-realm paths live in string literals all over gno, which is what
		// made the old behaviour actively wrong rather than merely untidy.
		const link = "gno.land/r/demo/mentioned"

		func Render(path string) string {
			return "see gno.land/r/demo/alsonot"
		}
	`}}

	got := NewAnalyzer(nil).ExtractImports(files)
	want := []string{"gno.land/p/demo/avl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractImports() = %v, want %v", got, want)
	}
}

// ImportsOnly stops at the end of the import block, so source that fails to
// compile further down still reports its imports correctly. This is the common
// shape of broken on-chain source, and it must not lose edges.
func TestExtractImportsSurvivesAnUnparseableBody(t *testing.T) {
	files := []MemFile{{Name: "a.gno", Body: `
		package main

		import "gno.land/p/demo/avl"

		func broken( { this is not go at all }
	`}}

	got := NewAnalyzer(nil).ExtractImports(files)
	want := []string{"gno.land/p/demo/avl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractImports() = %v, want %v", got, want)
	}
}

// When even the package clause will not parse, fall back to scanning. It
// over-reports, but a real import is never lost — the better failure for a graph.
func TestExtractImportsFallsBackWhenNothingParses(t *testing.T) {
	files := []MemFile{{Name: "a.gno", Body: `
		}}} not go {{{
		import "gno.land/p/demo/avl"
	`}}

	got := NewAnalyzer(nil).ExtractImports(files)
	want := []string{"gno.land/p/demo/avl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractImports() = %v, want %v", got, want)
	}
}

// ExtractMsgRunImports delegates to ExtractImports today. MsgRun source arrives
// as a single unnamed-or-arbitrary file, so the .gno suffix filter is the part
// that matters: a MsgRun body that is not named *.gno yields nothing.
func TestExtractMsgRunImports(t *testing.T) {
	tests := []struct {
		name  string
		files []MemFile
		want  []string
	}{
		{
			name:  "gno-suffixed body is scanned",
			files: []MemFile{{Name: "main.gno", Body: `import "gno.land/p/demo/avl"`}},
			want:  []string{"gno.land/p/demo/avl"},
		},
		{
			name:  "body without a .gno name is skipped",
			files: []MemFile{{Name: "main", Body: `import "gno.land/p/demo/avl"`}},
			want:  nil,
		},
	}

	a := NewAnalyzer(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ExtractMsgRunImports(tt.files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractMsgRunImports() = %v, want %v", got, tt.want)
			}
		})
	}
}

// seedPackage writes a package's source the way ProcessPackage would, then
// installs dependency rows as if an older extractor had produced them.
func seedPackage(t *testing.T, db *DB, network, path string, files []MemFile, staleEdges []string) {
	t.Helper()
	if err := db.UpsertPackage(network, path, "pkg", "g1creator", "tx-"+path, 1, "2026-01-01T00:00:00Z", true, len(files)); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	for _, f := range files {
		if err := db.UpsertPackageFile(network, path, f.Name, f.Body); err != nil {
			t.Fatalf("UpsertPackageFile: %v", err)
		}
	}
	if err := db.SetDependencies(network, path, staleEdges); err != nil {
		t.Fatalf("SetDependencies: %v", err)
	}
}

// Sync only moves forward from its cursor, so packages deployed before an
// extractor change keep whatever the old logic wrote. The regex used to count
// commented-out imports and paths in string literals, so those phantom edges
// would have outlived the fix without this.
func TestReextractDependenciesReplacesStaleEdges(t *testing.T) {
	db := newTestDB(t)
	analyzer := NewAnalyzer(db)

	src := []MemFile{{Name: "a.gno", Body: `package main

// import "gno.land/r/demo/commented"
import "gno.land/p/demo/avl"

const link = "gno.land/r/demo/mentioned"
`}}
	// What the old regex would have written for that file.
	seedPackage(t, db, "testnet", "gno.land/r/demo/target", src, []string{
		"gno.land/r/demo/commented",
		"gno.land/p/demo/avl",
		"gno.land/r/demo/mentioned",
	})

	if got := depsOf(t, db, "testnet", "gno.land/r/demo/target"); len(got) != 3 {
		t.Fatalf("seed failed: %v", got)
	}

	if err := analyzer.ReextractDependencies(); err != nil {
		t.Fatalf("ReextractDependencies: %v", err)
	}

	got := depsOf(t, db, "testnet", "gno.land/r/demo/target")
	want := []string{"gno.land/p/demo/avl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dependencies = %v, want %v", got, want)
	}
}

// The pass is marked done by extractor version, so a restart does not redo it —
// but bumping the version must.
func TestReextractDependenciesRunsOncePerVersion(t *testing.T) {
	db := newTestDB(t)
	analyzer := NewAnalyzer(db)

	src := []MemFile{{Name: "a.gno", Body: "package main\nimport \"gno.land/p/demo/avl\"\n"}}
	seedPackage(t, db, "testnet", "gno.land/r/demo/target", src, []string{"gno.land/r/demo/phantom"})

	if err := analyzer.ReextractDependencies(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got, _ := db.GetSyncState(dependencyExtractorKey); got != dependencyExtractorVersion {
		t.Fatalf("version marker = %q, want %q", got, dependencyExtractorVersion)
	}

	// Corrupt the table, then re-run: the marker means it must not be touched.
	if err := db.SetDependencies("testnet", "gno.land/r/demo/target", []string{"gno.land/r/demo/manual"}); err != nil {
		t.Fatalf("SetDependencies: %v", err)
	}
	if err := analyzer.ReextractDependencies(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := depsOf(t, db, "testnet", "gno.land/r/demo/target"); !reflect.DeepEqual(got, []string{"gno.land/r/demo/manual"}) {
		t.Errorf("second pass re-ran despite the version marker: %v", got)
	}

	// A new extractor version must run again.
	if err := db.SetSyncState(dependencyExtractorKey, "0"); err != nil {
		t.Fatalf("SetSyncState: %v", err)
	}
	if err := analyzer.ReextractDependencies(); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if got := depsOf(t, db, "testnet", "gno.land/r/demo/target"); !reflect.DeepEqual(got, []string{"gno.land/p/demo/avl"}) {
		t.Errorf("a bumped version did not re-extract: %v", got)
	}
}

func depsOf(t *testing.T, db *DB, network, pkgPath string) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT import_path FROM dependencies WHERE network = ? AND package_path = ? ORDER BY import_path`, network, pkgPath)
	if err != nil {
		t.Fatalf("query dependencies: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}
