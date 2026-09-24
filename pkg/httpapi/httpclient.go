package httpapi

import (
	"net"
	"net/http"
	"time"
)

// One connection pool for every outbound HTTP call this package makes.
//
// Each call site used to build its own *http.Client inline. A fresh
// *http.Client with no Transport set does not share anything: it falls back to
// http.DefaultTransport, whose MaxIdleConnsPerHost is 2, and the ones that did
// set a Transport built their own pool and threw it away when the call
// returned. Either way a burst of ABCI queries against one node spends most of
// its time in handshakes.
//
// Measured against rpc.gno.land on 2026-09-24, one vm/qrender:
//
//	new connection   0.46-0.53s, of which ~0.18s is the TLS handshake
//	reused connection 0.29s
//
// 40% off every query, and the govdao overview alone makes dozens of them
// (three renders for the list, two per proposal, one per author name).
//
// MaxIdleConnsPerHost is the field that matters here and the reason the default
// is not good enough: this package fans out one goroutine per proposal against
// a single host, so a pool that keeps 2 idle connections has the rest of the
// fan-out opening fresh ones anyway.
var sharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	// Sized to the widest fan-out in the package: one goroutine per govdao
	// proposal, each making two or three queries against the same RPC node.
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ForceAttemptHTTP2:     true,
}

// sharedClient returns a client with its own timeout over the shared pool.
//
// The timeout stays per-call-site on purpose: a gnockpit status page and a
// realm screenshot have nothing to say to each other about how long is too
// long. Only the connections are shared.
func sharedClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: sharedTransport}
}
