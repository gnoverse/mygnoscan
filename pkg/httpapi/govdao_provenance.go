package httpapi

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"regexp"

	"github.com/moul/mygnoscan/pkg/indexer"
)

// Assembling a proposal's provenance: the creating transaction, the
// lifecycle timeline, the parameter diff, and the identity checks that feed
// govdao_audit.go's signal rules.
//
// The split is deliberate. This file talks to the indexer and the chain;
// govdao_audit.go is pure and holds every rule. A rule that needed a network
// call to decide would be a rule nobody could test.

// creationsCacheTTL bounds how stale the proposal-creation index can be. A
// ProposalCreated event is emitted once per proposal, so this list changes
// only when governance does; a minute is well inside the window where a
// brand new proposal still looks live.
const creationsCacheTTL = 60 * time.Second

// creationsFailureTTL bounds retries when the indexer cannot answer. Matches
// govDAOFailureTTL: same kind of upstream, same reason.
const creationsFailureTTL = 5 * time.Second

var creationsCache = newMemo[[]indexer.Transaction](creationsCacheTTL, creationsFailureTTL)

// proposalCreations returns every ProposalCreated transaction on the network,
// cached, newest first. A failed refresh keeps serving the last good list
// rather than blanking every proposal's provenance over one bad round trip,
// the same policy as the render caches in govdao.go.
// This is the hottest cache on the govdao list page and the one that most
// wanted single-flight: AuditGovDAOProposal calls it, and the overview runs one
// AuditGovDAOProposal per proposal concurrently. Cold, that used to be one full
// indexer query plus one block-time stamping pass *per proposal*, all at once,
// against the same indexer.
//
// The old version also held the cache mutex across stampBlockTimes, which is a
// network call, so the concurrent callers queued behind it even when they were
// about to be served the same answer. memo runs the fetch outside the lock.
func (a *API) proposalCreations(ctx context.Context, network string) []indexer.Transaction {
	client := a.clientFor(network)
	if client == nil {
		return nil
	}
	return creationsCache.get(ctx, network, func(ctx context.Context) ([]indexer.Transaction, bool) {
		txs, err := client.GetGovDAOProposalCreations(ctx)
		if err != nil {
			return nil, false
		}
		a.stampBlockTimes(ctx, network, client, txs)
		return txs, true
	})
}

// proposalIDOf returns the proposal ID a ProposalCreated transaction
// announced. Reading it off the event rather than off the script is the
// whole point: gov/dao assigns the ID, the script never names it.
func proposalIDOf(tx indexer.Transaction) (int, bool) {
	for _, ev := range tx.Response.Events {
		if ev.Type != "ProposalCreated" || ev.PkgPath != indexer.GovDAORealm {
			continue
		}
		for _, at := range ev.Attrs {
			if at.Key == "id" {
				if id, err := strconv.Atoi(at.Value); err == nil {
					return id, true
				}
			}
		}
	}
	return 0, false
}

// codeFromCreation turns the creating transaction into the code panel:
// the script as submitted, its gno.land imports, the request constructors
// it called, and whether it voted in the same breath.
//
// Only the message that actually built the proposal is described. A
// transaction can carry several messages, and the proposal-building one is
// the one whose source or target names gov/dao.
func codeFromCreation(tx indexer.Transaction) *ProposalCode {
	for _, m := range tx.Messages {
		v := m.Value
		switch v.Typename {
		case "MsgRun":
			var name, body string
			if v.Package != nil && len(v.Package.Files) > 0 {
				// Scripts are single-file in every case seen on chain, but
				// concatenating is the honest fallback: showing only the
				// first file of a two-file script would hide half the code
				// this page exists to show.
				var parts []string
				for _, f := range v.Package.Files {
					parts = append(parts, f.Body)
				}
				name = v.Package.Files[0].Name
				if len(v.Package.Files) > 1 {
					name = v.Package.Files[0].Name + " (+" + strconv.Itoa(len(v.Package.Files)-1) + " more)"
				}
				body = strings.Join(parts, "\n")
			}
			return &ProposalCode{
				Kind: "run", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
				Caller: v.Caller, Success: tx.Success, FileName: name, Source: body,
				Imports: parseGnoImports(body), Requests: parseProposalRequests(body),
				CastsVote: sourceCastsVote(body),
			}
		case "MsgCall":
			if !strings.HasPrefix(v.PkgPath, indexer.GovDAORealm) {
				continue
			}
			return &ProposalCode{
				Kind: "call", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
				Caller: v.Caller, Success: tx.Success, PkgPath: v.PkgPath, Func: v.Func, Args: v.Args,
			}
		}
	}
	return nil
}

