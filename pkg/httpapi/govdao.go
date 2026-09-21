package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// govDAOCacheTTL bounds how stale a rendered proposal/member list can be.
// This reads gov/dao's *own* Render() output over RPC on every cache miss —
// short enough that a fresh vote or a new proposal shows up quickly, long
// enough that a page load never waits on more than one live RPC round trip.
const govDAOCacheTTL = 30 * time.Second

// govDAOListAuditLimit caps how many proposals the overview page audits in
// full. gov/dao has eight proposals on mainnet, so this is not a limit
// anyone hits today; it exists so a chain that accumulates hundreds does not
// turn one list page into hundreds of ABCI round trips. The newest IDs win,
// since those are the ones with a vote still open.
const govDAOListAuditLimit = 25

// fetchGovDAORender runs a vm/qrender ABCI query and returns the realm's
// rendered markdown for the given query string (e.g. "gno.land/r/gov/dao:4").
func fetchGovDAORender(ctx context.Context, rpcURL, query string) (string, error) {
	return fetchABCIQuery(ctx, rpcURL, "vm/qrender", query)
}

// fetchABCIQuery runs a CometBFT abci_query JSON-RPC call at the given
// query path (e.g. "vm/qrender", "vm/qpkgmeta_json", "vm/qinertpaths?limit=500")
// with data as the raw query payload, and returns the decoded response data.
//
// The `data` field of CometBFT's abci_query JSON-RPC is typed as raw bytes,
// which the JSON-RPC layer expects base64-encoded — not hex, despite this
// codebase's other RPC caller (fetchBalance) using a "0x"-prefixed empty
// payload for its no-data case. Hex looked plausible by analogy and produced
// "illegal base64 data" or a decoded-garbage query path; base64 is what
// CometBFT actually wants here.
func fetchABCIQuery(ctx context.Context, rpcURL, queryPath, data string) (string, error) {
	if rpcURL == "" {
		return "", fmt.Errorf("no verified RPC endpoint")
	}
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "abci_query",
		"params": map[string]any{
			"path": queryPath,
			"data": base64.StdEncoding.EncodeToString([]byte(data)),
		},
	})
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var result struct {
		Error  *struct{ Message string } `json:"error"`
		Result struct {
			Response struct {
				Log          string `json:"Log"`
				ResponseBase struct {
					Data  string `json:"Data"`
					Error any    `json:"Error"`
				} `json:"ResponseBase"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Error != nil {
		return "", fmt.Errorf("rpc error: %s", result.Error.Message)
	}
	if result.Result.Response.ResponseBase.Error != nil {
		return "", fmt.Errorf("abci error: %v (%s)", result.Result.Response.ResponseBase.Error, result.Result.Response.Log)
	}
	respData := result.Result.Response.ResponseBase.Data
	if respData == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(respData)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// GovDAOProposalSummary is one row of the proposal list. YesPercent through
// LastActivityTime are not on gov/dao's own list render — the caller (see
// HandleGovDAOOverview) fills them in from the per-proposal detail render
// and, for the two activity fields, live indexer data. AuthorAddress is
// filled in the same way, by resolving Author (a gno.land username, all
// gov/dao's render ever gives) against r/sys/users — see
// resolveGnoUsernameCached.
type GovDAOProposalSummary struct {
	ID                 int      `json:"id"`
	Title              string   `json:"title"`
	Author             string   `json:"author"`
	AuthorAddress      string   `json:"author_address,omitempty"`
	Status             string   `json:"status"`
	Tiers              []string `json:"tiers"`
	YesPercent         float64  `json:"yes_percent,omitempty"`
	NoPercent          float64  `json:"no_percent,omitempty"`
	AbstainPercent     float64  `json:"abstain_percent,omitempty"`
	CreatedHeight      int      `json:"created_height,omitempty"`
	CreatedTime        string   `json:"created_time,omitempty"`
	LastActivityHeight int      `json:"last_activity_height,omitempty"`
	LastActivityTime   string   `json:"last_activity_time,omitempty"`

	// Audit summary, so a proposal with an inconsistency is visible in the
	// list rather than only to whoever opens it. Audited separates "the
	// audit ran and found nothing" from "the audit did not run", which an
	// empty pair of counts cannot: the second must never read as a clean
	// bill of health.
	Audited   bool   `json:"audited"`
	Alerts    int    `json:"alerts,omitempty"`
	Warnings  int    `json:"warnings,omitempty"`
	TopSignal string `json:"top_signal,omitempty"`
}

// GovDAOMember is one row of the memberstore's member list.
type GovDAOMember struct {
	Address string `json:"address"`
	Tier    string `json:"tier"`
}

// GovDAOTierStat is one tier's membership and voting power, as gov/dao's own
// memberstore render reports it.
type GovDAOTierStat struct {
	Tier    string `json:"tier"`
	Members int    `json:"members"`
	Power   int    `json:"power"`
}

// GovDAOOverview is the whole /govdao landing page in one shape.
type GovDAOOverview struct {
	Proposals []GovDAOProposalSummary `json:"proposals"`
	Members   []GovDAOMember          `json:"members"`
	TierStats []GovDAOTierStat        `json:"tier_stats"`
	// Set when a render call failed; the fields it would have populated are
	// simply empty rather than the whole page failing.
	Errors []string `json:"errors,omitempty"`
}

// GovDAOProposalDetail is a single proposal's full detail page.
type GovDAOProposalDetail struct {
	ID              int                 `json:"id"`
	Title           string              `json:"title"`
	Author          string              `json:"author"`
	AuthorAddress   string              `json:"author_address,omitempty"`
	Description     string              `json:"description"`
	ExecutorPkgPath string              `json:"executor_pkg_path"`
	Status          string              `json:"status"`
	Tiers           []string            `json:"tiers"`
	YesPercent      float64             `json:"yes_percent"`
	NoPercent       float64             `json:"no_percent"`
	AbstainPercent  float64             `json:"abstain_percent"`
	Votes           []GovDAOVote        `json:"votes"`
	RelatedCalls    []GovDAORelatedCall `json:"related_calls"`
	RelatedMsgRuns  []store.MsgRunInfo  `json:"related_msgruns"`

	// Provenance and audit, all filled by AuditGovDAOProposal from data
	// gov/dao's own render does not carry. See govdao_audit.go for why each
	// of these exists; in short, the render says what a proposal is called
	// and who voted, and nothing about what it would do or who wrote it.
	Code         *ProposalCode         `json:"code,omitempty"`
	ParamChanges []ProposalParamChange `json:"param_changes,omitempty"`
	Addresses    []ProposalAddress     `json:"addresses,omitempty"`
	Timeline     []ProposalStep        `json:"timeline,omitempty"`
	Signals      []ProposalSignal      `json:"signals,omitempty"`

	Errors []string `json:"errors,omitempty"`
}

// GovDAOVote is one recorded vote. gov/dao's own render never gives a
// voter's address, only their gno.land username (Voter) — VoterAddress is
// filled in afterward by resolving it, same as GovDAOProposalDetail.Author /
// AuthorAddress. Named Voter, not Address: the field held a username under
// that name for one release, which silently broke "is this a clickable
// address" for every reader of the JSON, not just the frontend.
type GovDAOVote struct {
	Tier         string `json:"tier"`
	Option       string `json:"option"`
	Voter        string `json:"voter"`
	VoterAddress string `json:"voter_address,omitempty"`
}

// GovDAORelatedCall is a MsgCall this proposal ID can be traced through —
// creation, execution, or a vote — sourced live from the indexer since the
// locally synced `calls` table does not retain call arguments.
type GovDAORelatedCall struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time"`
	Caller      string `json:"caller"`
	Func        string `json:"func"`
	Success     bool   `json:"success"`
}

var propHeaderRe = regexp.MustCompile(`^#+\s*\[?Prop #(\d+)\s*-\s*(.+?)\]?(?:\(/r/gov/dao:\d+\))?$`)
var authorRe = regexp.MustCompile(`^Author:\s*\[@([^\]]+)\]`)
var statusListRe = regexp.MustCompile(`^Status:\s*(\S+)`)
var tiersRe = regexp.MustCompile(`^Tiers eligible to vote:\s*(.+)$`)

// parseGovDAOProposalList parses the markdown gov/dao's root Render() returns
// into structured rows. The format (confirmed live against mainnet) is a
// run of blocks, one per proposal, each:
//
//	### [Prop #<id> - <title>](/r/gov/dao:<id>)
//	Author: [@<user>](/u/<user>)
//
//	Status: <STATUS>
//
//	Tiers eligible to vote: <tiers>
//
//	---
func parseGovDAOProposalList(md string) []GovDAOProposalSummary {
	var out []GovDAOProposalSummary
	var cur *GovDAOProposalSummary
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if m := propHeaderRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				out = append(out, *cur)
			}
			id, _ := strconv.Atoi(m[1])
			cur = &GovDAOProposalSummary{ID: id, Title: unescapeMarkdown(m[2])}
			continue
		}
		if cur == nil {
			continue
		}
		if m := authorRe.FindStringSubmatch(line); m != nil {
			cur.Author = m[1]
		} else if m := statusListRe.FindStringSubmatch(line); m != nil {
			cur.Status = m[1]
		} else if m := tiersRe.FindStringSubmatch(line); m != nil {
			cur.Tiers = splitTiers(m[1])
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

var membersRowRe = regexp.MustCompile(`^\|\s*!\[[^\]]*\]\([^)]*\)\s*(T\d+)\s*\|\s*(g1[a-z0-9]+)\s*\|$`)

// parseGovDAOMembers parses "gno.land/r/gov/dao/memberstore/v0:members", a
// markdown table of `| <tier chip image> Tn | <address> |` rows.
func parseGovDAOMembers(md string) []GovDAOMember {
	var out []GovDAOMember
	for _, line := range strings.Split(md, "\n") {
		if m := membersRowRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, GovDAOMember{Tier: m[1], Address: m[2]})
		}
	}
	return out
}

