package httpapi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent callers for one cold key produce one fetch, not one each.
//
// This is the dogpile that made a cold /api/govdao/overview slower the more
// people wanted it: four concurrent readers ran four full copies of the same
// thirty-odd ABCI round trips against one node.
func TestMemoSingleFlights(t *testing.T) {
	m := newMemo[int](time.Hour, time.Minute)
	var calls atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	got := make([]int, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = m.get(context.Background(), "k", func(context.Context) (int, bool) {
				calls.Add(1)
				<-release
				return 42, true
			})
		}(i)
	}
	// Let every goroutine reach the fetch before any of them may finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("fetch ran %d times, want 1", n)
	}
	for i, v := range got {
		if v != 42 {
			t.Errorf("caller %d got %d, want 42", i, v)
		}
	}
}

// A failure is remembered for negativeTTL. Without this an unresolvable
// username cost one RPC round trip per proposal per request, forever.
func TestMemoRemembersFailure(t *testing.T) {
	m := newMemo[string](time.Hour, time.Hour)
	var calls atomic.Int32
	fetch := func(context.Context) (string, bool) {
		calls.Add(1)
		return "", false
	}
	for i := 0; i < 5; i++ {
		if v := m.get(context.Background(), "nobody", fetch); v != "" {
			t.Fatalf("got %q, want empty", v)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fetch ran %d times, want 1", n)
	}
}

// A failed refresh keeps serving the last good value rather than blanking it,
// and does not re-ask on every request while it is failing.
func TestMemoKeepsLastGoodOnFailure(t *testing.T) {
	m := newMemo[string](10*time.Millisecond, time.Hour)
	var fail atomic.Bool
	var calls atomic.Int32
	fetch := func(context.Context) (string, bool) {
		calls.Add(1)
		if fail.Load() {
			return "", false
		}
		return "good", true
	}
	if v := m.get(context.Background(), "k", fetch); v != "good" {
		t.Fatalf("got %q, want good", v)
	}
	fail.Store(true)
	time.Sleep(20 * time.Millisecond) // expire the success

	for i := 0; i < 3; i++ {
		if v := m.get(context.Background(), "k", fetch); v != "good" {
			t.Errorf("after failure got %q, want the last good value", v)
		}
	}
	// One fetch for the success, one for the failed refresh, and no more:
	// negativeTTL holds the rest off.
	if n := calls.Load(); n != 2 {
		t.Errorf("fetch ran %d times, want 2", n)
	}
}

// A success is served without re-fetching until its TTL expires.
func TestMemoServesWithinTTL(t *testing.T) {
	m := newMemo[int](time.Hour, time.Minute)
	var calls atomic.Int32
	fetch := func(context.Context) (int, bool) { calls.Add(1); return 7, true }
	for i := 0; i < 4; i++ {
		if v := m.get(context.Background(), "k", fetch); v != 7 {
			t.Fatalf("got %d, want 7", v)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fetch ran %d times, want 1", n)
	}
}

// A waiting caller that gives up does not cancel the fetch it was waiting on,
// and does not leave the key wedged.
func TestMemoWaiterHonoursItsOwnContext(t *testing.T) {
	m := newMemo[int](time.Hour, time.Minute)
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		m.get(context.Background(), "k", func(context.Context) (int, bool) {
			close(started)
			<-release
			return 5, true
		})
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m.get(ctx, "k", func(context.Context) (int, bool) { return 99, true })

	close(release)
	// The leader's value still lands, and the key is usable afterwards.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := m.peek("k"); ok && v == 5 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("leader's value never landed")
}
