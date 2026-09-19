package httpapi

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Auditing a GovDAO proposal.
//
// gov/dao's own render answers "what is this proposal called and who voted",
// and nothing else. It does not print the values a sys/params proposal would
// write, it does not say which transaction created it, and it cannot say
// whether the person who created it has ever done so before. A reader who
// wants to know what a ballot actually does has to leave the page.
//
// Everything in this file exists to close that gap from data the chain
// already has: the creating transaction (found exactly, by gov/dao's own
// ProposalCreated event), the script inside it, the request that script
// built, the current value of the parameter it would change, and a set of
// signals that say plainly which of those facts are reassuring and which
// are not.
//
// Two rules hold throughout:
//
//   - Everything is sourced. A signal names the evidence it was computed
//     from, because a warning a reader cannot check is worse than no
//     warning.
//   - Nothing here is a verdict. "First-time proposer" is a fact about the
//     chain, not an accusation, and the wording stays that way. The reader
//     decides; this only makes deciding possible without a terminal.

// Signal levels, in increasing order of "stop and read this".
const (
	SignalInfo  = "info"  // worth knowing, nothing is wrong
	SignalWarn  = "warn"  // unusual, or unverifiable; read the source
	SignalAlert = "alert" // the page found an inconsistency it cannot explain
)

// ProposalSignal is one line of the verification panel.
type ProposalSignal struct {
	Level string `json:"level"`
	Code  string `json:"code"`
	Title string `json:"title"`
	// Detail says what was compared against what. It is always the evidence,
	// never a restatement of Title.
	Detail string `json:"detail"`
}

// ProposalStep is one entry in a proposal's lifecycle timeline. Every step
// is anchored to a transaction: a step this page cannot point at a tx hash
// for does not get drawn.
type ProposalStep struct {
	Kind        string `json:"kind"` // created | vote | executed | attempt
	Label       string `json:"label"`
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	Caller      string `json:"caller,omitempty"`
	Success     bool   `json:"success"`
	Detail      string `json:"detail,omitempty"`
	// Effects are the GnoEvents the step's transaction emitted, other than
	// gov/dao's own bookkeeping. On an execution step these are the proof of
	// what actually changed on chain, which is the one thing no render shows.
	Effects []ProposalEffect `json:"effects,omitempty"`
}

// ProposalEffect is one emitted event, flattened for display.
type ProposalEffect struct {
	Type    string     `json:"type"`
	PkgPath string     `json:"pkg_path,omitempty"`
	Attrs   []EffectKV `json:"attrs,omitempty"`
}

// EffectKV keeps event attributes ordered; a map would reshuffle them on
// every serialization and make two loads of the same page disagree.
type EffectKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ProposalCode is the code that created the proposal, as it was submitted.
type ProposalCode struct {
	// Kind is "run" for a maketx-run script and "call" for a direct MsgCall
	// into a realm. The distinction matters to a reader: a run script is
	// arbitrary code with no template to diff against, a call is a named
	// function with named arguments.
	Kind        string   `json:"kind"`
	TxHash      string   `json:"tx_hash"`
	BlockHeight int      `json:"block_height"`
	BlockTime   string   `json:"block_time,omitempty"`
	Caller      string   `json:"caller"`
	Success     bool     `json:"success"`
	FileName    string   `json:"file_name,omitempty"`
	Source      string   `json:"source,omitempty"`
	PkgPath     string   `json:"pkg_path,omitempty"`
	Func        string   `json:"func,omitempty"`
	Args        []string `json:"args,omitempty"`
	Imports     []string `json:"imports,omitempty"`
	// Requests are the proposal-request constructors the script called, in
	// source order, with their arguments as written.
	Requests []ProposalRequestCall `json:"requests,omitempty"`
	// CastsVote is true when the creating script voted in the same
	// transaction, which is legal, common, and worth saying out loud.
	CastsVote bool `json:"casts_vote,omitempty"`
}

// ProposalRequestCall is a `x.NewSomethingRequest(...)` found in the source,
// with arguments split at top level and left exactly as written. No attempt
// is made to evaluate them: this is what the proposer typed, which is the
// thing a reader wants to check.
type ProposalRequestCall struct {
	Pkg  string   `json:"pkg"`
	Func string   `json:"func"`
	Args []string `json:"args"`
}

