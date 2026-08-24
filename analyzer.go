package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"log"
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

// dependencyExtractorVersion names the extraction logic that produced the rows
// currently in the dependencies table. Bump it whenever ExtractImports changes
// in a way that would give a different answer, and every package is re-extracted
// once on the next start.
//
// v2 replaced a regex scan for quoted gno.land paths with a real parse of the
// import block. The regex counted commented-out imports and paths that only
// appeared in string literals, so rows written before it carry edges that do not
// exist.
const dependencyExtractorVersion = "2"

const dependencyExtractorKey = "dependency_extractor_version"

// ReextractDependencies recomputes every package's dependencies from the source
// already in the database, once per extractor version.
//
// Sync only ever moves forward from its cursor, so a package deployed before an
// extraction change is never revisited and keeps whatever the old logic wrote.
// package_files holds the bodies, so this needs no indexer and no network.
func (a *Analyzer) ReextractDependencies() error {
	done, err := a.db.GetSyncState(dependencyExtractorKey)
	if err == nil && done == dependencyExtractorVersion {
		return nil
	}

	refs, err := a.db.StoredPackageRefs()
	if err != nil {
		return err
	}

	packages, edges, failed := 0, 0, 0
	for _, ref := range refs {
		files, err := a.db.StoredPackageFiles(ref.Network, ref.Path)
		if err != nil {
			// One bad package must not abandon the pass; the version marker is
			// only written if everything else got through, so a later start
			// retries.
			log.Printf("re-extract %s/%s: %v", ref.Network, ref.Path, err)
			failed++
			continue
		}
		imports := a.ExtractImports(files)
		if err := a.db.SetDependencies(ref.Network, ref.Path, imports); err != nil {
			log.Printf("re-extract %s/%s: %v", ref.Network, ref.Path, err)
			failed++
			continue
		}
		packages++
		edges += len(imports)
	}
	if failed > 0 {
		return fmt.Errorf("re-extracted %d packages, %d failed", packages, failed)
	}

	log.Printf("re-extracted dependencies for %d packages (%d edges) with extractor v%s",
		packages, edges, dependencyExtractorVersion)
	return a.db.SetSyncState(dependencyExtractorKey, dependencyExtractorVersion)
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
