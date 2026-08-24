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
