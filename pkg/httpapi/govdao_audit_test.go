package httpapi

import (
	"reflect"
	"strings"
	"testing"
)

const propSixSource = `package main

import (
	"gno.land/r/gov/dao"
	"gno.land/r/sys/params"
)

func main(cur realm) {
	r := params.NewSysParamStringsPropRequestAddWithTitle(
		cross(cur), "vm", "p", "pkg_approvers",
		"Add human package approvers alongside the gpao oracle",
		[]string{
			"g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", // aeddi
			"g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6", // jae
			"g1manfred47kzduec920z88wfr64ylksmdcedlf5", // moul
		},
	)
	pid := dao.MustCreateProposal(cross(cur), r)
	dao.MustVoteOnProposal(cross(cur), dao.NewVoteRequest(dao.YesVote, pid))
}
`

const propSevenSource = `package main

import (
	"gno.land/r/gov/dao"
	"gno.land/r/sys/params"
)

func main(cur realm) {
	r := params.NewSetHaltRequest(cross(cur), int64(162200), "")
	pid := dao.MustCreateProposal(cross(cur), r)
	dao.MustVoteOnProposalSimple(cross(cur), int64(pid), "YES")
	// No ExecuteProposal: aeddi alone is 3/9 = 33%. A 2nd T1 must co-vote.
}
`

func TestParseGnoImports(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"block form", propSixSource, []string{"gno.land/r/gov/dao", "gno.land/r/sys/params"}},
		{"single line", "package main\nimport \"gno.land/r/sys/params\"\n", []string{"gno.land/r/sys/params"}},
		{"aliased", "import (\n\tprms \"gno.land/r/sys/params\"\n)", []string{"gno.land/r/sys/params"}},
		{"stdlib ignored", "import (\n\t\"strconv\"\n\t\"gno.land/p/demo/avl\"\n)", []string{"gno.land/p/demo/avl"}},
		{"deduped", "import (\n\t\"gno.land/r/gov/dao\"\n\t\"gno.land/r/gov/dao\"\n)", []string{"gno.land/r/gov/dao"}},
		{"none", "package main\n\nfunc main() {}", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGnoImports(tt.src); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseGnoImports() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSourceAddresses(t *testing.T) {
	got := parseSourceAddresses(propSixSource)
	want := []ProposalAddress{
		{Address: "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", Comment: "aeddi", Line: 13, Verdict: AddrUnlabelled},
		{Address: "g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6", Comment: "jae", Line: 14, Verdict: AddrUnlabelled},
		{Address: "g1manfred47kzduec920z88wfr64ylksmdcedlf5", Comment: "moul", Line: 15, Verdict: AddrUnlabelled},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSourceAddresses() = %+v, want %+v", got, want)
	}

	// A comment next to two addresses names neither of them.
	two := parseSourceAddresses("\t[]string{\"g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m\", \"g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6\"} // the council\n")
	for _, a := range two {
		if a.Comment != "" {
			t.Errorf("address %s got comment %q from a two-address line; want none", a.Address, a.Comment)
		}
	}
}

func TestSplitCallArgs(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"flat", `f(a, b, c)`, []string{"a", "b", "c"}},
		{"nested call", `f(cross(cur), "vm", g(1, 2))`, []string{"cross(cur)", `"vm"`, "g(1, 2)"}},
		{"slice literal stays one arg", `f(a, []string{"x", "y"})`, []string{"a", `[]string{"x", "y"}`}},
		{"comma inside string", `f("a,b", c)`, []string{`"a,b"`, "c"}},
		{"empty", `f()`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := splitCallArgs(tt.src, strings.Index(tt.src, "("))
			if !ok {
				t.Fatalf("splitCallArgs() not ok")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitCallArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
	if _, ok := splitCallArgs("f(a, b", 1); ok {
		t.Error("splitCallArgs() on an unterminated call: want not ok")
	}
}

// A multi-line composite literal carries one // comment per element.
// Collapsing it to a single line without stripping them splices each comment
// into the element that follows, which is the exact misreading this page
// exists to prevent.
func TestSplitCallArgsStripsComments(t *testing.T) {
	calls := parseProposalRequests(propSixSource)
	if len(calls) == 0 {
		t.Fatal("no request calls parsed")
	}
	last := calls[0].Args[len(calls[0].Args)-1]
	if strings.Contains(last, "//") {
		t.Errorf("value argument still carries a comment: %q", last)
	}
	for _, want := range []string{"g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", "g1manfred47kzduec920z88wfr64ylksmdcedlf5"} {
		if !strings.Contains(last, want) {
			t.Errorf("value argument lost %s: %q", want, last)
		}
	}
}

func TestParseProposalRequests(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantFunc []string
	}{
		{"sys params add", propSixSource, []string{"NewSysParamStringsPropRequestAddWithTitle", "NewVoteRequest"}},
		{"named halt", propSevenSource, []string{"NewSetHaltRequest"}},
		{"propose prefix", `params.ProposeUnlockTransferRequest(cross(cur))`, []string{"ProposeUnlockTransferRequest"}},
		{"not a request", `dao.MustCreateProposal(cross(cur), r)`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, c := range parseProposalRequests(tt.src) {
				got = append(got, c.Func)
			}
			if !reflect.DeepEqual(got, tt.wantFunc) {
				t.Errorf("funcs = %v, want %v", got, tt.wantFunc)
			}
		})
	}
}

