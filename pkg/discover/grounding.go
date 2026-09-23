// Package discover holds the grounding rule for Discover's explanatory layers.
//
// Every Discover event is three layers. Layer 1 is the indexed fact in its own
// terms and cannot be wrong unless the indexer is. Layers 2 and 3 say what it
// means and why it matters, and they can be wrong invisibly: a fluent sentence
// that asserts something nothing in the database supports reads exactly like a
// correct one, and it is the sentence that ends up in a post.
//
// The rule: layers 2 and 3 may not introduce a single fact absent from layer 1.
// They rephrase and contextualise; they never add.
//
// That is a sentence until you can run it. It can be run here because layer 1
// is not prose: it is Facts, a closed set of key/value pairs the SQL produced.
// So "did this sentence invent something" becomes "is every number, name and
// claim in it present in that map", which is mechanical.
//
// Seven checks. Five are rules and live here; two are regression nets and live
// in the tests (G6 golden files, and G7's budgets asserted per kind). The
// checks exist before any emitter does, on purpose: retrofitting a gate onto
// twelve kinds of prose already written is much more expensive than writing the
// gate first, and the gate is also what would make a model safe to put in this
// path later. Templates pass G1 to G4 by construction, which is the argument
// for templates; the checks run on them anyway, because a template with a wrong
// field reference emits an ungrounded number just as happily as a model does.
package discover

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Facts is layer 1, structured. Values are numbers, strings or bools.
type Facts map[string]any

// Layer is one of the three statements about an event.
type Layer struct {
	Text string `json:"text"`
}

// Layers is the three-layer body of an event.
type Layers struct {
	What    Layer `json:"what"`
	Means   Layer `json:"means"`
	Matters Layer `json:"matters"`
}

// Budgets. Layer 2 is one sentence inside 90 characters because that is #262's
// headline row, and a card that truncates lies by omission. The cap is also
// what keeps layer 2 honest: the moment a template wants a second clause, the
// clause belongs in layer 3.
const (
	MaxWhat        = 140
	MaxMeans       = 90
	MaxMatters     = 200
	MaxExplanation = 290
)

// Headline and Explanation are the flat fields #262 reads, and they are
// derived here rather than set anywhere.
//
// The field names are frozen jointly with that spec. What matters more is that
// nothing may set them independently of the layers they project: a card and a
// feed reader should not have to know what a layer is, and an auditor should
// never find a headline that says something the structure it came from does
// not. One function, no second writer.
func (l Layers) Headline() string { return l.Means.Text }

func (l Layers) Explanation() string { return l.Means.Text + "\n" + l.Matters.Text }

// Violation is one failed check.
type Violation struct {
	Check string `json:"check"` // G1..G5, G7
	Field string `json:"field"` // what, means, matters
	Msg   string `json:"msg"`
}

func (v Violation) Error() string { return v.Check + " (" + v.Field + "): " + v.Msg }

// Ground runs every mechanical check over one event's layers.
//
// headwords is the glossary's term list, for G5. Passing it in rather than
// reaching for the package global keeps this callable from a test with a
// fixture vocabulary.
func Ground(facts Facts, l Layers, headwords []string) []Violation {
	var out []Violation
	out = append(out, checkBudgets(l)...)
	for _, f := range []struct {
		name string
		text string
	}{{"means", l.Means.Text}, {"matters", l.Matters.Text}} {
		out = append(out, checkNumbers(facts, f.name, f.text)...)
		out = append(out, checkEntities(facts, f.name, f.text)...)
		out = append(out, checkSuperlatives(facts, f.name, f.text)...)
		out = append(out, checkForwardLooking(f.name, f.text)...)
	}
	out = append(out, checkJargon(headwords, l.Means.Text)...)
	return out
}

// --- G7: budgets ------------------------------------------------------------

var sentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)

func checkBudgets(l Layers) []Violation {
	var out []Violation
	for _, b := range []struct {
		field string
		text  string
		max   int
	}{
		{"what", l.What.Text, MaxWhat},
		{"means", l.Means.Text, MaxMeans},
		{"matters", l.Matters.Text, MaxMatters},
	} {
		if b.text == "" {
			out = append(out, Violation{"G7", b.field, "empty"})
			continue
		}
		if n := len([]rune(b.text)); n > b.max {
			out = append(out, Violation{"G7", b.field, fmt.Sprintf("%d characters, the budget is %d", n, b.max)})
		}
	}
	if n := len(sentenceEnd.FindAllString(l.Means.Text, -1)); n > 1 {
		out = append(out, Violation{"G7", "means", fmt.Sprintf("%d sentences, layer 2 is exactly one", n)})
	}
	// Asserted separately rather than inferred from the two above, because 90
	// plus a newline plus 200 is 291 and the card's body budget is 290. A pair
	// of layers that each just fit can still overflow the thing they project
	// into.
	if n := len([]rune(l.Explanation())); n > MaxExplanation {
		out = append(out, Violation{"G7", "explanation", fmt.Sprintf(
			"the projection is %d characters, the budget is %d", n, MaxExplanation)})
	}
	return out
}

// --- G1: numeric closure ----------------------------------------------------

