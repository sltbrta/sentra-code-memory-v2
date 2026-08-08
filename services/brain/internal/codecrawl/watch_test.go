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

func TestWatchFSRefreshesAnEditAfterDebounce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := make(chan Stats, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- WatchFS(ctx, WatchOptions{
			Root:    root,
			GobPath: filepath.Join(root, "cache", DefaultIndexFile),
			Workers: 2, Debounce: 20 * time.Millisecond,
			OnRefresh: func(st Stats, _ bool) { events <- st },
		})
	}()

	select {
	case <-events:
	case err := <-errCh:
		t.Fatalf("watch exited before initial refresh: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for initial refresh")
	}
	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("package main\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-events:
		if st.Changed == 0 && st.Unchanged == 0 {
			t.Fatalf("edit refresh had no crawl accounting: %+v", st)
		}
	case err := <-errCh:
		t.Fatalf("watch exited before edit refresh: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for edit refresh")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop after cancellation")
	}
}