func TestDecodeParamChanges(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []ProposalParamChange
	}{
		{
			name: "generic strings add with title",
			src:  propSixSource,
			want: []ProposalParamChange{{
				Key: "vm:p:pkg_approvers", Verb: "add", Type: "Strings",
				Values: []string{
					"g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m",
					"g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6",
					"g1manfred47kzduec920z88wfr64ylksmdcedlf5",
				},
			}},
		},
		{
			// One constructor, two keys. Showing only the first would hide
			// the version gate a halt proposal sets.
			name: "named halt writes two keys",
			src:  propSevenSource,
			want: []ProposalParamChange{
				{Key: "node:p:halt_height", Verb: "set", Type: "Int64", Values: []string{"162200"}},
				{Key: "node:p:halt_min_version", Verb: "set", Type: "String", Values: []string{""}},
			},
		},
		{
			name: "named lock transfer",
			src:  `params.ProposeLockTransferRequest(cross(cur))`,
			want: []ProposalParamChange{{Key: "bank:p:restricted_denoms", Verb: "set", Type: "Strings", Values: []string{"ugnot"}}},
		},
		{
			name: "named remove is a remove",
			src:  `params.ProposeRemoveUnrestrictedAcctsRequest(cross(cur), address("g1manfred47kzduec920z88wfr64ylksmdcedlf5"))`,
			want: []ProposalParamChange{{Key: "auth:p:unrestricted_addrs", Verb: "remove", Type: "Strings", Values: []string{"g1manfred47kzduec920z88wfr64ylksmdcedlf5"}}},
		},
		{
			name: "generic scalar set",
			src:  `params.NewSysParamInt64PropRequest(cross(cur), "vm", "p", "gas_price", int64(42))`,
			want: []ProposalParamChange{{Key: "vm:p:gas_price", Verb: "set", Type: "Int64", Values: []string{"int64(42)"}}},
		},
		{
			// An unrecognized constructor decodes to nothing rather than to
			// a guess; the raw request is still shown to the reader.
			name: "unknown constructor",
			src:  `other.NewSomethingRequest(cross(cur), "x")`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []ProposalParamChange
			for _, c := range parseProposalRequests(tt.src) {
				got = append(got, decodeParamChanges(c)...)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeParamChanges() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyParamVerb(t *testing.T) {
	tests := []struct {
		name            string
		verb            string
		current, values []string
		want            []string
	}{
		{"add appends", "add", []string{"a"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"add dedupes", "add", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"remove drops", "remove", []string{"a", "b", "c"}, []string{"b"}, []string{"a", "c"}},
		{"remove all empties", "remove", []string{"a"}, []string{"a"}, []string{}},
		{"set replaces", "set", []string{"a", "b"}, []string{"c"}, []string{"c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyParamVerb(tt.verb, tt.current, tt.values); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("applyParamVerb() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSourceCastsVote(t *testing.T) {
	if !sourceCastsVote(propSixSource) {
		t.Error("proposal 6's script votes via MustVoteOnProposal; want true")
	}
	if !sourceCastsVote(propSevenSource) {
		t.Error("proposal 7's script votes via MustVoteOnProposalSimple; want true")
	}
	if sourceCastsVote("dao.MustCreateProposal(cross(cur), r)") {
		t.Error("a create-only script does not vote; want false")
	}
}

func TestUnwrapLiteral(t *testing.T) {
	tests := []struct{ in, want string }{
		{`int64(162200)`, "162200"},
		{`address("g1manfred47kzduec920z88wfr64ylksmdcedlf5")`, "g1manfred47kzduec920z88wfr64ylksmdcedlf5"},
		{`""`, ""},
		{`plain`, "plain"},
		{`f(a, b)`, "f(a, b)"},
	}
	for _, tt := range tests {
		if got := unwrapLiteral(tt.in); got != tt.want {
			t.Errorf("unwrapLiteral(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// signalCodes is the assertion the rule tests use: the set of codes fired,
// not their wording, so rewording a warning does not break a test.
func signalCodes(sigs []ProposalSignal) map[string]string {
	out := map[string]string{}
	for _, s := range sigs {
		out[s.Code] = s.Level
	}
	return out
}

func TestBuildProposalSignals(t *testing.T) {
	runCode := &ProposalCode{Kind: "run", TxHash: "abcdef0123456789", BlockHeight: 100, Caller: "g1author", Success: true, Source: propSixSource}
	members := map[string]string{"g1author": "T1"}

	tests := []struct {
		name string
		in   ProposalAuditInput
		// want maps a code to the level it must be reported at.
		want map[string]string
		// absent lists codes that must not fire.
		absent []string
	}{
		{
			name: "run script by a first-time non-member",
			in: ProposalAuditInput{
				ID: 1, Code: runCode, CreationFound: true,
				AuthorAddress: "g1stranger", MemberTier: members,
				ExecutorPkgPath: "gno.land/r/sys/params",
			},
			want: map[string]string{
				"created-by-run-script": SignalWarn,
				"proposer-not-member":   SignalAlert,
				"first-time-proposer":   SignalWarn,
				"new-executor-package":  SignalWarn,
				"not-executed":          SignalInfo,
			},
			absent: []string{"proposer-is-member", "repeat-proposer"},
		},
		{
			name: "known member with history",
			in: ProposalAuditInput{
				ID: 6, Code: runCode, CreationFound: true,
				AuthorAddress: "g1author", MemberTier: members,
				ExecutorPkgPath:          "gno.land/r/sys/params",
				PriorProposalsByAuthor:   []int{4},
				PriorProposalsByExecutor: []int{3, 4, 5},
			},
			want: map[string]string{
				"proposer-is-member":     SignalInfo,
				"repeat-proposer":        SignalInfo,
				"known-executor-package": SignalInfo,
			},
			absent: []string{"proposer-not-member", "first-time-proposer", "new-executor-package"},
		},
		{
			// The executor is the code that runs with GovDAO's authority.
			// One outside gov/ and sys/ is the single most consequential
			// thing a reader can be told about a proposal.
			name: "executor outside the system realms",
			in: ProposalAuditInput{
				ID: 9, Code: runCode, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				ExecutorPkgPath: "gno.land/r/someone/exploit",
			},
			want:   map[string]string{"executor-not-system-realm": SignalAlert},
			absent: []string{},
		},
		{
			name: "title in the script disagrees with the ballot",
			in: ProposalAuditInput{
				ID: 6, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				RenderedTitle: "Something else entirely",
				Code: &ProposalCode{
					Kind: "run", Success: true, Source: propSixSource,
					Requests: parseProposalRequests(propSixSource),
				},
			},
			want:   map[string]string{"title-mismatch": SignalAlert},
			absent: []string{"title-matches-source"},
		},
		{
			name: "title in the script matches the ballot",
			in: ProposalAuditInput{
				ID: 6, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				RenderedTitle: "Add human package approvers alongside the gpao oracle",
				Code: &ProposalCode{
					Kind: "run", Success: true, Source: propSixSource,
					Requests: parseProposalRequests(propSixSource),
				},
			},
			want:   map[string]string{"title-matches-source": SignalInfo},
			absent: []string{"title-mismatch"},
		},
		{
			// The cheapest lie in a governance script: a comment naming an
			// address as someone it is not.
			name: "an address comment disagrees with the registry",
			in: ProposalAuditInput{
				ID: 6, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Addresses: []ProposalAddress{
					{Address: "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", Comment: "jae", OnChain: "aeddi", Verdict: AddrMismatch},
				},
			},
			want:   map[string]string{"address-comment-mismatch": SignalAlert},
			absent: []string{"address-comments-verified"},
		},
		{
			name: "address comments all check out",
			in: ProposalAuditInput{
				ID: 6, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Addresses: []ProposalAddress{
					{Address: "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", Comment: "aeddi", OnChain: "aeddi", Verdict: AddrMatch},
				},
			},
			want:   map[string]string{"address-comments-verified": SignalInfo},
			absent: []string{"address-comment-mismatch", "address-comment-unverifiable"},
		},
		{
			name: "accepted but never executed",
			in: ProposalAuditInput{
				ID: 5, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Status: "ACCEPTED",
			},
			want:   map[string]string{"accepted-not-executed": SignalWarn},
			absent: []string{"executed", "not-executed"},
		},
		{
			name: "executed, and the parameter matches",
			in: ProposalAuditInput{
				ID: 7, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Status:   "ACCEPTED",
				Timeline: []ProposalStep{{Kind: "executed", Success: true, TxHash: "x"}},
				ParamChanges: []ProposalParamChange{{
					Key: "node:p:halt_height", Verb: "set", Values: []string{"162200"},
					Current: []string{"162200"}, Result: []string{"162200"}, CurrentKnown: true,
				}},
			},
			want:   map[string]string{"executed": SignalInfo, "param-applied": SignalInfo},
			absent: []string{"param-noop", "accepted-not-executed"},
		},
		{
			// The same comparison on a proposal nobody executed is a
			// warning, not a reassurance.
			name: "pending, and the parameter already matches",
			in: ProposalAuditInput{
				ID: 8, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				ParamChanges: []ProposalParamChange{{
					Key: "vm:p:x", Verb: "set", Values: []string{"a"},
					Current: []string{"a"}, Result: []string{"a"}, CurrentKnown: true,
				}},
			},
			want:   map[string]string{"param-noop": SignalWarn},
			absent: []string{"param-applied"},
		},
		{
			name: "a removal that empties the key",
			in: ProposalAuditInput{
				ID: 9, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				ParamChanges: []ProposalParamChange{{
					Key: "vm:p:run_submitters", Verb: "remove", Values: []string{"a"},
					Current: []string{"a"}, Result: []string{}, CurrentKnown: true,
				}},
			},
			want: map[string]string{"param-emptied": SignalAlert},
		},
		{
			name: "failed vote attempts are surfaced",
			in: ProposalAuditInput{
				ID: 5, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Timeline: []ProposalStep{{Kind: "attempt", Success: false, TxHash: "x"}},
			},
			want: map[string]string{"failed-attempts": SignalWarn},
		},
		{
			name:   "no creation event found",
			in:     ProposalAuditInput{ID: 0, CreationFound: false, AuthorAddress: "g1author", MemberTier: members},
			want:   map[string]string{"creation-unknown": SignalWarn},
			absent: []string{"first-time-proposer", "repeat-proposer"},
		},
		{
			// An empty title literal against a rendered title is still a
			// disagreement, and used to be silently skipped.
			name: "the script passes an empty title",
			in: ProposalAuditInput{
				ID: 9, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				RenderedTitle: "Something the ballot shows",
				Code: &ProposalCode{
					Kind: "run", Success: true,
					Requests: parseProposalRequests(`params.NewSysParamStringsPropRequestAddWithTitle(cross(cur), "vm", "p", "k", "", []string{"a"})`),
				},
			},
			want: map[string]string{"title-mismatch": SignalAlert},
		},
		{
			name: "an unvetted import",
			in: ProposalAuditInput{
				ID: 9, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Code: &ProposalCode{Kind: "run", Success: true, Imports: []string{"gno.land/r/gov/dao", "gno.land/r/someone/helper"}},
			},
			want: map[string]string{"unvetted-import": SignalWarn},
		},
		{
			name: "a failed creating transaction contradicts its own event",
			in: ProposalAuditInput{
				ID: 9, CreationFound: true, AuthorAddress: "g1author", MemberTier: members,
				Code: &ProposalCode{Kind: "run", Success: false, TxHash: "deadbeefcafe1234"},
			},
			want: map[string]string{"creation-tx-failed": SignalAlert},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := signalCodes(buildProposalSignals(tt.in))
			for code, level := range tt.want {
				if got[code] != level {
					t.Errorf("signal %q = %q, want %q (got %v)", code, got[code], level, got)
				}
			}
			for _, code := range tt.absent {
				if _, ok := got[code]; ok {
					t.Errorf("signal %q fired but should not have", code)
				}
			}
		})
	}
}

// Alerts have to come first: the panel is read top-down and the reader who
// stops after two lines must have seen the worst two.
func TestBuildProposalSignalsOrdersBySeverity(t *testing.T) {
	sigs := buildProposalSignals(ProposalAuditInput{
		ID: 1, CreationFound: true, AuthorAddress: "g1stranger",
		MemberTier:      map[string]string{"g1author": "T1"},
		ExecutorPkgPath: "gno.land/r/someone/exploit",
		Code:            &ProposalCode{Kind: "run", Success: true, TxHash: "abc"},
	})
	last := -1
	for _, s := range sigs {
		r := signalRank(s.Level)
		if r < last {
			t.Fatalf("signal %q (%s) came after a more severe one", s.Code, s.Level)
		}
		last = r
	}
	if len(sigs) == 0 || sigs[0].Level != SignalAlert {
		t.Fatalf("first signal = %+v, want an alert", sigs[0])
	}
}

func TestUsernameFromUserData(t *testing.T) {
	const registered = `(&(struct{("g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m" .uverse.address),("aeddi" string),(false bool)} gno.land/r/sys/users.UserData) *gno.land/r/sys/users.UserData)`
	if got := usernameFromUserData(registered, "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m"); got != "aeddi" {
		t.Errorf("usernameFromUserData() = %q, want %q", got, "aeddi")
	}
	if got := usernameFromUserData("(nil *gno.land/r/sys/users.UserData)", "g1nobody"); got != "" {
		t.Errorf("usernameFromUserData() on an unregistered address = %q, want empty", got)
	}
}

// A parameter key is decoded from a script anyone can submit, so it is
// attacker-controlled text on its way into an ABCI query path.
func TestParamKeyRe(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"vm:p:pkg_approvers", true},
		{"node:p:halt_height", true},
		{"bank:p:restricted_denoms", true},
		{"vm:p", false},
		{"vm:p:a:b", false},
		{"../../secret", false},
		{"vm:p:a b", false},
		{"VM:P:X", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := paramKeyRe.MatchString(tt.key); got != tt.want {
			t.Errorf("paramKeyRe.MatchString(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

// The address check is the one rule that accuses somebody, so it has to be
// conservative. Proposal 3 on mainnet is the case that forced this: its
// comment reads `// aeddi - incumbent, must be kept` and an exact-match
// comparison called that a mismatch against a registry saying "aeddi".
func TestAddressVerdict(t *testing.T) {
	tests := []struct {
		name    string
		comment string
		onChain string
		want    string
	}{
		{"bare name matches", "aeddi", "aeddi", AddrMatch},
		{"at-prefixed matches", "@aeddi", "aeddi", AddrMatch},
		{"case insensitive", "Aeddi", "aeddi", AddrMatch},
		{"prose mentioning the name", "aeddi - incumbent, must be kept", "aeddi", AddrMatch},
		{"prose mentioning it late", "keep this one, it is moul", "moul", AddrMatch},
		{"bare name contradicted", "jae", "aeddi", AddrMismatch},
		{"at-prefixed contradicted", "@jae, T1", "aeddi", AddrMismatch},
		{"prose with no isolable claim is not an accusation", "the treasury multisig", "aeddi", AddrUnlabelled},
		{"no comment", "", "aeddi", AddrUnlabelled},
		{"claim with no registration", "jae", "", AddrUnregistered},
		{"prose with no registration and no claim", "one of the founders", "", AddrUnlabelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addressVerdict(tt.comment, tt.onChain); got != tt.want {
				t.Errorf("addressVerdict(%q, %q) = %q, want %q", tt.comment, tt.onChain, got, tt.want)
			}
		})
	}
}

func TestCommentNameClaim(t *testing.T) {
	tests := []struct{ comment, want string }{
		{"jae", "jae"},
		{"@jae", "jae"},
		{"  @jae, T1 member  ", "jae"},
		{"aeddi - incumbent, must be kept", ""},
		{"the treasury", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := commentNameClaim(tt.comment); got != tt.want {
			t.Errorf("commentNameClaim(%q) = %q, want %q", tt.comment, got, tt.want)
		}
	}
}

// TestDecodeParamChangesSurvivesAZeroArgRequest pins the guard in
// decodeParamChanges' variadic branch.
//
// The source scan is a regex over a MsgRun script, so it matches a
// constructor name written anywhere, including inside a comment and
// including with no arguments at all. That yields a call whose Args is
// empty, and the variadic branch used to slice it from index 1, which
// panics and takes the whole /govdao/<id> response down with it.
func TestDecodeParamChangesSurvivesAZeroArgRequest(t *testing.T) {
	for _, fn := range []string{
		"ProposeAddUnrestrictedAcctsRequest",
		"ProposeRemoveUnrestrictedAcctsRequest",
	} {
		t.Run(fn, func(t *testing.T) {
			got := decodeParamChanges(ProposalRequestCall{Pkg: "pp", Func: fn})
			if len(got) != 0 {
				t.Fatalf("decodeParamChanges(no args) = %#v, want none: a call with\n"+
					"no arguments describes no parameter change", got)
			}
		})
	}
}

// TestParseProposalRequestsYieldsEmptyArgs is the other half: it shows the
// input above is one the parser really produces, rather than a literal only
// a test would build.
func TestParseProposalRequestsYieldsEmptyArgs(t *testing.T) {
	src := "package main\n\n" +
		"func main(cur realm) {\n" +
		"\t// we used to call pp.ProposeAddUnrestrictedAcctsRequest()\n" +
		"}\n"
	calls := parseProposalRequests(src)
	if len(calls) != 1 {
		t.Fatalf("parseProposalRequests found %d calls, want 1", len(calls))
	}
	if len(calls[0].Args) != 0 {
		t.Fatalf("Args = %#v, want empty", calls[0].Args)
	}
	decodeParamChanges(calls[0]) // must not panic
}