var tierStatRe = regexp.MustCompile(`Tier\s+(T\d+)\s+contains\s+(\d+)\s+members?\s+with\s+power:\s*(\d+)`)

// parseGovDAOTierStats parses "gno.land/r/gov/dao/memberstore/v0:", pulling
// the "Tier T1 contains N members with power: P" line per tier. It ignores
// the two embedded pie-chart data-URI images entirely — mygnoscan draws its
// own chart from these numbers instead of trying to reuse gov/dao's SVGs.
func parseGovDAOTierStats(md string) []GovDAOTierStat {
	var out []GovDAOTierStat
	for _, m := range tierStatRe.FindAllStringSubmatch(md, -1) {
		members, _ := strconv.Atoi(m[2])
		power, _ := strconv.Atoi(m[3])
		out = append(out, GovDAOTierStat{Tier: m[1], Members: members, Power: power})
	}
	return out
}

var detailTitleRe = regexp.MustCompile(`^#+\s*Prop #(\d+)\s*-\s*(.+)$`)
var executorRe = regexp.MustCompile("Executor created in:\\s*`([^`]+)`")
var statusAcceptedRe = regexp.MustCompile(`^-\s*\*\*PROPOSAL HAS BEEN (\w+)`)
var tiersDetailRe = regexp.MustCompile(`^-\s*Tiers eligible to vote:\s*(.+)$`)
var yesPercentRe = regexp.MustCompile(`YES PERCENT:\s*([\d.]+)%`)
var noPercentRe = regexp.MustCompile(`NO PERCENT:\s*([\d.]+)%`)
var abstainPercentRe = regexp.MustCompile(`ABSTAIN PERCENT:\s*([\d.]+)%`)

