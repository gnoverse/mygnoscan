package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

func seedAssets(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	xs := []store.TokenTransfer{
		// The registry knows this one, so it should come back verified.
		{Token: "gno.land/r/gnoswap/gns.GNS.0000000", From: "", To: "g1a", Value: 1000, BlockHeight: 1, BlockTime: now},
		{Token: "gno.land/r/gnoswap/gns.GNS.0000000", From: "g1a", To: "g1b", Value: 250, BlockHeight: 2, BlockTime: now},
		// The registry has never heard of this one.
		{Token: "gno.land/r/x/rando.RANDO.0000000", From: "", To: "g1c", Value: 7, BlockHeight: 3, BlockTime: now},
	}
	for i, x := range xs {
		if err := db.InsertTokenTransfer("alpha", "TX"+string(rune('a'+i)), 0, x); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestHandleAssets(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)

	var resp assetsResponse
	getJSON(t, api.HandleAssets, "/api/assets?network=alpha", &resp)

	if len(resp.Assets) != 2 {
		t.Fatalf("got %d assets, want 2: %+v", len(resp.Assets), resp.Assets)
	}
	byToken := map[string]assetRow{}
	for _, a := range resp.Assets {
		byToken[a.Token] = a
	}

	gns := byToken["gno.land/r/gnoswap/gns.GNS.0000000"]
	if gns.Supply != 1000 || gns.Holders != 2 {
		t.Errorf("gns = %+v, want supply 1000 across 2 holders", gns)
	}
	// Verified is the registry's claim, not the chain's: anyone may deploy a
	// realm called gns and nothing on chain distinguishes the real one.
	if !gns.Verified || gns.DisplaySymbol != "GNS" {
		t.Errorf("gns = %+v, want it verified with its registry symbol", gns)
	}

	rando := byToken["gno.land/r/x/rando.RANDO.0000000"]
	if rando.Verified {
		t.Error("a token the registry has never seen came back verified")
	}
	// Unverified is not rejected: the asset is still listed, with its own
	// symbol from the event key.
	if rando.Symbol != "RANDO" {
		t.Errorf("rando symbol = %q, want the event key's own name", rando.Symbol)
	}
}

func TestHandleAsset(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/asset/x?network=alpha", nil)
	req.SetPathValue("token", "gno.land/r/gnoswap/gns.GNS.0000000")
	api.HandleAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp assetDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Asset.Token != "gno.land/r/gnoswap/gns.GNS.0000000" {
		t.Fatalf("asset = %+v", resp.Asset)
	}
	if len(resp.Holders) != 2 || resp.Holders[0].Address != "g1a" {
		t.Errorf("holders = %+v, want g1a on top with 750", resp.Holders)
	}
	if resp.Holders[0].Balance != 750 {
		t.Errorf("top balance = %d, want 1000 received minus 250 sent", resp.Holders[0].Balance)
	}
	if len(resp.Transfers) != 2 {
		t.Errorf("transfers = %d, want 2", len(resp.Transfers))
	}
}

func TestHandleAssetUnknownToken(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/asset/gno.land/r/x/nope.NOPE.0?network=alpha", nil)
	req.SetPathValue("token", "gno.land/r/x/nope.NOPE.0")
	api.HandleAsset(rec, req)

	// A 404 rather than an empty asset: an empty one reads as a token that
	// exists and has never moved.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
