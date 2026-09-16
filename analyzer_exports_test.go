package main

import (
	"reflect"
	"testing"
)

// What a realm makes callable, as opposed to what has been called.
//
// The calls tab and the function heatmap can only show functions someone has
// already invoked, so a realm nobody has used yet advertised nothing at all and
// the only way to find out what it exports was to read the source and spot the
// capitals by eye.
func TestExportedFunctions(t *testing.T) {
	tests := []struct {
		name  string
		files []MemFile
		want  []string
	}{
		{
			name: "exported top-level functions, sorted",
			files: []MemFile{{Name: "realm.gno", Body: `package boards

import "std"

func Render(path string) string { return "" }
func CreateBoard(name string) { }
func unexported() { }
`}},
			// unexported is absent: a MsgCall naming it would be rejected.
			want: []string{"CreateBoard", "Render"},
		},
		{
			name: "methods are not callable and must not be listed",
			files: []MemFile{{Name: "realm.gno", Body: `package boards

type Board struct{}

func (b *Board) Post(text string) { }
func (b Board) Title() string { return "" }
func Post(text string) { }
`}},
			// A MsgCall names a package and a function, so a method is
			// unreachable — listing it advertises something nobody can invoke.
			want: []string{"Post"},
		},
		{
			name: "test files are not part of what a realm exports",
			files: []MemFile{
				{Name: "realm.gno", Body: "package boards\n\nfunc Real() {}\n"},
				{Name: "realm_test.gno", Body: "package boards\n\nfunc TestThing() {}\n"},
				{Name: "x_filetest.gno", Body: "package boards\n\nfunc FileTestThing() {}\n"},
			},
			want: []string{"Real"},
		},
		{
			name: "declarations across several files are merged and deduplicated",
			files: []MemFile{
				{Name: "a.gno", Body: "package boards\n\nfunc Alpha() {}\n"},
				{Name: "b.gno", Body: "package boards\n\nfunc Beta() {}\n"},
			},
			want: []string{"Alpha", "Beta"},
		},
		{
			// A body that will not parse is the common shape of source that
			// fails to compile, and it is exactly when a reader most wants to
			// know what the realm claims to offer. The regex over-reports
			// rather than losing real names.
			name: "source that does not parse falls back to a scan",
			files: []MemFile{{Name: "broken.gno", Body: `package boards

func Render(path string) string {
	this is not go at all {{{
}

func AlsoExported() {}
`}},
			want: []string{"AlsoExported", "Render"},
		},
		{
			name:  "no files",
			files: nil,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExportedFunctions(tt.files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExportedFunctions() = %v, want %v", got, tt.want)
			}
		})
	}
}
