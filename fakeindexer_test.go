package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake tx-indexer, served over HTTP by httptest.
//
// The seam is the network, not an interface. IndexerClient talks GraphQL over
// HTTP and everything interesting about it lives in that conversation: the
// element cap that returns rows *and* an error, a non-200 that used to surface
// as "invalid character '<'", the height windows that paging walks. An interface
// would stub all of that out and leave the tests exercising the stub. This runs
// the real doQuery, the real decoder, and the real error paths.
//
// It honours the height predicates and the sort order, because the paging
// contract is where the bugs were: a window that never advances retries genesis
// forever (#63), and one that is too wide returns 45MB to serve twenty rows.
type fakeIndexer struct {
	*httptest.Server

	mu      sync.Mutex
	txs     []Transaction
	blocks  []Block
	queries []string
	chainID string

	// latestHeight overrides the tip derived from the data. Zero means derive
	// it, which is what a real indexer does; setting it fakes an indexer that
	// lags behind, or one that has moved on from what the caller last saw.
	latestHeight int

	// Failure injection, each checked before any data is served.
	status        int    // non-200 to return instead of a response
	gqlError      string // a GraphQL error to return instead of data
	capAt         int    // >0: truncate to this many rows and report the element cap
	blocksFailing bool   // fail only getBlocks, leaving other queries healthy
	delay         time.Duration

	// emptyBlocksOnce makes the next getBlocks answer with no rows regardless of
	// what is stored, which is how a lagging replica (or a load balancer fronting
	// a partially-populated node) answers a range that genuinely has data.
	emptyBlocksOnce bool
}

var (
	reGT       = regexp.MustCompile(`height:\s*{[^}]*\bgt:\s*(-?\d+)`)
	reLT       = regexp.MustCompile(`height:\s*{[^}]*\blt:\s*(-?\d+)`)
	reLike     = regexp.MustCompile(`like:\s*"([^"]*)"`)
	reEq       = regexp.MustCompile(`\beq:\s*"([^"]*)"`)
	reHashEq   = regexp.MustCompile(`hash:\s*{\s*eq:\s*"([^"]*)"`)
	reHeightEq = regexp.MustCompile(`(?:block_)?height:\s*{\s*eq:\s*(-?\d+)`)
)

// newFakeIndexer starts a fake and returns it with a client pointed at it.
func newFakeIndexer(t *testing.T) (*fakeIndexer, *IndexerClient) {
	t.Helper()

	f := &fakeIndexer{chainID: "fake-1"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)

	return f, NewIndexerClient(f.URL)
}

func (f *fakeIndexer) serve(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.queries = append(f.queries, req.Query)
	status, gqlErr, capAt, delay := f.status, f.gqlError, f.capAt, f.delay
	blocksFailing := f.blocksFailing
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if blocksFailing && strings.Contains(req.Query, "getBlocks") {
		http.Error(w, "indexer unavailable", http.StatusInternalServerError)
		return
	}
	if status != 0 && status != http.StatusOK {
		// Deliberately HTML: a rate-limited indexer answers with an error page,
		// and reporting that as a JSON decode failure sends the reader looking
		// for a bug in the query rather than at the status code.
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html><body>%d</body></html>", status)
		return
	}
	if gqlErr != "" {
		writeGQL(w, map[string]any{"errors": []map[string]string{{"message": gqlErr}}})
		return
	}

	data, rows := f.resolve(req.Query)

	if capAt > 0 && rows > capAt {
		// The resolver stops iterating at the cap and returns the rows it
		// already has *alongside* the error. Both halves matter: a caller that
		// treats this as a plain failure throws away a usable partial page.
		writeGQL(w, map[string]any{
			"data":   truncate(data, capAt),
			"errors": []map[string]string{{"message": "max elements per query exceeded"}},
		})
		return
	}

	writeGQL(w, map[string]any{"data": data})
}