// effectsOf flattens a transaction's GnoEvents, dropping gov/dao's own
// bookkeeping event and the storage-deposit noise every transaction emits.
// What is left is what the transaction did to the rest of the chain, which
// on an execution step is the only on-chain proof of the change.
func effectsOf(tx indexer.Transaction) []ProposalEffect {
	var out []ProposalEffect
	for _, ev := range tx.Response.Events {
		if ev.Typename != "GnoEvent" || ev.Type == "" {
			continue
		}
		if ev.Type == "ProposalCreated" {
			continue
		}
		e := ProposalEffect{Type: ev.Type, PkgPath: ev.PkgPath}
		for _, at := range ev.Attrs {
			e.Attrs = append(e.Attrs, EffectKV{Key: at.Key, Value: at.Value})
		}
		out = append(out, e)
	}
	return out
}

// voteFuncs and executeFuncs name the gov/dao entry points whose first
// argument is a proposal ID. Matched by prefix so a versioned or "Simple"
// variant is classified rather than dropped into the generic bucket.
func classifyGovDAOFunc(fn string) (kind, label string) {
	switch {
	case strings.Contains(fn, "Vote"):
		return "vote", "vote"
	case strings.Contains(fn, "Execute"):
		return "executed", "execution"
	default:
		return "attempt", fn
	}
}

