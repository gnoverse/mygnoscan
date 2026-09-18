package syncer

import (
	"sync"
	"time"
)

// Whether our sync passes are succeeding, per network.
//
// Distinct from chain liveness, which the sanity page already reports and which
// answers a different question: liveness is about the chain producing blocks,
// this is about us being able to read it. The two come apart, and when they do
// the page said everything was fine. gno.land's mainnet indexer once rejected
// every query mygnoscan sent it for more than a day while the sanity page
// reported the chain alive and reachable, because a fallback endpoint in the
// same pool was answering and nothing tracked that the primary had stopped.
//
// Nothing here is persisted. It describes this process's experience since it
// started, which is the honest scope: a restart has no idea whether the
// previous one was failing.

// NetworkHealth is one network's sync record.
type NetworkHealth struct {
	// LastAttemptAt and LastSuccessAt are RFC 3339, or empty when it has not
	// happened yet. Strings rather than time.Time so the zero value serialises
	// as absent instead of as year 1.
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	// LastError is the message from the most recent failed pass, and it stays
	// after a later pass succeeds: a network flapping between success and
	// failure is worth seeing, and clearing the text on every success would
	// hide exactly that. LastErrorAt says when it was, so a stale message
	// cannot be mistaken for a current one.
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt string `json:"last_error_at,omitempty"`
	// ConsecutiveFailures is 0 whenever the most recent pass succeeded, which
	// is what "is it broken right now" should be read from.
	ConsecutiveFailures int `json:"consecutive_failures"`
	Passes              int `json:"passes"`
	Failures            int `json:"failures"`
	// Healthy is false once a pass has failed and no pass has succeeded since.
	// A network that has never run a pass is healthy: there is nothing to
	// report, and defaulting to alarm would make the page cry wolf on startup
	// and whenever sync is switched off.
	Healthy bool `json:"healthy"`
}

// Registry collects NetworkHealth across the sync goroutines, one per network.
type Registry struct {
	mu    sync.RWMutex
	state map[string]NetworkHealth
	now   func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{state: map[string]NetworkHealth{}, now: time.Now}
}

// Record files the outcome of one sync pass. A nil error is a success.
func (r *Registry) Record(network string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	at := r.now().UTC().Format(time.RFC3339)
	h := r.state[network]
	h.LastAttemptAt = at
	h.Passes++
	if err == nil {
		h.LastSuccessAt = at
		h.ConsecutiveFailures = 0
		h.Healthy = true
	} else {
		h.LastError = err.Error()
		h.LastErrorAt = at
		h.ConsecutiveFailures++
		h.Failures++
		h.Healthy = false
	}
	r.state[network] = h
}

// Snapshot copies the current state. A copy rather than the live map: the
// caller is an HTTP handler serialising it while the sync goroutines keep
// writing.
func (r *Registry) Snapshot() map[string]NetworkHealth {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]NetworkHealth, len(r.state))
	for k, v := range r.state {
		out[k] = v
	}
	return out
}
