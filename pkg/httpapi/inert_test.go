package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/store"
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

// TestStampInertStatus is the #194-part-3 regression: a parked package must
// read as parked in a list (realms/packages/search/address) itself, not only
// on its own detail page.
func TestStampInertStatus(t *testing.T) {
	const network = "stamptest"
	inertQueueCache.mu.Lock()
	delete(inertQueueCache.byNet, network)
	delete(inertQueueCache.fetched, network)
	inertQueueCache.mu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Path string
				Data string
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		var respData string
		switch req.Params.Path {
		case "vm/qinertpaths?limit=2000":
			respData = "gno.land/r/parked/pkg\n"
		case "vm/qpkgmeta_json":
			respData = `{"path":"gno.land/r/parked/pkg","status":"inert"}`
		default:
			t.Fatalf("unexpected query path %q", req.Params.Path)
		}
		resp := map[string]any{
			"result": map[string]any{
				"response": map[string]any{
					"ResponseBase": map[string]any{
						"Data": base64.StdEncoding.EncodeToString([]byte(respData)),
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := &API{
		networks: []config.NetworkConfig{{ID: network}},
		rpcPick:  map[string]string{network: srv.URL},
	}
	items := []store.PackageInfo{
		{Network: network, Path: "gno.land/r/parked/pkg"},
		{Network: network, Path: "gno.land/r/live/pkg"},
	}
	a.stampInertStatus(context.Background(), items)

	if items[0].Status != PackageStatusInert {
		t.Errorf("parked package Status = %q, want %q", items[0].Status, PackageStatusInert)
	}
	if items[1].Status != "" {
		t.Errorf("live package Status = %q, want empty", items[1].Status)
	}
}

// A network whose RPC cannot be reached must leave rows unstamped rather
// than failing the whole list — the same best-effort contract as the queue
// endpoint this reads.
func TestStampInertStatusUnreachableLeavesRowsUnstamped(t *testing.T) {
	const network = "stamptest-unreachable"
	inertQueueCache.mu.Lock()
	delete(inertQueueCache.byNet, network)
	delete(inertQueueCache.fetched, network)
	inertQueueCache.mu.Unlock()

	a := &API{
		networks: []config.NetworkConfig{{ID: network}},
		rpcPick:  map[string]string{}, // no verified RPC for this network
	}
	items := []store.PackageInfo{{Network: network, Path: "gno.land/r/some/pkg"}}
	a.stampInertStatus(context.Background(), items)

	if items[0].Status != "" {
		t.Errorf("Status = %q, want empty when the network's RPC is unreachable", items[0].Status)
	}
}