// buildTimeline orders a proposal's whole life into steps, each anchored to
// a transaction. Creation comes from the ProposalCreated event, everything
// after it from MsgCalls whose first argument is this proposal's ID.
//
// A vote cast inside the creating script is not a separate transaction and
// so is not a separate step; the creation step says so instead, which is
// more honest than inventing a row with the same tx hash.
func buildTimeline(id int, creation indexer.Transaction, hasCreation bool, calls []indexer.Transaction) []ProposalStep {
	var out []ProposalStep
	if hasCreation {
		code := codeFromCreation(creation)
		detail := "proposal created"
		if code != nil && code.CastsVote {
			detail = "proposal created, and voted on in the same transaction"
		}
		caller := ""
		if code != nil {
			caller = code.Caller
		}
		out = append(out, ProposalStep{
			Kind: "created", Label: "created", TxHash: creation.Hash,
			BlockHeight: creation.BlockHeight, BlockTime: creation.BlockTime,
			Caller: caller, Success: creation.Success, Detail: detail,
			Effects: effectsOf(creation),
		})
	}
	idStr := strconv.Itoa(id)
	for _, tx := range calls {
		for _, m := range tx.Messages {
			v := m.Value
			if v.Typename != "MsgCall" || len(v.Args) == 0 || v.Args[0] != idStr {
				continue
			}
			if !strings.HasPrefix(v.PkgPath, indexer.GovDAORealm) {
				continue
			}
			kind, label := classifyGovDAOFunc(v.Func)
			detail := v.Func
			if len(v.Args) > 1 {
				detail = v.Func + "(" + strings.Join(v.Args, ", ") + ")"
			}
			if !tx.Success {
				kind = "attempt"
			}
			out = append(out, ProposalStep{
				Kind: kind, Label: label, TxHash: tx.Hash,
				BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
				Caller: v.Caller, Success: tx.Success, Detail: detail,
				Effects: effectsOf(tx),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BlockHeight < out[j].BlockHeight })
	return out
}

// paramKeyRe bounds what may be concatenated into an ABCI query path.
//
// The key is decoded from a MsgRun script's string literals, and anyone can
// submit a MsgRun, so it is attacker-controlled text on its way into a query
// path. Every real key is "<module>:<submodule>:<name>"; anything else is
// refused rather than sent, which costs nothing (an invented key has no
// value to read anyway) and keeps this from being the place a crafted
// proposal gets to shape a query.
var paramKeyRe = regexp.MustCompile(`^[a-z0-9_]+:[a-z0-9_]+:[a-z0-9_]+$`)

// fetchChainParam reads a system parameter's current value over ABCI.
//
// The `params/<key>` path answers with the stored JSON: an array for the
// list-valued keys, a bare scalar otherwise, and an empty body for a key
// that has never been set. The bool return separates "read the chain, it is
// unset" from "could not read the chain" - two states an empty slice cannot
// tell apart, and the difference decides whether the page is allowed to say
// what the value would become.
func fetchChainParam(ctx context.Context, rpcURL, key string) ([]string, bool) {
	if rpcURL == "" || !paramKeyRe.MatchString(key) {
		return nil, false
	}
	raw, err := fetchABCIQuery(ctx, rpcURL, "params/"+key, "")
	if err != nil {
		return nil, false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return list, true
	}
	var scalar any
	if err := json.Unmarshal([]byte(raw), &scalar); err == nil {
		switch s := scalar.(type) {
		case string:
			return []string{s}, true
		case float64:
			return []string{strconv.FormatFloat(s, 'f', -1, 64)}, true
		case bool:
			return []string{strconv.FormatBool(s)}, true
		}
	}
	return []string{raw}, true
}

// addressNameCacheTTL matches usernameCacheTTL: the same registry, the same
// reason (a binding changes only on an explicit re-registration).
const addressNameCacheTTL = usernameCacheTTL

var addressNameCache = struct {
	mu      sync.Mutex
	byAddr  map[string]string
	fetched map[string]time.Time
}{byAddr: map[string]string{}, fetched: map[string]time.Time{}}

// resolveGnoAddressCached is resolveGnoUsernameCached's inverse: address to
// registered username, via r/sys/users.ResolveAddress. An empty result means
// "no username registered", which is a real answer here rather than a
// failure - it is what turns a `// jae` comment from confirmed into merely
// claimed.
func resolveGnoAddressCached(ctx context.Context, rpcURL, addr string) string {
	if addr == "" || rpcURL == "" {
		return ""
	}
	addressNameCache.mu.Lock()
	if name, ok := addressNameCache.byAddr[addr]; ok && time.Since(addressNameCache.fetched[addr]) < addressNameCacheTTL {
		addressNameCache.mu.Unlock()
		return name
	}
	addressNameCache.mu.Unlock()

	expr := "gno.land/r/sys/users.ResolveAddress(address(\"" + addr + "\"))"
	out, err := fetchABCIQuery(ctx, rpcURL, "vm/qeval", expr)
	if err != nil {
		addressNameCache.mu.Lock()
		defer addressNameCache.mu.Unlock()
		return addressNameCache.byAddr[addr]
	}
	name := usernameFromUserData(out, addr)

	addressNameCache.mu.Lock()
	defer addressNameCache.mu.Unlock()
	addressNameCache.byAddr[addr] = name
	addressNameCache.fetched[addr] = time.Now()
	return name
}

// gnoQuotedRe matches a double-quoted token in Gno's debug repr.
var gnoQuotedRe = regexp.MustCompile(`"([^"]*)"`)

// usernameFromUserData pulls the name out of r/sys/users' debug repr:
//
//	(&(struct{("g1aedd..." .uverse.address),("aeddi" string),(false bool)} ...UserData) *...UserData)
//
// The address is the first quoted token and the username the next one, so
// the name is the first quoted string that is not the address itself. A
// nil result (an unregistered address) has no quoted strings at all and
// yields "".
func usernameFromUserData(repr, addr string) string {
	for _, m := range gnoQuotedRe.FindAllStringSubmatch(repr, -1) {
		if m[1] != addr && m[1] != "" {
			return m[1]
		}
	}
	return ""
}

// AuditGovDAOProposal fills a proposal's provenance, timeline, decoded
// parameter change, identity checks and signals. Best effort throughout: a
// piece that cannot be fetched is left empty and the signal that depended
// on it is simply not emitted, rather than the page failing.
func (a *API) AuditGovDAOProposal(ctx context.Context, network, rpcURL string, detail *GovDAOProposalDetail) {
	creations := a.proposalCreations(ctx, network)

	var creation indexer.Transaction
	hasCreation := false
	priorByAuthor := []int{}
	priorByExecutor := []int{}
	type creationInfo struct {
		id      int
		caller  string
		imports []string
		height  int
	}
	var infos []creationInfo
	for _, tx := range creations {
		id, ok := proposalIDOf(tx)
		if !ok {
			continue
		}
		code := codeFromCreation(tx)
		caller := ""
		var imports []string
		if code != nil {
			caller = code.Caller
			imports = code.Imports
			if code.Kind == "call" {
				imports = []string{code.PkgPath}
			}
		}
		infos = append(infos, creationInfo{id: id, caller: caller, imports: imports, height: tx.BlockHeight})
		if id == detail.ID {
			creation, hasCreation = tx, true
		}
	}

	if hasCreation {
		detail.Code = codeFromCreation(creation)
	}
	authorAddr := detail.AuthorAddress
	if authorAddr == "" && detail.Code != nil {
		authorAddr = detail.Code.Caller
	}
	for _, in := range infos {
		if in.id >= detail.ID {
			continue
		}
		if authorAddr != "" && in.caller == authorAddr {
			priorByAuthor = append(priorByAuthor, in.id)
		}
		if detail.ExecutorPkgPath != "" {
			for _, imp := range in.imports {
				if imp == detail.ExecutorPkgPath {
					priorByExecutor = append(priorByExecutor, in.id)
					break
				}
			}
		}
	}
	sort.Ints(priorByAuthor)
	sort.Ints(priorByExecutor)

	calls, _ := a.fetchGovDAOTransactions(ctx, network)
	detail.Timeline = buildTimeline(detail.ID, creation, hasCreation, calls)

	if detail.Code != nil {
		// Every request, not just the first: NewSetHaltRequest alone writes
		// two keys, and showing one of them would be worse than showing
		// neither.
		for _, call := range detail.Code.Requests {
			for _, pc := range decodeParamChanges(call) {
				current, known := fetchChainParam(ctx, rpcURL, pc.Key)
				pc.Current, pc.CurrentKnown = current, known
				if known {
					pc.Result = applyParamVerb(pc.Verb, current, pc.Values)
				}
				detail.ParamChanges = append(detail.ParamChanges, pc)
			}
		}
		detail.Addresses = parseSourceAddresses(detail.Code.Source)
	}

	memberTier := map[string]string{}
	overview := FetchGovDAOOverview(ctx, network, rpcURL)
	for _, m := range overview.Members {
		memberTier[m.Address] = m.Tier
	}
	if len(overview.Members) == 0 {
		memberTier = nil
	}

	for i := range detail.Addresses {
		addr := &detail.Addresses[i]
		addr.OnChain = resolveGnoAddressCached(ctx, rpcURL, addr.Address)
		if tier, ok := memberTier[addr.Address]; ok {
			addr.IsMember, addr.Tier = true, tier
		}
		addr.Verdict = addressVerdict(addr.Comment, addr.OnChain)
	}

	detail.Signals = buildProposalSignals(ProposalAuditInput{
		ID:                       detail.ID,
		RenderedTitle:            detail.Title,
		ExecutorPkgPath:          detail.ExecutorPkgPath,
		Status:                   detail.Status,
		AuthorAddress:            authorAddr,
		Author:                   detail.Author,
		Code:                     detail.Code,
		ParamChanges:             detail.ParamChanges,
		Addresses:                detail.Addresses,
		Timeline:                 detail.Timeline,
		MemberTier:               memberTier,
		PriorProposalsByAuthor:   priorByAuthor,
		PriorProposalsByExecutor: priorByExecutor,
		CreationFound:            hasCreation,
	})
}
