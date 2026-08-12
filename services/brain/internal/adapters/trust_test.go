package adapters_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// authed issues a /dispatch request with the given Authorization header value
// (empty means none) and returns the decoded response and status.
func authed(t *testing.T, h http.Handler, auth, body string) (codeserve.Response, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dispatch", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var resp codeserve.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	return resp, rr.Code
}

func TestHTTPTokenRequiredWhenConfigured(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{Token: "s3cret"})

	// No Authorization → structured 401 with the unauthorized error code.
	resp, code := authed(t, h, "", `{"verb":"ping"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token want 401 got %d (%+v)", code, resp)
	}
	if resp["ok"] != false || resp["error_code"] != string(codeserve.ErrUnauthorized) {
		t.Fatalf("no token not structured unauthorized: %+v", resp)
	}

	// Wrong token → 401.
	_, code = authed(t, h, "Bearer wrong", `{"verb":"ping"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token want 401 got %d", code)
	}

	// Correct token → dispatched normally.
	resp, code = authed(t, h, "Bearer s3cret", `{"verb":"ping"}`)
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("right token want 200 ok, got %d (%+v)", code, resp)
	}
}

func TestHTTPTokenGatesHealthToo(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{Token: "s3cret"})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("health without token want 401 got %d", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("401 must carry WWW-Authenticate Bearer, got %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("health with token want 200 got %d", rr.Code)
	}
}

func TestHTTPNoTokenKeepsLoopbackOpen(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	resp, code := authed(t, h, "", `{"verb":"ping"}`)
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("loopback default must not require a token: %d (%+v)", code, resp)
	}
}

func TestCanonicalListenAddrPinsLocalhostToLoopback(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"localhost:8765", "LOCALHOST:0"} {
		got, err := adapters.CanonicalListenAddr(addr, adapters.TrustPolicy{})
		if err != nil {
			t.Fatalf("canonicalize %q: %v", addr, err)
		}
		if got != "127.0.0.1:"+strings.Split(addr, ":")[1] {
			t.Errorf("canonicalize %q = %q, want numeric loopback", addr, got)
		}
	}
	for _, addr := range []string{"127.0.0.1:8765", "[::1]:8765"} {
		got, err := adapters.CanonicalListenAddr(addr, adapters.TrustPolicy{})
		if err != nil || got != addr {
			t.Errorf("canonicalize numeric loopback %q = %q, %v", addr, got, err)
		}
	}
}

func TestCanonicalListenAddrStillValidatesNonLoopback(t *testing.T) {
	t.Parallel()
	if _, err := adapters.CanonicalListenAddr(":8765", adapters.TrustPolicy{}); err == nil {
		t.Fatal("wildcard without token must be refused")
	}
	got, err := adapters.CanonicalListenAddr(":8765", adapters.TrustPolicy{Token: "s3cret"})
	if err != nil || got != ":8765" {
		t.Fatalf("token-protected wildcard = %q, %v", got, err)
	}
}

func TestValidateListenAddrFailsClosed(t *testing.T) {
	t.Parallel()
	open := adapters.TrustPolicy{}
	token := adapters.TrustPolicy{Token: "s3cret"}
	insecure := adapters.TrustPolicy{AllowInsecure: true}

	// Loopback binds are always allowed, token or not.
	for _, addr := range []string{
		"127.0.0.1:8765", "127.0.0.2:0", "localhost:8765", "[::1]:8765",
	} {
		if err := adapters.ValidateListenAddr(addr, open); err != nil {
			t.Fatalf("loopback %q must be allowed: %v", addr, err)
		}
	}

	// Non-loopback binds without a token are refused.
	for _, addr := range []string{
		"0.0.0.0:8765", ":8765", "192.168.1.10:8765", "[::]:8765",
	} {
		if err := adapters.ValidateListenAddr(addr, open); err == nil {
			t.Fatalf("non-loopback %q without token must be refused", addr)
		}
		// Bearer token or the explicit opt-out unlocks the same bind.
		if err := adapters.ValidateListenAddr(addr, token); err != nil {
			t.Fatalf("non-loopback %q with token must be allowed: %v", addr, err)
		}
		if err := adapters.ValidateListenAddr(addr, insecure); err != nil {
			t.Fatalf("non-loopback %q with allow-insecure must be allowed: %v", addr, err)
		}
	}

	if err := adapters.ValidateListenAddr("not-an-address", open); err == nil {
		t.Fatal("garbage address must be rejected")
	}
}
