package adapters_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
)

// ServeMCP called readMCPLine inline and only checked ctx after a full line had
// been handled, so an idle server never observed cancellation. Combined with
// signal.NotifyContext -- which disables Go's default die-on-SIGINT -- the
// process could not be stopped by Ctrl-C or SIGTERM at all. Verified during the
// audit: SIGINT left it running and only SIGKILL ended it, so an MCP host
// stopping a server with SIGTERM leaked an orphan holding stdin and stdout.

// blockingReader never returns data and never reaches EOF, standing in for an
// idle stdin pipe held open by a parent process.
type blockingReader struct{ release chan struct{} }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func TestServeMCPReturnsWhenTheContextIsCancelledWhileIdle(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- adapters.ServeMCP(ctx, blockingReader{release: release}, &out, &errOut)
	}()

	// Let the loop reach its blocked read before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeMCP returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeMCP did not return after cancellation: an idle stdio server cannot be stopped")
	}
}
