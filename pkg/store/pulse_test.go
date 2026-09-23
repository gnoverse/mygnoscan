package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The pulse is a window, and every bug it can have is a row landing on the
// wrong side of an edge. So the fixture puts something in each of the three
// regions a query can confuse — inside the window, inside the window before it,
// and older than both — and every test below asserts on which region a figure
// counted.

type pulseClock struct {
	now       time.Time
	since     time.Time
	prevSince time.Time
}

func newPulseClock() pulseClock {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	return pulseClock{now: now, since: now.Add(-24 * time.Hour), prevSince: now.Add(-48 * time.Hour)}
}

func (c pulseClock) params(network string) PulseParams {
	return PulseParams{
		Network:   network,
		Since:     c.since.Format(time.RFC3339),
		PrevSince: c.prevSince.Format(time.RFC3339),
		Limit:     10,
	}
}

// inWindow, inPrev and ancient are timestamps in each of the three regions.
func (c pulseClock) inWindow(h int) string {
	return c.now.Add(-time.Duration(h) * time.Hour).UTC().Format(time.RFC3339)
}
func (c pulseClock) inPrev() string  { return c.now.Add(-36 * time.Hour).UTC().Format(time.RFC3339) }
func (c pulseClock) ancient() string { return c.now.Add(-200 * time.Hour).UTC().Format(time.RFC3339) }

