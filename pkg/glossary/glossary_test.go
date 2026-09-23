package glossary

import (
	"os"
	"strings"
	"testing"
)

// The real file is the unit under test. Fixtures prove the parser works;
// only this proves the shipped glossary is valid, and the shipped glossary is
// the thing a reader sees.
func realGlossary(t *testing.T) *Glossary {
	t.Helper()
	raw, err := os.ReadFile("../../docs/glossary.md")
	if err != nil {
		t.Fatalf("read docs/glossary.md: %v", err)
	}
	g, err := Parse(raw)
	if err != nil {
		t.Fatalf("docs/glossary.md does not satisfy its own rules: %v", err)
	}
	return g
}

func TestShippedGlossaryParsesAndHoldsItsRules(t *testing.T) {
	g := realGlossary(t)

	if len(g.Order) != len(g.Terms) {
		t.Fatalf("order has %d entries, terms has %d", len(g.Order), len(g.Terms))
	}
	if g.Version == "" {
		t.Fatal("no version")
	}

	// The terms the event vocabulary and #262's fallback tiles both name. A
	// glossary missing one of these is missing the reason it was written.
	for _, want := range []string{
		"realm", "package", "namespace", "MsgAddPackage", "inert", "GPAO", "gas",
		"storage deposit", "validator", "proposal", "transaction", "block",
		"address", "call", "unique callers", "mainnet", "parked", "render", "directory",
	} {
		if _, ok := g.Terms[want]; !ok {
			t.Errorf("missing term %q", want)
		}
	}

	// Two absences the spec argues for explicitly. They are as load-bearing as
	// the entries: a glossary that defines words the product never says is one
	// nobody trusts.
	for _, absent := range []string{"deploy", "token"} {
		if _, ok := g.Terms[absent]; ok {
			t.Errorf("%q is defined, and the spec argues it should not be", absent)
		}
	}

	// parked and inert are one state under two words, and the table has to say
	// so rather than leave a reader to guess.
	if also := g.Terms["parked"].Also; len(also) != 1 || also[0] != "inert" {
		t.Errorf("parked.Also = %v, want [inert]", also)
	}
	if len(g.Terms["inert"].Also) != 0 {
		t.Errorf("inert leans on %v; it is a root and should lean on nothing", g.Terms["inert"].Also)
	}
}

func TestParseRejectsWhatItMustReject(t *testing.T) {
	const head = "Version: 2026-09-23\n\n| term | plain-language gloss |\n|---|---|\n"
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a term defined twice",
			body: "| block | A batch. |\n| block | Something else. |\n",
			want: "defined twice",
		},
		{
			name: "a cross-reference to a word nobody defines",
			body: "| block | A batch of **transactions**. |\n",
			want: "which this table does not define",
		},
		{
			name: "a gloss that defines itself",
			body: "| block | A **block** is a batch. |\n",
			want: "defines itself",
		},
		{
			name: "two entries defined in terms of each other",
			body: "| block | A batch of **call** results. |\n| call | Something inside a **block**. |\n",
			want: "cycle",
		},
		{
			name: "a gloss that grew into a paragraph",
			body: "| block | One. Two. Three. Four. |\n",
			want: "the budget is 1 to 3",
		},
		{
			name: "a table with no rows",
			body: "",
			want: "no terms",
		},
		{
			name: "one row with an extra column, among good ones",
			body: "| block | A batch. |\n| call | Using a program. | added later |\n",
			want: "3 columns, want exactly 2",
		},
		{
			name: "a row with a term and no gloss",
			body: "| block |  |\n",
			want: "a term and a gloss are both required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(head + tt.body))
			if err == nil {
				t.Fatalf("parsed, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestVersionIsRequiredAndMustBeADate(t *testing.T) {
	const row = "| block | A batch. |\n"
	for _, tt := range []struct{ name, head, want string }{
		{"missing", "", "no `Version: YYYY-MM-DD` line"},
		{"not a date", "Version: soon\n", "no `Version: YYYY-MM-DD` line"},
		{"impossible date", "Version: 2026-13-01\n", "is not a date"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.head + row))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// Inline code is stripped before sentences are counted. `g1...` ends in three
// periods, and counting them would reject the one entry that explains the most
// confusing thing on the site.
func TestInlineCodeDoesNotCountAsSentences(t *testing.T) {
	g, err := Parse([]byte("Version: 2026-09-23\n\n| term | gloss |\n|---|---|\n" +
		"| address | An account, written as a long `g1...` string. It has a page. |\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := sentences(g.Terms["address"].Gloss); n != 2 {
		t.Fatalf("counted %d sentences, want 2", n)
	}
}

// A three-column table is a different document, and it is refused rather than
// half-parsed: dropping whatever the third column said, quietly, is how a
// glossary ends up shorter than the file a contributor is looking at.
func TestRowsMustBeExactlyTwoColumns(t *testing.T) {
	_, err := Parse([]byte("Version: 2026-09-23\n\n| term | gloss | note |\n|---|---|---|\n" +
		"| block | A batch. | added later |\n"))
	if err == nil || !strings.Contains(err.Error(), "3 columns, want exactly 2") {
		t.Fatalf("error %v, want it to reject a three-column table", err)
	}
}

// The header is found by the separator rule under it, not by matching the word
// "term". Renaming a column heading must not turn it into a definition.
func TestHeaderIsIdentifiedByPositionNotByItsText(t *testing.T) {
	g, err := Parse([]byte("Version: 2026-09-23\n\n| word | what it means |\n|---|---|\n" +
		"| block | A batch. |\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(g.Order) != 1 || g.Order[0] != "block" {
		t.Fatalf("terms = %v, want just [block]: the header was parsed as one", g.Order)
	}
}

// Prose after the table is prose. A pipe in a later sentence must not reopen it.
func TestProseAfterTheTableEndsIt(t *testing.T) {
	g, err := Parse([]byte("Version: 2026-09-23\n\n| term | gloss |\n|---|---|\n" +
		"| block | A batch. |\n\nSome closing prose.\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(g.Order) != 1 {
		t.Fatalf("terms = %v, want just [block]", g.Order)
	}
}
