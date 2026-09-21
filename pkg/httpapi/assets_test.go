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

// seedFactory writes two assets issued by one realm, which is the case the
// per-asset page exists for: on mainnet six of the twelve assets come out of
// `.../gnomi/padv3` and grc20factory is built to mint many, so "the token of
// this realm" names nothing.
func seedFactory(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	xs := []store.TokenTransfer{
		{Token: "gno.land/r/demo/factory.AAA.0000001", From: "", To: "g1a", Value: 100, BlockHeight: 10, BlockTime: now},
		{Token: "gno.land/r/demo/factory.BBB.0000002", From: "", To: "g1b", Value: 200, BlockHeight: 11, BlockTime: now},
		{Token: "gno.land/r/demo/factory.BBB.0000002", From: "g1b", To: "", Value: 50, BlockHeight: 12, BlockTime: now},
	}
	for i, x := range xs {
		if err := db.InsertTokenTransfer("alpha", "TXF"+string(rune('a'+i)), 0, x); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestHandleAssetsFilterByRealm(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)
	seedFactory(t, db)

	var all assetsResponse
	getJSON(t, api.HandleAssets, "/api/assets?network=alpha", &all)
	if len(all.Assets) != 4 {
		t.Fatalf("got %d assets unfiltered, want 4: %+v", len(all.Assets), all.Assets)
	}

	var byRealm assetsResponse
	getJSON(t, api.HandleAssets, "/api/assets?network=alpha&realm=gno.land/r/demo/factory", &byRealm)
	// Both of the factory's assets, and neither of the others. A filter that
	// returned one row would be the bug this page is here to prevent.
	if len(byRealm.Assets) != 2 {
		t.Fatalf("got %d assets for the factory realm, want both: %+v", len(byRealm.Assets), byRealm.Assets)
	}
	for _, a := range byRealm.Assets {
		if a.PkgPath != "gno.land/r/demo/factory" {
			t.Errorf("realm filter let through %q", a.Token)
		}
	}
}

func TestHandleAssetCarriesItsSiblings(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)
	seedFactory(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/asset/x?network=alpha", nil)
	req.SetPathValue("token", "gno.land/r/demo/factory.AAA.0000001")
	api.HandleAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp assetDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Siblings) != 1 || resp.Siblings[0].Token != "gno.land/r/demo/factory.BBB.0000002" {
		t.Fatalf("siblings = %+v, want the other asset from the same realm", resp.Siblings)
	}
	// The asset itself is not its own sibling.
	for _, s := range resp.Siblings {
		if s.Token == "gno.land/r/demo/factory.AAA.0000001" {
			t.Error("the asset is listed among its own siblings")
		}
	}
	// A token from another realm never leaks in, even though it is in the
	// same summaries call.
	for _, s := range resp.Siblings {
		if s.PkgPath != "gno.land/r/demo/factory" {
			t.Errorf("sibling %q comes from another realm", s.Token)
		}
	}

	// The window every figure on the page was computed over, so the page can
	// say so rather than present a partial supply as a total.
	if resp.Ledger.FirstBlock != 1 || resp.Ledger.LastBlock != 12 {
		t.Errorf("ledger window = %+v, want blocks 1 to 12", resp.Ledger)
	}
}

func TestHandleAssetSeparatesMintsFromBurns(t *testing.T) {
	api, db := newTestAPI(t)
	seedFactory(t, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/asset/x?network=alpha", nil)
	req.SetPathValue("token", "gno.land/r/demo/factory.BBB.0000002")
	api.HandleAsset(rec, req)
	var resp assetDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	a := resp.Asset
	// 200 minted, 50 burned, so a supply of 150. The difference alone cannot
	// tell that apart from "150 minted and nothing burned", which is why both
	// halves are carried.
	if a.Minted != 200 || a.Burned != 50 || a.Supply != 150 {
		t.Errorf("asset = minted %d burned %d supply %d, want 200/50/150", a.Minted, a.Burned, a.Supply)
	}
	if a.MintCount != 1 || a.BurnCount != 1 {
		t.Errorf("counts = %d mints %d burns, want one of each", a.MintCount, a.BurnCount)
	}
}

func TestHandleAssetSearchFindsBySymbol(t *testing.T) {
	api, db := newTestAPI(t)
	seedAssets(t, db)
	seedFactory(t, db)

	var resp assetsResponse
	getJSON(t, api.HandleAssetSearch, "/api/assets/search?network=alpha&q=BBB", &resp)
	if len(resp.Assets) != 1 || resp.Assets[0].Token != "gno.land/r/demo/factory.BBB.0000002" {
		t.Fatalf("search BBB = %+v, want the one asset", resp.Assets)
	}

	// A realm path matches every asset it issues, which is the point: typing
	// the factory's path must not collapse to one row.
	var byPath assetsResponse
	getJSON(t, api.HandleAssetSearch, "/api/assets/search?network=alpha&q=demo/factory", &byPath)
	if len(byPath.Assets) != 2 {
		t.Fatalf("search by realm path = %+v, want both of its assets", byPath.Assets)
	}
}