// ProposalParamChange is a sys/params request decoded out of the creation
// script: the key it writes, whether it sets/adds/removes, and the values.
//
// These values exist nowhere else a reader can reach. gov/dao renders a
// generic params proposal without them, so the ballot as published cannot
// be audited; decoding the creating transaction is the only way to see what
// a YES vote would actually write.
type ProposalParamChange struct {
	Key    string   `json:"key"`  // "vm:p:pkg_approvers"
	Verb   string   `json:"verb"` // set | add | remove
	Type   string   `json:"type"` // String | Strings | Int64 | Uint64 | Bool | Bytes
	Values []string `json:"values,omitempty"`
	// Current is the parameter's value on chain right now, read back over
	// ABCI. Empty when the key does not exist yet or the read failed.
	Current []string `json:"current,omitempty"`
	// Result is Current with Verb applied: what the key would hold if this
	// proposal executed against today's state. Advisory, since state can
	// change between now and execution.
	Result []string `json:"result,omitempty"`
	// CurrentKnown separates "read the chain, the key is unset" from "could
	// not read the chain", which look identical in an empty slice.
	CurrentKnown bool `json:"current_known"`
}

// ProposalAddress is a g1... literal found in the creation source, together
// with the comment that names it and what the chain says it is.
//
// This is the cheapest lie to tell in a governance script and the most
// expensive to catch by eye: three near-identical bech32 strings, one
// comment each, and a reader who recognizes the names but not the
// addresses. Resolving each one against r/sys/users turns that into a
// three-way check anyone can read.
type ProposalAddress struct {
	Address string `json:"address"`
	Comment string `json:"comment,omitempty"`
	OnChain string `json:"onchain_name,omitempty"`
	// Verdict: match | mismatch | unregistered | unlabelled
	Verdict  string `json:"verdict"`
	IsMember bool   `json:"is_member,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Line     int    `json:"line,omitempty"`
}

const (
	AddrMatch        = "match"
	AddrMismatch     = "mismatch"
	AddrUnregistered = "unregistered"
	AddrUnlabelled   = "unlabelled"
)

// --- source parsing -------------------------------------------------------
//
// Deliberately regex and brace counting rather than go/parser. A MsgRun
// script is Gno, not Go: `func main(cur realm)` and `cross(cur)` are not
// valid Go, and go/parser rejects the whole file over them, which would
// leave the one page that most needs to show code showing nothing. Failing
// softly on an odd construct and surfacing less is the correct trade here;
// failing hard on the common construct is not.

var importLineRe = regexp.MustCompile(`^\s*(?:[\w.]+\s+)?"(gno\.land/[^"]+)"`)
var singleImportRe = regexp.MustCompile(`^\s*import\s+(?:[\w.]+\s+)?"(gno\.land/[^"]+)"`)

// parseGnoImports returns the gno.land import paths of a source file, in
// source order, deduplicated.
func parseGnoImports(src string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := singleImportRe.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
		if strings.HasPrefix(trimmed, "import (") {
			inBlock = true
			continue
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				continue
			}
			if m := importLineRe.FindStringSubmatch(line); m != nil {
				add(m[1])
			}
		}
	}
	return out
}

var srcAddrRe = regexp.MustCompile(`g1[a-z0-9]{38}`)

// parseSourceAddresses pulls every bech32 address out of a source file,
// keeping the trailing line comment that names it. One entry per distinct
// address; the first occurrence wins, so the comment shown is the one next
// to the literal rather than one picked arbitrarily from a later mention.
func parseSourceAddresses(src string) []ProposalAddress {
	var out []ProposalAddress
	seen := map[string]bool{}
	for i, line := range strings.Split(src, "\n") {
		locs := srcAddrRe.FindAllString(line, -1)
		if len(locs) == 0 {
			continue
		}
		comment := ""
		if idx := strings.Index(line, "//"); idx >= 0 {
			comment = strings.TrimSpace(line[idx+2:])
		}
		for _, addr := range locs {
			if seen[addr] {
				continue
			}
			seen[addr] = true
			// A comment is only attributed when the line holds a single
			// address. `{"g1a", "g1b"} // the council` names neither of them,
			// and pinning it to both would manufacture two claims the
			// proposer never made.
			c := comment
			if len(locs) > 1 {
				c = ""
			}
			out = append(out, ProposalAddress{Address: addr, Comment: c, Line: i + 1, Verdict: AddrUnlabelled})
		}
	}
	return out
}

// requestCallRe finds proposal-request constructors: a selector call whose
// function name looks like a request builder. Matching on shape rather than
// on a fixed list keeps this working when r/sys/params grows another
// factory, which it has done twice already.
var requestCallRe = regexp.MustCompile(`\b([A-Za-z_]\w*)\.((?:New\w*Request\w*)|(?:New\w*PropRequest\w*)|(?:Propose[A-Z]\w*))\s*\(`)