// Every number in layers 2 and 3 has to be a number the facts contain, possibly
// rendered through a declared conversion. Nothing else. This is the check that
// catches a template reading the wrong field, and a model inventing a figure.

var numberToken = regexp.MustCompile(`\d[\d,\x{202f}\x{00a0} ]*(?:\.\d+)?`)

var smallWords = map[string]float64{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	"thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
	"seventeen": 17, "eighteen": 18, "nineteen": 19,
	"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5, "sixth": 6,
	"seventh": 7, "eighth": 8, "ninth": 9, "tenth": 10,
}

func checkNumbers(facts Facts, field, text string) []Violation {
	allowed := groundedNumbers(facts)
	var out []Violation

	for _, tok := range numberToken.FindAllString(text, -1) {
		v, ok := parseNumber(tok)
		if !ok {
			continue
		}
		if !nearAny(v, allowed) {
			out = append(out, Violation{"G1", field, fmt.Sprintf(
				"the number %s is in the text and in no fact: %s", strings.TrimSpace(tok), factList(facts))})
		}
	}
	// Number words and ordinals count too: "Ten people have published" asserts a
	// figure exactly as much as "10 people" does, and a denylist that only knew
	// digits would wave it through.
	for _, w := range regexp.MustCompile(`[A-Za-z]+`).FindAllString(strings.ToLower(text), -1) {
		v, isWord := smallWords[w]
		if !isWord {
			continue
		}
		// "first" and "second" are also claim words, and G3 owns that reading:
		// "for the first time" states a rank, not a count, so a licensed
		// superlative is not also required to be a number in the facts. Every
		// other word here is a figure and has to be grounded like a digit.
		// Reading this the other way round, which the first draft did, meant
		// "seven people have published" passed with no seven anywhere, because
		// "seven" is not a superlative and so was treated as licensed.
		if _, isClaim := superlatives[w]; isClaim && licensedSuperlative(facts, w) {
			continue
		}
		if !nearAny(v, allowed) {
			out = append(out, Violation{"G1", field, fmt.Sprintf(
				"%q states a figure that is in no fact: %s", w, factList(facts))})
		}
	}
	return out
}

// groundedNumbers is every value a sentence may legitimately print, derived
// from the facts through the declared conversions and nothing else.
func groundedNumbers(facts Facts) []float64 {
	var out []float64
	add := func(v float64) { out = append(out, v) }

	blockSeconds, hasBlockSeconds := numeric(facts["block_seconds"])
	for k, raw := range facts {
		v, ok := numeric(raw)
		if !ok {
			continue
		}
		add(v)
		// Rounding to 0 or 1 decimals: "13 times normal" for a ratio of 13.1,
		// "13 seconds" for 13.2. Printing a rounder number than the fact is a
		// rendering choice; printing a different one is not.
		add(math.Round(v))
		add(math.Round(v*10) / 10)

		key := strings.ToLower(k)
		switch {
		case strings.Contains(key, "ugnot"):
			// ugnot -> GNOT, the only currency conversion.
			add(v / 1e6)
			add(math.Round(v / 1e6))
			add(math.Round(v/1e5) / 10)
		case strings.Contains(key, "seconds"):
			add(v / 60)
			add(math.Round(v / 60))
			add(v / 3600)
			add(math.Round(v / 3600))
			add(v / 86400)
			add(math.Round(v / 86400))
		case strings.Contains(key, "blocks") && hasBlockSeconds:
			// blocks -> seconds only through a fact. A hardcoded block rate is
			// exactly the kind of number that is true until the chain retunes.
			add(v * blockSeconds)
			add(math.Round(v * blockSeconds))
			add(math.Round(v*blockSeconds/10) / 10)
		}
	}
	// A year in a date, and the obvious "one" of "one day", are grounded by the
	// text they sit in rather than by a fact. Keeping them out of the check
	// rather than out of the vocabulary would mean no template could ever say
	// "in one day", which is how the spec's own examples read.
	add(1)
	return out
}

// nearAny compares in floating point with a tolerance, because every path into
// this set goes through a division.
func nearAny(v float64, allowed []float64) bool {
	for _, a := range allowed {
		if math.Abs(a-v) < 1e-6 {
			return true
		}
	}
	return false
}

func parseNumber(tok string) (float64, bool) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ',', ' ', ' ', ' ':
			return -1
		}
		return r
	}, tok)
	v, err := strconv.ParseFloat(strings.TrimSuffix(cleaned, "."), 64)
	return v, err == nil
}

