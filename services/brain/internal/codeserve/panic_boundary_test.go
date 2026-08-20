package codeserve

import (
	"context"
	"strings"
	"testing"
)

// The first version of this test drove nine hostile requests and asserted each
// returned a map with an "ok" key. A fresh-eyes review pointed out that they do
// that without any recover at all -- deleting the entire recover block from
// Handle left the whole package green. It was cited in the ledger as proof of a
// boundary it could not observe.
//
// A panic boundary can only be tested by inducing a panic, so this installs a
// handler that panics. The seam is test-only: panicVerbHook is nil in every
// build, and the switch in Handle never consults it outside this file.

func TestHandleConvertsAHandlerPanicIntoAStructuredError(t *testing.T) {
	previous := panicVerbHook
	panicVerbHook = func() { panic("induced handler defect") }
	t.Cleanup(func() { panicVerbHook = previous })

	resp := Handle(context.Background(), Request{"verb": string(VerbPing)})

	if resp == nil {
		t.Fatal("Handle returned nil after a handler panic")
	}
	if resp["ok"] != false {
		t.Fatalf("ok = %v, want false after a panic: %v", resp["ok"], resp)
	}
	if code, _ := resp["error_code"].(string); code != string(ErrInternal) {
		t.Fatalf("error_code = %q, want %q", code, ErrInternal)
	}
	// The response is the one channel back to a model-facing caller, so it must
	// not carry a stack trace or any host path.
	message, _ := resp["error"].(string)
	if strings.Contains(message, "goroutine") || strings.Contains(message, ".go:") {
		t.Fatalf("panic detail leaked into the response: %q", message)
	}
	if verb, _ := resp["verb"].(string); verb != string(VerbPing) {
		t.Fatalf("verb = %q, want the dispatched verb so a caller can correlate", verb)
	}
}

// TestHandleStillAnswersNormallyWithoutAPanic keeps the boundary from swallowing
// healthy responses.
func TestHandleStillAnswersNormallyWithoutAPanic(t *testing.T) {
	resp := Handle(context.Background(), Request{"verb": string(VerbPing)})
	if resp["ok"] != true {
		t.Fatalf("ping should succeed: %v", resp)
	}
}
