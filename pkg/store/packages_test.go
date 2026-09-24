package store

import (
	"fmt"
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

// The detail endpoint and the listing must answer the same numbers for the same
// realm, because they serialize the same struct.
//
// They did not. GetPackageDetail read nine columns into a PackageDetail whose
// embedded PackageInfo carries nineteen, so `calls`, `unique_users`,
// `importers`, `gas_used` and the rest went out as zeros on
// GET /api/realm/{path} for every realm on the chain, r/gnoswap/router at 8,285
// calls included. A zero reads as an answer rather than as an absence: it cost
// a wrong conclusion about which realms had ever been called (2026-09-24).
func TestPackageDetailAgreesWithTheListing(t *testing.T) {
	db := NewTestDB(t)
	const path = "gno.land/r/x/busy"
	when := "2026-09-20T10:00:00Z"
	if err := db.UpsertPackage("alpha", path, "busy", "g1creator", "tx-deploy", 100, when, true, 2); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	for i, caller := range []string{"g1one", "g1two", "g1one"} {
		if err := db.InsertCall("alpha", fmt.Sprintf("tx-%d", i), 101+i, 0, when, caller, path, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	listed, err := db.ListPackages("alpha", PackageFilter{}, 10, 0, "")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	var want PackageInfo
	for _, p := range listed {
		if p.Path == path {
			want = p
		}
	}
	if want.Calls != 3 || want.UniqueUsers != 2 {
		t.Fatalf("the listing itself is wrong: %+v", want)
	}

	got, err := db.GetPackageDetail("alpha", path)
	if err != nil {
		t.Fatalf("GetPackageDetail: %v", err)
	}
	if got.Calls != want.Calls {
		t.Errorf("detail calls = %d, listing says %d", got.Calls, want.Calls)
	}
	if got.UniqueUsers != want.UniqueUsers {
		t.Errorf("detail unique_users = %d, listing says %d", got.UniqueUsers, want.UniqueUsers)
	}
	if got.LastCallHeight != want.LastCallHeight {
		t.Errorf("detail last_call_height = %d, listing says %d", got.LastCallHeight, want.LastCallHeight)
	}
	// call_count predates this and stays: it is in the public API.
	if got.CallCount != got.Calls {
		t.Errorf("call_count = %d and calls = %d, which is two answers to one question",
			got.CallCount, got.Calls)
	}
}
