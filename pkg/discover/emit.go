package discover

import (
	"fmt"
	"math"
	"strings"
)

// The emitters: one per event kind, each a pure function from the facts the
// SQL produced to the three layers a reader sees.
//
// Pure on purpose. An emitter that reached for a database would be a thing you
// can only test with one, and the golden files in testdata/ are the acceptance
// test for this file: they were written by a person obeying the grounding rule,
// so they are the standard, and every emitter here has to reproduce its own
// fixture exactly. That is also why the templates are spelled out rather than
// assembled from fragments, because a fragment reads fine and a sentence is
// what gets posted.
//
// Layer 1 is exempt from grounding by construction (it is the indexed fact in
// its own terms) and so may name things absent from Facts, such as the full
// package path and the signer's address. Layers 2 and 3 may not, and Ground
// enforces it. Every emitter below is checked against Ground in the tests, not
// merely against its fixture.

// PackageDeployed is a successful MsgAddPackage, deduplicated to the first
// submission of a path.
type PackageDeployed struct {
	Path       string // gno.land/r/moul/hello
	Creator    string // the signing address
	ActorLabel string // "@moul", or empty when the address has no registered name
	Height     int64
	IsRealm    bool
	NumFiles   int
	FirstEver  bool // this creator's first ever package
}

// Emit returns the facts and the three layers for a package.deployed event.
func (in PackageDeployed) Emit() (Facts, Layers) {
	ns, name := splitPath(in.Path)
	facts := Facts{
		"package_name": name,
		"namespace":    ns,
		"actor_label":  in.ActorLabel,
		"is_realm":     in.IsRealm,
		"num_files":    in.NumFiles,
		"first_ever":   in.FirstEver,
	}

	// "app" for a realm, "library" for a package. The distinction is the whole
	// reason the kind is package.* and carries is_realm rather than being two
	// kinds: 152 of 346 deployed things on mainnet are pure packages, and a
	// vocabulary that only names realms would have to lie about them.
	thing := "library"
	if in.IsRealm {
		thing = "app"
	}
	who, whose := in.ActorLabel, in.ActorLabel
	if who == "" {
		who, whose = "Somebody", "its author"
	}

	return facts, Layers{
		What: Layer{fmt.Sprintf("MsgAddPackage published %s, %s, by %s, at block %d.",
			in.Path, plural(in.NumFiles, "file"), in.Creator, in.Height)},
		Means: Layer{fmt.Sprintf("%s put a new %s on the chain, called %s.", who, thing, name)},
		Matters: Layer{fmt.Sprintf(
			"Anyone can look at it or use it now, and nobody can change it except %s. It is %s of code.",
			whose, plural(in.NumFiles, "file"))},
	}
}

// DeployerFirst is an address publishing for the first time on a chain.
type DeployerFirst struct {
	Address      string
	ActorLabel   string // empty when the address has no registered name
	PackageName  string // the package that marked the debut
	IsRealm      bool
	NetworkLabel string // "mainnet", for layer 1
	// DistinctDeployers is how many addresses have ever published here. Layer 3
	// may only mention it because it is in the facts; that is the rule.
	DistinctDeployers int
}

// Emit returns the facts and the three layers for a deployer.first event.
func (in DeployerFirst) Emit() (Facts, Layers) {
	facts := Facts{
		"actor_address":      in.Address,
		"actor_label":        in.ActorLabel,
		"package_name":       in.PackageName,
		"first_ever":         true,
		"distinct_deployers": in.DistinctDeployers,
	}

	thing := "library"
	if in.IsRealm {
		thing = "app"
	}
	who := in.ActorLabel
	if who == "" {
		who = "Somebody"
	}

	return facts, Layers{
		What: Layer{fmt.Sprintf("%s has no earlier successful MsgAddPackage on %s.",
			in.Address, in.NetworkLabel)},
		Means: Layer{fmt.Sprintf("%s published on the chain for the first time, an %s called %s.",
			who, thing, in.PackageName)},
		Matters: Layer{fmt.Sprintf("A new builder arrived. %s people have ever published on this chain.",
			spellSmall(in.DistinctDeployers))},
	}
}

