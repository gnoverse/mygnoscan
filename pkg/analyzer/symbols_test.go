package analyzer

import (
	"strings"
	"testing"

	"github.com/moul/mygnoscan/pkg/indexer"
)

func TestExtractSymbolsBasics(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `package foo

// Size is the digest size in bytes.
const Size = 32

const unexportedConst = 1

// Verbose turns on debug logging.
var Verbose bool

// Sum computes something.
func Sum(data []byte) []byte {
	return data
}

func helper() {}
`}}

	syms := ExtractSymbols(files)

	if len(syms.Consts) != 2 {
		t.Fatalf("Consts = %d, want 2: %+v", len(syms.Consts), syms.Consts)
	}
	// Exported sorts before unexported.
	if syms.Consts[0].Name != "Size" || syms.Consts[1].Name != "unexportedConst" {
		t.Errorf("Consts order = %v, want [Size unexportedConst]", []string{syms.Consts[0].Name, syms.Consts[1].Name})
	}
	if syms.Consts[0].Doc != "Size is the digest size in bytes." {
		t.Errorf("Size.Doc = %q", syms.Consts[0].Doc)
	}
	if !strings.Contains(syms.Consts[0].Signature, "Size = 32") {
		t.Errorf("Size.Signature = %q, want it to contain %q", syms.Consts[0].Signature, "Size = 32")
	}

	if len(syms.Vars) != 1 || syms.Vars[0].Name != "Verbose" {
		t.Fatalf("Vars = %+v, want one Verbose", syms.Vars)
	}

	if len(syms.Funcs) != 2 {
		t.Fatalf("Funcs = %d, want 2: %+v", len(syms.Funcs), syms.Funcs)
	}
	if syms.Funcs[0].Name != "Sum" || syms.Funcs[1].Name != "helper" {
		t.Errorf("Funcs order = %v, want [Sum helper]", []string{syms.Funcs[0].Name, syms.Funcs[1].Name})
	}
	if syms.Funcs[0].Doc != "Sum computes something." {
		t.Errorf("Sum.Doc = %q", syms.Funcs[0].Doc)
	}
}

// The body must not appear in a function's signature — the whole point of
// printing a signature rather than slicing raw source is eliding it.
func TestExtractSymbolsElidesFuncBody(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `package foo

func Register(moniker string, description string) (string, error) {
	if moniker == "" {
		return "", errSomething
	}
	return moniker, nil
}
`}}
	syms := ExtractSymbols(files)
	if len(syms.Funcs) != 1 {
		t.Fatalf("Funcs = %+v", syms.Funcs)
	}
	sig := syms.Funcs[0].Signature
	if strings.Contains(sig, "errSomething") || strings.Contains(sig, "{") {
		t.Errorf("Signature = %q, want the body elided (no braces, no body identifiers)", sig)
	}
	if !strings.Contains(sig, "func Register(moniker string, description string) (string, error)") {
		t.Errorf("Signature = %q, want the full param/return signature", sig)
	}
}

// Methods attach to their receiver TypeSymbol rather than appearing as
// top-level Funcs — the same "not reachable via MsgCall" distinction
// ExportedFunctions draws.
func TestExtractSymbolsGroupsMethodsUnderTheirType(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `package foo

// Stack is a LIFO of ints.
type Stack struct {
	items []int
}

// Push adds an item.
func (s *Stack) Push(v int) {
	s.items = append(s.items, v)
}

// Pop removes and returns the top item.
func (s *Stack) Pop() int {
	v := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return v
}

func TopLevel() {}
`}}
	syms := ExtractSymbols(files)

	if len(syms.Funcs) != 1 || syms.Funcs[0].Name != "TopLevel" {
		t.Fatalf("Funcs = %+v, want only TopLevel — methods must not appear here", syms.Funcs)
	}
	if len(syms.Types) != 1 || syms.Types[0].Name != "Stack" {
		t.Fatalf("Types = %+v, want one Stack", syms.Types)
	}
	methods := syms.Types[0].Methods
	if len(methods) != 2 {
		t.Fatalf("Stack.Methods = %+v, want Pop and Push", methods)
	}
	if methods[0].Name != "Pop" || methods[1].Name != "Push" {
		t.Errorf("Stack.Methods order = %v, want alphabetical [Pop Push]", []string{methods[0].Name, methods[1].Name})
	}
}