// parseProposalRequests finds every proposal-request constructor call in the
// source and splits its arguments at top level, as written.
func parseProposalRequests(src string) []ProposalRequestCall {
	var out []ProposalRequestCall
	for _, m := range requestCallRe.FindAllStringSubmatchIndex(src, -1) {
		open := m[1] - 1 // index of the '(' the regex consumed
		args, ok := splitCallArgs(src, open)
		if !ok {
			continue
		}
		out = append(out, ProposalRequestCall{
			Pkg:  src[m[2]:m[3]],
			Func: src[m[4]:m[5]],
			Args: args,
		})
	}
	return out
}

// splitCallArgs takes the index of an opening parenthesis and returns the
// call's arguments split on top-level commas, each trimmed of surrounding
// whitespace and newlines. Nested parens, brackets, braces and string
// literals are skipped over, so a `[]string{"a", "b"}` argument stays one
// argument.
func splitCallArgs(src string, open int) ([]string, bool) {
	if open < 0 || open >= len(src) || src[open] != '(' {
		return nil, false
	}
	depth := 0
	start := open + 1
	var args []string
	inStr := false
	var quote byte
	for i := open; i < len(src); i++ {
		c := src[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '`', '\'':
			inStr = true
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 && c == ')' {
				args = appendArg(args, src[start:i])
				return args, true
			}
		case ',':
			if depth == 1 {
				args = appendArg(args, src[start:i])
				start = i + 1
			}
		}
	}
	return nil, false
}

func appendArg(args []string, raw string) []string {
	s := strings.TrimSpace(stripLineComments(raw))
	if s == "" {
		return args
	}
	// Collapse the newlines and tabs a multi-line literal argument carries,
	// so a composite literal reads as one value in a table cell.
	s = strings.Join(strings.Fields(s), " ")
	return append(args, s)
}