// ChainSpike is a day on which far more addresses appeared than usual.
type ChainSpike struct {
	Day            string // 2026-09-18
	NetworkLabel   string
	NewAddresses   int
	BaselineMedian int
	// Ratio is NewAddresses over the baseline. Carried rather than recomputed so
	// the number in the sentence is the number the detector decided on.
	Ratio float64
}

// Emit returns the facts and the three layers for a chain.spike event.
func (in ChainSpike) Emit() (Facts, Layers) {
	facts := Facts{
		"day":             in.Day,
		"new_addresses":   in.NewAddresses,
		"baseline_median": in.BaselineMedian,
		"excess":          in.NewAddresses - in.BaselineMedian,
		"ratio":           in.Ratio,
	}

	return facts, Layers{
		What: Layer{fmt.Sprintf(
			"%d addresses appeared on %s for the first time on %s, against a 7-day median of %d.",
			in.NewAddresses, in.NetworkLabel, in.Day, in.BaselineMedian)},
		Means: Layer{fmt.Sprintf("%d new wallets showed up on the chain in one day.", in.NewAddresses)},
		Matters: Layer{fmt.Sprintf(
			"A normal day is about %d, so that is %d times normal. Something brought a lot of people at once.",
			in.BaselineMedian, int(math.Round(in.Ratio)))},
	}
}

// PackageEnabled is MsgEnablePackage making a parked package live.
type PackageEnabled struct {
	Path        string
	Height      int64
	WaitBlocks  int
	WaitSeconds float64
	IsRealm     bool
}

// Emit returns the facts and the three layers for a package.enabled event.
func (in PackageEnabled) Emit() (Facts, Layers) {
	_, name := splitPath(in.Path)
	facts := Facts{
		"package_name": name,
		"wait_blocks":  in.WaitBlocks,
		"wait_seconds": in.WaitSeconds,
	}

	thing := "library"
	if in.IsRealm {
		thing = "app"
	}

	return facts, Layers{
		What: Layer{fmt.Sprintf("MsgEnablePackage enabled %s at block %d, %s after submission.",
			in.Path, in.Height, plural(in.WaitBlocks, "block"))},
		Means:   Layer{fmt.Sprintf("An %s called %s was switched on.", thing, name)},
		Matters: Layer{"Anyone can use it now. " + waitedPhrase(in.WaitSeconds)},
	}
}

// waitedPhrase says how long a package sat parked, and refuses to say it in the
// singular.
//
// "second" is in the grounding gate's number vocabulary as the ordinal meaning
// 2, so "It waited 1 second" asserts the figure 2, which is in no fact, and G1
// rejects it. The plural "seconds" is not in that vocabulary, which is why the
// approved fixture at 13 seconds passes and a one-second wait does not. That is
// the gate being right rather than fussy: a sentence should not contain a
// number word that means something other than what it says.
//
// Under two seconds there is no figure worth quoting anyway, so the sentence
// drops the number rather than working around the check.
func waitedPhrase(seconds float64) string {
	if n := int(math.Round(seconds)); n >= 2 {
		return fmt.Sprintf("It waited %d seconds to be checked and approved.", n)
	}
	return "It was checked and approved almost immediately."
}

// splitPath turns gno.land/r/moul/hello into ("moul", "hello").
//
// The namespace is the segment after the r/ or p/, not the first segment of the
// path: every path here begins gno.land, so taking the first would make every
// namespace on the chain "gno.land".
func splitPath(path string) (namespace, name string) {
	parts := strings.Split(strings.TrimPrefix(path, "gno.land/"), "/")
	if len(parts) < 3 {
		// Not the usual shape. Return the last segment as the name and no
		// namespace rather than inventing one.
		if len(parts) > 0 {
			return "", parts[len(parts)-1]
		}
		return "", path
	}
	return parts[1], parts[len(parts)-1]
}

