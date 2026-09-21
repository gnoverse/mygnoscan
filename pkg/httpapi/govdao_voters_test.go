package httpapi

import "testing"

// The join has three ways to go wrong, and each of them is silent: a member
// who never voted disappearing, a voter counted twice because the username
// and the address keyed separately, and an unresolvable username collapsing
// into one row with every other unresolvable username.
func TestAggregateGovDAOVoters(t *testing.T) {
	const (
		alice = "g1alice00000000000000000000000000000000"
		bob   = "g1bob0000000000000000000000000000000000"
		carol = "g1carol00000000000000000000000000000000"
	)
	resolve := func(user string) string {
		switch user {
		case "alice":
			return alice
		case "bob":
			return bob
		case "carol":
			return carol
		}
		return "" // an unregistered name, which r/sys/users answers with nothing
	}

	overview := GovDAOOverview{
		Members: []GovDAOMember{
			{Address: alice, Tier: "T1"},
			{Address: bob, Tier: "T2"},
			{Address: carol, Tier: "T3"},
		},
		Proposals: []GovDAOProposalSummary{
			{ID: 2, Author: "alice"},
			{ID: 1, Author: "alice"},
		},
	}
	ids := []int{2, 1}
	details := []GovDAOProposalDetail{
		{ID: 2, Votes: []GovDAOVote{
			{Voter: "alice", Tier: "T1", Option: "YES"},
			{Voter: "bob", Tier: "T2", Option: "NO"},
			{Voter: "ghost", Tier: "T3", Option: "YES"},
		}},
		{ID: 1, Votes: []GovDAOVote{
			{Voter: "alice", Tier: "T1", Option: "ABSTAIN"},
		}},
	}

	got := aggregateGovDAOVoters(overview, ids, details, resolve)

	if got.Proposals != 2 {
		t.Errorf("Proposals = %d, want 2", got.Proposals)
	}
	if got.TotalCast != 4 {
		t.Errorf("TotalCast = %d, want 4", got.TotalCast)
	}

	byKey := map[string]GovDAOVoter{}
	for _, v := range got.Voters {
		k := v.Address
		if k == "" {
			k = "@" + v.User
		}
		byKey[k] = v
	}

	for _, tt := range []struct {
		name string
		key  string
		want GovDAOVoter
	}{
		{
			// Two votes and two proposals authored, all under one row: the
			// member seeded by address and the votes arriving by username
			// have to land on the same record.
			name: "a member who votes and authors",
			key:  alice,
			want: GovDAOVoter{Address: alice, User: "alice", Tier: "T1", Yes: 1, Abstain: 1, Cast: 2, Authored: 2, LastVoted: 2, Resolved: true, Member: true},
		},
		{
			name: "a member who voted once",
			key:  bob,
			want: GovDAOVoter{Address: bob, User: "bob", Tier: "T2", No: 1, Cast: 1, LastVoted: 2, Resolved: true, Member: true},
		},
		{
			// The whole point of seeding from the memberstore: silence is a
			// row of zeroes, not an absence.
			name: "a member who has never voted",
			key:  carol,
			want: GovDAOVoter{Address: carol, Tier: "T3", Resolved: true, Member: true},
		},
		{
			// Counted, flagged, and not merged into anyone else.
			name: "a voter whose username does not resolve",
			key:  "@ghost",
			want: GovDAOVoter{User: "ghost", Tier: "T3", Yes: 1, Cast: 1, LastVoted: 2},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := byKey[tt.key]
			if !ok {
				t.Fatalf("no row for %s; rows: %+v", tt.key, byKey)
			}
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}

	if len(got.Voters) != 4 {
		t.Errorf("got %d rows, want 4 — a duplicate means the username and the address keyed separately", len(got.Voters))
	}
	// Most active first: alice (2 casts), then bob and ghost (1 each), then
	// carol (0).
	if got.Voters[0].Address != alice {
		t.Errorf("first row is %+v, want the most active voter", got.Voters[0])
	}
	if last := got.Voters[len(got.Voters)-1]; last.Address != carol {
		t.Errorf("last row is %+v, want the member who never voted", last)
	}
}
