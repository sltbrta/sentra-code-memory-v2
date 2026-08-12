package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
)

// runHTTP serves the canonical local HTTP adapter (Phase 5, issue #35). It
// exposes /health and /dispatch over codeserve.Handle with bounded requests
// and structured errors; JSONL and the direct CLI are unchanged.
func runHTTP(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	fs.SetOutput(errOut)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address (loopback default)")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request dispatch timeout (0 = no limit)")
	token := fs.String("token", os.Getenv("SENTRA_CODE_MEMORY_HTTP_TOKEN"),
		"bearer token required on every endpoint (env SENTRA_CODE_MEMORY_HTTP_TOKEN)")
	allowInsecure := fs.Bool("allow-insecure", false,
		"permit a non-loopback bind without a bearer token (explicit opt-out)")
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
	handler := adapters.NewHTTP(adapters.HTTPConfig{Addr: listenAddr, Timeout: *timeout, Token: *token})
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		fmt.Fprintf(out, "sentra-code-memory http listening on %s (health=/health dispatch=/dispatch)\n", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(errOut, "http: %v\n", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "mcp does not accept positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := adapters.ServeMCP(ctx, in, out, errOut); err != nil {
		fmt.Fprintf(errOut, "mcp: %v\n", err)
		return 1
	}
	return 0
}
