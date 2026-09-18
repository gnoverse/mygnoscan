package indexer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A fake tx-indexer, served over HTTP by httptest.
//
// The seam is the network, not an interface. Client talks GraphQL over
// HTTP and everything interesting about it lives in that conversation: the
// element cap that returns rows *and* an error, a non-200 that used to surface
// as "invalid character '<'", the height windows that paging walks. An interface
// would stub all of that out and leave the tests exercising the stub. This runs
// the real doQuery, the real decoder, and the real error paths.
//
// It honours the height predicates and the sort order, because the paging
// contract is where the bugs were: a window that never advances retries genesis
// forever (#63), and one that is too wide returns 45MB to serve twenty rows.
// TB is the slice of *testing.T this fake needs.
//
// Declared here rather than taking *testing.T so this file does not import
// `testing`: that package registers its flags on init, and any binary linking
// it grows a -test.* flag set it never asked for. Every *testing.T satisfies
// this already.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

type Fake struct {
	*httptest.Server

	mu      sync.Mutex
	txs     []Transaction
	blocks  []Block
	queries []string
	ChainID string

	// latestHeight overrides the tip derived from the data. Zero means derive
	// it, which is what a real indexer does; setting it fakes an indexer that
	// lags behind, or one that has moved on from what the caller last saw.
	latestHeight int

	// NoInertTypes makes the fake reject any query selecting the inert-package
	// message types, the way an older tx-indexer does. Pearl's did.
	NoInertTypes bool

	// NoSessionTypes is the same for the account-session message types, and is
	// the shape every live gno.land indexer actually has: MsgEnablePackage
	// defined, MsgCreateSession not. Checked on mainnet 2026-09-18.
	NoSessionTypes bool

	// Failure injection, each checked before any data is served.
	Status        int    // non-200 to return instead of a response
	GQLError      string // a GraphQL error to return instead of data
	CapAt         int    // >0: truncate to this many rows and report the element cap
	BlocksFailing bool   // fail only getBlocks, leaving other queries healthy
	Delay         time.Duration

	// emptyBlocksOnce makes the next getBlocks answer with no rows regardless of
	// what is stored, which is how a lagging replica (or a load balancer fronting
	// a partially-populated node) answers a range that genuinely has data.
	emptyBlocksOnce bool
}

var (
	reGT = regexp.MustCompile(`height:\s*{[^}]*\bgt:\s*(-?\d+)`)
	reLT = regexp.MustCompile(`height:\s*{[^}]*\blt:\s*(-?\d+)`)
	// Every comparator applied to a height, so the fake can reject the ones
	// FilterInt does not have. See intFilterOps.
	reHeightOps = regexp.MustCompile(`height:\s*{([^}]*)}`)
	reOpName    = regexp.MustCompile(`(\w+)\s*:`)
	reLike      = regexp.MustCompile(`like:\s*"([^"]*)"`)
	reEq        = regexp.MustCompile(`\beq:\s*"([^"]*)"`)
	reHashEq    = regexp.MustCompile(`hash:\s*{\s*eq:\s*"([^"]*)"`)
	reHeightEq  = regexp.MustCompile(`(?:block_)?height:\s*{\s*eq:\s*(-?\d+)`)
)

