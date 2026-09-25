package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Per-IP rate limiting, for a surface that has no authentication.
//
// The MCP endpoint is deliberately open: the data is a public blockchain, and
// an API key would make it useless to the agents it exists for. Rate limiting
// per IP is the trade that makes no-auth safe.
//
// Two limits, not one, because they fail differently. A requests-per-minute
// cap keeps a polling agent honest; a concurrency cap keeps one client from
// holding every database connection at once. The second is the one that
// actually protects the instance, and it is the one usually forgotten: sixty
// requests a minute is a gentle number right up until all sixty are in flight
// together against a realm-state read that takes twelve seconds.

// IPLimiter bounds request rate and concurrency per client address.
//
// The zero value is not usable; construct with NewIPLimiter.
type IPLimiter struct {
	perMinute  int
	concurrent int

	// now is the clock, injectable so the tests do not sleep through a
	// refill window.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*ipBucket
	// lastSweep bounds the size of buckets. An open endpoint on the public
	// internet sees a long tail of one-shot addresses, and a map keyed by
	// remote IP with no eviction is a slow leak that only shows up in
	// production.
	lastSweep time.Time
}

type ipBucket struct {
	// tokens is a fractional allowance refilled continuously rather than in
	// steps, so a client is never told to wait a whole window for the one
	// request it is over by.
	tokens   float64
	refilled time.Time
	inFlight int
	seen     time.Time
}

// ipBucketTTL is how long an idle bucket is kept. Longer than any refill
// window, so an eviction can never hand somebody a fresh allowance sooner
// than waiting would have.
const ipBucketTTL = 10 * time.Minute

// NewIPLimiter builds a limiter allowing perMinute requests per minute and
// concurrent simultaneous requests from any one address.
//
// A non-positive bound means unlimited, which is what the tests and a local
// single-user run want; it is not a configuration the shipped defaults use.
func NewIPLimiter(perMinute, concurrent int) *IPLimiter {
	return &IPLimiter{
		perMinute:  perMinute,
		concurrent: concurrent,
		now:        time.Now,
		buckets:    make(map[string]*ipBucket),
	}
}

// Acquire takes one request's worth of allowance for ip.
//
// On success it returns a release function the caller must call when the
// request is done, which is what frees the concurrency slot. On refusal it
// returns how long the caller should wait, which becomes the Retry-After
// header: a 429 with no hint of when to come back invites an immediate retry,
// which is the behaviour the limit exists to prevent.
func (l *IPLimiter) Acquire(ip string) (release func(), retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	b := l.buckets[ip]
	if b == nil {
		b = &ipBucket{tokens: float64(l.perMinute), refilled: now}
		l.buckets[ip] = b
	}
	b.seen = now

	if l.perMinute > 0 {
		rate := float64(l.perMinute) / 60.0 // tokens per second
		b.tokens += now.Sub(b.refilled).Seconds() * rate
		if b.tokens > float64(l.perMinute) {
			b.tokens = float64(l.perMinute)
		}
		b.refilled = now
		if b.tokens < 1 {
			// Round up: a Retry-After of zero seconds is an invitation to
			// retry immediately, which is exactly what was just refused.
			wait := time.Duration((1-b.tokens)/rate*float64(time.Second)) + time.Second
			return nil, wait.Truncate(time.Second), false
		}
	}

	// Concurrency is checked after the rate, and the token is only spent once
	// both pass. A request refused for concurrency has not consumed anything:
	// the client is not over its rate, it is merely early, and charging it
	// would turn a burst into a rate ban.
	if l.concurrent > 0 && b.inFlight >= l.concurrent {
		return nil, time.Second, false
	}

	if l.perMinute > 0 {
		b.tokens--
	}
	b.inFlight++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if b.inFlight > 0 {
				b.inFlight--
			}
		})
	}, 0, true
}

// sweepLocked drops buckets nobody has used in a while. Called on the request
// path rather than from a goroutine: the map only grows when requests arrive,
// so that is the only moment it can need shrinking, and a background ticker on
// an idle process is work for nothing.
func (l *IPLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for ip, b := range l.buckets {
		if b.inFlight == 0 && now.Sub(b.seen) > ipBucketTTL {
			delete(l.buckets, ip)
		}
	}
}

// ClientIP is the address a limit should be keyed on.
//
// RemoteAddr is the honest answer and the only one that cannot be forged, but
// behind a reverse proxy it is the proxy: every caller in the world would
// share one bucket, and the first busy agent would rate-limit everybody else
// off the endpoint. That is not hypothetical here, the deployed instance sits
// behind one.
//
// So the forwarded chain is trusted only when the immediate peer is loopback
// or a private address, which is where a proxy of ours can be and an internet
// client cannot. A client reaching us directly can put whatever it likes in
// X-Forwarded-For and it is ignored.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isLocalPeer(host) {
		return host
	}
	// The left-most entry is the original client. Everything to its right was
	// appended by intermediaries, and the whole list is forgeable, which is
	// why this branch is gated on the peer above.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return host
}

func isLocalPeer(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		// A unix socket or a test's fake address. Treated as local: nothing
		// reaches those from the internet.
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
