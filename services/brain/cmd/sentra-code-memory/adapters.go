package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// runHTTP serves the canonical local HTTP adapter (Phase 5, issue #35). It
// exposes /health and /dispatch over codeserve.Handle with bounded requests
// and structured errors; JSONL and the direct CLI are unchanged.
// newHTTPServer builds the serving http.Server with the full set of bounds.
//
// Only ReadHeaderTimeout was set. MaxBytesReader bounds a body's size but not
// the time taken to send it, so a client trickling a body held a connection
// and its goroutine indefinitely; a slow response had no bound at all, and an
// idle keep-alive connection was never reclaimed.
//
// It is a named function rather than a literal inside runHTTP so the bounds
// can be asserted without opening a socket and waiting out a minute of them.
func newHTTPServer(listenAddr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

func runHTTP(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	fs.SetOutput(errOut)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address (loopback default)")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request dispatch timeout (0 = no limit)")
	token := fs.String("token", os.Getenv("SENTRA_CODE_MEMORY_HTTP_TOKEN"),
		"bearer token required on every endpoint (env SENTRA_CODE_MEMORY_HTTP_TOKEN)")
	allowInsecure := fs.Bool("allow-insecure", false,
		"permit a non-loopback bind without a bearer token (explicit opt-out)")
	trust := fs.Bool("operator-trust", false,
		"admit mutating verbs without a per-request opt-in header. Off by default; "+
			"env "+operatorTrustEnvVar+"=1.")
	root := fs.String("root", "",
		"confine every request to this subtree (default: the working directory). "+
			"Pass --root=/ to serve the whole filesystem.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "http does not accept positional arguments")
		return 2
	}
	// Explicit local trust (issue #41): fail closed on a non-loopback bind
	// without a bearer token before the socket is opened. Canonicalization also
	// prevents localhost from being resolved again after validation.
	policy := adapters.TrustPolicy{Token: *token, AllowInsecure: *allowInsecure}
	listenAddr, err := adapters.CanonicalListenAddr(*addr, policy)
	if err != nil {
		fmt.Fprintf(errOut, "http: %v\n", err)
		return 2
	}
	pin, code := resolveRootPin(*root, errOut)
	if code != 0 {
		return code
	}
	handler := adapters.NewHTTP(adapters.HTTPConfig{
		Addr: listenAddr, Timeout: *timeout, Token: *token,
		OperatorTrust: operatorTrustRequested(*trust),
		RootPin:       pin,
	})
	server := newHTTPServer(listenAddr, handler)

	// Bind before announcing. ListenAndServe was called inside a goroutine
	// *after* the "listening on ..." line was printed, so a failed bind printed
	// a success banner, wrote the error, and still exited 0 -- which defeats
	// supervisor restart policies and any script grepping for that line.
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(errOut, "http: listen %s: %v\n", listenAddr, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	fmt.Fprintf(out, "sentra-code-memory http listening on %s (health=/health dispatch=/dispatch)\n", listenAddr)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(errOut, "http: %v\n", err)
			return 1
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutCtx)
	return 0
}

// runMCP serves the canonical MCP stdio adapter (Phase 5, issue #35). It reads
// newline-delimited JSON-RPC from stdin and writes responses to stdout,
// dispatching tools/call over codeserve.Handle so behavior matches JSONL/CLI.
func runMCP(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errOut)
	trust := fs.Bool("operator-trust", false,
		"admit mutating verbs (code_apply_changeset, hooks install/uninstall/run) "+
			"on this stdio stream. Off by default: the arguments on this surface are "+
			"authored by a model, so no tool argument can grant it. Env "+
			operatorTrustEnvVar+"=1.")
	root := fs.String("root", "",
		"confine every request to this subtree (default: the working directory). "+
			"Pass --root=/ to serve the whole filesystem, which is what this surface "+
			"did unconditionally before.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "mcp does not accept positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if operatorTrustRequested(*trust) {
		ctx = codeserve.WithOperatorTrust(ctx)
	}
	pin, code := resolveRootPin(*root, errOut)
	if code != 0 {
		return code
	}
	ctx = codeserve.WithRootPin(ctx, pin)
	if err := adapters.ServeMCP(ctx, in, out, errOut); err != nil {
		fmt.Fprintf(errOut, "mcp: %v\n", err)
		return 1
	}
	return 0
}

// resolveRootPin adapts codeserve.ResolveRootFlag to a command's exit-code
// convention. The resolution itself is shared, so the two JSONL surfaces
// cannot drift apart again.
func resolveRootPin(flagValue string, errOut io.Writer) (string, int) {
	pin, err := codeserve.ResolveRootFlag(flagValue)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return "", 2
	}
	return pin, 0
}
