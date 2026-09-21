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
