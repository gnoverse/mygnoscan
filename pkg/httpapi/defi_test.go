package httpapi

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/indexer"
)

const (
	realmAddr   = "g1realm0000000000000000000000000000"
	depositAddr = "g1deposit00000000000000000000000000"
	outsider    = "g1outsider0000000000000000000000000"
)

func transferTx(hash string, height int, legs ...indexer.TxEvent) indexer.Transaction {
	return indexer.Transaction{
		Hash:        hash,
		BlockHeight: height,
		Response:    &indexer.TxResponse{Events: legs},
	}
}

func leg(from, to, coins string) indexer.TxEvent {
	return indexer.TxEvent{Typename: "TransferEvent", From: from, To: to, Coins: coins}
}

// The reconstruction is the whole feature, and the thing it gets wrong silently
// is the sign: a reader that treats every leg as a receipt still ends on a
// plausible number, just the gross one.
func TestCoinFlowsForSignsAndAttributesEachLeg(t *testing.T) {
	txs := []indexer.Transaction{
		transferTx("b", 20,
			leg(realmAddr, outsider, "300ugnot"),
			// A leg of the same transaction that has nothing to do with this
			// realm: one transaction carries many transfers and only some are
			// ours.
			leg(outsider, "g1third0000000000000000000000000000", "999ugnot"),
			// A non-transfer event in the same list, which must not be read as
			// one.
			indexer.TxEvent{Typename: "GnoEvent", Type: "Transfer"},
		),
		transferTx("a", 10, leg(outsider, realmAddr, "1000ugnot")),
	}

	flows, net := coinFlowsFor(txs, realmAddr, depositAddr)
	if len(flows) != 2 {
		t.Fatalf("got %d flows, want the 2 legs touching this realm: %+v", len(flows), flows)
	}
	if net != 700 {
		t.Errorf("net = %d, want 1000 received less 300 sent", net)
	}
	// Newest first, so the table reads like every other one on the site and the
	// chart's reverse lands chronological.
	if flows[0].BlockHeight != 20 || flows[1].BlockHeight != 10 {
		t.Errorf("order = %d, %d; want newest first", flows[0].BlockHeight, flows[1].BlockHeight)
	}
	if flows[0].Amount != -300 || flows[0].Counterparty != outsider {
		t.Errorf("outgoing leg = %+v, want -300 to the outsider", flows[0])
	}
	if flows[1].Amount != 1000 {
		t.Errorf("incoming leg = %d, want +1000", flows[1].Amount)
	}
	for _, f := range flows {
		if f.Account != "banker" {
			t.Errorf("account = %q, want banker", f.Account)
		}
	}
}

// The deposit account's legs belong in the table (they are how a deposit was
// funded) but not in the net: summing both accounts and comparing the result
// against the balance of one is how a page reports a gap that is not there.
func TestCoinFlowsForKeepsTheDepositOutOfTheNet(t *testing.T) {
	txs := []indexer.Transaction{
		transferTx("a", 10, leg(outsider, realmAddr, "1000ugnot")),
		transferTx("b", 11, leg(outsider, depositAddr, "500ugnot")),
	}
	flows, net := coinFlowsFor(txs, realmAddr, depositAddr)
	if len(flows) != 2 {
		t.Fatalf("got %d flows, want both accounts' legs in the table", len(flows))
	}
	if net != 1000 {
		t.Errorf("net = %d, want the banker's 1000 alone", net)
	}
	var sawDeposit bool
	for _, f := range flows {
		if f.Account == "storage deposit" {
			sawDeposit = true
		}
	}
	if !sawDeposit {
		t.Error("the deposit leg is missing from the table")
	}
}

// A transfer in a denom that is not ugnot cannot be represented as a signed
// ugnot amount. Parsing it to zero is right; dropping the chain's own string
// with it would leave a row reading "0" with nothing to say otherwise.
func TestCoinFlowsForKeepsTheCoinStringForOtherDenoms(t *testing.T) {
	txs := []indexer.Transaction{
		transferTx("a", 10, leg(outsider, realmAddr, "42foo")),
	}
	flows, net := coinFlowsFor(txs, realmAddr, depositAddr)
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	if net != 0 {
		t.Errorf("net = %d, want 0: a foo is not a ugnot and summing them would invent a figure", net)
	}
	if flows[0].Coins != "42foo" {
		t.Errorf("coins = %q, want the chain's own string kept", flows[0].Coins)
	}
}

// A package with no deposit account (a /e/.../run path) must not match the
// empty string against every leg whose counterparty the chain left blank.
func TestCoinFlowsForIgnoresAnEmptyDepositAddress(t *testing.T) {
	txs := []indexer.Transaction{
		transferTx("a", 10, leg("", outsider, "1000ugnot")),
	}
	flows, net := coinFlowsFor(txs, realmAddr, "")
	if len(flows) != 0 || net != 0 {
		t.Errorf("got %d flows netting %d, want none: neither end is this realm", len(flows), net)
	}
}

