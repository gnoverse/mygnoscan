package registry

import (
	"fmt"
	"strings"
)

// Moderation: the one lever that removes.
//
// Everything else in this package adds or corrects. This subtracts, which makes
// it the part most worth being careful with and the part most worth keeping
// visible: the file is committed, every entry carries its reason, and the
// reasons are served to the page so a reader can see that the list is curated
// and how.
//
// It exists because auto-discovery ranks by usage and usage is not worth. A
// realm can be busy because it is a load test, because one script calls it in a
// loop, or because it is farming something. An explorer that renders a scam as
// a card, with a screenshot and a call count, lends it exactly the credibility
// it lends everything else.

// Skip is one realm kept off the directory.
type Skip struct {
	Path string `json:"path"`
	// Why is required. An unexplained skip is indistinguishable from
	// censorship, and in six months nobody will remember which it was.
	Why string `json:"why"`
}

// Moderation is the parsed skip list.
type Moderation struct {
	Skips []Skip `json:"skips"`
}

// Skipped indexes the list by path.
func (m *Moderation) Skipped() map[string]string {
	out := make(map[string]string, len(m.Skips))
	for _, s := range m.Skips {
		out[s.Path] = s.Why
	}
	return out
}

func loadModeration() (*Moderation, error) {
	b, err := files.ReadFile("data/moderation.toml")
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	m, err := parseModeration(string(b))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range m.Skips {
		switch {
		case !realmPath.MatchString(s.Path):
			return nil, fmt.Errorf("moderation.toml: %q is not a gno.land path", s.Path)
		case seen[s.Path]:
			return nil, fmt.Errorf("moderation.toml: %s is listed twice", s.Path)
		case strings.TrimSpace(s.Why) == "":
			return nil, fmt.Errorf("moderation.toml: %s has no reason, and a skip without one is indistinguishable from censorship", s.Path)
		}
		seen[s.Path] = true
	}
	return m, nil
}

// parseModeration reads the `[[skip]]` tables.
//
// Hand-rolled rather than a TOML dependency, because the grammar this file is
// allowed to use is three lines long and a parser that accepts more than that
// invites a file nobody can read at a glance. Anything it does not understand
// is an error rather than a silent skip: a typo in a moderation file must never
// quietly mean "moderate nothing".
func parseModeration(src string) (*Moderation, error) {
	m := &Moderation{Skips: []Skip{}}
	var cur *Skip
	for n, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[[skip]]" {
			m.Skips = append(m.Skips, Skip{})
			cur = &m.Skips[len(m.Skips)-1]
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || cur == nil {
			return nil, fmt.Errorf("moderation.toml:%d: %q is not a key inside a [[skip]] table", n+1, line)
		}
		v, err := tomlString(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("moderation.toml:%d: %w", n+1, err)
		}
		switch strings.TrimSpace(key) {
		case "path":
			cur.Path = v
		case "why":
			cur.Why = v
		default:
			return nil, fmt.Errorf("moderation.toml:%d: unknown key %q", n+1, strings.TrimSpace(key))
		}
	}
	return m, nil
}

func tomlString(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("%s is not a quoted string", s)
	}
	return s[1 : len(s)-1], nil
}