// A method can be declared before its type in source order, or in a
// different file entirely — Go does not require either ordering, so the
// extractor must resolve methods in a pass separate from collecting types.
func TestExtractSymbolsAttachesMethodDeclaredBeforeItsType(t *testing.T) {
	files := []indexer.MemFile{
		{Name: "a.gno", Body: `package foo

func (s *Stack) Push(v int) { s.items = append(s.items, v) }
`},
		{Name: "b.gno", Body: `package foo

type Stack struct{ items []int }
`},
	}
	syms := ExtractSymbols(files)
	if len(syms.Types) != 1 || len(syms.Types[0].Methods) != 1 || syms.Types[0].Methods[0].Name != "Push" {
		t.Fatalf("Types = %+v, want Stack with method Push", syms.Types)
	}
	if len(syms.Funcs) != 0 {
		t.Errorf("Funcs = %+v, want Push not duplicated into the flat function list", syms.Funcs)
	}
}

// A grouped const/var block's per-item comment belongs to that item; the
// block's own leading comment must not bleed onto every item in it.
func TestExtractSymbolsGroupedDeclDocComments(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `package foo

// Package-wide status codes.
const (
	// StatusOK means it worked.
	StatusOK = 0
	StatusFail = 1
)
`}}
	syms := ExtractSymbols(files)
	if len(syms.Consts) != 2 {
		t.Fatalf("Consts = %+v", syms.Consts)
	}
	var ok, fail Symbol
	for _, c := range syms.Consts {
		switch c.Name {
		case "StatusOK":
			ok = c
		case "StatusFail":
			fail = c
		}
	}
	if ok.Doc != "StatusOK means it worked." {
		t.Errorf("StatusOK.Doc = %q", ok.Doc)
	}
	if fail.Doc != "" {
		t.Errorf("StatusFail.Doc = %q, want empty — the block comment describes the block, not this item", fail.Doc)
	}
}

func TestExtractSymbolsPackageDoc(t *testing.T) {
	files := []indexer.MemFile{
		{Name: "a.gno", Body: `// Package foo does something useful.
package foo

func Real() {}
`},
		{Name: "b.gno", Body: `package foo

func Other() {}
`},
	}
	syms := ExtractSymbols(files)
	if syms.PackageDoc != "Package foo does something useful." {
		t.Errorf("PackageDoc = %q", syms.PackageDoc)
	}
}

func TestExtractSymbolsNoPackageDoc(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `package foo
func Real() {}`}}
	syms := ExtractSymbols(files)
	if syms.PackageDoc != "" {
		t.Errorf("PackageDoc = %q, want empty", syms.PackageDoc)
	}
}

func TestExtractSymbolsSkipsTestFiles(t *testing.T) {
	files := []indexer.MemFile{
		{Name: "a.gno", Body: `package foo
func Real() {}`},
		{Name: "a_test.gno", Body: `package foo
func TestSomething() {}`},
	}
	syms := ExtractSymbols(files)
	if len(syms.Funcs) != 1 || syms.Funcs[0].Name != "Real" {
		t.Errorf("Funcs = %+v, want only Real", syms.Funcs)
	}
}

func TestExtractSymbolsUnparseableFileContributesNothing(t *testing.T) {
	files := []indexer.MemFile{{Name: "a.gno", Body: `this is not valid gno at all {{{`}}
	syms := ExtractSymbols(files)
	if len(syms.Consts)+len(syms.Vars)+len(syms.Types)+len(syms.Funcs) != 0 {
		t.Errorf("expected no symbols from unparseable source, got %+v", syms)
	}
}
