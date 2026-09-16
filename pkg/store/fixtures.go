package store

import (
	"database/sql"
	"path/filepath"
	"time"
)

// Fixtures shared by tests in other packages.
//
// The API, syncer and websocket tests all need a real database with the real
// schema, and duplicating the setup in each package is how the copies drift
// apart. Exported from here for the same reason indexer.Fake is exported from
// package indexer: the thing under test owns its test double.

// TB is the slice of *testing.T these helpers need.
//
// Declared rather than taking *testing.T so this file does not import
// `testing`, which registers its flags on init and would put a -test.* flag set
// on any binary linking the store.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
	Cleanup(func())
}

// NewTestDB opens a real SQLite file in a temp dir. The driver is pure Go, so
// this works everywhere including CI, and it exercises the actual schema rather
// than a mock.
//
// Takes TB rather than *testing.T so benchmarks can build the same database the
// tests do.
func NewTestDB(tb TB) *DB {
	tb.Helper()
	db, err := NewDB(filepath.Join(tb.TempDir(), "test.db"))
	if err != nil {
		tb.Fatalf("NewDB: %v", err)
	}
	tb.Cleanup(func() { db.Close() })
	return db
}

// seedNetwork writes one row into every network-scoped table.
func SeedNetwork(t TB, db *DB, network string, height int) {
	t.Helper()

	if err := db.UpsertPackage(network, "gno.land/r/demo/foo", "foo", "g1creator", "TXHASH", height, "", true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	if err := db.InsertPackageSubmission(network, "TXHASH", 0, "gno.land/r/demo/foo", "foo", "g1creator", height, "", true, 1, true); err != nil {
		t.Fatalf("insert package submission: %v", err)
	}
	if err := db.UpsertPackageFile(network, "gno.land/r/demo/foo", "foo.gno", "package foo"); err != nil {
		t.Fatalf("upsert package file: %v", err)
	}
	if err := db.SetDependencies(network, "gno.land/r/demo/foo", []string{"gno.land/p/demo/avl"}); err != nil {
		t.Fatalf("set dependencies: %v", err)
	}
	if err := db.InsertCall(network, "TXHASH", height, 0, "", "g1caller", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	if err := db.InsertMsgRun(network, "TXHASH", height, "", "g1caller", "package main", true); err != nil {
		t.Fatalf("insert msg run: %v", err)
	}
	if err := db.InsertBankSend(network, "TXHASH", height, "", "g1from", "g1to", "1ugnot", true); err != nil {
		t.Fatalf("insert bank send: %v", err)
	}
	if err := db.UpsertTransaction(network, "TXHASH", height, "", 100, 200, 1, true); err != nil {
		t.Fatalf("upsert transaction: %v", err)
	}
	proposerID, err := db.InternProposer(network, "g1proposer")
	if err != nil {
		t.Fatalf("intern proposer: %v", err)
	}
	if err := db.UpsertBlock(network, height, "", proposerID, 1); err != nil {
		t.Fatalf("upsert block: %v", err)
	}
}

// SQL exposes the underlying handle.
//
// A test seam, not part of the store's contract: tests in other packages assert
// on rows the store has no accessor for ("did the syncer write exactly these
// four dependency edges"), and adding a bespoke method per assertion would grow
// the real API to serve tests. Nothing in the running binary calls this.
func (d *DB) SQL() *sql.DB { return d.db }

func MustCall(t TB, db *DB, network, hash string, height int, ts time.Time, caller, pkgPath, fn string) {
	t.Helper()
	if err := db.InsertCall(network, hash, height, 0, rfc3339(ts), caller, pkgPath, fn, true); err != nil {
		t.Fatalf("insert call: %v", err)
	}
}

// rfc3339 is the timestamp format every stored block_time uses.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
