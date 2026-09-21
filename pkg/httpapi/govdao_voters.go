package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// The voter roster: who is in the memberstore, and what each of them has
// actually done with the vote they hold.
//
// gov/dao publishes the two halves separately and never joins them. The
// member list (`:members`) carries addresses and tiers but no votes; each
// proposal's `/votes` render carries usernames and options but no addresses
// and no notion of who *could* have voted. So a reader who wants to know
// whether a member votes at all has to open every proposal and keep score by
// hand. This does that once, server-side, over renders that are already
// cached for the overview page.
//
// The join is by address: vote renders name a gno.land username, which
// resolveGnoUsernameCached turns back into the address the memberstore uses.
// A username that will not resolve is still counted, keyed on the username
// itself, and flagged Resolved=false — dropping it would quietly understate
// turnout, which is the one number this page exists to report.

// GovDAOVoter is one member's record across every proposal counted.
type GovDAOVoter struct {
	Address string `json:"address,omitempty"`
	User    string `json:"user,omitempty"`
	Tier    string `json:"tier,omitempty"`
	Yes     int    `json:"yes"`
	No      int    `json:"no"`
	Abstain int    `json:"abstain"`
	Cast    int    `json:"cast"`
	// Proposals this voter authored. Authorship is not a vote, but it is the
	// other way a member moves governance, and a roster that omitted it would
	// rank an active author who abstains below a silent member.
	Authored int `json:"authored"`
	// The highest proposal ID this voter cast on, so "last seen" is
	// answerable without opening anything.
	LastVoted int `json:"last_voted,omitempty"`
	// False when the voter came from a vote render whose username r/sys/users
	// would not resolve, so the row could not be matched to a member.
	Resolved bool `json:"resolved"`
	// False when the address is not in the current memberstore: a past member
	// whose votes are still on the record.
	Member bool `json:"member"`
}

// GovDAOVoters is the /api/govdao/voters payload.
type GovDAOVoters struct {
	Voters []GovDAOVoter `json:"voters"`
	// How many proposals were counted, and the ID range they cover. A
	// participation rate is meaningless without them.
	Proposals int      `json:"proposals"`
	TotalCast int      `json:"total_cast"`
	Errors    []string `json:"errors,omitempty"`
}

// HandleGovDAOVoters serves the per-member voting record. Every render it
// reads is the same one the overview and the proposal pages already fetch,
// so on a warm cache this endpoint does no RPC at all.
func (a *API) HandleGovDAOVoters(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	rpcURL := a.rpcURLFor(network)
	JSONResponse(w, a.govDAOVoters(r.Context(), network, rpcURL))
}

func (a *API) govDAOVoters(ctx context.Context, network, rpcURL string) GovDAOVoters {
	overview := FetchGovDAOOverview(ctx, network, rpcURL)

	// Newest first and cut at the same limit the overview audits to, so the
	// two pages never disagree about which proposals are in scope.
	ids := make([]int, 0, len(overview.Proposals))
	for _, p := range overview.Proposals {
		ids = append(ids, p.ID)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	if len(ids) > govDAOListAuditLimit {
		ids = ids[:govDAOListAuditLimit]
	}

	details := make([]GovDAOProposalDetail, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i, id int) {
			defer wg.Done()
			details[i] = FetchGovDAOProposal(ctx, network, rpcURL, id)
		}(i, id)
	}
	wg.Wait()

	return aggregateGovDAOVoters(overview, ids, details, func(user string) string {
		return resolveGnoUsernameCached(ctx, rpcURL, user)
	})
}

// aggregateGovDAOVoters is the join itself, with the fetching lifted out so
// it can be tested without an RPC endpoint. ids[i] is the proposal
// details[i] describes; resolve turns a gno.land username into its address,
// or "" when the registry does not know it.
func aggregateGovDAOVoters(overview GovDAOOverview, ids []int, details []GovDAOProposalDetail, resolve func(string) string) GovDAOVoters {
	out := GovDAOVoters{Voters: []GovDAOVoter{}, Errors: overview.Errors, Proposals: len(ids)}

	// Seed the roster with the memberstore, so a member who has never voted
	// is a row of zeroes rather than an absence. "Who is not voting" is the
	// question this page is most often opened for.
	byKey := map[string]*GovDAOVoter{}
	// keyFor resolves a username to the same key the memberstore seeded, so
	// one person is one row whether they arrive as a member, a voter or an
	// author. An unresolvable name keys on itself rather than colliding with
	// every other unresolvable name under the empty address.
	keyFor := func(user string) (string, string) {
		addr := resolve(user)
		if addr != "" {
			return addr, addr
		}
		return "@" + strings.ToLower(user), ""
	}
	for _, m := range overview.Members {
		byKey[m.Address] = &GovDAOVoter{Address: m.Address, Tier: m.Tier, Resolved: true, Member: true}
	}

	for i, d := range details {
		if i >= len(ids) {
			break
		}
		for _, v := range d.Votes {
			user := strings.TrimPrefix(v.Voter, "@")
			key, addr := keyFor(user)
			rec := byKey[key]
			if rec == nil {
				rec = &GovDAOVoter{Address: addr, Tier: v.Tier, Resolved: addr != ""}
				byKey[key] = rec
			}
			if rec.User == "" {
				rec.User = user
			}
			if rec.Tier == "" {
				rec.Tier = v.Tier
			}
			rec.Cast++
			switch strings.ToUpper(v.Option) {
			case "YES":
				rec.Yes++
			case "NO":
				rec.No++
			case "ABSTAIN":
				rec.Abstain++
			}
			if ids[i] > rec.LastVoted {
				rec.LastVoted = ids[i]
			}
			out.TotalCast++
		}
	}

	// Authorship is not a vote, but it is the other way a member moves
	// governance, and a roster that omitted it would rank an active author
	// who abstains below a silent member. Counted over every proposal
	// gov/dao lists, not just the ones in scope above: the author is on the
	// list render itself, so it costs nothing extra.
	for _, p := range overview.Proposals {
		user := strings.TrimPrefix(p.Author, "@")
		if user == "" {
			continue
		}
		key, addr := keyFor(user)
		rec := byKey[key]
		if rec == nil {
			rec = &GovDAOVoter{Address: addr, User: user, Resolved: addr != ""}
			byKey[key] = rec
		}
		if rec.User == "" {
			rec.User = user
		}
		rec.Authored++
	}

	for _, v := range byKey {
		out.Voters = append(out.Voters, *v)
	}
	// Most active first, then by tier, then by key, so the order is stable
	// across refreshes and two rows never swap for no reason.
	sort.Slice(out.Voters, func(i, j int) bool {
		a, b := out.Voters[i], out.Voters[j]
		if a.Cast != b.Cast {
			return a.Cast > b.Cast
		}
		if a.Authored != b.Authored {
			return a.Authored > b.Authored
		}
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return a.Address+a.User < b.Address+b.User
	})
	return out
}