// New starts a fake and returns it with a client pointed at it.
func NewFake(t TB) (*Fake, *Client) {
	t.Helper()

	f := &Fake{ChainID: "fake-1"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)

	return f, NewClient(f.URL)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.queries = append(f.queries, req.Query)
	Status, gqlErr, CapAt, Delay := f.Status, f.GQLError, f.CapAt, f.Delay
	BlocksFailing := f.BlocksFailing
	f.mu.Unlock()

	if Delay > 0 {
		time.Sleep(Delay)
	}
	if BlocksFailing && strings.Contains(req.Query, "getBlocks") {
		http.Error(w, "indexer unavailable", http.StatusInternalServerError)
		return
	}
	if Status != 0 && Status != http.StatusOK {
		// Deliberately HTML: a rate-limited indexer answers with an error page,
		// and reporting that as a JSON decode failure sends the reader looking
		// for a bug in the query rather than at the Status code.
		w.WriteHeader(Status)
		fmt.Fprintf(w, "<html><body>%d</body></html>", Status)
		return
	}
	f.mu.Lock()
	noInert := f.NoInertTypes
	noSessions := f.NoSessionTypes
	f.mu.Unlock()
	if noSessions {
		if strings.Contains(req.Query, `__type(name: "MsgCreateSession")`) {
			writeGQL(w, map[string]any{"data": map[string]any{"__type": nil}})
			return
		}
		if strings.Contains(req.Query, "MsgCreateSession") || strings.Contains(req.Query, "MsgRevokeSession") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeGQL(w, map[string]any{"errors": []map[string]string{
				{"message": `Unknown type "MsgCreateSession".`},
			}})
			return
		}
	}
	if noInert {
		// The schema probe answers honestly...
		if strings.Contains(req.Query, `__type(name: "MsgEnablePackage")`) {
			writeGQL(w, map[string]any{"data": map[string]any{"__type": nil}})
			return
		}
		// ...and anything still selecting them is rejected the way a real
		// indexer rejects an unknown type: a validation error, not a 200 with
		// missing fields.
		if strings.Contains(req.Query, "MsgEnablePackage") || strings.Contains(req.Query, "MsgRejectPackage") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeGQL(w, map[string]any{"errors": []map[string]string{
				{"message": `Unknown type "MsgEnablePackage".`},
			}})
			return
		}
	}

	if gqlErr != "" {
		writeGQL(w, map[string]any{"errors": []map[string]string{{"message": gqlErr}}})
		return
	}
	if op := unsupportedHeightOp(req.Query); op != "" {
		writeGQL(w, map[string]any{"errors": []map[string]string{{
			"message": fmt.Sprintf("Field %q is not defined by type \"FilterInt\"", op),
		}}})
		return
	}

	data, rows := f.resolve(req.Query)

	if CapAt > 0 && rows > CapAt {
		// The resolver stops iterating at the cap and returns the rows it
		// already has *alongside* the error. Both halves matter: a caller that
		// treats this as a plain failure throws away a usable partial page.
		writeGQL(w, map[string]any{
			"data":   truncate(data, CapAt),
			"errors": []map[string]string{{"message": "max elements per query exceeded"}},
		})
		return
	}

	writeGQL(w, map[string]any{"data": data})
}

// resolve answers one query, applying whatever height filters and ordering it
// carries. It returns the data payload and the number of rows in it.
func (f *Fake) resolve(q string) (map[string]any, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Every requested top-level field is resolved, not just the first match.
	//
	// This used to be a switch, so a query selecting `latestBlockHeight` and
	// `getBlocks` together got only the height back and the rest silently came
	// out empty. A real GraphQL server resolves every field a query asks for,
	// and the endpoint pool's probe asks for two at once precisely because they
	// have to be read from the same moment — the tip alone cannot tell "further
	// along" from "a different chain".
	out := map[string]any{}
	count := 0
	// Schema introspection, so a client can ask what this indexer supports.
	// NoInertTypes answers this earlier, in serve.
	if strings.Contains(q, "__type") {
		out["__type"] = map[string]any{"name": "MsgEnablePackage"}
		count++
	}
	if strings.Contains(q, "latestBlockHeight") {
		out["latestBlockHeight"] = f.tip()
		count++
	}
	if strings.Contains(q, "getBlocks") {
		if f.emptyBlocksOnce {
			f.emptyBlocksOnce = false
			out["getBlocks"] = []Block{}
		} else {
			blocks := filterBlocks(f.blocks, q)
			out["getBlocks"] = blocks
			count += len(blocks)
		}
	}
	if strings.Contains(q, "getTransactions") {
		txs := filterTxs(f.txs, q)
		out["getTransactions"] = txs
		count += len(txs)
	}
	return out, count
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
func (f *Fake) tip() int {
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

// intFilterOps is FilterInt's full Set of comparators, read off the live
// schema. It is deliberately short: there is no `gte`.
//
// A fake that answers queries the real indexer rejects is worse than no fake,
// because it reports the bug as fixed. This exact gap shipped a `gte` bound
// that passed every test here and returned a GRAPHQL_VALIDATION_FAILED against
// gno.land — "Field \"gte\" is not defined by type \"FilterInt\"".
var intFilterOps = map[string]bool{"exists": true, "eq": true, "gt": true, "lt": true}

// unsupportedHeightOp returns the first comparator used on a height that
// FilterInt does not define, or "" when the query is valid.
func unsupportedHeightOp(q string) string {
	for _, block := range reHeightOps.FindAllStringSubmatch(q, -1) {
		for _, op := range reOpName.FindAllStringSubmatch(block[1], -1) {
			if !intFilterOps[op[1]] {
				return op[1]
			}
		}
	}
	return ""
}

func heightBounds(where string) (lo, hi int) {
	lo, hi = -1<<62, 1<<62
	if m := reHeightEq.FindStringSubmatch(where); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, n
	}
	if m := reGT.FindStringSubmatch(where); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n < 0 {
			// A negative bound is not a way to reach genesis: the indexer
			// answers `gt: -1` with a null result Set, not with every row.
			return 1 << 62, -1 << 62
		}
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
// a hash equality, a Set of message types, a Set of exact string values, and a
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
// values, so distinguishing them would Add machinery no test can observe.
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

// Set pins block 1's hash and the reported tip. Block 1 is the fingerprint the
// syncer compares against to notice a chain reset.
func (f *Fake) Set(hash string, height int) {
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
		Hash: hash, Height: 1, ChainID: f.ChainID, Time: "2026-01-01T00:00:00Z",
	})
}

