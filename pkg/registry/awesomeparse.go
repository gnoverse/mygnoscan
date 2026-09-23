package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// Parsing awesome-gno's README into structured entries.
//
// The list is a hand-written markdown document and will stay one: it is read on
// GitHub by people, and asking the community to maintain a JSON file so that an
// explorer has an easier time would be the tail wagging the dog. So the parser
// lives here, it is strict about the shape it expects, and `make awesome` fails
// loudly rather than silently emitting an empty section when the shape moves.
//
// Everything it understands is one of two lines:
//
//	## Section Title
//	- [Name](url) - description, possibly with [more](links) in it.
//
// plus an optional `_italic note_` under a heading. That is the whole grammar
// of an awesome list, and the parser rejects a section that turns out to hold
// nothing rather than shipping a heading with no bullets under it.

var (
	awesomeHeading  = regexp.MustCompile(`^##\s+(.+?)\s*$`)
	awesomeNote     = regexp.MustCompile(`^_(.+)_$`)
	awesomeBullet   = regexp.MustCompile(`^[-*]\s+(.*)$`)
	markdownLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	awesomeSlugJunk = regexp.MustCompile(`[^a-z0-9]+`)
	// A gno.land URL, on any subdomain the list happens to name (it links
	// staging as readily as mainnet). Only `r/` and `p/` are paths; the bare
	// homepage and play.gno.land are not realms and must not be read as one.
	gnoLandRealmURL = regexp.MustCompile(`^https?://(?:[a-z0-9-]+\.)?gno\.land/([rp]/[^\s?#]+)$`)
)

// awesomeSkipSections are the headings that are navigation or instructions
// rather than content. The table of contents would otherwise parse as twelve
// entries linking to anchors, and Contributing is prose we replace with our own
// call to action anyway.
var awesomeSkipSections = map[string]bool{
	"contents":     true,
	"contributing": true,
}

// ParseAwesome turns the README's markdown into sections and entries.
//
// Exported so the generator and its test share one implementation: the test
// runs against a fixture in testdata, which is the only way the parser is
// covered at all, since the generator itself needs the network.
func ParseAwesome(md string) ([]AwesomeSection, error) {
	var (
		out     []AwesomeSection
		current *AwesomeSection
		skip    bool
	)
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)

		if m := awesomeHeading.FindStringSubmatch(line); m != nil {
			title := stripInline(m[1])
			if awesomeSkipSections[strings.ToLower(title)] {
				current, skip = nil, true
				continue
			}
			out = append(out, AwesomeSection{Title: title, Slug: awesomeSlug(title)})
			current, skip = &out[len(out)-1], false
			continue
		}
		if skip || current == nil || line == "" {
			continue
		}
		// A heading of any other depth ends the section rather than folding
		// into it: an `### Subsection` would otherwise have its bullets
		// attributed to the parent with no sign that a level was lost.
		if strings.HasPrefix(line, "#") {
			current, skip = nil, true
			continue
		}
		if m := awesomeNote.FindStringSubmatch(line); m != nil && len(current.Entries) == 0 {
			current.Note = stripInline(m[1])
			continue
		}
		m := awesomeBullet.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if e, ok := parseAwesomeEntry(m[1]); ok {
			current.Entries = append(current.Entries, e)
		}
	}

	for _, s := range out {
		// A section that parsed to nothing means the document moved under us.
		// Emitting the empty heading would put a blank card group on the page
		// and look like the community deleted a category.
		if len(s.Entries) == 0 {
			return nil, fmt.Errorf("awesome: section %q parsed to zero entries", s.Title)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("awesome: no sections found; the README shape has changed")
	}
	return out, nil
}

func parseAwesomeEntry(body string) (AwesomeEntry, bool) {
	var e AwesomeEntry
	rest := strings.TrimSpace(body)
	if rest == "" {
		return e, false
	}

	// The leading link is the entry's own destination. An entry without one is
	// still an entry: the Archive section names retired networks that have no
	// URL left to point at, and dropping them would make the list look like it
	// never mentioned them.
	if m := markdownLink.FindStringSubmatchIndex(rest); m != nil && m[0] == 0 {
		e.Name = stripInline(rest[m[2]:m[3]])
		e.URL = rest[m[4]:m[5]]
		rest = strings.TrimSpace(rest[m[1]:])
	} else if i := strings.Index(rest, " - "); i >= 0 {
		e.Name, rest = stripInline(rest[:i]), strings.TrimSpace(rest[i:])
	} else {
		e.Name, rest = stripInline(rest), ""
	}
	if e.Name == "" {
		return e, false
	}

	rest = strings.TrimSpace(strings.TrimPrefix(rest, "-"))
	for _, m := range markdownLink.FindAllStringSubmatch(rest, -1) {
		e.Links = append(e.Links, AwesomeLink{Text: stripInline(m[1]), URL: m[2]})
	}
	// Links are lifted out of the prose, not left in it: the frontend builds
	// DOM and never sets innerHTML, so raw `[text](url)` would render to the
	// reader verbatim.
	e.Description = stripInline(markdownLink.ReplaceAllString(rest, "$1"))
	e.Path = awesomeRealmPath(e)
	return e, true
}

// awesomeRealmPath finds the realm an entry points at, if any.
//
// The entry's own URL comes first and its inline links second, which is what
// makes Kourt work: its destination is kourt.xyz and the realm is named in a
// `[realm](...)` link at the end of the sentence.
func awesomeRealmPath(e AwesomeEntry) string {
	urls := make([]string, 0, 1+len(e.Links))
	if e.URL != "" {
		urls = append(urls, e.URL)
	}
	for _, l := range e.Links {
		urls = append(urls, l.URL)
	}
	for _, u := range urls {
		m := gnoLandRealmURL.FindStringSubmatch(strings.TrimRight(u, "/"))
		if m == nil {
			continue
		}
		// `r/gnoland/blog:p/gno-debugger` is a page of a realm, not the realm.
		// Truncating to the realm would file the Gno debugger under the blog
		// and show the blog's call count beside it, which is a wrong number
		// presented as a right one.
		if strings.Contains(m[1], ":") {
			continue
		}
		p := "gno.land/" + m[1]
		if realmPath.MatchString(p) {
			return p
		}
	}
	return ""
}

// stripInline removes the markdown that carries no meaning once the text is
// going into a DOM node: backticks, bold and italic markers. It deliberately
// does not touch anything else, because the descriptions are prose written for
// humans and rewriting them further would be editorialising someone else's
// list.
func stripInline(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "**", "")
	return strings.TrimSpace(s)
}

func awesomeSlug(title string) string {
	s := awesomeSlugJunk.ReplaceAllString(strings.ToLower(title), "-")
	return strings.Trim(s, "-")
}