// plural renders a count with its noun: "3 files", "1 file".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// numberWords are the spellings the grounding gate recognises as figures, so a
// spelled number is checked against the facts exactly as a digit is. Spelling
// small counts out is what makes layer 3 read like a sentence rather than a
// readout, and the gate is what keeps it honest.
var numberWords = []string{
	"Zero", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight",
	"Nine", "Ten", "Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen",
	"Sixteen", "Seventeen", "Eighteen", "Nineteen",
}

// spellSmall spells a count the gate knows how to check, and falls back to
// digits above its vocabulary rather than inventing a word it cannot verify.
func spellSmall(n int) string {
	if n >= 0 && n < len(numberWords) {
		return numberWords[n]
	}
	return fmt.Sprintf("%d", n)
}

// The kinds below were written after the four above, and they are a step down
// in confidence, stated here rather than in a commit message.
//
// Layer 2 is the spec's own line from the vocabulary table (§5.1), so the
// sentence a reader meets first is decided, not invented. Layer 1 is mechanical:
// it is the indexed fact in its own terms. **Layer 3 is drafted**, because
// §10.1 works only four kinds and these are not among them, so the "why it
// matters" line follows the pattern of the four that were approved rather than
// a rule. Every one satisfies the grounding gate, which is a floor and not a
// substitute for someone reading them.
//
// Three v1 kinds are deliberately absent: proposal.opened, proposal.closed and
// transfer.large. The first two cannot keep layer 2 inside 90 characters while
// carrying a proposal title, which is a real editorial decision about what to
// drop; the third needs a "large" threshold, and a cutoff that decides what a
// reader is shown is a judgement rather than a constant to guess at.

// PackageFirstCall is the first successful call a package ever received, which
// is a different event from its deployment and usually much later.
type PackageFirstCall struct {
	Path   string
	Ref    string // r/gnoswap/router, the short form a reader recognises
	Caller string
	Height int64
	// External is true when the caller is not the package's creator. A first
	// call by anyone is news on a young chain; a first call by a stranger is a
	// different kind of news, and the flag lets the filter separate them
	// without doubling the vocabulary.
	External bool
}

// Emit returns the facts and the three layers for a package.first_call event.
func (in PackageFirstCall) Emit() (Facts, Layers) {
	_, name := splitPath(in.Path)
	facts := Facts{
		"package_name": name,
		"package_ref":  in.Ref,
		"external":     in.External,
		// first_ever is what licenses the word "first" in layer 2. G3 refuses
		// the claim without it, which is the gate working: "for the first time"
		// is a rank, and a rank has to come from somewhere.
		"first_ever": true,
	}
	matters := "A deployed package that nobody has called is just stored code. Somebody has started running this one."
	if in.External {
		matters = "A deployed package that nobody has called is just stored code. Somebody other than its author has started running this one."
	}
	return facts, Layers{
		What:    Layer{fmt.Sprintf("The first successful call to %s was at block %d, by %s.", in.Path, in.Height, in.Caller)},
		Means:   Layer{fmt.Sprintf("Somebody used %s for the first time.", in.Ref)},
		Matters: Layer{matters},
	}
}

// PackageSpike is a package taking far more calls in a day than it usually does.
type PackageSpike struct {
	Ref            string
	Day            string
	Calls          int
	Callers        int
	BaselineMedian int
	Ratio          float64
}