// resolve answers one query, applying whatever height filters and ordering it
// carries. It returns the data payload and the number of rows in it.
func (f *fakeIndexer) resolve(q string) (map[string]any, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.Contains(q, "latestBlockHeight"):
		return map[string]any{"latestBlockHeight": f.tip()}, 1

	case strings.Contains(q, "getBlocks"):
		if f.emptyBlocksOnce {
			f.emptyBlocksOnce = false
			return map[string]any{"getBlocks": []Block{}}, 0
		}
		blocks := filterBlocks(f.blocks, q)
		return map[string]any{"getBlocks": blocks}, len(blocks)

	case strings.Contains(q, "getTransactions"):
		txs := filterTxs(f.txs, q)
		return map[string]any{"getTransactions": txs}, len(txs)
	}
	return map[string]any{}, 0
}

func truncate(data map[string]any, n int) map[string]any {
	out := map[string]any{}
	for k, v := range data {
		switch rows := v.(type) {
		case []Transaction:
			out[k] = rows[:min(n, len(rows))]
		case []Block:
			out[k] = rows[:min(n, len(rows))]
		default:
			out[k] = v
		}
	}
	return out
}

// tip is the height the indexer reports as latest. Callers hold the lock.
func (f *fakeIndexer) tip() int {
	if f.latestHeight != 0 {
		return f.latestHeight
	}
	tip := 0
	for _, b := range f.blocks {
		tip = max(tip, b.Height)
	}
	for _, tx := range f.txs {
		tip = max(tip, tx.BlockHeight)
	}
	return tip
}

// whereClause extracts the balanced `where: { ... }` argument.
//
// Matching against the whole query would be wrong in a way that quietly passes:
// the field selection names every message type (`... on MsgCall`, `... on
// BankMsgSend`), so a filter read off the raw text matches everything. Only the
// argument says what was actually asked for.
func whereClause(q string) string {
	i := strings.Index(q, "where:")
	if i < 0 {
		return ""
	}
	open := strings.Index(q[i:], "{")
	if open < 0 {
		return ""
	}
	depth, start := 0, i+open
	for j := start; j < len(q); j++ {
		switch q[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return q[start : j+1]
			}
		}
	}
	return q[start:]
}

func heightBounds(where string) (lo, hi int) {
	lo, hi = -1<<62, 1<<62
	if m := reHeightEq.FindStringSubmatch(where); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, n
	}
	if m := reGT.FindStringSubmatch(where); m != nil {
		n, _ := strconv.Atoi(m[1])
		lo = n + 1
	}
	if m := reLT.FindStringSubmatch(where); m != nil {
		n, _ := strconv.Atoi(m[1])
		hi = n - 1
	}
	return lo, hi
}

func descending(q string) bool { return strings.Contains(q, "DESC") }

