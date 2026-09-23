package syncer

import (
	"context"
	"testing"

	"github.com/moul/mygnoscan/pkg/indexer"
)

// userTx builds a transaction carrying one r/sys/users event.
func userTx(hash string, height int, evType string, attrs ...string) indexer.Transaction {
	ev := indexer.TxEvent{Typename: "GnoEvent", Type: evType, PkgPath: usersRealm}
	for i := 0; i+1 < len(attrs); i += 2 {
		ev.Attrs = append(ev.Attrs, indexer.EventAttr{Key: attrs[i], Value: attrs[i+1]})
	}
	return indexer.Transaction{
		Hash: hash, BlockHeight: height, Success: true,
		Response: &indexer.TxResponse{Events: []indexer.TxEvent{ev}},
	}
}

// The registry replays from its own events, genesis included. A third of
// mainnet's registrations were written in block 0, which is the case a
// cursor-driven walk above the last stored height never revisits -- so the pass
// asks for these transactions by package path rather than watching for them.
func TestSyncUsersReplaysTheRegistry(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "alpha")

	fake.Add(
		userTx("TX1", 0, userRegisteredEvent, "name", "moul", "address", "g1manfred"),
		userTx("TX2", 0, userRegisteredEvent, "name", "onbloc", "address", "g1onbloc"),
		userTx("TX3", 120, userRegisteredEvent, "name", "nym-someone001", "address", "g1nym"),
	)

	syncer.syncUsers(ctx)

	n, err := db.UserCount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("users = %d, want 3 including the two at genesis", n)
	}
	got, err := db.SearchUsers("alpha", "moul", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != "g1manfred" || got[0].BlockHeight != 0 {
		t.Fatalf("got %+v, want moul at genesis", got)
	}
}

// Updated carries the new name under `alias` and demotes whatever the address
// held before. The old name stays resolvable, because r/sys/users keeps it on
// purpose: releasing it would let someone else take a name people still follow.
func TestSyncUsersDemotesAPreviousName(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "alpha")

	fake.Add(
		userTx("TX1", 10, userRegisteredEvent, "name", "oldname", "address", "g1a"),
		userTx("TX2", 20, userUpdatedEvent, "alias", "newname", "address", "g1a"),
	)
	syncer.syncUsers(ctx)

	current, err := db.UserByAddress("alpha", "g1a")
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Name != "newname" {
		t.Fatalf("current = %+v, want newname", current)
	}
	old, err := db.SearchUsers("alpha", "oldname", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || !old[0].Alias {
		t.Fatalf("old name = %+v, want it kept and marked an alias", old)
	}
}

// Deleted tombstones every name the address held. The rows stay: the realm's
// Delete sets deleted=true and leaves the name entry exactly so the name cannot
// be revived, and a search that dropped the row would say it is free.
func TestSyncUsersTombstonesADeletedUser(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "alpha")

	fake.Add(
		userTx("TX1", 10, userRegisteredEvent, "name", "gone", "address", "g1a"),
		userTx("TX2", 11, userRegisteredEvent, "name", "stays", "address", "g1b"),
		userTx("TX3", 20, userDeletedEvent, "address", "g1a"),
	)
	syncer.syncUsers(ctx)

	n, err := db.UserCount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("live users = %d, want 1", n)
	}
	got, err := db.SearchUsers("alpha", "gone", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Deleted {
		t.Fatalf("got %+v, want the name still listed and marked deleted", got)
	}
}

// A failed transaction registered nobody. Its events are reported all the same,
// and replaying them would put names in the index the registry does not hold.
func TestSyncUsersIgnoresFailedTransactions(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "alpha")

	failed := userTx("TX1", 10, userRegisteredEvent, "name", "never", "address", "g1a")
	failed.Success = false
	fake.Add(failed)
	syncer.syncUsers(ctx)

	n, err := db.UserCount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}
}

// An event from another realm is not a registration, however similar it looks.
// r/sys/namereg/v0 emits its own "Registration" and is only one of several
// whitelisted controllers, which is why the pass reads the registry rather than
// any of the realms that front it.
func TestSyncUsersIgnoresOtherRealms(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "alpha")

	other := userTx("TX1", 10, userRegisteredEvent, "name", "impostor", "address", "g1a")
	other.Response.Events[0].PkgPath = "gno.land/r/someone/fakeusers"
	fake.Add(other)
	syncer.syncUsers(ctx)

	n, err := db.UserCount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}
}
