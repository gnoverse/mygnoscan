package main

import (
	"encoding/json"
	"testing"
)

// This fixture is the actual vm/qpkgmeta_json response for a real parked
// package on mainnet, captured 2026-09-14.
const inertPkgMetaFixture = `{"path":"gno.land/r/moul/x/daily/wrapped/v0","status":"inert","creator":"g1manfred47kzduec920z88wfr64ylksmdcedlf5","height":26416,"max_deposit":"100000000ugnot","reason":"waiting for a package approver to enable it","pending":true}`

func TestInertPackageMetaJSON(t *testing.T) {
	var meta InertPackage
	if err := json.Unmarshal([]byte(inertPkgMetaFixture), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Path != "gno.land/r/moul/x/daily/wrapped/v0" {
		t.Errorf("Path = %q", meta.Path)
	}
	if meta.Status != PackageStatusInert {
		t.Errorf("Status = %q, want %q", meta.Status, PackageStatusInert)
	}
	if meta.Creator != "g1manfred47kzduec920z88wfr64ylksmdcedlf5" {
		t.Errorf("Creator = %q", meta.Creator)
	}
	if meta.Height != 26416 {
		t.Errorf("Height = %d, want 26416", meta.Height)
	}
	if !meta.Pending {
		t.Error("Pending = false, want true")
	}
	if meta.Reason == "" {
		t.Error("Reason is empty")
	}
}

func TestComputeInertStats(t *testing.T) {
	enabled := []InertLifecycleEvent{
		{Kind: "enabled", WaitBlocks: 10, WaitSeconds: 50},
		{Kind: "enabled", WaitBlocks: 20, WaitSeconds: 100},
		{Kind: "enabled", WaitBlocks: 30, WaitSeconds: 150},
	}
	rejected := []InertLifecycleEvent{
		{Kind: "rejected"},
	}
	stats := ComputeInertStats(5, enabled, rejected)
	if stats.QueueLength != 5 {
		t.Errorf("QueueLength = %d, want 5", stats.QueueLength)
	}
	if stats.EnabledCount != 3 {
		t.Errorf("EnabledCount = %d, want 3", stats.EnabledCount)
	}
	if stats.RejectedCount != 1 {
		t.Errorf("RejectedCount = %d, want 1", stats.RejectedCount)
	}
	if stats.MedianWaitBlocks != 20 {
		t.Errorf("MedianWaitBlocks = %d, want 20", stats.MedianWaitBlocks)
	}
	if stats.AverageWaitBlocks != 20 {
		t.Errorf("AverageWaitBlocks = %v, want 20", stats.AverageWaitBlocks)
	}
	if stats.MedianWaitSeconds != 100 {
		t.Errorf("MedianWaitSeconds = %v, want 100", stats.MedianWaitSeconds)
	}
}

func TestComputeInertStatsEmpty(t *testing.T) {
	stats := ComputeInertStats(0, nil, nil)
	if stats.MedianWaitBlocks != 0 || stats.AverageWaitBlocks != 0 {
		t.Errorf("expected zero stats on empty input, got %+v", stats)
	}
}
