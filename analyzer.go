package main

import (
	"regexp"
	"strings"
)

// importRegex matches Go import statements for gno.land packages.
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

		matches := importRegex.FindAllStringSubmatch(f.Body, -1)
		for _, m := range matches {
			imp := m[1]
			if !seen[imp] {
				seen[imp] = true
				imports = append(imports, imp)
			}
		}
	}
	return imports
}

// ExtractMsgRunImports parses MsgRun source for gno.land imports.
func (a *Analyzer) ExtractMsgRunImports(files []MemFile) []string {
	return a.ExtractImports(files) // same logic
}

// ProcessPackage analyzes a package and stores its dependency info.
func (a *Analyzer) ProcessPackage(pkg *MemPackage, creator, txHash string, blockHeight int, success bool) error {
	isRealm := strings.HasPrefix(pkg.Path, "gno.land/r/")

	// Store package
	if err := a.db.UpsertPackage(pkg.Path, pkg.Name, creator, txHash, blockHeight, isRealm, len(pkg.Files)); err != nil {
		return err
	}

	// Store files
	for _, f := range pkg.Files {
		if err := a.db.UpsertPackageFile(pkg.Path, f.Name, f.Body); err != nil {
			return err
		}
	}

	// Extract and store dependencies
	imports := a.ExtractImports(pkg.Files)
	if err := a.db.SetDependencies(pkg.Path, imports); err != nil {
		return err
	}

	return nil
}

// ProcessCall stores a function call record.
func (a *Analyzer) ProcessCall(txHash string, blockHeight int, caller, pkgPath, funcName string, success bool) error {
	return a.db.InsertCall(txHash, blockHeight, caller, pkgPath, funcName, success)
}

// ProcessMsgRun stores MsgRun with full source for import analysis.
func (a *Analyzer) ProcessMsgRun(txHash string, blockHeight int, caller string, files []MemFile, success bool) error {
	// Concatenate source for search
	var source strings.Builder
	for _, f := range files {
		source.WriteString(f.Body)
		source.WriteString("\n")
	}
	return a.db.InsertMsgRun(txHash, blockHeight, caller, source.String(), success)
}