// stripLineComments drops // comments, which a multi-line composite literal
// argument carries one of per element. Collapsing such an argument to a
// single line without this splices each comment into the code that follows
// it: `[]string{ "g1aedd...", // aeddi "g1ecsu..." }` reads as though the
// second address were part of the first one's comment, which is exactly the
// kind of misreading this page exists to prevent.
//
// String-literal aware, so a `//` inside a quoted URL survives.
func stripLineComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		inStr := false
		var quote byte
		cut := -1
		for i := 0; i < len(line); i++ {
			c := line[i]
			if inStr {
				switch c {
				case '\\':
					i++
				case quote:
					inStr = false
				}
				continue
			}
			switch c {
			case '"', '`', '\'':
				inStr, quote = true, c
			case '/':
				if i+1 < len(line) && line[i+1] == '/' {
					cut = i
				}
			}
			if cut >= 0 {
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

var sysParamFuncRe = regexp.MustCompile(`^NewSysParam(String|Strings|Int64|Uint64|Bool|Bytes)PropRequest(Add|Remove)?(?:WithTitle)?$`)

var stringLitRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// decodeSysParamRequest turns a r/sys/params request constructor into the
// key/verb/values shape the page shows. Returns nil for any other request:
// a wrong decode is worse than none, so anything outside the known factory
// family is left as the raw call for a human to read.
//
// Argument order comes from r/sys/params itself:
//
//	New...PropRequest(cur, module, submodule, name, value)
//	New...PropRequest[Add|Remove]WithTitle(cur, module, submodule, name, title, value)
//
// so the realm argument is dropped, the next three build the key, a title
// argument is skipped when the factory name says there is one, and whatever
// remains is the value.
// namedParamRequest describes one r/sys/params request constructor that does
// not take its key as arguments but hard-codes it.
//
// Transcribed from examples/gno.land/r/sys/params in gnolang/gno, read on
// 2026-09-19. It is a copy of someone else's constants, so it can go stale:
// when it does, the request still shows up in the "requests" list as written
// and only the decoded key/value view is missing. That is the intended
// failure mode - no decode is fine, a wrong decode is not - so nothing here
// guesses at a constructor it does not recognize.
type namedParamRequest struct {
	key  string
	verb string
	typ  string
	// argIndex picks which argument holds the value: 0 means "the constant
	// below", n>=1 means args[n], and -1 means "every argument from 1 on",
	// for the variadic address list constructors.
	argIndex int
	constVal []string
}

var namedParamRequests = map[string][]namedParamRequest{
	// NewSetHaltRequest(cur, height int64, minVersion string) writes two keys.
	"NewSetHaltRequest": {
		{key: "node:p:halt_height", verb: "set", typ: "Int64", argIndex: 1},
		{key: "node:p:halt_min_version", verb: "set", typ: "String", argIndex: 2},
	},
	"NewSetFeeCollectorRequest":             {{key: "auth:p:fee_collector", verb: "set", typ: "String", argIndex: 1}},
	"ProposeUnlockTransferRequest":          {{key: "bank:p:restricted_denoms", verb: "set", typ: "Strings", constVal: []string{}}},
	"ProposeLockTransferRequest":            {{key: "bank:p:restricted_denoms", verb: "set", typ: "Strings", constVal: []string{"ugnot"}}},
	"ProposeAddUnrestrictedAcctsRequest":    {{key: "auth:p:unrestricted_addrs", verb: "add", typ: "Strings", argIndex: -1}},
	"ProposeRemoveUnrestrictedAcctsRequest": {{key: "auth:p:unrestricted_addrs", verb: "remove", typ: "Strings", argIndex: -1}},
	"ProposeSetRunSubmitters":               {{key: "vm:p:run_submitters", verb: "set", typ: "Strings", argIndex: 1}},
}

// decodeParamChanges returns every parameter this request would write:
// the generic sys/params factories, which carry their key in their
// arguments, plus the named constructors that hard-code one. Empty for any
// request neither family recognizes.
func decodeParamChanges(call ProposalRequestCall) []ProposalParamChange {
	if pc := decodeSysParamRequest(call); pc != nil {
		return []ProposalParamChange{*pc}
	}
	specs, ok := namedParamRequests[call.Func]
	if !ok {
		return nil
	}
	var out []ProposalParamChange
	for _, spec := range specs {
		pc := ProposalParamChange{Key: spec.key, Verb: spec.verb, Type: spec.typ}
		switch {
		case spec.argIndex == 0:
			pc.Values = spec.constVal
		case spec.argIndex == -1:
			for _, a := range call.Args[1:] {
				pc.Values = append(pc.Values, unwrapLiteral(a))
			}
		case spec.argIndex < len(call.Args):
			pc.Values = decodeParamValues(spec.typ, call.Args[spec.argIndex])
			for i := range pc.Values {
				pc.Values[i] = unwrapLiteral(pc.Values[i])
			}
		default:
			continue
		}
		out = append(out, pc)
	}
	return out
}

// unwrapLiteral strips the conversion a Gno caller has to write around a
// literal - `int64(162200)`, `address("g1...")` - so the value shown is the
// value, not its spelling. Anything that is not a conversion around a single
// literal is returned untouched.
var conversionRe = regexp.MustCompile(`^[A-Za-z_][\w.]*\(\s*(.*?)\s*\)$`)

func unwrapLiteral(s string) string {
	s = strings.TrimSpace(s)
	for {
		m := conversionRe.FindStringSubmatch(s)
		if m == nil || strings.ContainsAny(m[1], "(),") {
			return unquote(s)
		}
		s = m[1]
	}
}

func decodeSysParamRequest(call ProposalRequestCall) *ProposalParamChange {
	m := sysParamFuncRe.FindStringSubmatch(call.Func)
	if m == nil {
		return nil
	}
	hasTitle := strings.HasSuffix(call.Func, "WithTitle")
	want := 5
	if hasTitle {
		want = 6
	}
	if len(call.Args) < want {
		return nil
	}
	key := strings.Join([]string{
		unquote(call.Args[1]), unquote(call.Args[2]), unquote(call.Args[3]),
	}, ":")
	verb := "set"
	switch m[2] {
	case "Add":
		verb = "add"
	case "Remove":
		verb = "remove"
	}
	valueArg := call.Args[want-1]
	return &ProposalParamChange{
		Key:    key,
		Verb:   verb,
		Type:   m[1],
		Values: decodeParamValues(m[1], valueArg),
	}
}

// decodeParamValues renders a value argument as the list of values it
// represents. A slice literal becomes its elements; a scalar becomes one
// entry. Anything that is not a literal (a variable, a function call) is
// returned as written, so the page never silently prints a value the source
// does not contain.
func decodeParamValues(typ, arg string) []string {
	if typ == "Strings" || strings.HasPrefix(arg, "[]string{") || strings.HasPrefix(arg, "[]byte{") {
		var out []string
		for _, m := range stringLitRe.FindAllStringSubmatch(arg, -1) {
			out = append(out, unescapeGoString(m[1]))
		}
		if out == nil {
			return []string{arg}
		}
		return out
	}
	if strings.HasPrefix(arg, `"`) {
		return []string{unquote(arg)}
	}
	return []string{arg}
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`') && s[len(s)-1] == s[0] {
		return unescapeGoString(s[1 : len(s)-1])
	}
	return s
}

func unescapeGoString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	if out, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return out
	}
	return s
}

// applyParamVerb computes what the key would hold if the proposal executed
// against `current`. Advisory: the chain's value can move between now and
// execution, and the page says so next to the result.
func applyParamVerb(verb string, current, values []string) []string {
	switch verb {
	case "add":
		out := append([]string{}, current...)
		have := map[string]bool{}
		for _, c := range current {
			have[c] = true
		}
		for _, v := range values {
			if !have[v] {
				have[v] = true
				out = append(out, v)
			}
		}
		return out
	case "remove":
		drop := map[string]bool{}
		for _, v := range values {
			drop[v] = true
		}
		out := []string{}
		for _, c := range current {
			if !drop[c] {
				out = append(out, c)
			}
		}
		return out
	default:
		return append([]string{}, values...)
	}
}

var voteCallRe = regexp.MustCompile(`\b(?:MustVoteOnProposal|VoteOnProposal|MustVoteOnProposalSimple)\s*\(`)

// sourceCastsVote reports whether the creating script also voted.
func sourceCastsVote(src string) bool { return voteCallRe.MatchString(src) }

// titleLiterals returns the string literals a request constructor was given
// as its title argument, for comparison against the title gov/dao renders.
// Only the WithTitle factories have one; the rest let gov/dao generate a
// title, and comparing against a generated string proves nothing.
func titleLiterals(calls []ProposalRequestCall) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasSuffix(c.Func, "WithTitle") {
			continue
		}
		m := sysParamFuncRe.FindStringSubmatch(c.Func)
		if m == nil || len(c.Args) < 6 {
			continue
		}
		// Only a literal is comparable. A title built from a variable or a
		// concatenation cannot be checked against the rendered one from the
		// source alone, and guessing at it would manufacture a mismatch.
		// An empty literal is still a literal, and a script that passes ""
		// while the ballot renders a title is exactly the disagreement this
		// is here to catch.
		if strings.HasPrefix(strings.TrimSpace(c.Args[4]), `"`) {
			out = append(out, unquote(c.Args[4]))
		}
	}
	return out
}

// commentNameClaim returns the username a source comment claims for an
// address, or "" when it makes no claim this page is willing to check.
//
// Only two shapes count as a claim: a bare single token (`// jae`) and an
// @-prefixed one (`// @jae, T1`). Everything else is prose, and prose is
// where a naive comparison starts crying wolf: proposal 3 on mainnet
// comments an address `// aeddi - incumbent, must be kept`, which an
// exact-match check called a mismatch against a registry that says
// "aeddi". An alert panel that fires on an honest comment is worse than one
// that stays quiet, so a comment whose claim cannot be isolated is treated
// as making none.
var claimTokenRe = regexp.MustCompile(`^@?([A-Za-z0-9_.-]+)\s*$`)
var atClaimRe = regexp.MustCompile(`^@([A-Za-z0-9_.-]+)\b`)

func commentNameClaim(comment string) string {
	c := strings.TrimSpace(comment)
	if c == "" {
		return ""
	}
	if m := claimTokenRe.FindStringSubmatch(c); m != nil {
		return m[1]
	}
	if m := atClaimRe.FindStringSubmatch(c); m != nil {
		return m[1]
	}
	return ""
}

// commentMentions reports whether a comment names `name` as a whole word.
// Deliberately generous: a comment that mentions the registered username
// anywhere is consistent with the registry, whatever else it says.
func commentMentions(comment, name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(comment)
	target := strings.ToLower(name)
	// Word characters for a username as r/sys/users allows them, plus the
	// separators a comment can put next to one.
	isWordRune := func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	}
	for _, f := range strings.FieldsFunc(lower, func(r rune) bool { return !isWordRune(r) }) {
		if f == target {
			return true
		}
	}
	return false
}

