// Package glossary parses docs/glossary.md, the one place this product defines
// the words it uses, and serves it to anything that needs a gloss.
//
// The whole point is that there is one copy. The same bytes are the document a
// human reads in the repo, the JSON at GET /api/glossary, and the tooltip text
// in the product. A generator, or a Go map beside the markdown, would be two
// copies, and two copies of a definition means one of them is the stale one a
// reader is looking at.
//
// The rules the file states about itself are enforced here rather than trusted,
// because a glossary is prose and prose rots quietly: a term that defines
// itself, a cross-reference to a word nobody defined, a "gloss" that grew into
// a paragraph. Each of those still renders fine and still reads as authority.
//
// One rule is deliberately not enforced: present tense. There is no honest
// mechanical test for it, and a check that approximates it would fail on good
// entries and pass bad ones, which is worse than a rule the reviewer keeps.
package glossary

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxSentences is the budget per gloss.
//
// The spec this came from said two. Its own table then broke that in four
// entries, every one of them for a good reason: the third sentence of
// "namespace" is the `g1...` case, which is the single most confusing thing a
// newcomer meets on this site, and dropping it to satisfy the count would make
// the glossary worse at the job it exists for. Three, enforced, beats two,
// aspired to.
const MaxSentences = 3

// Entry is one definition.
type Entry struct {
	Gloss string `json:"gloss"`
	// Also lists the other headwords this gloss leans on, in the order they
	// appear. A renderer attaches their glosses on first use; a feed, which has
	// no hover, inlines them as parentheticals.
	Also []string `json:"also,omitempty"`
}

// Glossary is the parsed file.
type Glossary struct {
	// Version is the file's own date line, not a build stamp. It travels in
	// every Discover response so a consumer that drafted text against one set
	// of definitions can tell when they changed underneath it.
	Version string           `json:"version"`
	Terms   map[string]Entry `json:"terms"`
	// Order is the table's order, so a rendering does not have to re-sort and
	// disagree with the document.
	Order []string `json:"order"`
}

var (
	versionRe = regexp.MustCompile(`(?m)^Version:\s*(\d{4}-\d{2}-\d{2})\s*$`)
	// Bold spans are the only cross-references the parser sees. Everything else
	// in a gloss is ordinary English by construction, which is what makes "the
	// roots are ordinary English" a property rather than a hope.
	refRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	// Inline code is stripped before sentences are counted: `g1...` ends in
	// three periods and would otherwise read as three sentences.
	codeRe = regexp.MustCompile("`[^`]*`")
	// A sentence ends at .!? followed by a space or the end of the gloss.
	sentenceRe = regexp.MustCompile(`[.!?](\s|$)`)
)

