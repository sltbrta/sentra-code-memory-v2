package codecrawl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchFSMaxCyclesExits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gob := filepath.Join(root, DefaultIndexFile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := WatchFS(ctx, WatchOptions{
		Root: root, GobPath: gob, Workers: 2, MaxCycles: 1, Debounce: 10 * time.Millisecond,
	})
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("WatchFS max-cycles=1 took too long: %s", time.Since(start))
	}
}
