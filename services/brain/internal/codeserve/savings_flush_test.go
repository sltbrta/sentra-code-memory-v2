package codeserve

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
)

// Steps are queued and written every savingsFlushEvery, which bounds what a
// crash loses. A *graceful* exit should lose nothing, and without a flush it
// lost everything queued since the last write -- so a short-lived process that
// answered a handful of queries recorded none of them, which is
// indistinguishable from the no-producer bug the ledger producer fixed.

func TestAGracefulExitFlushesQueuedSavings(t *testing.T) {
	root, cache := savingsRepo(t)
	ctx := context.Background()

	if resp := Handle(ctx, Request{
		"verb": "code_index", "root": root, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	// Fewer searches than the flush threshold, so everything is still queued.
	const searches = 3
	if searches >= savingsFlushEvery {
		t.Fatalf("the fixture must stay under the flush threshold (%d)", savingsFlushEvery)
	}
	for i := 0; i < searches; i++ {
		if resp := Handle(ctx, Request{
			"verb": "code_search", "root": root, "index_cache": cache,
			"q": "ValidateToken", "top_k": 5,
		}); resp["ok"] != true {
			t.Fatalf("search: %v", resp)
		}
	}

	// Read the file directly: openSavingsLedgerForRead would flush, which is
	// the behaviour under test rather than a way to observe it.
	ledger, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	if steps := ledger.Summary().Steps; steps != 0 {
		t.Fatalf("expected the steps to still be queued, found %d on disk", steps)
	}

	FlushPendingSavings()

	flushed, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	if steps := flushed.Summary().Steps; steps != searches {
		t.Fatalf("a graceful flush wrote %d steps, want %d: work already done "+
			"is lost when the process exits", steps, searches)
	}
}

// TestFlushIsSafeWithNothingQueued keeps the shutdown path from being able to
// fail on an idle process.
func TestFlushIsSafeWithNothingQueued(t *testing.T) {
	FlushPendingSavings()
	FlushPendingSavings()
}
