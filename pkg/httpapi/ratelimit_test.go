package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withClock replaces the limiter's clock so a refill window can be crossed
// without a test sleeping through it.
func withClock(l *IPLimiter, at *time.Time) *IPLimiter {
	l.now = func() time.Time { return *at }
	return l
}

func TestIPLimiterRefills(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := withClock(NewIPLimiter(60, 0), &now) // one per second

	for i := 0; i < 60; i++ {
		if _, _, ok := l.Acquire("1.2.3.4"); !ok {
			t.Fatalf("request %d refused inside the budget", i)
		}
	}
	_, retry, ok := l.Acquire("1.2.3.4")
	if ok {
		t.Fatal("the 61st request in the same instant was allowed")
	}
	if retry <= 0 {
		t.Errorf("retryAfter = %v, want a positive wait", retry)
	}

	// Continuous refill, not a window that resets: a client one request over
	// waits a second, not a minute.
	now = now.Add(2 * time.Second)
	if _, _, ok := l.Acquire("1.2.3.4"); !ok {
		t.Error("still refused two seconds later, so the bucket is not refilling")
	}
}

// A refusal for concurrency must not also spend a token. The client is not
// over its rate, it is early, and charging it would turn one burst into a rate
// ban that outlives the burst.
func TestIPLimiterConcurrencyDoesNotSpendRate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := withClock(NewIPLimiter(60, 1), &now)

	release, _, ok := l.Acquire("1.2.3.4")
	if !ok {
		t.Fatal("first acquire refused")
	}
	for i := 0; i < 10; i++ {
		if _, _, ok := l.Acquire("1.2.3.4"); ok {
			t.Fatal("a second concurrent request was allowed")
		}
	}
	release()

	// 59 tokens left, not 49: the ten refusals cost nothing.
	for i := 0; i < 59; i++ {
		r, _, ok := l.Acquire("1.2.3.4")
		if !ok {
			t.Fatalf("request %d refused; the refusals were charged against the rate", i)
		}
		r()
	}
}

func TestIPLimiterIsPerAddress(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := withClock(NewIPLimiter(1, 0), &now)

	if _, _, ok := l.Acquire("1.2.3.4"); !ok {
		t.Fatal("first address refused")
	}
	if _, _, ok := l.Acquire("1.2.3.4"); ok {
		t.Fatal("first address allowed a second request")
	}
	if _, _, ok := l.Acquire("5.6.7.8"); !ok {
		t.Fatal("a second address was refused, so one client can close the endpoint for everybody")
	}
}

// A non-positive bound means unlimited, which is what a local single-user run
// and the tool tests want.
func TestIPLimiterUnlimited(t *testing.T) {
	l := NewIPLimiter(0, 0)
	for i := 0; i < 500; i++ {
		if _, _, ok := l.Acquire("1.2.3.4"); !ok {
			t.Fatalf("request %d refused by a limiter with no limits", i)
		}
	}
}

// An open endpoint on the public internet sees a long tail of one-shot
// addresses, and a map keyed by remote IP with no eviction is a leak that only
// shows up in production.
func TestIPLimiterEvictsIdleBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := withClock(NewIPLimiter(60, 0), &now)

	for i := 0; i < 100; i++ {
		// Released straight away: a bucket with a request still in flight is
		// deliberately not evictable, which the next test covers.
		release, _, _ := l.Acquire(time.Duration(i).String())
		release()
	}
	if len(l.buckets) != 100 {
		t.Fatalf("buckets = %d, want 100", len(l.buckets))
	}

	now = now.Add(2 * ipBucketTTL)
	release, _, _ := l.Acquire("fresh")
	defer release()
	if len(l.buckets) != 1 {
		t.Errorf("buckets = %d after the TTL passed, want 1", len(l.buckets))
	}
}

// A slot held by a request in flight is never evicted, or the concurrency
// count it carries is lost and the limit stops meaning anything.
func TestIPLimiterKeepsBucketsWithRequestsInFlight(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := withClock(NewIPLimiter(60, 1), &now)

	release, _, ok := l.Acquire("1.2.3.4")
	if !ok {
		t.Fatal("acquire refused")
	}
	now = now.Add(2 * ipBucketTTL)
	other, _, _ := l.Acquire("someone-else") // any call, to trigger the sweep
	other()
	if _, _, ok := l.Acquire("1.2.3.4"); ok {
		t.Error("the in-flight bucket was evicted, so its concurrency slot was lost")
	}
	release()
}

// Behind a reverse proxy every caller shares one RemoteAddr, and the first
// busy agent would rate-limit everybody else off the endpoint. Reaching us
// directly, a client can claim any address it likes and must be ignored.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{
			name:   "no proxy, no headers",
			remote: "203.0.113.7:4242",
			want:   "203.0.113.7",
		},
		{
			name:    "a direct client cannot forge its address",
			remote:  "203.0.113.7:4242",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1"},
			want:    "203.0.113.7",
		},
		{
			name:    "a loopback proxy is trusted",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "a private-network proxy is trusted",
			remote:  "10.0.0.2:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:   "the left-most entry is the original client",
			remote: "127.0.0.1:5555",
			headers: map[string]string{
				"X-Forwarded-For": "198.51.100.9, 10.0.0.5, 10.0.0.6",
			},
			want: "198.51.100.9",
		},
		{
			name:    "X-Real-IP when there is no chain",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Real-IP": "198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:   "a loopback proxy that set no header stays loopback",
			remote: "127.0.0.1:5555",
			want:   "127.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, MCPPath, nil)
			r.RemoteAddr = tt.remote
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := ClientIP(r); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}
