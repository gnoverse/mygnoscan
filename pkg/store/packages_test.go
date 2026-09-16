package store

import (
	"path/filepath"
	"testing"
)

// GetPackageDetail's own SELECT used to omit block_time entirely, even
// though the packages table has always carried it and ListPackages already
// selected it — every realm detail page's BlockTime read as permanently
// empty. Caught by #195's provenance box trying to show a deploy date that
// was never wrong, just never fetched.
func TestGetPackageDetailIncludesBlockTime(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "detail.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	const when = "2026-08-01T00:00:00Z"
	if err := db.UpsertPackage("net", "gno.land/r/demo/boards", "boards", "g1creator", "tx1", 100, when, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	detail, err := db.GetPackageDetail("net", "gno.land/r/demo/boards")
	if err != nil {
		t.Fatalf("GetPackageDetail: %v", err)
	}
	if detail.BlockTime != when {
		t.Errorf("BlockTime = %q, want %q", detail.BlockTime, when)
	}
}

// A genesis package (or any row synced before its block time was known)
// legitimately carries no block_time — this must come back as an empty
// string, not an error, since the API layer's own fallback ("genesis" or
// "—") depends on being able to tell the two apart from a normal 500.
func TestGetPackageDetailToleratesMissingBlockTime(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "detail.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	if err := db.UpsertPackage("net", "gno.land/r/demo/boards", "boards", "g1creator", "tx1", 0, "", true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	detail, err := db.GetPackageDetail("net", "gno.land/r/demo/boards")
	if err != nil {
		t.Fatalf("GetPackageDetail: %v", err)
	}
	if detail.BlockTime != "" {
		t.Errorf("BlockTime = %q, want empty", detail.BlockTime)
	}
}
