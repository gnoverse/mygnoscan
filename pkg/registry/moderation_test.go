package registry

import (
	"fmt"
	"strings"
	"testing"
)

// The reason is required, and this is the test that keeps it required. A skip
// without one is indistinguishable from censorship, and in six months nobody
// will remember which it was.
func TestModerationRefusesASkipWithNoReason(t *testing.T) {
	for _, tt := range []struct {
		name string
		toml string
		want string
	}{
		{"no reason", "[[skip]]\npath = \"gno.land/r/x/y\"\n", "no reason"},
		{"not a path", "[[skip]]\npath = \"whatever\"\nwhy = \"x\"\n", "not a gno.land path"},
		{"a key outside a table", "path = \"gno.land/r/x/y\"\n", "not a key inside"},
		{"an unknown key", "[[skip]]\npath = \"gno.land/r/x/y\"\nwhy = \"x\"\nreason = \"z\"\n", "unknown key"},
		{"an unquoted value", "[[skip]]\npath = gno.land/r/x/y\n", "not a quoted string"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := parseModeration(tt.toml)
			if err == nil {
				err = validateParsed(m)
			}
			if err == nil {
				t.Fatalf("accepted %q", tt.toml)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error is %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// validateParsed mirrors what loadModeration does after parsing, so the table
// above can exercise both halves without an embedded file per case.
func validateParsed(m *Moderation) error {
	seen := map[string]bool{}
	for _, s := range m.Skips {
		switch {
		case !realmPath.MatchString(s.Path):
			return fmt.Errorf("moderation.toml: %q is not a gno.land path", s.Path)
		case seen[s.Path]:
			return fmt.Errorf("moderation.toml: %s is listed twice", s.Path)
		case strings.TrimSpace(s.Why) == "":
			return fmt.Errorf("moderation.toml: %s has no reason, and a skip without one is indistinguishable from censorship", s.Path)
		}
		seen[s.Path] = true
	}
	return nil
}

func TestModerationParsesASkip(t *testing.T) {
	m, err := parseModeration(`
# a comment, and a blank line

[[skip]]
path = "gno.land/r/x/spam"
why  = "38,000 calls from two addresses in one afternoon"
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Skips) != 1 || m.Skips[0].Path != "gno.land/r/x/spam" {
		t.Fatalf("got %+v", m.Skips)
	}
	if m.Skips[0].Why == "" {
		t.Error("the reason was dropped, which is the one field that must survive")
	}
	if got := m.Skipped()["gno.land/r/x/spam"]; got == "" {
		t.Error("Skipped() lost the reason")
	}
}

// The shipped file has to load, and an empty skip list is a valid state: the
// page says "0 kept off this page" rather than failing to start.
func TestShippedModerationLoads(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Moderation == nil {
		t.Fatal("no moderation list embedded")
	}
	for _, s := range reg.Moderation.Skips {
		if strings.TrimSpace(s.Why) == "" {
			t.Errorf("%s is skipped with no reason", s.Path)
		}
	}
}
