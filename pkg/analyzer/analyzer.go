package analyzer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// importRegex matches any quoted gno.land path. It is the fallback for source
// that will not parse at all — see fileImports.
var importRegex = regexp.MustCompile(`"(gno\.land/[^"]+)"`)

type Analyzer struct {
	db *store.DB
}

func NewAnalyzer(db *store.DB) *Analyzer {
	return &Analyzer{db: db}
}

// ExtractImports parses Go source files and extracts gno.land import paths.
func (a *Analyzer) ExtractImports(pkgPath string, files []indexer.MemFile) []string {
	seen := make(map[string]bool)
	var imports []string

	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".gno") || isTestFile(f.Name) {
			continue
		}

		for _, imp := range fileImports(f) {
			// The graph tracks on-chain dependencies, so stdlib imports are not
			// edges in it.
			if !strings.HasPrefix(imp, "gno.land/") || seen[imp] {
				continue
			}
			// A package cannot import itself at runtime. Test files legitimately
			// import the package under test, so before filetests were excluded
			// this produced self-edges that rendered as a realm depending on
			// itself. Kept as a guard: the graph should never contain one
			// whatever the extraction does.
			if imp == pkgPath {
				continue
			}
			seen[imp] = true
			imports = append(imports, imp)
		}
	}
	return imports
}

// isTestFile reports whether a file is one of gno's two test conventions.
//
// `_filetest.gno` was being missed: it does not end in `_test.gno`, so filetests
// were parsed as production source. They import the package under test, which is
// how six self-edges reached the live dependency graph, and they pull in test-only
// packages that are not part of what the realm actually uses.
func isTestFile(name string) bool {
	return strings.HasSuffix(name, "_test.gno") || strings.HasSuffix(name, "_filetest.gno")
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
func fileImports(f indexer.MemFile) []string {
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

// exportedFuncRegex is the fallback for source go/parser will not read.
var exportedFuncRegex = regexp.MustCompile(`(?m)^func\s+([A-Z]\w*)\s*\(`)

// ExportedFunctions returns the exported top-level functions a package declares.
//
// This is what a realm makes callable. The calls tab and the function heatmap
// only ever showed functions that have *been* called, so a realm nobody had
// used yet advertised nothing at all, and the only way to find out what it
// exports was to read its source and spot the capitals by eye.
//
// Methods are excluded deliberately: a MsgCall names a package path and a
// function, so a method on a type is not reachable that way and listing it
// would advertise something the reader cannot invoke.
//
// Unlike fileImports this needs the whole file parsed, not just the import
// block — declarations are the body. A file whose body does not compile
// therefore falls back to the regex, which over-reports (it cannot tell a
// commented-out declaration from a live one) but never loses a real function.
// For a list whose purpose is "what can I call here", a spurious name the user
// can try and get an error from beats a missing one they never learn about.
func ExportedFunctions(files []indexer.MemFile) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	for _, f := range files {
		if isTestFile(f.Name) {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f.Name, f.Body, parser.SkipObjectResolution)
		if err != nil {
			for _, m := range exportedFuncRegex.FindAllStringSubmatch(f.Body, -1) {
				add(m[1])
			}
			continue
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if !fn.Name.IsExported() {
				continue
			}
			add(fn.Name.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ExtractMsgRunImports parses MsgRun source for gno.land imports.
//
// A MsgRun has no package path of its own to exclude — it is ephemeral code, not
// a deployed package — so there is nothing it could self-import.
func (a *Analyzer) ExtractMsgRunImports(files []indexer.MemFile) []string {
	return a.ExtractImports("", files)
}

// ProcessPackage analyzes a package and stores its dependency info.
//
// msgIndex follows blockHeight, matching InsertCall's parameter order (#152)
// — that ordering was chosen once, deliberately, after a mechanical fix-up
// script and this function's own signature disagreed on it and silently
// swapped the two everywhere calls was touched.
func (a *Analyzer) ProcessPackage(network string, pkg *indexer.MemPackage, creator, txHash string, blockHeight, msgIndex int, blockTime string, success bool) error {
	isRealm := strings.HasPrefix(pkg.Path, "gno.land/r/")

	// store package: current-state, overwritten by a later submission at the
	// same path.
	if err := a.db.UpsertPackage(network, pkg.Path, pkg.Name, creator, txHash, blockHeight, blockTime, isRealm, len(pkg.Files)); err != nil {
		return err
	}
	// store the submission itself, kept even once a later one replaces the
	// row above — see InsertPackageSubmission and gno-meta#126.
	if err := a.db.InsertPackageSubmission(network, txHash, msgIndex, pkg.Path, pkg.Name, creator, blockHeight, blockTime, isRealm, len(pkg.Files), success); err != nil {
		return err
	}

	// store files
	for _, f := range pkg.Files {
		if err := a.db.UpsertPackageFile(network, pkg.Path, f.Name, f.Body); err != nil {
			return err
		}
	}

	// Extract and store dependencies
	imports := a.ExtractImports(pkg.Path, pkg.Files)
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
// v3 excludes `_filetest.gno` — gno's other test convention, which the
// `_test.gno` check did not match — and refuses self-edges outright.
const dependencyExtractorVersion = "3"

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

	// There is work to do, so wait out the startup ANALYZE before taking the
	// write lock against it. Skipped entirely on the common path above, where
	// the marker already matches and nothing is rewritten.
	a.db.WaitBackground()

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
		imports := a.ExtractImports(ref.Path, files)
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

// ProcessCall stores a function call record. msgIndex is the message's
// position within its transaction; see InsertCall.
func (a *Analyzer) ProcessCall(network, txHash string, blockHeight, msgIndex int, blockTime, caller, pkgPath, funcName string, success bool) error {
	return a.db.InsertCall(network, txHash, blockHeight, msgIndex, blockTime, caller, pkgPath, funcName, success)
}

// ProcessMsgRun stores MsgRun with full source for import analysis.
func (a *Analyzer) ProcessMsgRun(network, txHash string, blockHeight int, blockTime, caller string, files []indexer.MemFile, success bool) error {
	// Concatenate source for search
	var source strings.Builder
	for _, f := range files {
		source.WriteString(f.Body)
		source.WriteString("\n")
	}
	return a.db.InsertMsgRun(network, txHash, blockHeight, blockTime, caller, source.String(), success)
}