// Parse reads the markdown and validates every rule the file claims for itself.
func Parse(raw []byte) (*Glossary, error) {
	text := string(raw)

	vm := versionRe.FindStringSubmatch(text)
	if vm == nil {
		return nil, fmt.Errorf("no `Version: YYYY-MM-DD` line: every consumer stamps responses with it")
	}
	if _, err := time.Parse("2006-01-02", vm[1]); err != nil {
		return nil, fmt.Errorf("version %q is not a date: %w", vm[1], err)
	}

	g := &Glossary{Version: vm[1], Terms: map[string]Entry{}}
	inTable := false
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		// The `|---|---|` rule starts the table, which means the header above it
		// is identified by position rather than by matching its text. Matching
		// the word "term" would make renaming a column heading silently turn it
		// into a definition.
		if isSeparatorRow(line) {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !isPipeRow(line) {
			inTable = false // prose after the table ends it
			continue
		}
		term, gloss, err := tableRow(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if _, dup := g.Terms[term]; dup {
			return nil, fmt.Errorf("line %d: %q is defined twice", i+1, term)
		}
		if n := sentences(gloss); n == 0 || n > MaxSentences {
			return nil, fmt.Errorf("line %d: %q has %d sentences, the budget is 1 to %d", i+1, term, n, MaxSentences)
		}
		g.Terms[term] = Entry{Gloss: gloss, Also: refs(gloss)}
		g.Order = append(g.Order, term)
	}
	if len(g.Order) == 0 {
		return nil, fmt.Errorf("no terms: the table is missing or its rows are not two columns")
	}

	if err := g.checkClosure(); err != nil {
		return nil, err
	}
	return g, g.checkAcyclic()
}

func isPipeRow(line string) bool {
	return strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") && len(line) > 1
}

func isSeparatorRow(line string) bool {
	if !isPipeRow(line) {
		return false
	}
	return strings.Trim(line, "|-: ") == "" && strings.Contains(line, "-")
}

// tableRow pulls a `| term | gloss |` row apart.
//
// A row inside the table that is not exactly two columns is an error rather
// than a row to skip. Skipping was the first shape of this and it is the wrong
// one: one accidental third column drops that term out of the glossary, the
// parse still succeeds, and the word is simply undefined from then on, which is
// the silent failure this package exists to prevent.
func tableRow(line string) (term, gloss string, err error) {
	cells := strings.Split(strings.Trim(line, "|"), "|")
	if len(cells) != 2 {
		return "", "", fmt.Errorf("%d columns, want exactly 2: %s", len(cells), line)
	}
	term = strings.TrimSpace(cells[0])
	gloss = strings.TrimSpace(cells[1])
	if term == "" || gloss == "" {
		return "", "", fmt.Errorf("a term and a gloss are both required: %s", line)
	}
	return term, gloss, nil
}

func sentences(gloss string) int {
	return len(sentenceRe.FindAllString(codeRe.ReplaceAllString(gloss, " "), -1))
}

func refs(gloss string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range refRe.FindAllStringSubmatch(gloss, -1) {
		r := strings.TrimSpace(m[1])
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// checkClosure: nothing may point at a word this table does not define, and
// nothing may point at itself. A self-referential gloss is the purest form of
// the failure this package exists to catch, and it reads as perfectly fluent.
func (g *Glossary) checkClosure() error {
	for _, term := range g.Order {
		for _, ref := range g.Terms[term].Also {
			if ref == term {
				return fmt.Errorf("%q defines itself", term)
			}
			if _, ok := g.Terms[ref]; !ok {
				return fmt.Errorf("%q leans on %q, which this table does not define", term, ref)
			}
		}
	}
	return nil
}

// checkAcyclic proves the definitions bottom out. Without it two entries can
// each be written in terms of the other and a reader following the references
// never reaches a word they already knew.
func (g *Glossary) checkAcyclic() error {
	const (
		open = 1
		done = 2
	)
	state := map[string]int{}
	var path []string
	var visit func(string) error
	visit = func(term string) error {
		switch state[term] {
		case done:
			return nil
		case open:
			return fmt.Errorf("the definitions form a cycle: %s -> %s", strings.Join(path, " -> "), term)
		}
		state[term] = open
		path = append(path, term)
		for _, ref := range g.Terms[term].Also {
			if err := visit(ref); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[term] = done
		return nil
	}
	terms := append([]string(nil), g.Order...)
	sort.Strings(terms) // deterministic error message whichever cycle member is hit first
	for _, term := range terms {
		if err := visit(term); err != nil {
			return err
		}
	}
	return nil
}

// Default is the process-wide glossary, loaded once from the embedded file.
//
// A package-level value rather than a field on every consumer: the document is
// immutable, identical for every request and every network, and threading it
// through would buy nothing but argument noise. MustLoad is called from the
// root package's init, so it is set before anything can serve a request.
var Default *Glossary

// MustLoad parses and installs the glossary, or stops the program.
//
// Fatal rather than degraded, for the same reason the registry is: a glossary
// that silently failed to parse means the product goes back to showing jargon
// with no gloss, which is invisible in testing and is exactly the complaint the
// file exists to answer.
func MustLoad(raw []byte) {
	g, err := Parse(raw)
	if err != nil {
		panic("glossary: docs/glossary.md: " + err.Error())
	}
	Default = g
}

// Get returns the loaded glossary, or nil when nothing loaded it.
func Get() *Glossary { return Default }
