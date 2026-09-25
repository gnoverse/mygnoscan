package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Discovery: the directory finds itself.
//
// The old shape was a curated list with the chain bolted on for numbers, and it
// had the failure every curated list has: ten entries against two hundred and
// ten realms, and a contributor's only workflow was to stare at a JSON file.
// This inverts it. The chain proposes, and curation corrects: a human adds what
// the ranking misses, fixes what it gets wrong, and removes what should not be
// there. Three small levers over a list that maintains itself, instead of one
// large one over a list that does not.
//
// What makes that safe is that every default here is the chain's own word or
// the realm's own word. A call count is a fact. A package's doc comment was
// written by whoever wrote the realm. Neither is this repo inventing a sentence
// about somebody else's code.

// Ranking weights. The whole tuning surface, deliberately in one place and
// deliberately only two numbers.
//
// Distinct callers outrank raw calls by an order of magnitude, because they
// answer different questions. Calls measure activity, and activity is trivially
// manufactured: one script in a loop is thousands of calls and one caller.
// Callers measure reach, which is the thing a directory is actually ranking
// for, and buying it costs a funded address per unit.
//
// Both are windowed. An app that was busy last year and is dead now should not
// outrank one that is busy today, and a directory whose front page is a
// historical record is the thing /realms already is.
const (
	// ScoreCallerWeight is what one distinct caller is worth.
	ScoreCallerWeight = 10
	// ScoreCallWeight is what one call is worth.
	ScoreCallWeight = 1
)

// DiscoveredApp is one realm the chain suggests, with everything needed to draw
// a card and nothing that needed a human.
type DiscoveredApp struct {
	Path        string `json:"path"`
	Calls       int    `json:"calls"`
	Callers     int    `json:"callers"`
	CallsWindow int    `json:"calls_window"`
	// CallersWindow is the one the ranking leans on, and is therefore the one
	// worth showing beside it: a reader who sees the order should be able to
	// see what produced it.
	CallersWindow int    `json:"callers_window"`
	LastCall      string `json:"last_call,omitempty"`
	DeployedAt    string `json:"deployed_at,omitempty"`
	Creator       string `json:"creator,omitempty"`
	// PackageDoc is the realm's own doc comment, and the default description.
	PackageDoc string `json:"package_doc,omitempty"`
	Score      int    `json:"score"`
}

