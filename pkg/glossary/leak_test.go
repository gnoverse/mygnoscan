package glossary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The glossary only wins if nothing else states a definition.
//
// The failure this guards against is not malice, it is convenience: somebody
// needs a one-line explanation of "parked" in a tooltip, writes one there, and
// now two places define it and only one of them gets updated. Both still read
// as authority.
//
// The check is on the glosses, not on the headwords. Greping the frontend for
// the word "realm" would flag several hundred legitimate uses and get switched
// off within a week; greping it for "A program that lives on the chain and
// remembers" flags exactly the thing that went wrong.
func TestNoGlossIsRestatedElsewhere(t *testing.T) {
	g := realGlossary(t)

	// Enough to be unmistakable, short enough that a reformatted copy with a
	// different tail still trips it.
	const probe = 40
	needles := map[string]string{}
	for term, e := range g.Terms {
		flat := strings.Join(strings.Fields(e.Gloss), " ")
		if len(flat) > probe {
			flat = flat[:probe]
		}
		needles[term] = flat
	}

	root := filepath.Join("..", "..")
	skip := map[string]bool{
		filepath.Join(root, "docs", "glossary.md"): true, // the source itself
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".tmp", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".html", ".js", ".md":
		default:
			return nil
		}
		if skip[path] || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		for term, needle := range needles {
			if strings.Contains(text, needle) {
				t.Errorf("%s restates the gloss for %q; point at GET /api/glossary instead", path, term)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