// parseGovDAOProposalDetail parses "gno.land/r/gov/dao:<id>". See prop4.md
// captured live from mainnet for the reference shape this was written
// against: a title line, an "Author:" line, free-text description, an
// "Executor created in: `pkgpath`" line, a blank-line-separated "### Stats"
// block with the accepted/rejected marker, eligible tiers and vote
// percentages, then an "### Actions" block of vote links this deliberately
// does not surface (mygnoscan is read-only — no wallet to sign with).
func parseGovDAOProposalDetail(id int, md string) GovDAOProposalDetail {
	d := GovDAOProposalDetail{ID: id}
	lines := strings.Split(md, "\n")
	var descLines []string
	inDescription := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if m := detailTitleRe.FindStringSubmatch(trimmed); m != nil {
			d.Title = unescapeMarkdown(m[2])
			inDescription = true
			continue
		}
		if m := authorRe.FindStringSubmatch(trimmed); m != nil {
			d.Author = m[1]
			continue
		}
		if m := executorRe.FindStringSubmatch(trimmed); m != nil {
			d.ExecutorPkgPath = m[1]
			inDescription = false
			continue
		}
		if trimmed == "### store.Stats" || trimmed == "### Actions" {
			inDescription = false
			continue
		}
		if m := statusAcceptedRe.FindStringSubmatch(trimmed); m != nil {
			d.Status = m[1]
			continue
		}
		if m := tiersDetailRe.FindStringSubmatch(trimmed); m != nil {
			d.Tiers = splitTiers(m[1])
			continue
		}
		if m := yesPercentRe.FindStringSubmatch(trimmed); m != nil {
			d.YesPercent, _ = strconv.ParseFloat(m[1], 64)
			continue
		}
		if m := noPercentRe.FindStringSubmatch(trimmed); m != nil {
			d.NoPercent, _ = strconv.ParseFloat(m[1], 64)
			continue
		}
		if m := abstainPercentRe.FindStringSubmatch(trimmed); m != nil {
			d.AbstainPercent, _ = strconv.ParseFloat(m[1], 64)
			continue
		}
		if inDescription && trimmed != "" && trimmed != "---" {
			descLines = append(descLines, trimmed)
		}
	}
	d.Description = cleanGovDAOMarkdown(unescapeMarkdown(strings.Join(descLines, "\n")))
	return d
}