// DiscoverApps ranks the realms a chain suggests are worth looking at.
//
// Realms only: a `p/` package is a library, has no page to open and no user to
// count, and belongs in a directory of packages, which /packages already is.
//
// `since` windows both the calls and the callers that feed the score. An empty
// window means all of history, which is what a reader asking for "all time"
// wants and what the tests pin.
func (d *DB) DiscoverApps(network, since string, limit int) ([]DiscoveredApp, error) {
	if limit <= 0 {
		limit = 60
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	// The window sits inside aggregate expressions in the SELECT list, so its
	// arguments bind before anything in the WHERE clause. Appending them after
	// the filters is how this silently matched nothing once already
	// (see AppStats).
	windowCalls, windowCallers := "COUNT(*)", "COUNT(DISTINCT c.caller)"
	args := []any{}
	if since != "" {
		windowCalls = "SUM(CASE WHEN c.block_time >= ? THEN 1 ELSE 0 END)"
		windowCallers = "COUNT(DISTINCT CASE WHEN c.block_time >= ? THEN c.caller END)"
		args = append(args, since, since, since, since)
	}
	score := fmt.Sprintf("(%s) * %d + (%s) * %d",
		windowCallers, ScoreCallerWeight, windowCalls, ScoreCallWeight)

	where := []string{d.networkFilter("c.network", network), "p.is_realm = 1"}
	args = append(args, limit)

	// The join carries the network as well as the path: joining on pkg_path
	// alone is the documented way to mix two chains here (AGENTS.md), and it
	// would rank one chain's realm by another chain's traffic.
	//
	// symbol_index is a LEFT JOIN because the doc is a bonus, not a gate. A
	// realm whose source has not been indexed yet is still a realm people use,
	// and dropping it from the directory until a background pass catches up
	// would make the page's contents depend on an implementation detail.
	q := `
		SELECT c.pkg_path,
		       COUNT(*), COUNT(DISTINCT c.caller),
		       ` + windowCalls + `, ` + windowCallers + `,
		       COALESCE(MAX(c.block_time), ''), COALESCE(MAX(p.creator), ''),
		       COALESCE(MIN(p.block_time), ''), COALESCE(MAX(si.package_doc), ''),
		       ` + score + ` AS score
		FROM calls c
		JOIN packages p ON p.path = c.pkg_path AND p.network = c.network
		LEFT JOIN symbol_index si ON si.package_path = c.pkg_path AND si.network = c.network
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY c.pkg_path
		HAVING score > 0
		ORDER BY score DESC, c.pkg_path
		LIMIT ?`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DiscoveredApp{}
	for rows.Next() {
		var a DiscoveredApp
		if err := rows.Scan(&a.Path, &a.Calls, &a.Callers, &a.CallsWindow, &a.CallersWindow,
			&a.LastCall, &a.Creator, &a.DeployedAt, &a.PackageDoc, &a.Score); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PackageDocs returns the stored doc comment for each path.
//
// For the entries discovery did not produce: a pinned realm with no traffic at
// all never appears in DiscoverApps, and it still deserves its own description
// rather than a blank card.
func (d *DB) PackageDocs(network string, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := make([]any, 0, len(paths))
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := d.db.Query(`
		SELECT package_path, package_doc FROM symbol_index
		WHERE `+d.networkFilter("network", network)+`
		  AND package_path IN (`+sqlPlaceholders(len(paths))+`)
		  AND package_doc <> ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, doc string
		if err := rows.Scan(&path, &doc); err != nil {
			return nil, err
		}
		out[path] = doc
	}
	return out, rows.Err()
}

// PackageReadmes returns the first descriptive line of each package's
// README.md.
//
// The fallback below the package doc comment, and it matters more than that
// ordering suggests: on mainnet most realms carry no `// Package x ...` comment
// at all, and several of the busiest ship a README that opens with exactly the
// sentence a card wants. gnoswap is the worked example, with a README on every
// one of its six realms and a doc comment on one.
//
// Still the project's own words, which is the whole rule for a default. The
// alternative is this repo writing a sentence about somebody else's code and
// presenting it as description rather than as a guess.
func (d *DB) PackageReadmes(network string, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := make([]any, 0, len(paths))
	for _, p := range paths {
		args = append(args, p)
	}
	// Bounded: a README can be tens of kilobytes and only its opening matters,
	// so the database sends a prefix rather than the whole file for every realm
	// on the page.
	rows, err := d.db.Query(`
		SELECT package_path, SUBSTR(body, 1, 2000) FROM package_files
		WHERE `+d.networkFilter("network", network)+`
		  AND LOWER(file_name) = 'readme.md'
		  AND package_path IN (`+sqlPlaceholders(len(paths))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, body string
		if err := rows.Scan(&path, &body); err != nil {
			return nil, err
		}
		if line := readmeLead(body); line != "" {
			out[path] = line
		}
	}
	return out, rows.Err()
}

// readmeLead finds the first line of a README that is prose about the project.
//
// Everything a README opens with that is not prose has to be stepped over, and
// each of these was found in a real one: the `# Title` heading (which repeats
// the name the card already shows), badge and image lines, HTML wrappers,
// blockquotes, list items, code fences and front matter. Taking "the first
// non-empty line" instead produces cards that say "# gns" or
// "<div align="center">".
func readmeLead(body string) string {
	inFence := false
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			inFence = !inFence
			continue
		case inFence:
			continue
		case strings.HasPrefix(line, "#"), strings.HasPrefix(line, "<"),
			strings.HasPrefix(line, ">"), strings.HasPrefix(line, "!"),
			strings.HasPrefix(line, "---"), strings.HasPrefix(line, "|"),
			strings.HasPrefix(line, "-"), strings.HasPrefix(line, "*"),
			strings.HasPrefix(line, "["):
			continue
		}
		// Inline markdown is stripped rather than rendered: the frontend builds
		// DOM and would otherwise print `**bold**` and `[text](url)` verbatim.
		line = mdLink.ReplaceAllString(line, "$1")
		line = strings.NewReplacer("**", "", "`", "", "_", "").Replace(line)
		if line = strings.TrimSpace(line); len(line) > 2 {
			return line
		}
	}
	return ""
}

var mdLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
