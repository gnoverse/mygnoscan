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

// TestExtractImportsMatchesAnyQuotedPath pins a known limitation rather than
// endorsing it: extraction is a regex over the file body, not a parse of the
// import block, so any quoted gno.land path anywhere in the source is reported
// as a dependency — including ones in comments, in string literals, and in
// imports that were commented out.
//
// The effect is a dependency graph that over-reports. It never misses a real
// import, which is why this is tolerable, but a realm that merely mentions a
// path in a string gets an edge it does not have. Changing it means parsing the
// source properly; this test exists so that change is visible when it happens.
func TestExtractImportsMatchesAnyQuotedPath(t *testing.T) {
	files := []MemFile{{Name: "a.gno", Body: `
		package main

		// import "gno.land/r/demo/commented"
		import "gno.land/p/demo/avl"

		const link = "gno.land/r/demo/mentioned"
	`}}

	got := NewAnalyzer(nil).ExtractImports(files)
	want := []string{
		"gno.land/r/demo/commented",
		"gno.land/p/demo/avl",
		"gno.land/r/demo/mentioned",
	}
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