func numeric(raw any) (float64, bool) {
	switch v := raw.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func factList(facts Facts) string {
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, facts[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// --- G2: entity closure -----------------------------------------------------

// Anything shaped like a name has to be a name the facts carry. This is what
// stops a template picking up a realm path or a handle from the wrong row,
// which is the failure that produces a confident sentence about the wrong
// project.

var entityShapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a package path", regexp.MustCompile(`gno\.land/[^\s,.)]+`)},
	{"an address", regexp.MustCompile(`g1[a-z0-9]{8,}`)},
	{"a handle", regexp.MustCompile(`@[A-Za-z0-9_-]+`)},
	// A run of two or more capitalised words is a proper noun that the chain
	// did not supply. Single capitals are left alone: they are sentence starts.
	{"a proper noun", regexp.MustCompile(`\b[A-Z][a-z]+(?: [A-Z][a-z]+)+`)},
}

func checkEntities(facts Facts, field, text string) []Violation {
	var out []Violation
	values := factStrings(facts)
	for _, shape := range entityShapes {
		for _, tok := range shape.re.FindAllString(text, -1) {
			if !mentionedIn(values, tok) {
				out = append(out, Violation{"G2", field, fmt.Sprintf(
					"%s, %q, is in the text and in no fact", shape.name, tok)})
			}
		}
	}
	return out
}

func factStrings(facts Facts) []string {
	out := make([]string, 0, len(facts))
	for _, v := range facts {
		out = append(out, fmt.Sprintf("%v", v))
	}
	return out
}

func mentionedIn(values []string, tok string) bool {
	for _, v := range values {
		if v == tok || strings.Contains(v, tok) || strings.Contains(tok, v) && v != "" {
			return true
		}
	}
	return false
}

// --- G3: the superlative ban ------------------------------------------------

// Claim words no fact set can ground on its own. This is the check that catches
// "the first lending protocol on gno" when layer 1 only knows that a package
// path was deployed, and that sentence is the one that reaches an audience.
//
// Each word is permitted only when a named fact licenses it. A word with no
// licence never passes, at any confidence.
var superlatives = map[string][]string{
	"first":   {"first_ever"},
	"most":    {"rank_"},
	"largest": {"rank_"},
	"biggest": {"rank_"},
	"fastest": {"rank_"},
	// "ever" is licensed by an all-time count, which is what makes "ten people
	// have ever published" a reading of distinct_deployers rather than a claim
	// on top of it. The spec's denylist says nothing licenses it and the spec's
	// own worked example then uses it, correctly; this is that example's rule
	// written down.
	"ever": {"first_ever", "distinct_", "_all_time"},
	"only": nil, "never": nil, "unprecedented": nil, "revolutionary": nil,
	"leading": nil, "popular": nil, "major": nil, "huge": nil,
}

func checkSuperlatives(facts Facts, field, text string) []Violation {
	var out []Violation
	for _, w := range regexp.MustCompile(`[A-Za-z]+`).FindAllString(strings.ToLower(text), -1) {
		if _, banned := superlatives[w]; !banned {
			continue
		}
		if !licensedSuperlative(facts, w) {
			out = append(out, Violation{"G3", field, fmt.Sprintf(
				"%q is a claim no fact licenses; %s", w, licenceHint(w))})
		}
	}
	return out
}

func licensedSuperlative(facts Facts, word string) bool {
	licences, banned := superlatives[word]
	if !banned {
		return true
	}
	for _, want := range licences {
		for k, v := range facts {
			if !strings.Contains(strings.ToLower(k), strings.Trim(want, "_")) {
				continue
			}
			if b, isBool := v.(bool); isBool {
				if b {
					return true
				}
				continue
			}
			return true
		}
	}
	return false
}

func licenceHint(word string) string {
	licences := superlatives[word]
	if len(licences) == 0 {
		return "nothing licenses it, so it never passes"
	}
	return "it needs one of: " + strings.Join(licences, ", ")
}

// --- G4: no forward-looking claims ------------------------------------------

// The chain records what happened. It never records why, or what next. A
// sentence that says what somebody plans to do with what they deployed is
// inventing a motive, and a motive is the part a reader remembers.
var forwardLooking = regexp.MustCompile(`(?i)\b(will|plans to|aims to|intends|intend to|expects|expect to|is building|are building|going to|upcoming|soon)\b`)

func checkForwardLooking(field, text string) []Violation {
	var out []Violation
	for _, tok := range forwardLooking.FindAllString(text, -1) {
		out = append(out, Violation{"G4", field, fmt.Sprintf(
			"%q looks forward; the chain records what happened, never what is next", tok)})
	}
	return out
}

// --- G5: no jargon in layer 2 -----------------------------------------------

// The mechanical definition of "in a way that is easy to understand": layer 2's
// words and the glossary's headwords do not intersect. Layer 1 may use jargon
// freely, it is the technical statement; layer 3 may use a headword and the
// renderer attaches its gloss on first use. Layer 2 is the one line a reader
// skimming a card will read, so it gets no jargon at all.
func checkJargon(headwords []string, means string) []Violation {
	var out []Violation
	lower := " " + strings.ToLower(strings.Join(strings.Fields(means), " ")) + " "
	for _, hw := range headwords {
		h := strings.ToLower(hw)
		// Whole words and whole phrases only: "called" is not "call", and
		// "storage deposit" has to match as the pair it is.
		if strings.Contains(lower, " "+h+" ") || strings.Contains(lower, " "+h+", ") ||
			strings.Contains(lower, " "+h+". ") || strings.Contains(lower, " "+h+"s ") {
			out = append(out, Violation{"G5", "means", fmt.Sprintf(
				"%q is glossary jargon; layer 2 is the line that may not need a gloss", hw)})
		}
	}
	return out
}
