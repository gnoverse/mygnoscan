package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These fixtures are gov/dao's actual Render() output, captured live from
// gno.land mainnet via vm/qrender on 2026-09-14. If gov/dao ever changes its
// markdown shape, these tests are the tripwire.

const govDAOListFixture = `# GovDAO
## Members
[> Go to Memberstore <](/r/gov/dao/memberstore/v0)
## Proposals
### [Prop #4 - Proposal to unlock the transfer of ugnot\.](/r/gov/dao:4)
Author: [@moul](/u/moul)

Status: ACCEPTED

Tiers eligible to vote: T1, T2, T3

---

### [Prop #3 - Set the run\_submitters allowlist](/r/gov/dao:3)
Author: [@aeddi](/u/aeddi)

Status: ACCEPTED

Tiers eligible to vote: T1, T2, T3

---

### [Prop #2 - New T1 Member Proposal](/r/gov/dao:2)
Author: [@aeddi](/u/aeddi)

Status: ACCEPTED

Tiers eligible to vote: T1

---
`

const govDAOProposalFixture = "## Prop #4 - Proposal to unlock the transfer of ugnot\\.\n" +
	"Author: [@moul](/u/moul)\n" +
	"\n" +
	"This proposal wants to add a new key to sys/params: bank:p:restricted_denoms\n" +
	"\n" +
	"Executor created in: `gno.land/r/sys/params`\n" +
	"\n" +
	"\n" +
	"\n" +
	"\n" +
	"---\n" +
	"\n" +
	"### store.Stats\n" +
	"- **PROPOSAL HAS BEEN ACCEPTED**\n" +
	"- Tiers eligible to vote: T1, T2, T3\n" +
	"- YES PERCENT: 66.66666666666666%\n" +
	"- NO PERCENT: 0%\n" +
	"- ABSTAIN PERCENT: 0%\n" +
	"\n" +
	"[Detailed voting list](/r/gov/dao:4/votes)\n" +
	"\n" +
	"---\n" +
	"\n" +
	"### Actions\n" +
	"[Vote YES](/r/gov/dao$help&func=MustVoteOnProposalSimple&option=YES&pid=4) | [Vote NO](/r/gov/dao$help&func=MustVoteOnProposalSimple&option=NO&pid=4) | [Vote ABSTAIN](/r/gov/dao$help&func=MustVoteOnProposalSimple&option=ABSTAIN&pid=4)\n" +
	"\n" +
	"WARNING: Please double check transaction data before voting.\n"

const govDAOVotesFixture = `# Proposal #4 - Vote List

YES from T1 (VPPM 3):

- [@aeddi](/u/aeddi)
- [@moul](/u/moul)

YES from T2 (VPPM 2):


YES from T3 (VPPM 1):


NO from T1 (VPPM 3):


NO from T2 (VPPM 2):


NO from T3 (VPPM 1):


ABSTAIN from T1 (VPPM 3):


ABSTAIN from T2 (VPPM 2):


ABSTAIN from T3 (VPPM 1):

`

const govDAOMembersFixture = `# Memberstore Govdao v0
[\> Go to Tiers summary \<](/r/gov/dao/memberstore/v0)

**Filter members by tiers:** | [~~T1~~](?filter=T1)  | [~~T2~~](?filter=T2)  | [~~T3~~](?filter=T3)
| [Tier](?sort-desc=Tier) | [Address](?sort-desc=Address) |
| --- | --- |
| ![T1 colored chip](data:image/svg+xml;base64,PHN2ZyB4) T1 | g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m |
| ![T1 colored chip](data:image/svg+xml;base64,PHN2ZyB4) T1 | g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6 |
| ![T1 colored chip](data:image/svg+xml;base64,PHN2ZyB4) T1 | g1manfred47kzduec920z88wfr64ylksmdcedlf5 |

`

const govDAOTierStatsFixture = `# Memberstore Govdao v0
[\> Go to Members list \<](/r/gov/dao/memberstore/v0:members)
- ![T1 colored chip](data:image/svg+xml;base64,PHN2ZyB4) Tier T1 contains 3 members with power: 9
- ![T2 colored chip](data:image/svg+xml;base64,PHN2ZyB4) Tier T2 contains 0 members with power: 0
- ![T3 colored chip](data:image/svg+xml;base64,PHN2ZyB4) Tier T3 contains 0 members with power: 0

## Members distribution:
![Pie Chart Members distribution:](data:image/svg+xml;base64,PHN2ZyB4)
## Power distribution:
![Pie Chart Power distribution:](data:image/svg+xml;base64,PHN2ZyB4)
`