// Emit returns the facts and the three layers for a package.spike event.
func (in PackageSpike) Emit() (Facts, Layers) {
	facts := Facts{
		"package_ref":     in.Ref,
		"day":             in.Day,
		"calls":           in.Calls,
		"callers":         in.Callers,
		"baseline_median": in.BaselineMedian,
		"ratio":           in.Ratio,
	}
	return facts, Layers{
		What: Layer{fmt.Sprintf("%s took %d calls on %s from %d callers, against a 7-day median of %d.",
			in.Ref, in.Calls, in.Day, in.Callers, in.BaselineMedian)},
		Means: Layer{fmt.Sprintf("%s took %dx its usual daily calls, from %d different people.",
			in.Ref, int(math.Round(in.Ratio)), in.Callers)},
		Matters: Layer{fmt.Sprintf(
			"A normal day there is about %d calls. %d people in one day is not one script running twice.",
			in.BaselineMedian, in.Callers)},
	}
}

// NamespaceRegistered is an address claiming a name through r/sys/namereg.
type NamespaceRegistered struct {
	Name    string
	Address string
	Height  int64
}

// Emit returns the facts and the three layers for a namespace.registered event.
func (in NamespaceRegistered) Emit() (Facts, Layers) {
	facts := Facts{
		"name":        in.Name,
		"actor_label": "@" + in.Name,
		"address":     in.Address,
	}
	return facts, Layers{
		What:  Layer{fmt.Sprintf("r/sys/namereg recorded the name %s for %s at block %d.", in.Name, in.Address, in.Height)},
		Means: Layer{fmt.Sprintf("@%s claimed their name on chain.", in.Name)},
		Matters: Layer{fmt.Sprintf(
			"Packages published by that address can live under %s from now on, and the name is theirs alone.", in.Name)},
	}
}

// ValidatorRegistered is an address registering as a candidate validator.
type ValidatorRegistered struct {
	Moniker string
	Address string
	Height  int64
}

// Emit returns the facts and the three layers for a validator.registered event.
func (in ValidatorRegistered) Emit() (Facts, Layers) {
	facts := Facts{
		"moniker": in.Moniker,
		"address": in.Address,
	}
	return facts, Layers{
		What:  Layer{fmt.Sprintf("valoper registered %s for %s at block %d.", in.Moniker, in.Address, in.Height)},
		Means: Layer{fmt.Sprintf("%s registered as a validator.", in.Moniker)},
		Matters: Layer{
			"Registering is not the same as validating: it puts them forward, and the chain decides separately whether they produce blocks."},
	}
}

// PackageRejected is an approver refusing a parked package.
type PackageRejected struct {
	Path   string
	Ref    string
	Actor  string // the approver's address
	Height int64
}

// Emit returns the facts and the three layers for a package.rejected event.
func (in PackageRejected) Emit() (Facts, Layers) {
	_, name := splitPath(in.Path)
	facts := Facts{
		"package_name": name,
		"package_ref":  in.Ref,
	}
	return facts, Layers{
		What:  Layer{fmt.Sprintf("MsgRejectPackage refused %s at block %d, by %s.", in.Path, in.Height, in.Actor)},
		Means: Layer{fmt.Sprintf("A package approver turned down %s.", in.Ref)},
		Matters: Layer{
			"The code stayed parked rather than going live. Submitting a changed version under the same path is allowed."},
	}
}

// ProposalOpened is a GovDAO proposal being created.
type ProposalOpened struct {
	ID         int
	Title      string
	ActorLabel string // "@aeddi", or empty
	Actor      string // the address, for layer 1
	Height     int64
}

