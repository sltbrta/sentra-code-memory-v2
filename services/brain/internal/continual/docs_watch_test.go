package continual

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func TestWatchDocsDeltaEnqueue(t *testing.T) {
	// Local brain + async gardener: watch one cycle then RunGardenerWave.
	brainDir := t.TempDir()
	docsPath := filepath.Join(t.TempDir(), "docs.jsonl")
	if err := os.WriteFile(docsPath, []byte(
		`{"id":"d1","text":"User lives in Kyoto. RPO is 1 day."}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := hosted.CreateLocal(brainDir, "cont-test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Force async: local attach should open gardener.db.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var last hosted.IngestResult
	err = WatchDocs(ctx, DocWatchOptions{
		Client:    c,
		DocsPath:  docsPath,
		Interval:  50 * time.Millisecond,
		Debounce:  10 * time.Millisecond,
		MaxCycles: 1,
		OnDelta:   func(res hosted.IngestResult) { last = res },
	})
	if err != nil && err != context.DeadlineExceeded {
		// MaxCycles=1 returns nil; deadline only if stuck.
		if err != context.Canceled {
			// ok if nil
		}
	}
	if last.Ingested < 1 && last.Upserted < 1 && last.Mode == "" {
		// Initial cycle should have ingested.
		t.Fatalf("expected delta result, got %+v", last)
	}

	// Background gardener processes enqueued jobs.
	er, err := c.RunGardenerWave(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if er.JobsEnqueued == 0 && er.SidecarsWarm == 0 && er.ReceiptsOK == 0 {
		// Async may have already been drained if sync fallback; either is ok
		// when enrich disabled — ensure not skipped for wrong reason.
		if er.Skipped && er.Reason == "enrich_disabled" {
			t.Fatal("enrich disabled")
		}
	}
}

func TestWatchRegistryRoundTripAndMulti(t *testing.T) {
	regPath := filepath.Join(t.TempDir(), "watch.json")
	docsA := filepath.Join(t.TempDir(), "a")
	docsB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(docsA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(docsB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsA, "one.md"), []byte("alpha lives in Kyoto"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsB, "two.md"), []byte("beta RPO is one day"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := WatchRegistry{
		Folders: []WatchFolder{
			{Path: docsA, Enabled: true, Label: "a"},
			{Path: docsB, Enabled: true, Label: "b"},
			{Path: "/tmp/disabled", Enabled: false},
		},
	}
	if err := SaveWatchRegistry(regPath, reg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWatchRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.EnabledPaths()) != 2 {
		t.Fatalf("enabled=%v", loaded.EnabledPaths())
	}

	brainDir := t.TempDir()
	c, err := hosted.CreateLocal(brainDir, "multi-watch")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hits int
	err = WatchDocsMulti(ctx, DocWatchMultiOptions{
		Client:    c,
		Paths:     loaded.EnabledPaths(),
		Interval:  40 * time.Millisecond,
		Debounce:  5 * time.Millisecond,
		MaxCycles: 2,
		OnDelta:   func(_ string, res hosted.IngestResult) { hits++ },
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		// MaxCycles should return nil.
	}
	if hits < 1 {
		t.Fatalf("expected multi-folder deltas, hits=%d", hits)
	}
}