var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
var mdBoldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
var mdCodeRe = regexp.MustCompile("`([^`]+)`")
var mdHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s*`)

// cleanGovDAOMarkdown strips the markdown decoration a proposal description
// can carry (links, bold, inline code, a stray sub-heading — a real one
// found live on mainnet's proposal #1, whose author formatted a "Portfolio"
// section with its own #### heading) down to plain text. mygnoscan renders
// this as text content, never HTML, so there is no injection risk either
// way — this is purely about not showing a reader raw "**T1**" and
// "[@moul](/u/moul)" syntax instead of the words it was meant to convey.
func cleanGovDAOMarkdown(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdBoldRe.ReplaceAllString(s, "$1")
	s = mdCodeRe.ReplaceAllString(s, "$1")
	s = mdHeadingRe.ReplaceAllString(s, "")
	return s
}

var voteSectionRe = regexp.MustCompile(`^(YES|NO|ABSTAIN)\s+from\s+(T\d+)\s*\(VPPM\s*\d+\):$`)
var voteAddrRe = regexp.MustCompile(`^-\s*\[@([^\]]+)\]\(/u/([^)]+)\)`)

// parseGovDAOVotes parses "gno.land/r/gov/dao:<id>/votes": nine headed
// sections (YES/NO/ABSTAIN x T1/T2/T3), each followed by zero or more
// "- [@user](/u/user)" bullet lines naming who voted that way.
func parseGovDAOVotes(md string) []GovDAOVote {
	var out []GovDAOVote
	var option, tier string
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := voteSectionRe.FindStringSubmatch(trimmed); m != nil {
			option, tier = m[1], m[2]
			continue
		}
		if m := voteAddrRe.FindStringSubmatch(trimmed); m != nil && option != "" {
			out = append(out, GovDAOVote{Tier: tier, Option: option, Voter: m[2]})
		}
	}
	return out
}

func splitTiers(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// unescapeMarkdown undoes the backslash-escaping gov/dao applies to
// punctuation in user-supplied titles/descriptions (e.g. "ugnot\." for a
// literal period), so the text mygnoscan shows matches what the author
// typed rather than carrying gov/dao's own markdown escaping.
func unescapeMarkdown(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// govDAOOverviewCache and govDAOProposalCache hold last-good renders,
// mirroring gnockpit.go's pattern: a failed refresh keeps serving the last
// good value rather than blanking a page over one bad RPC round trip.
var govDAOOverviewCache = struct {
	mu      sync.Mutex
	byNet   map[string]GovDAOOverview
	fetched map[string]time.Time
}{byNet: map[string]GovDAOOverview{}, fetched: map[string]time.Time{}}

// FetchGovDAOOverview returns the proposal list, members and tier stats for
// a network, best-effort and cached.
func FetchGovDAOOverview(ctx context.Context, network, rpcURL string) GovDAOOverview {
	govDAOOverviewCache.mu.Lock()
	if cached, ok := govDAOOverviewCache.byNet[network]; ok && time.Since(govDAOOverviewCache.fetched[network]) < govDAOCacheTTL {
		govDAOOverviewCache.mu.Unlock()
		return cached
	}
	govDAOOverviewCache.mu.Unlock()

	var out GovDAOOverview
	if listMD, err := fetchGovDAORender(ctx, rpcURL, store.GovDAOPathPrefix+":"); err != nil {
		out.Errors = append(out.Errors, "proposals: "+err.Error())
	} else {
		out.Proposals = parseGovDAOProposalList(listMD)
	}
	if membersMD, err := fetchGovDAORender(ctx, rpcURL, store.GovDAOPathPrefix+"/memberstore/v0:members"); err != nil {
		out.Errors = append(out.Errors, "members: "+err.Error())
	} else {
		out.Members = parseGovDAOMembers(membersMD)
	}
	if tierMD, err := fetchGovDAORender(ctx, rpcURL, store.GovDAOPathPrefix+"/memberstore/v0:"); err != nil {
		out.Errors = append(out.Errors, "tier stats: "+err.Error())
	} else {
		out.TierStats = parseGovDAOTierStats(tierMD)
	}

	govDAOOverviewCache.mu.Lock()
	defer govDAOOverviewCache.mu.Unlock()
	// Only replace the cache on at least partial success — an all-error
	// result means the RPC is down, and the previous good overview (if any)
	// is more useful to serve than an empty one.
	if len(out.Proposals) > 0 || len(out.Members) > 0 || len(out.TierStats) > 0 {
		govDAOOverviewCache.byNet[network] = out
		govDAOOverviewCache.fetched[network] = time.Now()
		return out
	}
	if cached, ok := govDAOOverviewCache.byNet[network]; ok {
		return cached
	}
	return out
}

var govDAOProposalCache = struct {
	mu      sync.Mutex
	byKey   map[string]GovDAOProposalDetail
	fetched map[string]time.Time
}{byKey: map[string]GovDAOProposalDetail{}, fetched: map[string]time.Time{}}

// FetchGovDAOProposal returns one proposal's detail + votes, best-effort and
// cached. Related on-chain activity (calls, msg runs) is looked up by the
// caller, which has access to the indexer client and local DB this file
// does not.
func FetchGovDAOProposal(ctx context.Context, network, rpcURL string, id int) GovDAOProposalDetail {
	key := network + ":" + strconv.Itoa(id)
	govDAOProposalCache.mu.Lock()
	if cached, ok := govDAOProposalCache.byKey[key]; ok && time.Since(govDAOProposalCache.fetched[key]) < govDAOCacheTTL {
		govDAOProposalCache.mu.Unlock()
		return cached
	}
	govDAOProposalCache.mu.Unlock()

	query := fmt.Sprintf("%s:%d", store.GovDAOPathPrefix, id)
	detail := GovDAOProposalDetail{ID: id}
	if detailMD, err := fetchGovDAORender(ctx, rpcURL, query); err != nil {
		detail.Errors = append(detail.Errors, "detail: "+err.Error())
	} else if detailMD == "" {
		detail.Errors = append(detail.Errors, "no such proposal")
	} else {
		detail = parseGovDAOProposalDetail(id, detailMD)
		if detail.Title == "" {
			// gov/dao answers an out-of-range ID with its own "# Proposal
			// not found" page rather than an empty response or an ABCI
			// error, so nothing above catches it — the parse just finds no
			// title line to match. Checking the parse's own postcondition
			// (no title) is more durable than matching gov/dao's wording.
			detail.Errors = append(detail.Errors, "no such proposal")
		}
	}
	if votesMD, err := fetchGovDAORender(ctx, rpcURL, query+"/votes"); err != nil {
		detail.Errors = append(detail.Errors, "votes: "+err.Error())
	} else {
		detail.Votes = parseGovDAOVotes(votesMD)
	}

	if len(detail.Errors) == 0 || detail.Title != "" {
		govDAOProposalCache.mu.Lock()
		govDAOProposalCache.byKey[key] = detail
		govDAOProposalCache.fetched[key] = time.Now()
		govDAOProposalCache.mu.Unlock()
		return detail
	}
	govDAOProposalCache.mu.Lock()
	defer govDAOProposalCache.mu.Unlock()
	if cached, ok := govDAOProposalCache.byKey[key]; ok {
		return cached
	}
	return detail
}

// gnoAddressRe pulls a bech32 address out of Gno's own debug-repr text (see
// resolveGnoUsername) — the one part of that blob with a fixed, recognizable
// shape regardless of which fields UserData happens to carry.
var gnoAddressRe = regexp.MustCompile(`"(g1[a-z0-9]+)"`)

// resolveGnoUsername resolves a registered gno.land username (e.g. "aeddi")
// to its bech32 address via gno.land/r/sys/users.ResolveName — the same
// registry gov/dao's own render draws "@username" authorship and voter
// names from. gov/dao's markdown never carries the address itself, only the
// username, so without this a proposal's author or a vote's voter can never
// be linked the way the memberstore's member list already is (that one
// comes with real addresses baked in, from a different render entirely).
//
// vm/qeval, not vm/qrender: this calls a real function
// (ResolveName(name) (*UserData, bool)) rather than rendering markdown.
// Confirmed live against mainnet — vm/qeval's `data` is
// "<pkgpath>.<expression>", not a bare call (the ABCI layer says so
// verbatim on anything else: "expected <pkgpath>.<expression> syntax"), and
// the response is Gno's own debug representation of the result, not JSON:
//
//	(&(struct{("g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m" .uverse.address),("aeddi" string),(false bool)} ...) *...UserData)
//	(true bool)
//
// The address is simply the first quoted g1... string in that text.
func resolveGnoUsername(ctx context.Context, rpcURL, username string) (string, error) {
	if username == "" {
		return "", fmt.Errorf("empty username")
	}
	expr := fmt.Sprintf("gno.land/r/sys/users.ResolveName(%q)", username)
	out, err := fetchABCIQuery(ctx, rpcURL, "vm/qeval", expr)
	if err != nil {
		return "", err
	}
	m := gnoAddressRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no address in qeval response for %q", username)
	}
	return m[1], nil
}

// usernameCacheTTL is longer than govDAOCacheTTL: a username's bound address
// changes only on an explicit re-registration (ProposeUpdateName), not on
// every new vote or proposal, so there is no reason to re-resolve it as
// often as gov/dao's own render.
const usernameCacheTTL = 10 * time.Minute

var usernameCache = struct {
	mu      sync.Mutex
	byName  map[string]string
	fetched map[string]time.Time
}{byName: map[string]string{}, fetched: map[string]time.Time{}}

// resolveGnoUsernameCached is the best-effort, cached front for
// resolveGnoUsername: an empty string on failure (unregistered name, RPC
// hiccup) rather than an error, so a caller can fall back to showing the
// plain "@username" text exactly as it did before this existed, and a
// failed refresh serves the last good address rather than dropping a link
// that was working a moment ago — the same pattern as every other cache in
// this file.
func resolveGnoUsernameCached(ctx context.Context, rpcURL, username string) string {
	if username == "" {
		return ""
	}
	usernameCache.mu.Lock()
	if addr, ok := usernameCache.byName[username]; ok && time.Since(usernameCache.fetched[username]) < usernameCacheTTL {
		usernameCache.mu.Unlock()
		return addr
	}
	usernameCache.mu.Unlock()

	addr, err := resolveGnoUsername(ctx, rpcURL, username)

	usernameCache.mu.Lock()
	defer usernameCache.mu.Unlock()
	if err == nil && addr != "" {
		usernameCache.byName[username] = addr
		usernameCache.fetched[username] = time.Now()
		return addr
	}
	return usernameCache.byName[username]
}