// FailBlock1 makes block queries fail while leaving the rest healthy, which is
// how an indexer that is up but struggling actually behaves.
func (f *Fake) FailBlock1(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BlocksFailing = failing
}

// SeedChain fills the fake with n blocks and one call per block, dated a minute
// apart so heights and timestamps agree.
func (f *Fake) SeedChain(from, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		h := from + i
		when := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		f.blocks = append(f.blocks, Block{
			Hash: fmt.Sprintf("block-%d", h), Height: h, ChainID: f.ChainID,
			Time: when, NumTxs: 1, TotalTxs: i + 1, ProposerAddressRaw: "g1proposer",
		})
		f.txs = append(f.txs, Call(h, when, fmt.Sprintf("g1caller%d", i%3),
			"gno.land/r/demo/boards", "Post"))
	}
}

// BlockTime is the deterministic height-to-time mapping SetBlockRange uses,
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

func BlockTime(height int) time.Time {
	return fakeBlockEpoch.Add(time.Duration(height) * fakeBlockSpacing)
}

// BlockHeightAt is BlockTime's inverse, rounded down.
func BlockHeightAt(t time.Time) int {
	return int(t.Sub(fakeBlockEpoch) / fakeBlockSpacing)
}

// SetBlockRange replaces the fake's blocks with the contiguous range [lo, hi]
// and reports hi as the tip. Nothing exists below lo, which is what a pruned
// indexer looks like from the outside.
func (f *Fake) SetBlockRange(lo, hi int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.blocks = f.blocks[:0]
	for h := lo; h <= hi; h++ {
		f.blocks = append(f.blocks, Block{
			Hash: fmt.Sprintf("block-%d", h), Height: h, ChainID: f.ChainID,
			Time:               BlockTime(h).Format(time.RFC3339),
			NumTxs:             1,
			TotalTxs:           h - lo + 1,
			ProposerAddressRaw: "g1proposer",
		})
	}
	f.latestHeight = hi
}

// ForceEmptyRange makes the very next getBlocks return no rows, then clears
// itself, so a test can inject one transient empty page.
func (f *Fake) ForceEmptyRange() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyBlocksOnce = true
}

