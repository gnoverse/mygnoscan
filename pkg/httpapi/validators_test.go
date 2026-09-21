package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// seedProposedBlocks gives two proposers an uneven share, so the share
// arithmetic has something to be wrong about.
func seedProposedBlocks(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		addr := "g1val_a"
		if i >= 4 {
			addr = "g1val_b"
		}
		id, err := db.InternProposer("alpha", addr)
		if err != nil {
			t.Fatalf("proposer id: %v", err)
		}
		if err := db.UpsertBlock("alpha", 100+i, now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339), id, i); err != nil {
			t.Fatalf("upsert block: %v", err)
		}
	}
}

// gnockpitDown points the client at a server that refuses, so the test
// exercises the degrade path deterministically. Without this the suite reaches
// the real gnockpit.gno.land, which makes it depend on a third party being up
// and on whatever mainnet's set happens to be.
func gnockpitDown(t *testing.T) {
	t.Helper()
	resetGnockpitCache(t)
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Cleanup(func() { resetGnockpitCache(t) })
}

// gnockpitUp serves a two-validator set whose addresses match the seeded
// proposers, which is the real-world case: verified against mainnet on
// 2026-09-20, gnockpit's addresses and the interned proposer addresses agreed
// exactly.
func gnockpitUp(t *testing.T) {
	t.Helper()
	resetGnockpitCache(t)
	useGnockpitFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"validators":[
			{"name":"val-a","address":"g1val_a","voting_power":"60","spof":false,"missed_100":0,"missed_24h":3,"avg_block_ms":3300},
			{"name":"val-b","address":"g1val_b","voting_power":"60","spof":true,"missed_100":1,"missed_24h":0,"avg_block_ms":3100}
		]}`)
	})
	t.Cleanup(func() { resetGnockpitCache(t) })
}

func TestValidatorDetail(t *testing.T) {
	api, db := newTestAPI(t)
	gnockpitDown(t)
	seedProposedBlocks(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/validator/x?network=alpha", nil)
	req.SetPathValue("addr", "g1val_a")
	api.HandleValidator(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp validatorDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Validator.Blocks != 4 {
		t.Errorf("blocks = %d, want 4", resp.Validator.Blocks)
	}
	// Not in the live set, because gnockpit is unreachable here. The page uses
	// this to say "proposed blocks but is not listed now" rather than showing a
	// validator that looks active.
	if resp.InSet {
		t.Error("in_set = true without a live set")
	}
	if len(resp.Blocks) != 4 {
		t.Errorf("got %d blocks, want 4", len(resp.Blocks))
	}
	if len(resp.Blocks) > 1 && resp.Blocks[0].Height < resp.Blocks[1].Height {
		t.Error("blocks are not newest-first")
	}
}

// An address that is neither in the set nor has ever proposed is a 404, not an
// empty page: an empty one reads as an idle validator.
func TestValidatorDetailUnknownAddress(t *testing.T) {
	api, db := newTestAPI(t)
	gnockpitDown(t)
	seedProposedBlocks(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/validator/x?network=alpha", nil)
	req.SetPathValue("addr", "g1nobody")
	api.HandleValidator(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The share series is the observable half of a valset timeline: no historical
// set is stored anywhere, so a validator joining or leaving shows up as its
// share appearing or going to zero.
func TestValidatorShareSeries(t *testing.T) {
	_, db := newTestAPI(t)
	seedProposedBlocks(t, db)

	pts, err := db.ValidatorShareSeries("alpha", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) == 0 {
		t.Fatal("no points")
	}
	var total float64
	for _, share := range pts[len(pts)-1].Shares {
		total += share
	}
	// Shares within a day are a partition of that day's blocks, so they sum to
	// one. A count per day would not be comparable across days.
	if total < 0.99 || total > 1.01 {
		t.Errorf("shares sum to %f, want 1", total)
	}
}

// A validator that is in the live set and has also proposed gets both halves in
// one response: the name and missed blocks from gnockpit, the history from
// local blocks. That join works because the two sources use the same consensus
// address, which is the thing the operator address is not.
func TestValidatorDetailMergesLiveWithHistory(t *testing.T) {
	api, db := newTestAPI(t)
	gnockpitUp(t)
	seedProposedBlocks(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/validator/x?network=alpha", nil)
	req.SetPathValue("addr", "g1val_a")
	api.HandleValidator(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp validatorDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.InSet {
		t.Error("in_set = false with gnockpit listing this address")
	}
	if resp.Validator.Name != "val-a" || resp.Validator.Missed24h != 3 {
		t.Errorf("live half missing: %+v", resp.Validator)
	}
	if resp.Validator.Blocks != 4 {
		t.Errorf("local half missing: %+v", resp.Validator)
	}
}

// A validator in the live set that this instance has never seen propose is
// still a validator, and must not 404.
func TestValidatorDetailForALiveMemberWithNoBlocks(t *testing.T) {
	api, _ := newTestAPI(t)
	gnockpitUp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/validator/x?network=alpha", nil)
	req.SetPathValue("addr", "g1val_b")
	api.HandleValidator(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the live member served: %s", rec.Code, rec.Body.String())
	}
	var resp validatorDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.InSet || resp.Validator.Blocks != 0 {
		t.Errorf("got %+v, want it in the set with no blocks", resp.Validator)
	}
}