func TestPulseCountsOnlyTheWindow(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	// Three calls now, two in the window before, one long ago.
	for i, when := range []string{c.inWindow(1), c.inWindow(2), c.inWindow(3), c.inPrev(), c.inPrev(), c.ancient()} {
		if err := db.InsertCall("n", fmt.Sprintf("call-%d", i), 100+i, 0, when,
			"g1caller", "gno.land/r/demo/boards", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	if err := db.InsertBankSend("n", "send-now", 200, c.inWindow(1), "g1a", "g1b", "7ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
	if err := db.InsertBankSend("n", "send-old", 201, c.ancient(), "g1a", "g1b", "900ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"calls in window", p.Window.Calls, 3},
		{"calls in previous window", p.Prev.Calls, 2},
		{"sends in window", p.Window.Sends, 1},
		{"txs in window", p.Window.Txs, 4},
		{"active addresses in window", p.Window.ActiveAddrs, 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// ugnot_amount is summed from the parsed column, so the ancient 900 must
	// not leak in and the recent 7 must not be dropped.
	if p.Window.UgnotSent != 7 {
		t.Errorf("ugnot sent in window = %d, want 7", p.Window.UgnotSent)
	}
	if !p.HasPrev {
		t.Error("HasPrev = false with a previous window requested")
	}
}

// A redeploy is not a new package. Under the inert submission policy a stuck
// deploy is retried until it lands, so counting submissions instead of first
// submissions would report a week-old realm as new on every retry.
func TestPulseNewPackagesCountFirstSubmissionNotRedeploy(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	// An old realm, resubmitted inside the window.
	mustSubmit(t, db, "n", "old-1", "gno.land/r/demo/old", "g1dev", 10, c.ancient(), true)
	mustSubmit(t, db, "n", "old-2", "gno.land/r/demo/old", "g1dev", 11, c.inWindow(2), true)
	// A genuinely new realm, and a genuinely new pure package.
	mustSubmit(t, db, "n", "new-1", "gno.land/r/demo/fresh", "g1dev", 12, c.inWindow(3), true)
	mustSubmit(t, db, "n", "new-2", "gno.land/p/demo/lib", "g1dev", 13, c.inWindow(4), false)

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if p.Window.NewRealms != 1 {
		t.Errorf("new realms = %d, want 1 (the redeployed one is not new)", p.Window.NewRealms)
	}
	if p.Window.NewPackages != 1 {
		t.Errorf("new packages = %d, want 1", p.Window.NewPackages)
	}
	// Deploys counts submissions, which is the other half of the distinction:
	// three landed in the window, but only two paths were new.
	if p.Window.Deploys != 3 {
		t.Errorf("deploys = %d, want 3", p.Window.Deploys)
	}
}

func TestPulseHotRealmsRankAndCarryThePreviousWindow(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	seed := []struct {
		path   string
		caller string
		when   string
		ok     bool
	}{
		{"gno.land/r/demo/busy", "g1a", c.inWindow(1), true},
		{"gno.land/r/demo/busy", "g1b", c.inWindow(2), true},
		{"gno.land/r/demo/busy", "g1b", c.inWindow(3), false},
		{"gno.land/r/demo/quiet", "g1a", c.inWindow(4), true},
		// The previous window: quiet was the busy one yesterday.
		{"gno.land/r/demo/quiet", "g1a", c.inPrev(), true},
		{"gno.land/r/demo/quiet", "g1c", c.inPrev(), true},
		{"gno.land/r/demo/busy", "g1a", c.inPrev(), true},
	}
	for i, s := range seed {
		if err := db.InsertCall("n", fmt.Sprintf("tx-%d", i), 100+i, 0, s.when,
			s.caller, s.path, "Do", s.ok); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if len(p.HotRealms) != 2 {
		t.Fatalf("hot realms = %d rows, want 2", len(p.HotRealms))
	}
	top := p.HotRealms[0]
	if top.Path != "gno.land/r/demo/busy" {
		t.Errorf("top realm = %q, want the one with most calls in the window", top.Path)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"calls", top.Calls, 3},
		{"distinct callers", top.Callers, 2},
		{"failed calls", top.Failed, 1},
		{"previous window calls", top.PrevCalls, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("top realm %s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if p.HotRealms[1].PrevCalls != 2 {
		t.Errorf("second realm prev calls = %d, want 2", p.HotRealms[1].PrevCalls)
	}
}

func TestPulseHotTokens(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	fungible := "gno.land/r/demo/foo.FOO.0000001"
	nft := "gno.land/r/demo/art.ART.0000002"
	seed := []struct {
		token      string
		from, to   string
		value      int64
		when       string
		hash       string
		blockHeigt int
	}{
		{fungible, "", "g1a", 100, c.inWindow(1), "t1", 10},   // mint
		{fungible, "g1a", "g1b", 40, c.inWindow(2), "t2", 11}, // transfer
		{fungible, "g1b", "", 5, c.inWindow(3), "t3", 12},     // burn
		{fungible, "g1a", "g1b", 999, c.inPrev(), "t4", 13},   // previous window
		{nft, "g1a", "g1b", 0, c.inWindow(1), "t5", 14},       // GRC721: no amount
	}
	for i, s := range seed {
		if err := db.InsertTokenTransfer("n", s.hash, i, TokenTransfer{
			Token: s.token, From: s.from, To: s.to, Value: s.value,
			BlockHeight: s.blockHeigt, BlockTime: s.when,
		}); err != nil {
			t.Fatalf("InsertTokenTransfer: %v", err)
		}
	}
	// ugnot moves through BankMsgSend, not through a Transfer event.
	for i, when := range []string{c.inWindow(1), c.inWindow(2), c.inWindow(3), c.inWindow(4)} {
		if err := db.InsertBankSend("n", fmt.Sprintf("s%d", i), 200+i, when,
			"g1a", "g1b", "1000ugnot", true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	byToken := map[string]HotToken{}
	for _, tok := range p.HotTokens {
		byToken[tok.Token] = tok
	}

	native, ok := byToken["ugnot"]
	if !ok {
		t.Fatal("ugnot missing from hot tokens: the chain's own coin is the asset that moves most")
	}
	if !native.Native || native.Transfers != 4 || native.Volume != 4000 {
		t.Errorf("ugnot row = %+v, want 4 transfers of 4000 total marked native", native)
	}
	// Four ugnot sends beat three FOO transfers, so ugnot ranks first.
	if p.HotTokens[0].Token != "ugnot" {
		t.Errorf("hot tokens[0] = %q, want ugnot ranked by transfer count", p.HotTokens[0].Token)
	}

	foo := byToken[fungible]
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"transfers", int64(foo.Transfers), 3},
		{"volume", foo.Volume, 145},
		{"mints", int64(foo.Mints), 1},
		{"burns", int64(foo.Burns), 1},
		{"previous window transfers", int64(foo.PrevTransfers), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("FOO %s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if foo.Symbol != "FOO" {
		t.Errorf("FOO symbol = %q, want FOO", foo.Symbol)
	}

	// A GRC721 transfer carries no amount. Reporting its volume as 0 would read
	// as "nothing moved" rather than "this arithmetic does not apply".
	art := byToken[nft]
	if art.Fungible {
		t.Error("GRC721 token reported fungible")
	}
	if art.Volume != 0 {
		t.Errorf("GRC721 volume = %d, want 0", art.Volume)
	}
}

func TestPulseHotFlowsRankPerAssetAndOrderNewestFirst(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	token := "gno.land/r/demo/foo.FOO.0000001"
	for i, s := range []struct {
		value  int64
		height int
	}{{1, 10}, {500, 11}, {9, 12}} {
		if err := db.InsertTokenTransfer("n", fmt.Sprintf("tok-%d", i), i, TokenTransfer{
			Token: token, From: "g1a", To: "g1b", Value: s.value,
			BlockHeight: s.height, BlockTime: c.inWindow(5),
		}); err != nil {
			t.Fatalf("InsertTokenTransfer: %v", err)
		}
	}
	// A ugnot send larger than every token value, and one too small to place.
	if err := db.InsertBankSend("n", "big", 30, c.inWindow(1), "g1a", "g1b", "80000ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
	if err := db.InsertBankSend("n", "small", 9, c.inWindow(6), "g1a", "g1b", "1ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
	// Outside the window entirely.
	if err := db.InsertBankSend("n", "old", 40, c.ancient(), "g1a", "g1b", "99999ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if len(p.HotFlows) == 0 {
		t.Fatal("no flows")
	}
	for _, f := range p.HotFlows {
		if f.TxHash == "old" {
			t.Error("a transfer from before the window reached the flow list")
		}
	}
	// Newest first, so the highest block leads whichever asset it belongs to.
	for i := 1; i < len(p.HotFlows); i++ {
		if p.HotFlows[i-1].BlockHeight < p.HotFlows[i].BlockHeight {
			t.Fatalf("flows not ordered newest-first: %d before %d",
				p.HotFlows[i-1].BlockHeight, p.HotFlows[i].BlockHeight)
		}
	}
	// Each asset contributes its own largest: the 500-unit token transfer is
	// not crowded out by an 80,000 ugnot send it cannot be compared with.
	var sawToken, sawNative bool
	for _, f := range p.HotFlows {
		if f.Native && f.Value == 80000 {
			sawNative = true
		}
		if !f.Native && f.Value == 500 {
			sawToken = true
		}
	}
	if !sawNative || !sawToken {
		t.Errorf("want the largest of each asset, got %+v", p.HotFlows)
	}
}

func TestPulseHotDevsMarkFirstTimeDeployers(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	// A returning deployer: shipped long before the window, and twice inside it.
	mustSubmit(t, db, "n", "vet-0", "gno.land/r/vet/one", "g1veteran", 10, c.ancient(), true)
	mustSubmit(t, db, "n", "vet-1", "gno.land/r/vet/two", "g1veteran", 11, c.inWindow(2), true)
	mustSubmit(t, db, "n", "vet-2", "gno.land/r/vet/three", "g1veteran", 12, c.inWindow(1), true)
	// A first-timer, whose one submission failed. Written directly rather than
	// through the helper: a failed MsgAddPackage leaves a submission row and no
	// current-state row, which is exactly the state being asserted on.
	if err := db.InsertPackageSubmission("n", "new-0", 0, "gno.land/p/rookie/lib", "lib",
		"g1rookie", 13, c.inWindow(3), false, 1, false); err != nil {
		t.Fatalf("InsertPackageSubmission: %v", err)
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if len(p.HotDevs) != 2 {
		t.Fatalf("hot devs = %d rows, want 2", len(p.HotDevs))
	}
	vet, rookie := p.HotDevs[0], p.HotDevs[1]
	if vet.Address != "g1veteran" {
		t.Fatalf("top dev = %q, want the one with most deploys", vet.Address)
	}
	if vet.New {
		t.Error("a deployer whose first package predates the window is marked new")
	}
	if vet.Deploys != 2 || vet.Paths != 2 || vet.Realms != 2 {
		t.Errorf("veteran = %d deploys / %d paths / %d realms, want 2/2/2",
			vet.Deploys, vet.Paths, vet.Realms)
	}
	// Newest first, so the reader sees what they just shipped.
	if len(vet.Recent) != 2 || vet.Recent[0] != "gno.land/r/vet/three" {
		t.Errorf("veteran recent = %v, want the newest path first", vet.Recent)
	}
	if !rookie.New {
		t.Error("a deployer whose first package is inside the window is not marked new")
	}
	if rookie.Failed != 1 {
		t.Errorf("rookie failed = %d, want 1", rookie.Failed)
	}
	if rookie.Realms != 0 {
		t.Errorf("rookie realms = %d, want 0: /p/ is not a realm", rookie.Realms)
	}
}

func TestPulseHotLibsCountNewPackagesOnly(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	// Two packages first deployed inside the window, both importing avl; one
	// old package that also imports it, which must not count toward the window
	// but must count toward the all-time figure.
	mustSubmit(t, db, "n", "a", "gno.land/r/demo/a", "g1dev", 10, c.inWindow(2), true)
	mustSubmit(t, db, "n", "b", "gno.land/r/demo/b", "g1dev", 11, c.inWindow(3), true)
	mustSubmit(t, db, "n", "old", "gno.land/r/demo/old", "g1dev", 5, c.ancient(), true)
	for path, imports := range map[string][]string{
		"gno.land/r/demo/a":   {"gno.land/p/demo/avl", "strings", "gno.land/p/demo/ufmt"},
		"gno.land/r/demo/b":   {"gno.land/p/demo/avl", "std"},
		"gno.land/r/demo/old": {"gno.land/p/demo/avl"},
	} {
		if err := db.SetDependencies("n", path, imports); err != nil {
			t.Fatalf("SetDependencies: %v", err)
		}
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if len(p.HotLibs) == 0 {
		t.Fatal("no hot libraries")
	}
	top := p.HotLibs[0]
	if top.Path != "gno.land/p/demo/avl" {
		t.Fatalf("top library = %q, want avl", top.Path)
	}
	if top.NewImporters != 2 {
		t.Errorf("avl new importers = %d, want 2 (the pre-window package does not count)", top.NewImporters)
	}
	if top.TotalImporters != 3 {
		t.Errorf("avl total importers = %d, want 3 (all time, including the old package)", top.TotalImporters)
	}
	if len(top.Importers) != 2 {
		t.Errorf("avl importers = %v, want the two new packages", top.Importers)
	}
	// std and strings are imported by the same new packages and would outrank
	// every on-chain package on every chain, every window.
	for _, l := range p.HotLibs {
		if l.Path == "std" || l.Path == "strings" {
			t.Errorf("standard library %q reached the hot-libraries list", l.Path)
		}
	}
}

// Everything is network-scoped, and a window query is no exception: two chains
// blended is the failure mode this repo has hit most often.
func TestPulseScopesToOneNetwork(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	for _, net := range []string{"one", "two"} {
		for i := range 3 {
			if err := db.InsertCall(net, fmt.Sprintf("%s-%d", net, i), 100+i, 0, c.inWindow(1),
				"g1"+net, "gno.land/r/demo/boards", "Post", true); err != nil {
				t.Fatalf("InsertCall: %v", err)
			}
		}
		mustSubmit(t, db, net, net+"-deploy", "gno.land/r/"+net+"/pkg", "g1"+net, 200, c.inWindow(2), true)
	}

	p, err := db.GetPulse(c.params("one"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if p.Window.Calls != 3 {
		t.Errorf("calls = %d, want 3: the other chain's rows leaked in", p.Window.Calls)
	}
	if len(p.HotRealms) != 1 || p.HotRealms[0].Calls != 3 {
		t.Errorf("hot realms = %+v, want one row of 3 calls", p.HotRealms)
	}
	if p.HotRealms[0].Network != "one" {
		t.Errorf("hot realm network = %q, want one", p.HotRealms[0].Network)
	}
	if len(p.HotDevs) != 1 {
		t.Errorf("hot devs = %d rows, want 1", len(p.HotDevs))
	}
}

func TestPackagePathsScopeToNetwork(t *testing.T) {
	db := NewTestDB(t)
	for _, net := range []string{"one", "two"} {
		if err := db.UpsertPackage(net, "gno.land/r/"+net+"/pkg", "pkg", "g1dev",
			net+"-tx", 10, "", true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}
	paths, err := db.PackagePaths("one")
	if err != nil {
		t.Fatalf("PackagePaths: %v", err)
	}
	if len(paths) != 1 || paths[0] != "gno.land/r/one/pkg" {
		t.Errorf("paths = %v, want just one network's", paths)
	}
}

// mustSubmit writes both halves of a deploy: the append-only submission the
// window queries read, and the current-state row the dependency join needs.
func mustSubmit(t *testing.T, db *DB, network, hash, path, creator string, height int, when string, isRealm bool) {
	t.Helper()
	name := path[strings.LastIndex(path, "/")+1:]
	if err := db.InsertPackageSubmission(network, hash, 0, path, name, creator, height, when, isRealm, 1, true); err != nil {
		t.Fatalf("InsertPackageSubmission: %v", err)
	}
	if err := db.UpsertPackage(network, path, name, creator, hash, height, when, isRealm, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
}

// One busy asset must not own the table. The list is filled round robin over
// the per-asset rankings, so a chain where ugnot moved twenty times and one
// token moved three still shows all three of the token's.
func TestPulseHotFlowsShareTheListBetweenAssets(t *testing.T) {
	db := NewTestDB(t)
	c := newPulseClock()

	token := "gno.land/r/demo/foo.FOO.0000001"
	for i := range 3 {
		if err := db.InsertTokenTransfer("n", fmt.Sprintf("tok-%d", i), i, TokenTransfer{
			Token: token, From: "g1a", To: "g1b", Value: int64(10 - i),
			BlockHeight: 100 + i, BlockTime: c.inWindow(2),
		}); err != nil {
			t.Fatalf("InsertTokenTransfer: %v", err)
		}
	}
	for i := range 20 {
		if err := db.InsertBankSend("n", fmt.Sprintf("send-%d", i), 200+i, c.inWindow(3),
			"g1a", "g1b", fmt.Sprintf("%dugnot", 1000-i), true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}

	p, err := db.GetPulse(c.params("n"))
	if err != nil {
		t.Fatalf("GetPulse: %v", err)
	}
	if len(p.HotFlows) != 10 {
		t.Fatalf("flows = %d, want the limit of 10", len(p.HotFlows))
	}
	var tokenRows, nativeRows int
	for _, f := range p.HotFlows {
		if f.Native {
			nativeRows++
		} else {
			tokenRows++
		}
	}
	if tokenRows != 3 {
		t.Errorf("token rows = %d, want all 3: twenty ugnot sends crowded the asset out", tokenRows)
	}
	if nativeRows != 7 {
		t.Errorf("ugnot rows = %d, want 7 to fill the rest of the list", nativeRows)
	}
}