func filterTxs(all []Transaction, q string) []Transaction {
	where := whereClause(q)
	lo, hi := heightBounds(where)
	out := []Transaction{}
	for _, tx := range all {
		if tx.BlockHeight < lo || tx.BlockHeight > hi {
			continue
		}
		if !matchesWhere(tx, where) {
			continue
		}
		out = append(out, tx)
	}
	sortByHeight(len(out), descending(q),
		func(i int) int { return out[i].BlockHeight },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func filterBlocks(all []Block, q string) []Block {
	lo, hi := heightBounds(whereClause(q))
	out := []Block{}
	for _, b := range all {
		if b.Height >= lo && b.Height <= hi {
			out = append(out, b)
		}
	}
	sortByHeight(len(out), descending(q),
		func(i int) int { return out[i].Height },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// matchesWhere applies the predicates the read-path queries actually use:
// a hash equality, a set of message types, a set of exact string values, and a
// substring match on package path.
//
// `like` is a plain substring match with no wildcard syntax — a `%` is matched
// literally, which is how a filter for "%govdao%" matched nothing at all while
// looking perfectly reasonable. The fake reproduces that rather than being
// forgiving, so a query written with wildcards finds nothing here too.
func matchesWhere(tx Transaction, where string) bool {
	if m := reHashEq.FindStringSubmatch(where); m != nil {
		return tx.Hash == m[1]
	}

	types := map[string]bool{}
	for _, kind := range []string{"MsgAddPackage", "MsgCall", "MsgRun", "BankMsgSend"} {
		if strings.Contains(where, kind) {
			types[kind] = true
		}
	}

	eqs := map[string]bool{}
	for _, m := range reEq.FindAllStringSubmatch(where, -1) {
		eqs[m[1]] = true
	}

	like := ""
	if m := reLike.FindStringSubmatch(where); m != nil {
		like = m[1]
	}

	if len(types) == 0 && len(eqs) == 0 && like == "" {
		return true
	}

	for _, msg := range tx.Messages {
		if len(types) > 0 && !types[msg.Value.Typename] {
			continue
		}
		if like != "" && !strings.Contains(msg.Value.PkgPath, like) {
			continue
		}
		if len(eqs) > 0 && !matchesAnyField(msg.Value, eqs) {
			continue
		}
		return true
	}
	return false
}

// matchesAnyField reports whether a message carries one of the wanted values in
// a field a query filters on. Queries name the field precisely; the fake does
// not, because no read-path query filters two different fields to two different
// values, so distinguishing them would add machinery no test can observe.
func matchesAnyField(v MessageValue, want map[string]bool) bool {
	fields := []string{v.Caller, v.Creator, v.FromAddress, v.ToAddress, v.PkgPath}
	if v.Package != nil {
		fields = append(fields, v.Package.Path)
	}
	for _, f := range fields {
		if f != "" && want[f] {
			return true
		}
	}
	return false
}

// sortByHeight is an insertion sort: stable, and these fixtures are tiny.
func sortByHeight(n int, desc bool, height func(int) int, swap func(int, int)) {
	for i := 1; i < n; i++ {
		for j := i; j > 0; j-- {
			a, b := height(j-1), height(j)
			if (desc && a >= b) || (!desc && a <= b) {
				break
			}
			swap(j-1, j)
		}
	}
}

func writeGQL(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- fixture control -------------------------------------------------------

// set pins block 1's hash and the reported tip. Block 1 is the fingerprint the
// syncer compares against to notice a chain reset.
func (f *fakeIndexer) set(hash string, height int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.latestHeight = height
	for i := range f.blocks {
		if f.blocks[i].Height == 1 {
			f.blocks[i].Hash = hash
			return
		}
	}
	f.blocks = append(f.blocks, Block{
		Hash: hash, Height: 1, ChainID: f.chainID, Time: "2026-01-01T00:00:00Z",
	})
}

// failBlock1 makes block queries fail while leaving the rest healthy, which is
// how an indexer that is up but struggling actually behaves.
func (f *fakeIndexer) failBlock1(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocksFailing = failing
}

// seedChain fills the fake with n blocks and one call per block, dated a minute
// apart so heights and timestamps agree.
func (f *fakeIndexer) seedChain(from, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		h := from + i
		when := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		f.blocks = append(f.blocks, Block{
			Hash: fmt.Sprintf("block-%d", h), Height: h, ChainID: f.chainID,
			Time: when, NumTxs: 1, TotalTxs: i + 1, ProposerAddressRaw: "g1proposer",
		})
		f.txs = append(f.txs, fakeCall(h, when, fmt.Sprintf("g1caller%d", i%3),
			"gno.land/r/demo/boards", "Post"))
	}
}

// fakeBlockTime is the deterministic height-to-time mapping setBlockRange uses,
// so a test that cares about block *times* (the -block-history-days cutoff) can
// convert between the two in either direction.
//
// The spacing is an hour rather than the few seconds a real chain uses. The
// cutoff is computed with AddDate, so it always lands on a whole-day boundary:
// at realistic spacing a chain short enough to seed as real rows would span
// minutes, and no whole-day cutoff could fall inside it. An hour per block lets
// a few hundred rows span a few weeks.
var fakeBlockEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const fakeBlockSpacing = time.Hour

func fakeBlockTime(height int) time.Time {
	return fakeBlockEpoch.Add(time.Duration(height) * fakeBlockSpacing)
}

// fakeBlockHeightAt is fakeBlockTime's inverse, rounded down.
func fakeBlockHeightAt(t time.Time) int {
	return int(t.Sub(fakeBlockEpoch) / fakeBlockSpacing)
}

// setBlockRange replaces the fake's blocks with the contiguous range [lo, hi]
// and reports hi as the tip. Nothing exists below lo, which is what a pruned
// indexer looks like from the outside.
func (f *fakeIndexer) setBlockRange(lo, hi int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.blocks = f.blocks[:0]
	for h := lo; h <= hi; h++ {
		f.blocks = append(f.blocks, Block{
			Hash: fmt.Sprintf("block-%d", h), Height: h, ChainID: f.chainID,
			Time:               fakeBlockTime(h).Format(time.RFC3339),
			NumTxs:             1,
			TotalTxs:           h - lo + 1,
			ProposerAddressRaw: "g1proposer",
		})
	}
	f.latestHeight = hi
}

// forceEmptyRange makes the very next getBlocks return no rows, then clears
// itself, so a test can inject one transient empty page.
func (f *fakeIndexer) forceEmptyRange() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyBlocksOnce = true
}

// blockQueryCount reports how many getBlocks queries have been asked, so a test
// can assert that a terminated backfill stops re-querying.
func (f *fakeIndexer) blockQueryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(q, "getBlocks") {
			n++
		}
	}
	return n
}

// redate stamps every block and transaction with the same timestamp, to make
// one chain unambiguously more recent than another regardless of its heights.
func (f *fakeIndexer) redate(when string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.blocks {
		f.blocks[i].Time = when
	}
	for i := range f.txs {
		f.txs[i].BlockTime = when
	}
}

func (f *fakeIndexer) add(txs ...Transaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs = append(f.txs, txs...)
}

func (f *fakeIndexer) askedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func fakeCall(height int, when, caller, pkgPath, fn string) Transaction {
	return Transaction{
		Hash: fmt.Sprintf("tx-call-%d", height), Success: true, BlockHeight: height,
		GasWanted: 200000, GasUsed: 100000 + height, BlockTime: when,
		Messages: []TxMessage{{
			TypeURL: "exec", Route: "vm",
			Value: MessageValue{Typename: "MsgCall", Caller: caller, PkgPath: pkgPath, Func: fn},
		}},
		Response: &TxResponse{Events: []TxEvent{{
			Typename: "GnoEvent", Type: "PostCreated", PkgPath: pkgPath,
			Attrs: []EventAttr{{Key: "id", Value: strconv.Itoa(height)}},
		}}},
	}
}

func fakePackage(height int, when, creator, path string) Transaction {
	return Transaction{
		Hash: fmt.Sprintf("tx-pkg-%d", height), Success: true, BlockHeight: height, BlockTime: when,
		Messages: []TxMessage{{
			TypeURL: "add_package", Route: "vm",
			Value: MessageValue{Typename: "MsgAddPackage", Creator: creator, Package: &MemPackage{
				Name: "pkg", Path: path,
				Files: []MemFile{{Name: "pkg.gno", Body: "package pkg\n\nimport \"gno.land/p/demo/avl\"\n"}},
			}},
		}},
	}
}

func fakeSend(height int, when, from, to, amount string) Transaction {
	return Transaction{
		Hash: fmt.Sprintf("tx-send-%d", height), Success: true, BlockHeight: height, BlockTime: when,
		Messages: []TxMessage{{
			TypeURL: "send", Route: "bank",
			Value: MessageValue{Typename: "BankMsgSend", FromAddress: from, ToAddress: to, Amount: amount},
		}},
	}
}