// Paging is the difference between "the page shows 500 of 3,095" and "the page
// can show all 3,095", so the arithmetic that cuts it is worth pinning: an
// offset past the end used to be a slice panic waiting for the first reader who
// edited the URL.
func TestPageFlowsClampsAndNeverReturnsNil(t *testing.T) {
	ten := make([]coinFlow, 10)
	for i := range ten {
		ten[i].BlockHeight = 100 - i
	}

	tests := []struct {
		name         string
		flows        []coinFlow
		offset       int
		limit        int
		wantLen      int
		wantOffset   int
		wantFirstHgt int
	}{
		{"whole set fits", ten, 0, 500, 10, 0, 100},
		{"first page", ten, 0, 4, 4, 0, 100},
		{"second page resumes where the first stopped", ten, 4, 4, 4, 4, 96},
		{"last page is short", ten, 8, 4, 2, 8, 92},
		{"offset at the end", ten, 10, 4, 0, 10, 0},
		{"offset past the end clamps", ten, 9999, 4, 0, 10, 0},
		{"a zero limit is a legitimate totals-only ask", ten, 0, 0, 0, 0, 0},
		{"no flows at all", []coinFlow{}, 0, 500, 0, 0, 0},
		{"nil flows", nil, 0, 500, 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, offset := pageFlows(tt.flows, tt.offset, tt.limit)
			if page == nil {
				t.Fatal("page is nil, which marshals as null instead of []")
			}
			if len(page) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(page), tt.wantLen)
			}
			if offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", offset, tt.wantOffset)
			}
			if tt.wantFirstHgt != 0 && page[0].BlockHeight != tt.wantFirstHgt {
				t.Errorf("first row is height %d, want %d", page[0].BlockHeight, tt.wantFirstHgt)
			}
		})
	}
}

// The sign flip is the whole risk here, and it is invisible: both conventions
// produce a plausible leaderboard, and the wrong one says the biggest loser is
// the biggest winner.
func TestCounterpartiesForNetsFromTheCounterpartysSide(t *testing.T) {
	txs := []indexer.Transaction{
		// A player who staked 400 and was paid 100: down 300.
		transferTx("a", 10, leg(outsider, realmAddr, "400ugnot")),
		transferTx("b", 11, leg(realmAddr, outsider, "100ugnot")),
		// One who only ever took money out: up 250.
		transferTx("c", 12, leg(realmAddr, "g1winner000000000000000000000000000", "250ugnot")),
		// A storage-deposit leg, which belongs to a different account and must
		// not join the realm's own ledger.
		transferTx("d", 13, leg("g1funder000000000000000000000000000", depositAddr, "900ugnot")),
	}
	flows, _ := coinFlowsFor(txs, realmAddr, depositAddr)

	parties, total := counterpartiesFor(flows)
	if total != 2 {
		t.Fatalf("total = %d, want 2 (the storage-deposit funder is not one)", total)
	}

	byAddr := map[string]counterparty{}
	for _, c := range parties {
		byAddr[c.Address] = c
	}
	tests := []struct {
		addr                string
		sent, received, net int64
		legs                int
	}{
		{outsider, 400, 100, -300, 2},
		{"g1winner000000000000000000000000000", 0, 250, 250, 1},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			c, ok := byAddr[tc.addr]
			if !ok {
				t.Fatalf("missing")
			}
			if c.Sent != tc.sent || c.Received != tc.received {
				t.Errorf("sent/received = %d/%d, want %d/%d", c.Sent, c.Received, tc.sent, tc.received)
			}
			if c.Net != tc.net {
				t.Errorf("net = %d, want %d (positive means this account came out ahead)", c.Net, tc.net)
			}
			if c.Legs != tc.legs {
				t.Errorf("legs = %d, want %d", c.Legs, tc.legs)
			}
		})
	}

	// Gross, not net: the 500-gross account that comes out level still outranks
	// the 250-gross one that is up 250.
	if parties[0].Address != outsider {
		t.Errorf("ranked %q first, want the largest gross mover %q", parties[0].Address, outsider)
	}
}

// Both directions have to survive the collapse. Netting at the point of
// aggregation would make an account that moved 400 each way indistinguishable
// from one that never moved any, which is the difference between a market
// maker and a bystander.
func TestCounterpartiesForKeepsBothDirections(t *testing.T) {
	txs := []indexer.Transaction{
		transferTx("a", 10, leg(outsider, realmAddr, "400ugnot")),
		transferTx("b", 11, leg(realmAddr, outsider, "400ugnot")),
	}
	flows, _ := coinFlowsFor(txs, realmAddr, depositAddr)
	parties, _ := counterpartiesFor(flows)
	if len(parties) != 1 {
		t.Fatalf("got %d counterparties, want 1", len(parties))
	}
	c := parties[0]
	if c.Net != 0 {
		t.Errorf("net = %d, want 0", c.Net)
	}
	if c.Sent != 400 || c.Received != 400 {
		t.Errorf("sent/received = %d/%d, want 400/400: netting here loses the actor", c.Sent, c.Received)
	}
}
