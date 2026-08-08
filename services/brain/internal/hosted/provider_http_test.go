package hosted

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #290: provider clients must share one pooled transport while keeping
// timeouts strictly per call and carrying zero cross-request state.
func TestProviderHTTPClientsShareOnePooledTransport(t *testing.T) {
	a := providerHTTPClient(1 * time.Second)
	b := providerHTTPClient(2 * time.Second)
	if a == b {
		t.Fatal("per-call clients must be independent values")
	}
	if a.Timeout != 1*time.Second || b.Timeout != 2*time.Second {
		t.Fatalf("per-call timeouts not preserved: a=%v b=%v", a.Timeout, b.Timeout)
	}
	if a.Transport != b.Transport {
		t.Fatal("provider clients must share one transport")
	}
	if a.Transport != http.RoundTripper(sharedProviderTransport) {
		t.Fatal("provider clients must use sharedProviderTransport")
	}
	if a.Jar != nil || b.Jar != nil {
		t.Fatal("provider clients must not carry cookie state")
	}
	if sharedProviderTransport == http.DefaultTransport {
		t.Fatal("shared transport must not mutate http.DefaultTransport")
	}
	if got := sharedProviderTransport.MaxIdleConnsPerHost; got < 8 {
		t.Fatalf("MaxIdleConnsPerHost=%d; pool too small for provider fan-out", got)
	}
	if sharedProviderTransport.IdleConnTimeout <= 0 {
		t.Fatal("idle connections must expire")
	}
	// Building a provider transport must not mutate the process-wide
	// http.DefaultTransport. Compare its pool limits before and after the call
	// rather than asserting Go's stock values, which are an implementation
	// detail that may change between Go releases.
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		type poolLimits struct {
			maxIdleConns        int
			maxIdleConnsPerHost int
			idleConnTimeout     time.Duration
		}
		snapshot := func() poolLimits {
			return poolLimits{
				maxIdleConns:        dt.MaxIdleConns,
				maxIdleConnsPerHost: dt.MaxIdleConnsPerHost,
				idleConnTimeout:     dt.IdleConnTimeout,
			}
		}
		before := snapshot()
		fresh := newProviderTransport()
		if after := snapshot(); after != before {
			t.Fatalf("newProviderTransport mutated http.DefaultTransport: before=%+v after=%+v", before, after)
		}
		if fresh == dt {
			t.Fatal("newProviderTransport must return a clone, not http.DefaultTransport itself")
		}
		if fresh.MaxIdleConnsPerHost == before.maxIdleConnsPerHost {
			t.Fatalf("provider transport did not widen the idle pool: %d", fresh.MaxIdleConnsPerHost)
		}
	}
}

// Redirects are fail-closed: a 3xx must be returned to the caller, never
// followed, so per-request BYOK credentials cannot reach a Location host.
func TestProviderClientsDoNotFollowRedirects(t *testing.T) {
	var redirectTargetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHits.Add(1)
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer byok-key")
	resp, err := providerHTTPClient(2 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("redirect response must be returned, not error: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d; want the 302 surfaced verbatim", resp.StatusCode)
	}
	if got := redirectTargetHits.Load(); got != 0 {
		t.Fatalf("redirect target was contacted %d times; credentials must never be replayed", got)
	}
}

func TestProviderCallsReuseWarmConnections(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		// A fresh client per call mirrors the provider call sites; reuse must
		// come from the shared transport, not from a long-lived client value.
		resp, err := providerHTTPClient(2 * time.Second).Do(req)
		if err != nil {
			cancel()
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("dialed %d connections for 5 sequential provider calls; want 1 (pooled reuse)", got)
	}
}

func TestProviderTimeoutIsPerRequestAndDoesNotPoisonPool(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })

	started := time.Now()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := providerHTTPClient(100 * time.Millisecond).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("slow provider call must fail its own deadline")
	}
	var uerr *url.Error
	if ok := errors.As(err, &uerr); !ok || !uerr.Timeout() {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("per-request timeout not enforced: %v", elapsed)
	}

	// The shared pool must stay healthy for the next call after a timeout.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/fast", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := providerHTTPClient(2 * time.Second).Do(req2)
	if err != nil {
		t.Fatalf("follow-up call on shared transport failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status=%d", resp2.StatusCode)
	}
	once.Do(func() { close(release) })
}

func TestProviderCallsDoNotLeakStateAcrossRequests(t *testing.T) {
	type seen struct {
		auth   string
		cookie string
	}
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{auth: r.Header.Get("Authorization"), cookie: r.Header.Get("Cookie")})
		mu.Unlock()
		// A misbehaving shared client with a jar would replay this.
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "tenant-a"})
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	for _, key := range []string{"key-a", "key-b"} {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := providerHTTPClient(2 * time.Second).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("server saw %d requests; want 2", len(got))
	}
	if got[0].auth != "Bearer key-a" || got[1].auth != "Bearer key-b" {
		t.Fatalf("per-request auth not isolated: %+v", got)
	}
	if got[0].cookie != "" || got[1].cookie != "" {
		t.Fatalf("cookie state leaked across provider calls: %+v", got)
	}
}
