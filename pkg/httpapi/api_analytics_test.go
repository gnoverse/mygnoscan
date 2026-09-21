package httpapi

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// seedGas writes n realms, each deployed by its own transaction, so the gas
// attribution has something to group by. GetGasStats joins every branch of
// that attribution to `transactions`, which is why seedActivity alone gives
// an empty top-N list however many packages it writes.
func seedGas(t *testing.T, db *store.DB, network string, realms int) {
	t.Helper()
	when := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < realms; i++ {
		hash := fmt.Sprintf("%s-deploy-%d", network, i)
		if err := db.UpsertPackage(network, fmt.Sprintf("gno.land/r/%s/pkg%d", network, i), "pkg",
			"g1creator", hash, 200+i, when, true, 2); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.UpsertTransaction(network, hash, 200+i, when, 1000*(i+1), 2000*(i+1), 100, true); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
	}
}

// `limit` decides how many rows each of /api/gas's three top-N lists carries:
// 20 for the /gas summary, 100 for the list pages behind it. It is a LIMIT on
// three aggregates over every transaction the chain has, so an unbounded one
// would be a full scan any caller could ask for in a query string.
func TestGasLimitIsClampedAndOptional(t *testing.T) {
	api, db := newTestAPI(t)
	// More realms than the default limit, so a clamp that failed open and one
	// that failed shut both show up in the row count.
	const realms = 40
	seedGas(t, db, "alpha", realms)

	for _, tt := range []struct {
		name  string
		query string
		want  int
	}{
		{"absent is the summary's 20", "", 20},
		{"a list page asks for more", "&limit=30", 30},
		{"past the cap is the cap", "&limit=99999", gasTopNMax},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			api.HandleGas(rec, httptest.NewRequest("GET", "/api/gas?network=alpha"+tt.query, nil))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				TopRealms []struct{} `json:"top_realms"`
			}
			mustJSON(t, rec.Body.Bytes(), &got)

			// The fixture has fewer realms than gasTopNMax, so the cap case
			// can only assert that nothing past the cap came back.
			want := min(tt.want, realms)
			if len(got.TopRealms) != want {
				t.Errorf("top_realms has %d rows, want %d", len(got.TopRealms), want)
			}
		})
	}
}

// The clamp itself, which the handler test above cannot see: its fixture has
// fewer realms than gasTopNMax, so a cap that did nothing would still return
// the same rows.
func TestGasTopN(t *testing.T) {
	for _, tt := range []struct {
		query string
		want  int
	}{
		{"", gasTopNDefault},
		{"?limit=1", 1},
		{"?limit=100", 100},
		{"?limit=" + strconv.Itoa(gasTopNMax), gasTopNMax},
		{"?limit=" + strconv.Itoa(gasTopNMax+1), gasTopNMax},
		{"?limit=99999999", gasTopNMax},
		{"?limit=0", gasTopNDefault},
		{"?limit=-5", gasTopNDefault},
		{"?limit=all", gasTopNDefault},
		{"?limit=", gasTopNDefault},
	} {
		t.Run(tt.query, func(t *testing.T) {
			got := gasTopN(httptest.NewRequest("GET", "/api/gas"+tt.query, nil))
			if got != tt.want {
				t.Errorf("gasTopN(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}
