package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// Keeping the symbol index in step with package_files.
//
// ExtractSymbols is a pure function over a package's source, and the docs tab
// calls it per request. That is fine for one package and useless for search:
// answering "which package declares IterateByOffset" that way means parsing
// every package on the chain on every keystroke. So the same extraction runs
// once per source change and the result is stored.
//
// The pass lives here rather than in store because store cannot import
// analyzer: analyzer already imports store, and the cycle is why ExportedFuncs
// is a []string on the store type and Symbols is on an API wrapper.

// SymbolFingerprint identifies a package's source.
//
// Over names and bodies rather than over the deploy height, because a resync
// rewrites rows at the same height: a height-keyed check would leave the index
// describing source nobody can see any more. Cheap enough to compute for every
// package on every pass — it is one hash over bytes already being read.
func SymbolFingerprint(files []indexer.MemFile) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Name, len(f.Body))
		h.Write([]byte(f.Body))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// FlattenSymbols turns the docs tab's nested table into storable rows.
//
// Methods become rows of their own with kind "method" and recv set, rather than
// staying nested under their type: a search hits a name, and a name is flat.
// The nesting is a presentation choice the docs tab rebuilds, and a WHERE
// clause cannot see through it.
func FlattenSymbols(ps PackageSymbols) []store.SymbolRow {
	out := make([]store.SymbolRow, 0, len(ps.Consts)+len(ps.Vars)+len(ps.Types)+len(ps.Funcs))
	add := func(kind, recv string, s Symbol) {
		out = append(out, store.SymbolRow{
			Kind: kind, Recv: recv, Name: s.Name, Signature: s.Signature,
			Doc: s.Doc, File: s.File, Line: s.Line, Exported: s.Exported,
		})
	}
	for _, s := range ps.Consts {
		add("const", "", s)
	}
	for _, s := range ps.Vars {
		add("var", "", s)
	}
	for _, t := range ps.Types {
		add("type", "", t.Symbol)
		for _, m := range t.Methods {
			add("method", t.Name, m)
		}
	}
	for _, s := range ps.Funcs {
		add("func", "", s)
	}
	return out
}

// IndexPackageSymbols extracts and stores one package's symbols, skipping the
// work when the source has not moved.
//
// Returns whether anything was written, so a pass can report how much it did
// rather than how much it looked at.
func (a *Analyzer) IndexPackageSymbols(network, pkgPath string, files []indexer.MemFile) (bool, error) {
	fp := SymbolFingerprint(files)
	prev, err := a.db.SymbolFingerprint(network, pkgPath)
	if err != nil {
		return false, err
	}
	if prev == fp {
		return false, nil
	}
	rows := FlattenSymbols(ExtractSymbols(files))
	if err := a.db.ReplaceSymbols(network, pkgPath, fp, rows); err != nil {
		return false, err
	}
	return true, nil
}

// SymbolIndexResult is what one pass did.
type SymbolIndexResult struct {
	Scanned int
	Indexed int
	Errors  int
	Took    time.Duration
}

func (r SymbolIndexResult) String() string {
	return "scanned " + strconv.Itoa(r.Scanned) + ", indexed " + strconv.Itoa(r.Indexed) +
		", errors " + strconv.Itoa(r.Errors) + ", took " + r.Took.Round(time.Millisecond).String()
}

// RefreshSymbolIndex walks every package that has source and re-indexes the
// ones whose source changed.
//
// Whole-corpus rather than incremental, and that is a measurement rather than a
// preference: the skip is one indexed lookup and one hash over bytes the walk
// is reading anyway, so a pass over an unchanged corpus is cheap and a pass
// after a redeploy touches exactly the package that moved. An incremental
// version would need a watermark that a resync invalidates, which is the
// mechanism this deliberately does not have.
//
// Cancellable, because it runs on a ticker beside the syncer and a shutdown
// should not wait for a full corpus walk.
func (a *Analyzer) RefreshSymbolIndex(ctx context.Context) (SymbolIndexResult, error) {
	start := time.Now()
	var res SymbolIndexResult

	refs, err := a.db.StoredPackageRefs()
	if err != nil {
		return res, fmt.Errorf("list packages: %w", err)
	}
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			res.Took = time.Since(start)
			return res, ctx.Err()
		default:
		}
		res.Scanned++
		files, err := a.db.StoredPackageFiles(ref.Network, ref.Path)
		if err != nil {
			res.Errors++
			continue
		}
		if len(files) == 0 {
			// Source gone. Leaving the rows would keep search offering
			// declarations from a package that no longer has any.
			if err := a.db.DeleteSymbolIndex(ref.Network, ref.Path); err != nil {
				res.Errors++
			}
			continue
		}
		wrote, err := a.IndexPackageSymbols(ref.Network, ref.Path, files)
		if err != nil {
			res.Errors++
			log.Printf("symbol index: %s/%s: %v", ref.Network, ref.Path, err)
			continue
		}
		if wrote {
			res.Indexed++
		}
	}
	res.Took = time.Since(start)
	return res, nil
}
