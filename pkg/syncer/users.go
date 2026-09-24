package syncer

import (
	"context"
	"log"
	"sort"

	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// The user registry pass.
//
// gno.land/r/sys/users is the chain's own name registry and it publishes every
// change as an event, so the whole thing replays from the indexer. Nothing else
// this explorer holds is a substitute: package namespaces answer for 11 of
// mainnet's 78 registrations, and the realm exposes no enumeration to query --
// its Render prints a count and its API is ResolveName / ResolveAddress, both
// one name at a time.
//
// A pass of its own rather than a hook on the syncCalls walk, for two reasons.
// A third of the registry was written at genesis, at height 0, which a
// cursor-driven walk above the last stored height never revisits; and the
// filtered query is 63 transactions on mainnet against the walk's hundreds of
// thousands, so asking for exactly these is cheaper than watching for them.

// usersRealm is the registry itself, not the controller that fronts it.
//
// r/sys/namereg/v0 is the realm a person calls, and it emits its own
// "Registration" event -- but it is one controller among several by design
// (registration is whitelisted per realm), and it is not the realm GovDAO calls
// for a vanity name. r/sys/users is downstream of all of them, so a name that
// exists at all passed through here.
const usersRealm = "gno.land/r/sys/users"

// The three events, as declared in the realm's store.gno.
const (
	userRegisteredEvent = "Registered"
	userUpdatedEvent    = "Updated"
	userDeletedEvent    = "Deleted"
)

// syncUsers replays the registry into the users table.
//
// Unbounded (`need = 0`): the query is small and the whole point is to reach
// genesis, which the windowed form only does after widening past the tip.
//
// Best-effort, like backfillValopers. A registry this pass could not refresh
// costs the search box some names; failing SyncAll over it would cost the
// packages, calls and blocks behind it.
func (s *Syncer) syncUsers(ctx context.Context) {
	txs, err := s.client.GetEventsByPkgPath(ctx, usersRealm, 0)
	if err != nil {
		log.Printf("[%s] sync users: %v", s.networkID, err)
		return
	}

	// Oldest first. The events are a log and replaying them out of order gets
	// the alias ranking backwards: Updated demotes every *other* name the
	// address holds, so applying an older one after a newer one would demote
	// the name that is current.
	//
	// The query orders by height DESC, and heightAndIndex is the indexer's own
	// within-block ordering -- reversing the slice therefore restores it, where
	// sorting on block_height alone would leave two registrations in the same
	// block in whichever order they came back. Genesis puts 17 of them in block
	// 0, so that is not hypothetical.
	ordered := make([]indexer.Transaction, len(txs))
	for i, tx := range txs {
		ordered[len(txs)-1-i] = tx
	}
	// A stable sort on height only reorders across blocks, which the reversal
	// above has already made monotonic in the happy case; it is here so a
	// backend that does not honour the order clause still replays correctly.
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].BlockHeight < ordered[j].BlockHeight
	})

	// The selection set carries no block_time -- see txFieldsTemplate, which
	// selects the height and leaves the clock to the blocks query. Every other
	// pass resolves it the same way.
	times := s.fetchBlockTimes(ctx, ordered)

	seen := 0
	for _, tx := range ordered {
		if tx.Response == nil || !tx.Success {
			// A failed transaction registered nobody. Its events are still
			// reported, and replaying them would put names in the index that
			// the registry does not hold.
			continue
		}
		bt := times[tx.BlockHeight]
		for _, ev := range tx.Response.Events {
			if ev.Typename != "GnoEvent" || ev.PkgPath != usersRealm {
				continue
			}
			if s.recordUserEvent(tx, ev, bt) {
				seen++
			}
		}
	}
	if seen > 0 {
		log.Printf("[%s] user registry: %d events replayed", s.networkID, seen)
	}
}

// recordUserEvent applies one registry event, reporting whether it was one.
func (s *Syncer) recordUserEvent(tx indexer.Transaction, ev indexer.TxEvent, blockTime string) bool {
	var name, address string
	for _, attr := range ev.Attrs {
		switch attr.Key {
		case "name", "alias":
			// Registered carries `name`, Updated carries `alias`. Both are the
			// name the address answers to from that block on.
			name = attr.Value
		case "address":
			address = attr.Value
		}
	}

	switch ev.Type {
	case userRegisteredEvent, userUpdatedEvent:
		if name == "" || address == "" {
			return false
		}
		u := store.User{
			Name:        name,
			Address:     address,
			TxHash:      tx.Hash,
			BlockHeight: tx.BlockHeight,
			BlockTime:   blockTime,
		}
		if err := s.db.UpsertUser(s.networkID, u); err != nil {
			log.Printf("[%s] store user %q: %v", s.networkID, name, err)
			return false
		}
		if err := s.db.DemoteUserAliases(s.networkID, address, name); err != nil {
			log.Printf("[%s] demote aliases of %q: %v", s.networkID, address, err)
		}
		return true
	case userDeletedEvent:
		if address == "" {
			return false
		}
		if err := s.db.MarkUserDeleted(s.networkID, address); err != nil {
			log.Printf("[%s] delete user %q: %v", s.networkID, address, err)
			return false
		}
		return true
	}
	return false
}
