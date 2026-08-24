package main

import (
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// importRegex matches any quoted gno.land path. It is the fallback for source
// that will not parse at all — see fileImports.
var importRegex = regexp.MustCompile(`"(gno\.land/[^"]+)"`)

type Analyzer struct {
	db *DB
}

func NewAnalyzer(db *DB) *Analyzer {
	return &Analyzer{db: db}
}

// ExtractImports parses Go source files and extracts gno.land import paths.
func (a *Analyzer) ExtractImports(files []MemFile) []string {
	seen := make(map[string]bool)
	var imports []string

	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".gno") {
			continue
		}
		// Skip test files
		if strings.HasSuffix(f.Name, "_test.gno") {
			continue
		}

		for _, imp := range fileImports(f) {
			// The graph tracks on-chain dependencies, so stdlib imports are not
			// edges in it.
			if !strings.HasPrefix(imp, "gno.land/") || seen[imp] {
				continue
			}
			seen[imp] = true
			imports = append(imports, imp)
		}
	}
	return imports
}

// fileImports returns the paths one file imports.
//
// gno's import syntax is Go's, so go/parser reads it directly. ImportsOnly stops
// at the end of the import block, which means a file whose *body* does not parse
// still yields correct imports — worth knowing, because that is the common shape
// of source that fails to compile.
//
// Scanning for quoted paths instead, as this used to, cannot tell an import from
// any other string. It counted commented-out imports and paths that merely
// appear in string literals, and cross-realm paths in string literals are common
// enough in gno that the dependency graph carried edges that do not exist.
//
// The regex survives as a fallback for source where even the package clause and
// import block will not parse — likely not Go at all. It over-reports, but a
// real import is never lost, which is the better failure for a graph.
func fileImports(f MemFile) []string {
	parsed, err := parser.ParseFile(token.NewFileSet(), f.Name, f.Body, parser.ImportsOnly)
	if err != nil {
		var out []string
		for _, m := range importRegex.FindAllStringSubmatch(f.Body, -1) {
			out = append(out, m[1])
		}
		return out
	}

	out := make([]string, 0, len(parsed.Imports))
	for _, imp := range parsed.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, path)
	}
	return out
}

// ExtractMsgRunImports parses MsgRun source for gno.land imports.
func (a *Analyzer) ExtractMsgRunImports(files []MemFile) []string {
	return a.ExtractImports(files) // same logic
}

// ProcessPackage analyzes a package and stores its dependency info.
func (a *Analyzer) ProcessPackage(network string, pkg *MemPackage, creator, txHash string, blockHeight int, blockTime string, success bool) error {
	isRealm := strings.HasPrefix(pkg.Path, "gno.land/r/")

	// Store package
	if err := a.db.UpsertPackage(network, pkg.Path, pkg.Name, creator, txHash, blockHeight, blockTime, isRealm, len(pkg.Files)); err != nil {
		return err
	}

	// Store files
	for _, f := range pkg.Files {
		if err := a.db.UpsertPackageFile(network, pkg.Path, f.Name, f.Body); err != nil {
			return err
		}
	}

	// Extract and store dependencies
	imports := a.ExtractImports(pkg.Files)
	if err := a.db.SetDependencies(network, pkg.Path, imports); err != nil {
		return err
	}

	return nil
}

// ProcessCall stores a function call record.
func (a *Analyzer) ProcessCall(network, txHash string, blockHeight int, blockTime, caller, pkgPath, funcName string, success bool) error {
	return a.db.InsertCall(network, txHash, blockHeight, blockTime, caller, pkgPath, funcName, success)
}

// ProcessMsgRun stores MsgRun with full source for import analysis.
func (a *Analyzer) ProcessMsgRun(network, txHash string, blockHeight int, blockTime, caller string, files []MemFile, success bool) error {
	// Concatenate source for search
	var source strings.Builder
	for _, f := range files {
		source.WriteString(f.Body)
		source.WriteString("\n")
	}
	return a.db.InsertMsgRun(network, txHash, blockHeight, blockTime, caller, source.String(), success)
}