// Emit returns the facts and the three layers for a proposal.opened event.
func (in ProposalOpened) Emit() (Facts, Layers) {
	who := in.ActorLabel
	if who == "" {
		who = "Somebody"
	}
	facts := Facts{
		"proposal_id": in.ID,
		"title":       in.Title,
		"actor_label": in.ActorLabel,
	}

	// The title goes in layer 2 when it fits and is dropped when it does not.
	//
	// Real mainnet titles run 20 to 53 characters (measured 2026-09-25 over the
	// five proposals there), so with an ordinary handle the full line is about
	// 82 and fits. A long handle and a long title together do not, and the
	// budget is not negotiable: it is the card's headline row. Dropping the
	// title is the right thing to drop, because layers 1 and 3 both still carry
	// it, and a truncated title is a title that says something else.
	means := fmt.Sprintf("%s opened proposal %d, %s.", who, in.ID, in.Title)
	if len(means) > MaxMeans {
		means = fmt.Sprintf("%s opened proposal %d.", who, in.ID)
	}

	return facts, Layers{
		What:  Layer{fmt.Sprintf("ProposalCreated recorded proposal %d by %s at block %d.", in.ID, in.Actor, in.Height)},
		Means: Layer{means},
		Matters: Layer{fmt.Sprintf(
			"GovDAO members can vote on it. Its subject is %s.", in.Title)},
	}
}

// ProposalClosed is a proposal reaching a verdict, with execution folded in.
//
// One kind rather than two, because on mainnet proposal 7 was created at
// 10:25:07 and executed at 10:25:23: sixteen seconds apart is one event to a
// reader, and two rows about it is the failure this page exists to avoid.
type ProposalClosed struct {
	ID       int
	Title    string
	Outcome  string // "passed" or "rejected"
	YesPct   int
	Executed bool
	Height   int64
}

// Emit returns the facts and the three layers for a proposal.closed event.
func (in ProposalClosed) Emit() (Facts, Layers) {
	facts := Facts{
		"proposal_id": in.ID,
		"title":       in.Title,
		"outcome":     in.Outcome,
		"yes_pct":     in.YesPct,
		"executed":    in.Executed,
	}

	tail := "."
	if in.Executed {
		tail = ", and was executed."
	}
	means := fmt.Sprintf("Proposal %d %s with %d%% yes%s", in.ID, in.Outcome, in.YesPct, tail)

	matters := fmt.Sprintf("The vote is settled at %d%% yes. Its subject was %s.", in.YesPct, in.Title)
	if in.Executed {
		matters = fmt.Sprintf("The change is already in effect, not merely agreed. Its subject was %s.", in.Title)
	}

	return facts, Layers{
		What: Layer{fmt.Sprintf("Proposal %d closed %s at block %d, with %d%% yes.",
			in.ID, in.Outcome, in.Height, in.YesPct)},
		Means:   Layer{means},
		Matters: Layer{matters},
	}
}

// TransferLargeThresholdGNOT is the cutoff for showing a bank transfer.
//
// Not a guess: from 1,005 sampled BankMsgSend amounts (design §5.3), 100,000
// GNOT is the 99th-percentile-and-up band, 0.7% of sends, which over mainnet's
// 3,856 sends is roughly 27 events in nine days. Three a day is a rate a person
// can read; the p90 of 10,000 GNOT would be ten times that and stop being news.
const TransferLargeThresholdGNOT = 100_000

// TransferLarge is a bank transfer above the threshold.
type TransferLarge struct {
	GNOT   int64
	From   string
	To     string
	Height int64
}

// Emit returns the facts and the three layers for a transfer.large event.
func (in TransferLarge) Emit() (Facts, Layers) {
	facts := Facts{
		"gnot":           in.GNOT,
		"threshold_gnot": TransferLargeThresholdGNOT,
		// parties licenses the word "two". A BankMsgSend has exactly one sender
		// and one recipient, so the count is a fact rather than a flourish, and
		// without it the gate rejects the sentence for asserting a figure.
		"parties": 2,
	}
	return facts, Layers{
		What: Layer{fmt.Sprintf("BankMsgSend moved %d GNOT from %s to %s at block %d.",
			in.GNOT, in.From, in.To, in.Height)},
		Means: Layer{fmt.Sprintf("%d GNOT moved between two accounts.", in.GNOT)},
		Matters: Layer{fmt.Sprintf(
			"Transfers are listed here from %d GNOT upward, which is why this one appears. Neither end is a realm.",
			TransferLargeThresholdGNOT)},
	}
}
