package analyzer

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strings"

	"github.com/moul/mygnoscan/pkg/indexer"
)

// Symbol is one const, var, type or top-level func a package declares.
//
// A method is not its own top-level Symbol — it is attached to its receiver
// type's TypeSymbol.Methods instead, the same distinction ExportedFunctions
// draws for the same reason: a MsgCall names a package path and a function,
// so a method is not reachable that way and listing it as if it were would
// advertise something the reader cannot invoke.
type Symbol struct {
	Kind      string `json:"kind"` // "const", "var", "type", "func"
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Exported  bool   `json:"exported"`
}

// TypeSymbol is a type declaration together with the methods declared on it
// (or its pointer) anywhere in the package.
type TypeSymbol struct {
	Symbol
	Methods []Symbol `json:"methods,omitempty"`
}

// PackageSymbols is a package's full symbol table, godoc's own ordering:
// constants, then variables, then types (each with its methods), then
// top-level functions.
//
// Within each group, exported symbols sort before unexported ones,
// alphabetically within that split — the split is what lets a docs page
// collapse unexported symbols behind one toggle without re-sorting anything
// the toggle reveals.
type PackageSymbols struct {
	Consts []Symbol     `json:"consts,omitempty"`
	Vars   []Symbol     `json:"vars,omitempty"`
	Types  []TypeSymbol `json:"types,omitempty"`
	Funcs  []Symbol     `json:"funcs,omitempty"`
}

// pendingMethod is a method whose receiver type may not have been seen yet
// at the point its FuncDecl is visited — Go does not require a type's
// declaration to precede its methods, and does not require them in the same
// file. Resolved once every file has been parsed.
type pendingMethod struct {
	receiver string
	sym      Symbol
}

// ExtractSymbols parses a package's source and returns its full symbol
// table.
//
// Unlike ExportedFunctions this does not fall back to a regex scan on a
// parse failure: a signature and doc comment need a real AST, and a
// spurious guess at either would be worse than omitting the file's symbols
// entirely — the opposite tradeoff from ExportedFunctions, whose regex
// fallback only ever guesses a name.
func ExtractSymbols(files []indexer.MemFile) PackageSymbols {
	fset := token.NewFileSet()
	var consts, vars, funcs []Symbol
	var types []TypeSymbol
	typeIdx := map[string]int{}
	var pending []pendingMethod

	for _, f := range files {
		if isTestFile(f.Name) {
			continue
		}
		parsed, err := parser.ParseFile(fset, f.Name, f.Body, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, decl := range parsed.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				switch d.Tok {
				case token.CONST, token.VAR:
					kw := d.Tok.String()
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						doc := specDoc(vs.Doc, d.Doc, len(d.Specs))
						sig := kw + " " + printNode(fset, vs)
						for _, name := range vs.Names {
							if name.Name == "_" {
								continue
							}
							sym := Symbol{
								Kind: kw, Name: name.Name, Signature: sig, Doc: doc,
								File: f.Name, Line: fset.Position(name.Pos()).Line,
								Exported: name.IsExported(),
							}
							if d.Tok == token.CONST {
								consts = append(consts, sym)
							} else {
								vars = append(vars, sym)
							}
						}
					}
				case token.TYPE:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || ts.Name == nil {
							continue
						}
						doc := specDoc(ts.Doc, d.Doc, len(d.Specs))
						typeIdx[ts.Name.Name] = len(types)
						types = append(types, TypeSymbol{Symbol: Symbol{
							Kind: "type", Name: ts.Name.Name, Signature: "type " + printNode(fset, ts),
							Doc: doc, File: f.Name, Line: fset.Position(ts.Pos()).Line,
							Exported: ts.Name.IsExported(),
						}})
					}
				}
			case *ast.FuncDecl:
				if d.Name == nil {
					continue
				}
				sym := Symbol{
					Kind: "func", Name: d.Name.Name, Signature: funcSignature(fset, d),
					Doc: docText(d.Doc), File: f.Name, Line: fset.Position(d.Pos()).Line,
					Exported: d.Name.IsExported(),
				}
				if d.Recv == nil || len(d.Recv.List) == 0 {
					funcs = append(funcs, sym)
				} else {
					pending = append(pending, pendingMethod{receiverTypeName(d.Recv.List[0].Type), sym})
				}
			}
		}
	}

	for _, m := range pending {
		if idx, ok := typeIdx[m.receiver]; ok {
			types[idx].Methods = append(types[idx].Methods, m.sym)
			continue
		}
		// A method whose receiver type this pass never saw — should not
		// happen for a package that compiles, but dropping it would be a
		// worse failure than showing it somewhere. Falls back to the flat
		// function list rather than disappearing.
		funcs = append(funcs, m.sym)
	}

	sortSymbols(consts)
	sortSymbols(vars)
	sortSymbols(funcs)
	sort.SliceStable(types, func(i, j int) bool { return lessSymbol(types[i].Symbol, types[j].Symbol) })
	for i := range types {
		sortSymbols(types[i].Methods)
	}

	return PackageSymbols{Consts: consts, Vars: vars, Types: types, Funcs: funcs}
}

func lessSymbol(a, b Symbol) bool {
	if a.Exported != b.Exported {
		return a.Exported
	}
	return a.Name < b.Name
}

func sortSymbols(syms []Symbol) {
	sort.SliceStable(syms, func(i, j int) bool { return lessSymbol(syms[i], syms[j]) })
}

// specDoc prefers a spec's own doc comment (the per-item comment inside a
// grouped `const (...)`/`var (...)`/`type (...)` block) and falls back to
// the surrounding GenDecl's doc comment only when that decl declares
// exactly one spec — a shared decl-level doc comment on a multi-spec block
// describes the block, not any one item in it, and attaching it to every
// item would repeat one comment across unrelated symbols.
func specDoc(specDoc, declDoc *ast.CommentGroup, numSpecs int) string {
	if specDoc != nil {
		return strings.TrimSpace(specDoc.Text())
	}
	if numSpecs == 1 {
		return docText(declDoc)
	}
	return ""
}

func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimSpace(cg.Text())
}

// funcSignature prints a function's signature with its body elided —
// godoc's own convention, and the reason this needs a real AST rather than
// a slice of source text: eliding a body correctly means not printing it,
// not finding where it starts and stopping.
func funcSignature(fset *token.FileSet, fn *ast.FuncDecl) string {
	cp := *fn
	cp.Body = nil
	cp.Doc = nil
	return printNode(fset, &cp)
}

func printNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces, Tabwidth: 4}
	if err := cfg.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// receiverTypeName reads the receiver's type name off a method's FuncDecl,
// unwrapping a pointer receiver and a generic type parameter list — either
// of which can appear between the type name and the method being attached
// to the right TypeSymbol.
func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}
