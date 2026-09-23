package analyzer

import (
	"context"
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

// SourceKeyOf builds the key that says whether a package's source has moved.
//
// It has to agree byte for byte with the SQL expression the corpus pass uses,
// which is why it takes the deploy's identity rather than deriving anything:
// the two sides are the same string assembled in two languages, and a
// disagreement would re-index the whole corpus on every pass, silently.
//
// Not a hash of the bodies. The hash was the obvious design and it is the
// expensive one: package_files is the largest table on a busy chain, and a
// pass that hashes every body is reading hundreds of megabytes every few
// minutes to discover that nothing moved. A body cannot change without a new
// MsgAddPackage, so the transaction hash carries it, and the file count and
// byte total notice a package that was half-synced when the last pass ran.
func SourceKeyOf(txHash string, blockHeight int, files []indexer.MemFile) string {
	bytes := 0
	for _, f := range files {
		bytes += len(f.Body)
	}
	return fmt.Sprintf("%s|%d|%d|%d", txHash, blockHeight, len(files), bytes)
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
	// Asked of the database rather than assembled here, so the read path and
	// the corpus pass cannot disagree about what a package's key is.
	key, err := a.db.PackageSourceKey(network, pkgPath)
	if err != nil {
		return false, err
	}
	if key == "" {
		return false, nil // no source stored for this package
	}
	prev, err := a.db.SymbolSourceKey(network, pkgPath)
	if err != nil {
		return false, err
	}
	if prev == key {
		return false, nil
	}
	return true, a.indexWithKey(network, pkgPath, key, files)
}

func (a *Analyzer) indexWithKey(network, pkgPath, key string, files []indexer.MemFile) error {
	syms := ExtractSymbols(files)
	// The package doc travels with the symbols because it is extracted by the
	// same parse and invalidated by the same source key. Storing it anywhere
	// else would mean a second pass over the same files to keep a sentence in
	// step with the declarations beside it.
	return a.db.ReplaceSymbols(network, pkgPath, key, syms.PackageDoc, FlattenSymbols(syms))
}

// SymbolIndexResult is what one pass did.
type SymbolIndexResult struct {
	// Scanned is how many packages the pass found worth looking at, not how
	// many exist: the staleness check is a single query and never reads a body
	// it does not have to.
	Scanned int
	Indexed int
	Dropped int
	Errors  int
	Took    time.Duration
}

func (r SymbolIndexResult) String() string {
	return "stale " + strconv.Itoa(r.Scanned) + ", indexed " + strconv.Itoa(r.Indexed) +
		", dropped " + strconv.Itoa(r.Dropped) + ", errors " + strconv.Itoa(r.Errors) +
		", took " + r.Took.Round(time.Millisecond).String()
}

// RefreshSymbolIndex walks every package that has source and re-indexes the
// ones whose source changed.
//
// Whole-corpus rather than watermarked, and cheap because the "has anything
// changed" question is answered entirely in SQL: one join over an aggregate of
// package_files against the key each package was last indexed under. Source is
// read only for the packages that actually moved. A watermark would be the
// other way to do this and it would need invalidating on every resync, which is
// the mechanism this deliberately does not have.
//
// Cancellable, because it runs on a ticker beside the syncer and a shutdown
// should not wait for a full corpus walk.
func (a *Analyzer) RefreshSymbolIndex(ctx context.Context) (SymbolIndexResult, error) {
	start := time.Now()
	var res SymbolIndexResult

	// One query decides the whole pass, and it reads no source. On an unchanged
	// corpus this comes back empty and the pass is over.
	cands, err := a.db.SymbolIndexCandidates()
	if err != nil {
		return res, fmt.Errorf("list candidates: %w", err)
	}
	res.Scanned = len(cands)
	for _, c := range cands {
		select {
		case <-ctx.Done():
			res.Took = time.Since(start)
			return res, ctx.Err()
		default:
		}
		files, err := a.db.StoredPackageFiles(c.Network, c.Path)
		if err != nil || len(files) == 0 {
			res.Errors++
			continue
		}
		if err := a.indexWithKey(c.Network, c.Path, c.SourceKey, files); err != nil {
			res.Errors++
			log.Printf("symbol index: %s/%s: %v", c.Network, c.Path, err)
			continue
		}
		res.Indexed++
	}

	// Source gone entirely. Leaving the rows would keep search offering
	// declarations from a package that no longer has any.
	orphans, err := a.db.OrphanedSymbolIndexes()
	if err != nil {
		return res, fmt.Errorf("list orphans: %w", err)
	}
	for _, o := range orphans {
		if err := a.db.DeleteSymbolIndex(o.Network, o.Path); err != nil {
			res.Errors++
			continue
		}
		res.Dropped++
	}
	res.Took = time.Since(start)
	return res, nil
}
