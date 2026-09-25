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
