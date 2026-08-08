package hosted

import (
	"net/http"
	"time"
)

// Issue #290: all hosted provider HTTP calls (synthesis, embed, rerank,
// multiquery, Qdrant, FAISS sidecar) share one pooled transport so repeated
// calls reuse warm TCP/TLS connections instead of re-dialing per request.
//
// Safety invariants:
//   - Deadlines stay per-request: every call site keeps its own context
//     deadline and/or per-call client Timeout; the shared transport never
//     extends or overrides them.
//   - No cross-request state: the transport carries only the connection pool
//     and TLS session cache. Clients built here have no cookie jar, and BYOK
//     secrets remain per-request headers that are never stored on the
//     transport.
//   - Redirects are fail-closed: CheckRedirect returns http.ErrUseLastResponse
//     so a 3xx from a provider is surfaced verbatim instead of being followed.
//     Go's default policy would replay the request against the Location host,
//     which for a hijacked or misconfigured provider endpoint means forwarding
//     BYOK Authorization headers to an unvetted host. Provider APIs are
//     fixed-endpoint, so never redirecting costs nothing.
//   - Provider ordering, cooldowns, hedging, and ledger diagnostics are
//     unchanged; only the dial/reuse layer is shared.
var sharedProviderTransport = newProviderTransport()

// newProviderTransport clones http.DefaultTransport (keeping proxy, dialer,
// and HTTP/2 defaults) and widens the idle pool. DefaultTransport keeps only
// 2 idle conns per host, which forces TLS re-handshakes when one vendor host
// serves hedged synthesis plus parallel multiquery/rerank traffic.
func newProviderTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Defensive: DefaultTransport was replaced with a non-*http.Transport;
		// fall back to an explicit pooled transport with equivalent limits.
		return &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}
	}
	t := base.Clone()
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 16
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// providerHTTPClient returns a client bound to the shared pooled transport
// with a per-call overall timeout. The client struct is a cheap stateless
// wrapper (no Jar, and a stateless fail-closed redirect policy); the pooled
// transport underneath is the shared piece. timeout <= 0 means "context
// deadline only".
func providerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     sharedProviderTransport,
		Timeout:       timeout,
		CheckRedirect: noRedirect,
	}
}

// noRedirect stops the client before it follows a 3xx, returning the redirect
// response itself (with no error) to the caller. This keeps per-request BYOK
// credentials from ever being replayed to a Location host the caller did not
// choose.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