func TestParseGovDAOProposalList(t *testing.T) {
	props := parseGovDAOProposalList(govDAOListFixture)
	if len(props) != 3 {
		t.Fatalf("got %d proposals, want 3: %+v", len(props), props)
	}
	first := props[0]
	if first.ID != 4 {
		t.Errorf("first.ID = %d, want 4", first.ID)
	}
	if first.Title != "Proposal to unlock the transfer of ugnot." {
		t.Errorf("first.Title = %q, want unescaped period", first.Title)
	}
	if first.Author != "moul" {
		t.Errorf("first.Author = %q, want moul", first.Author)
	}
	if first.Status != "ACCEPTED" {
		t.Errorf("first.Status = %q, want ACCEPTED", first.Status)
	}
	if len(first.Tiers) != 3 || first.Tiers[0] != "T1" || first.Tiers[2] != "T3" {
		t.Errorf("first.Tiers = %v, want [T1 T2 T3]", first.Tiers)
	}
	last := props[2]
	if last.ID != 2 || len(last.Tiers) != 1 || last.Tiers[0] != "T1" {
		t.Errorf("last proposal parsed wrong: %+v", last)
	}
}

func TestParseGovDAOProposalDetail(t *testing.T) {
	d := parseGovDAOProposalDetail(4, govDAOProposalFixture)
	if d.ID != 4 {
		t.Errorf("ID = %d, want 4", d.ID)
	}
	if d.Title != "Proposal to unlock the transfer of ugnot." {
		t.Errorf("Title = %q", d.Title)
	}
	if d.Author != "moul" {
		t.Errorf("Author = %q, want moul", d.Author)
	}
	if d.ExecutorPkgPath != "gno.land/r/sys/params" {
		t.Errorf("ExecutorPkgPath = %q, want gno.land/r/sys/params", d.ExecutorPkgPath)
	}
	if d.Status != "ACCEPTED" {
		t.Errorf("Status = %q, want ACCEPTED", d.Status)
	}
	if len(d.Tiers) != 3 {
		t.Errorf("Tiers = %v, want 3 entries", d.Tiers)
	}
	if d.YesPercent < 66.6 || d.YesPercent > 66.7 {
		t.Errorf("YesPercent = %v, want ~66.67", d.YesPercent)
	}
	if d.NoPercent != 0 || d.AbstainPercent != 0 {
		t.Errorf("NoPercent/AbstainPercent = %v/%v, want 0/0", d.NoPercent, d.AbstainPercent)
	}
	if d.Description == "" {
		t.Error("Description is empty")
	}
	// The Actions block's vote links must never leak into the description —
	// mygnoscan has no wallet to sign with, and rendering them as live
	// controls would silently promise a feature that does not exist.
	if strings.Contains(d.Description, "MustVoteOnProposalSimple") || strings.Contains(d.Description, "Vote YES") {
		t.Errorf("Description leaked the Actions block: %q", d.Description)
	}
}

// TestCleanGovDAOMarkdown is against a real description found live on
// mainnet's proposal #1 — a mid-sentence link, bold, inline code, and a
// stray "####" sub-heading a proposal author added themselves.
func TestCleanGovDAOMarkdown(t *testing.T) {
	raw := "This is a proposal to add `[@moul](/u/moul)` to **T1**.\n" +
		"#### `[@moul](/u/moul)`'s Portfolio:\n" +
		"Manfred is the VP of Engineering of Gno.land"
	got := cleanGovDAOMarkdown(raw)
	if strings.ContainsAny(got, "*`#") {
		t.Errorf("cleanGovDAOMarkdown left markdown syntax behind: %q", got)
	}
	if strings.Contains(got, "](/u/") {
		t.Errorf("cleanGovDAOMarkdown left a raw link target behind: %q", got)
	}
	if !strings.Contains(got, "@moul") || !strings.Contains(got, "T1") || !strings.Contains(got, "Portfolio") {
		t.Errorf("cleanGovDAOMarkdown dropped real content: %q", got)
	}
}