// addressVerdict decides what a source comment and the registry together
// say about an address. Pure, so the rule is one testable place rather than
// three string comparisons scattered through the fetch path.
func addressVerdict(comment, onChainName string) string {
	claim := commentNameClaim(comment)
	switch {
	case comment == "":
		return AddrUnlabelled
	case onChainName == "":
		if claim == "" {
			return AddrUnlabelled
		}
		return AddrUnregistered
	case commentMentions(comment, onChainName):
		return AddrMatch
	case claim != "":
		return AddrMismatch
	default:
		// Prose that neither names the registered account nor makes an
		// isolable claim. Nothing to contradict.
		return AddrUnlabelled
	}
}

// --- signals --------------------------------------------------------------

// systemRealms are the realms a proposal executor normally lives in. A
// proposal pointing anywhere else is not wrong, but it means the code that
// would run with GovDAO's authority is code GovDAO does not maintain, and
// that is the single most consequential thing a reader can be told.
var systemRealms = []string{"gno.land/r/gov/", "gno.land/r/sys/"}

func isSystemRealm(path string) bool {
	for _, p := range systemRealms {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// ProposalAuditInput is everything buildProposalSignals needs, already
// fetched. Kept as a struct so the signal rules are a pure function of
// facts, with no network in the middle: every rule below is exercised in
// govdao_audit_test.go against a literal of this type.
type ProposalAuditInput struct {
	ID              int
	RenderedTitle   string
	ExecutorPkgPath string
	Status          string
	AuthorAddress   string
	Author          string
	Code            *ProposalCode
	ParamChanges    []ProposalParamChange
	Addresses       []ProposalAddress
	Timeline        []ProposalStep
	// MemberTier maps a member address to its tier; absence means not a member.
	MemberTier map[string]string
	// PriorProposalsByAuthor lists the IDs this author created before this one.
	PriorProposalsByAuthor []int
	// PriorProposalsByExecutor lists the IDs that used the same executor
	// package before this one.
	PriorProposalsByExecutor []int
	// CreationFound is false when no ProposalCreated event could be matched.
	CreationFound bool
}

// buildProposalSignals turns the collected facts into the verification
// panel, most severe first. Every signal states the evidence it used.
func buildProposalSignals(in ProposalAuditInput) []ProposalSignal {
	var out []ProposalSignal
	add := func(level, code, title, detail string) {
		out = append(out, ProposalSignal{Level: level, Code: code, Title: title, Detail: detail})
	}

	// Counted before the rules below because several of them read it: an
	// already-executed proposal whose parameter matches the chain is the
	// success case, and the identical comparison on a pending one is a
	// warning.
	var executed, failedAttempts int
	for _, s := range in.Timeline {
		switch {
		case s.Kind == "executed" && s.Success:
			executed++
		case !s.Success:
			failedAttempts++
		}
	}

	// How it was created.
	switch {
	case !in.CreationFound:
		add(SignalWarn, "creation-unknown", "Creating transaction not found",
			"No ProposalCreated event for this ID was returned by the indexer. The proposal may predate this indexer's history, or this indexer may not support filtering on event attributes. Nothing below that depends on the creation transaction could be checked.")
	case in.Code != nil && in.Code.Kind == "run":
		add(SignalWarn, "created-by-run-script", "Created by a `maketx run` script, not a proposal template",
			"The proposal was built by arbitrary Gno the proposer wrote and ran once, in transaction "+shortHash(in.Code.TxHash)+". There is no template to diff it against, so the source below is the entire specification of what it does. Read it.")
	case in.Code != nil && in.Code.Kind == "call":
		add(SignalInfo, "created-by-call", "Created by a direct realm call",
			"`"+in.Code.PkgPath+"."+in.Code.Func+"` in transaction "+shortHash(in.Code.TxHash)+". The arguments are indexed and shown below exactly as submitted.")
	}

	if in.Code != nil && !in.Code.Success {
		add(SignalAlert, "creation-tx-failed", "The creating transaction is marked failed",
			"gov/dao emitted ProposalCreated in transaction "+shortHash(in.Code.TxHash)+", but the indexer reports that transaction as unsuccessful. These two facts should not both be true; treat everything derived from this transaction as unreliable.")
	}

	// Who created it.
	switch {
	case in.AuthorAddress == "":
		add(SignalWarn, "author-unresolved", "The author's address could not be resolved",
			"gov/dao's render names the author as @"+in.Author+" but carries no address, and r/sys/users did not resolve that name. Membership and history below could not be checked.")
	case in.MemberTier == nil:
		// Memberstore unavailable; say nothing rather than guess.
	default:
		if tier, ok := in.MemberTier[in.AuthorAddress]; ok {
			add(SignalInfo, "proposer-is-member", "Proposer is a GovDAO member ("+tier+")",
				"Found in gov/dao's memberstore at tier "+tier+".")
		} else {
			add(SignalAlert, "proposer-not-member", "Proposer is not in the GovDAO memberstore",
				in.AuthorAddress+" does not appear in gno.land/r/gov/dao/memberstore/v0. Proposal creation is not restricted to members, so this is legal, but the proposer has no vote on their own proposal.")
		}
	}

	if in.CreationFound {
		if n := len(in.PriorProposalsByAuthor); n == 0 {
			add(SignalWarn, "first-time-proposer", "First proposal from this address",
				"No earlier ProposalCreated event on this chain names "+shortAddr(in.AuthorAddress)+" as caller. A first proposal is not a bad proposal; it only means there is no track record to weigh it against.")
		} else {
			add(SignalInfo, "repeat-proposer", fmt.Sprintf("%d earlier proposal(s) from this address", n),
				"Previously created "+joinIDs(in.PriorProposalsByAuthor)+".")
		}
	}

	if in.Code != nil && in.Code.CastsVote {
		add(SignalInfo, "self-vote-at-creation", "The creating transaction also cast a vote",
			"The script calls a vote function in the same transaction that created the proposal, so the first YES on the tally below was cast by the proposer at block "+fmtInt(in.Code.BlockHeight)+", not in a separate transaction a reader can find in the vote trail.")
	}

	// What it would run.
	if in.ExecutorPkgPath != "" {
		if !isSystemRealm(in.ExecutorPkgPath) {
			add(SignalAlert, "executor-not-system-realm", "The executor package is not a gov/ or sys/ realm",
				"`"+in.ExecutorPkgPath+"` is outside gno.land/r/gov/ and gno.land/r/sys/. Whatever that package's code does will run with GovDAO's authority if this proposal is executed.")
		}
		if in.CreationFound {
			if n := len(in.PriorProposalsByExecutor); n == 0 {
				add(SignalWarn, "new-executor-package", "First proposal to use this executor package",
					"No earlier proposal on this chain was created by a script importing `"+in.ExecutorPkgPath+"`. The package itself may be long-established; what is new is its use as a governance executor.")
			} else {
				add(SignalInfo, "known-executor-package", fmt.Sprintf("Executor package used by %d earlier proposal(s)", n),
					"`"+in.ExecutorPkgPath+"` also executed "+joinIDs(in.PriorProposalsByExecutor)+".")
			}
		}
	}

	if in.Code != nil && len(in.Code.Imports) > 0 {
		var foreign []string
		for _, imp := range in.Code.Imports {
			if !isSystemRealm(imp) && !strings.HasPrefix(imp, "gno.land/p/") {
				foreign = append(foreign, imp)
			}
		}
		if len(foreign) > 0 {
			add(SignalWarn, "unvetted-import", fmt.Sprintf("%d import(s) outside gov/, sys/ and p/", len(foreign)),
				"The creating script imports "+strings.Join(backtickAll(foreign), ", ")+". Code in an imported realm runs as part of building the proposal request; read those packages too.")
		}
	}

	// Does the ballot say what the code says.
	if in.Code != nil {
		lits := titleLiterals(in.Code.Requests)
		switch {
		case len(lits) == 0:
			// No explicit title in the source, nothing to compare.
		case containsFold(lits, in.RenderedTitle):
			add(SignalInfo, "title-matches-source", "On-chain title matches the creating script",
				"gov/dao renders \""+in.RenderedTitle+"\", and the script passed that same string as its title argument.")
		default:
			add(SignalAlert, "title-mismatch", "On-chain title does not match the creating script",
				"gov/dao renders \""+in.RenderedTitle+"\" but the script's title argument is \""+lits[0]+"\". One of the two is not describing what this proposal does.")
		}
	}

	// The named-constructor table is a transcription of someone else's
	// constants, so it can go stale without anything failing: a renamed or
	// added factory simply stops decoding, and the page quietly shows one
	// less thing. Saying so is the guard. It fires exactly when a reader is
	// looking at a proposal whose effect this page could not work out, which
	// is the only moment the staleness matters.
	if in.Code != nil {
		var undecoded []string
		for _, c := range in.Code.Requests {
			if strings.HasPrefix(c.Func, "NewVote") || c.Func == "NewVoteRequest" {
				continue // a vote request writes no parameter
			}
			if len(decodeParamChanges(c)) == 0 {
				undecoded = append(undecoded, "`"+c.Pkg+"."+c.Func+"`")
			}
		}
		if len(undecoded) > 0 {
			add(SignalWarn, "request-not-decoded", fmt.Sprintf("%d proposal request(s) this page cannot decode", len(undecoded)),
				strings.Join(undecoded, ", ")+" is not a constructor this explorer knows how to read, so what it would write is not shown below. The arguments are listed as written; read the constructor's source to see what it does with them.")
		}
	}

	if len(in.ParamChanges) > 0 {
		var keys []string
		for _, pc := range in.ParamChanges {
			keys = append(keys, "`"+pc.Key+"`")
		}
		add(SignalWarn, "params-not-rendered-on-chain", "The parameter values below are decoded from the transaction, not from the ballot",
			"gov/dao renders a sys/params proposal without the values it would write, so "+strings.Join(keys, ", ")+" cannot be audited from the proposal page on chain. They were decoded from the creating transaction's source and are shown against each parameter's current on-chain value.")
		for _, pc := range in.ParamChanges {
			if pc.Verb == "remove" && len(pc.Current) > 0 && len(pc.Result) == 0 {
				add(SignalAlert, "param-emptied", "This would leave `"+pc.Key+"` empty",
					"The key currently holds "+fmtInt(len(pc.Current))+" value(s) and this proposal removes all of them.")
			}
			if pc.Verb == "add" && pc.CurrentKnown && len(pc.Current) == 0 {
				add(SignalWarn, "param-unset", "`"+pc.Key+"` is currently unset",
					"The key holds no value on chain today, so this add creates it rather than extending an existing list.")
			}
			if pc.CurrentKnown && sameValues(pc.Current, pc.Result) {
				if executed > 0 {
					add(SignalInfo, "param-applied", "`"+pc.Key+"` holds exactly what this proposal wrote",
						"The key's current on-chain value equals the result of applying this proposal, which is what an executed proposal should look like. Nothing has overwritten it since.")
				} else {
					add(SignalWarn, "param-noop", "`"+pc.Key+"` would not change",
						"Applying this proposal to the key's current on-chain value produces the same value, and no execution transaction was found. Either something else already wrote it, or the proposal is a no-op against today's state.")
				}
			}
		}
	}

	// Do the names in the code match the chain.
	var mismatched, unregistered []string
	for _, a := range in.Addresses {
		switch a.Verdict {
		case AddrMismatch:
			mismatched = append(mismatched, shortAddr(a.Address)+" commented `// "+a.Comment+"`, registered as @"+a.OnChain)
		case AddrUnregistered:
			if a.Comment != "" {
				unregistered = append(unregistered, shortAddr(a.Address)+" commented `// "+a.Comment+"`")
			}
		}
	}
	if len(mismatched) > 0 {
		add(SignalAlert, "address-comment-mismatch", fmt.Sprintf("%d address comment(s) disagree with r/sys/users", len(mismatched)),
			strings.Join(mismatched, "; ")+". The comment is the proposer's claim; the registry is the chain's.")
	}
	if len(unregistered) > 0 {
		add(SignalWarn, "address-comment-unverifiable", fmt.Sprintf("%d address comment(s) cannot be verified", len(unregistered)),
			strings.Join(unregistered, "; ")+". These addresses have no username registered in r/sys/users, so the comment naming them is unverifiable from chain data.")
	}
	if len(in.Addresses) > 0 && len(mismatched) == 0 && len(unregistered) == 0 {
		named := 0
		for _, a := range in.Addresses {
			if a.Verdict == AddrMatch {
				named++
			}
		}
		if named > 0 {
			add(SignalInfo, "address-comments-verified", fmt.Sprintf("%d address comment(s) match r/sys/users", named),
				"Every commented address in the source resolves to the username the comment claims.")
		}
	}

	// Where it stands.
	if failedAttempts > 0 {
		add(SignalWarn, "failed-attempts", fmt.Sprintf("%d failed transaction(s) name this proposal", failedAttempts),
			"Shown in the timeline. A failed vote or execution usually means the caller was not eligible or the proposal was not in a state that allows it.")
	}
	switch {
	case executed > 0:
		add(SignalInfo, "executed", "Executed on chain",
			"The timeline's execution step lists the events the execution emitted, which is the proof of what actually changed.")
	case strings.EqualFold(in.Status, "ACCEPTED"):
		add(SignalWarn, "accepted-not-executed", "Accepted but no execution transaction found",
			"gov/dao reports this proposal as accepted, but no successful ExecuteProposal call naming it was found. Acceptance does not apply a change; someone still has to execute it.")
	default:
		add(SignalInfo, "not-executed", "No execution transaction yet",
			"Nothing on chain has run this proposal's executor. Until it does, the values below are a request, not state.")
	}

	sort.SliceStable(out, func(i, j int) bool {
		return signalRank(out[i].Level) < signalRank(out[j].Level)
	})
	return out
}

// sameValues compares two value lists elementwise. Order matters: a params
// list is stored as written, so a reordering is a real change even when the
// set is identical.
func sameValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func signalRank(level string) int {
	switch level {
	case SignalAlert:
		return 0
	case SignalWarn:
		return 1
	default:
		return 2
	}
}

func containsFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}

func backtickAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = "`" + s + "`"
	}
	return out
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "#" + strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

func fmtInt(n int) string { return strconv.Itoa(n) }

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:8] + "..."
}

func shortAddr(a string) string {
	if len(a) <= 14 {
		return a
	}
	return a[:8] + "..." + a[len(a)-4:]
}