// BlockQueryCount reports how many getBlocks queries have been asked, so a test
// can assert that a terminated backfill stops re-querying.
func (f *Fake) BlockQueryCount() int {
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

// Redate stamps every block and transaction with the same timestamp, to make
// one chain unambiguously more recent than another regardless of its heights.
func (f *Fake) Redate(when string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.blocks {
		f.blocks[i].Time = when
	}
	for i := range f.txs {
		f.txs[i].BlockTime = when
	}
}

// SetLatestHeight overrides the tip the fake reports, independently of the
// blocks it holds — the shape of an indexer that is behind its own chain.
func (f *Fake) SetLatestHeight(h int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestHeight = h
}

// AddBlocks appends blocks without the transactions SeedChain would pair them
// with, for tests that care about block-shaped answers on their own.
func (f *Fake) AddBlocks(blocks ...Block) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks = append(f.blocks, blocks...)
}

func (f *Fake) Add(txs ...Transaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs = append(f.txs, txs...)
}

func (f *Fake) AskedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func Call(height int, when, caller, pkgPath, fn string) Transaction {
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

func Package(height int, when, creator, path string) Transaction {
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

func Send(height int, when, from, to, amount string) Transaction {
	return Transaction{
		Hash: fmt.Sprintf("tx-send-%d", height), Success: true, BlockHeight: height, BlockTime: when,
		Messages: []TxMessage{{
			TypeURL: "send", Route: "bank",
			Value: MessageValue{Typename: "BankMsgSend", FromAddress: from, ToAddress: to, Amount: amount},
		}},
	}
}

// TruncatingServer imitates the tx-indexer resolver: it iterates in the
// requested order, stops at the element cap, and returns the rows it already has
// *alongside* the error rather than refusing the query. txsPerBlock transactions
// per block, heights 1..tip.
type TruncatingServer struct {
	Tip         int
	TxsPerBlock int

	Mu        sync.Mutex
	TxQueries []string
}

func (f *TruncatingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	query := string(body)
	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(query, "latestBlockHeight") {
		fmt.Fprintf(w, `{"data":{"latestBlockHeight":%d}}`, f.Tip)
		return
	}

	// The capability probe is not a transaction query; recording it would make
	// it query zero and break every assertion that reads the first one.
	//
	// Answered before taking the lock: returning from inside the critical
	// section leaves the mutex held and deadlocks every request after it.
	if strings.Contains(query, `__type(name:`) {
		writeGQL(w, map[string]any{"data": map[string]any{"__type": map[string]any{"name": "MsgEnablePackage"}}})
		return
	}

	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.TxQueries = append(f.TxQueries, query)

	from, to := 0, f.Tip
	if gt := strings.Index(query, "gt:"); gt >= 0 {
		fmt.Sscanf(strings.TrimSpace(query[gt+3:]), "%d", &from)
	}
	if lt := strings.Index(query, "lt:"); lt >= 0 {
		var v int
		fmt.Sscanf(strings.TrimSpace(query[lt+3:]), "%d", &v)
		to = min(v-1, f.Tip)
	}

	var rows []string
	emit := func(h int) {
		for i := 0; i < f.TxsPerBlock; i++ {
			rows = append(rows, fmt.Sprintf(`{"hash":"tx-%d-%d","block_height":%d}`, h, i, h))
		}
	}
	if strings.Contains(query, "DESC") {
		for h := to; h > from && len(rows) < ElementCap; h-- {
			emit(h)
		}
	} else {
		for h := from + 1; h <= to && len(rows) < ElementCap; h++ {
			emit(h)
		}
	}

	// The resolver checks its counter before appending the next row, so a result
	// set of exactly the cap is reported as truncated too.
	truncated := len(rows) >= ElementCap
	if truncated {
		rows = rows[:ElementCap]
	}

	data := fmt.Sprintf(`"data":{"getTransactions":[%s]}`, strings.Join(rows, ","))
	if truncated {
		fmt.Fprintf(w, `{%s,"errors":[{"message":"max elements per query reached (%d)"}]}`,
			data, ElementCap)
		return
	}
	fmt.Fprintf(w, `{%s}`, data)
}

// blockOf groups a page by block height, so tests can assert no block came back
// half-populated.
func blockOf(txs []Transaction) map[int]int {
	per := make(map[int]int)
	for _, tx := range txs {
		per[tx.BlockHeight]++
	}
	return per
}