// govDAOResolveNameFixture is the real vm/qeval response for
// gno.land/r/sys/users.ResolveName("aeddi"), captured live from mainnet
// 2026-09-16 — Gno's own debug representation of the returned
// (*UserData, bool), not JSON.
const govDAOResolveNameFixture = `(&(struct{("g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m" .uverse.address),("aeddi" string),(false bool)} gno.land/r/sys/users.UserData) *gno.land/r/sys/users.UserData)
(true bool)`

func TestResolveGnoUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct{ Data string } `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		decoded, _ := base64.StdEncoding.DecodeString(req.Params.Data)
		want := `gno.land/r/sys/users.ResolveName("aeddi")`
		if string(decoded) != want {
			t.Errorf("qeval expression = %q, want %q", decoded, want)
		}
		resp := map[string]any{
			"result": map[string]any{
				"response": map[string]any{
					"ResponseBase": map[string]any{
						"Data": base64.StdEncoding.EncodeToString([]byte(govDAOResolveNameFixture)),
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	addr, err := resolveGnoUsername(context.Background(), srv.URL, "aeddi")
	if err != nil {
		t.Fatalf("resolveGnoUsername: %v", err)
	}
	if addr != "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m" {
		t.Errorf("addr = %q, want g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m", addr)
	}
}

func TestResolveGnoUsernameCachedFallsBackOnFailure(t *testing.T) {
	usernameCache.mu.Lock()
	usernameCache.byName = map[string]string{}
	usernameCache.fetched = map[string]time.Time{}
	usernameCache.mu.Unlock()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			resp := map[string]any{
				"result": map[string]any{
					"response": map[string]any{
						"ResponseBase": map[string]any{
							"Data": base64.StdEncoding.EncodeToString([]byte(govDAOResolveNameFixture)),
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	first := resolveGnoUsernameCached(context.Background(), srv.URL, "aeddi")
	if first != "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m" {
		t.Fatalf("first resolve = %q", first)
	}

	// Force the TTL to have expired, then fail the request. The cached
	// address should still come back rather than an empty string — a
	// transient RPC hiccup should not un-link a name that resolved fine a
	// moment ago.
	usernameCache.mu.Lock()
	usernameCache.fetched["aeddi"] = time.Now().Add(-2 * usernameCacheTTL)
	usernameCache.mu.Unlock()

	second := resolveGnoUsernameCached(context.Background(), srv.URL, "aeddi")
	if second != first {
		t.Errorf("second resolve = %q after a failed refresh, want the stale value %q", second, first)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one per resolve attempt)", calls)
	}
}

func TestParseGovDAOVotes(t *testing.T) {
	votes := parseGovDAOVotes(govDAOVotesFixture)
	if len(votes) != 2 {
		t.Fatalf("got %d votes, want 2: %+v", len(votes), votes)
	}
	for _, v := range votes {
		if v.Option != "YES" || v.Tier != "T1" {
			t.Errorf("unexpected vote: %+v", v)
		}
	}
	voters := map[string]bool{votes[0].Voter: true, votes[1].Voter: true}
	if !voters["aeddi"] || !voters["moul"] {
		t.Errorf("votes = %+v, want aeddi and moul", votes)
	}
}

func TestParseGovDAOMembers(t *testing.T) {
	members := parseGovDAOMembers(govDAOMembersFixture)
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3: %+v", len(members), members)
	}
	for _, m := range members {
		if m.Tier != "T1" {
			t.Errorf("member %+v has unexpected tier", m)
		}
	}
	if members[2].Address != "g1manfred47kzduec920z88wfr64ylksmdcedlf5" {
		t.Errorf("members[2].Address = %q", members[2].Address)
	}
}

func TestParseGovDAOTierStats(t *testing.T) {
	stats := parseGovDAOTierStats(govDAOTierStatsFixture)
	if len(stats) != 3 {
		t.Fatalf("got %d tier stats, want 3: %+v", len(stats), stats)
	}
	if stats[0].Tier != "T1" || stats[0].Members != 3 || stats[0].Power != 9 {
		t.Errorf("stats[0] = %+v, want {T1 3 9}", stats[0])
	}
	if stats[1].Members != 0 || stats[2].Members != 0 {
		t.Errorf("expected T2/T3 to have 0 members: %+v", stats)
	}
}
