package codeserve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

func TestCodeReadIsBoundedAndRootSafe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\nline2\nline3\nline4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_read", "root": root, "path": "sample.go",
		"start_line": 2, "max_lines": 2,
	})
	if resp["ok"] != true {
		t.Fatalf("read: %+v", resp)
	}
	content, _ := resp["content"].(string)
	if content != "line2\nline3" || resp["start_line"] != 2 || resp["end_line"] != 3 {
		t.Fatalf("bounded read: %+v", resp)
	}
	if resp["truncated"] != true {
		t.Fatalf("expected truncation metadata: %+v", resp)
	}

	eof := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_read", "root": root, "path": "sample.go", "start_line": 99,
	})
	if eof["ok"] != true || eof["end_line"] != 98 {
		t.Fatalf("past-EOF window: %+v", eof)
	}
	long := strings.Repeat("x", 70<<10) + "\n"
	if err := os.WriteFile(filepath.Join(root, "long.go"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	longRead := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_read", "root": root, "path": "long.go", "max_lines": 1,
	})
	if longRead["ok"] != true || len(longRead["content"].(string)) != len(long)-1 {
		t.Fatalf("long line read: %+v", longRead)
	}

	for _, path := range []string{"../outside.go", "/etc/passwd"} {
		bad := codeserve.Handle(context.Background(), codeserve.Request{
			"verb": "code_read", "root": root, "path": path,
		})
		if bad["ok"] != false {
			t.Fatalf("path escape accepted (%q): %+v", path, bad)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape.go")); err == nil {
		bad := codeserve.Handle(context.Background(), codeserve.Request{
			"verb": "code_read", "root": root, "path": "escape.go",
		})
		if bad["ok"] != false {
			t.Fatalf("symlink escape accepted: %+v", bad)
		}
	}
}

func TestCodeImportsIsLiveExactImportLane(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_imports", "root": root, "q": "fmt", "top_k": 4,
	})
	if resp["ok"] != true {
		t.Fatalf("imports: %+v", resp)
	}
	if resp["verb"] != "code_imports" || resp["search_backend"] != "codeindex" {
		t.Fatalf("imports envelope: %+v", resp)
	}
	if !strings.Contains(string(mustJSON(t, resp["result"])), "fmt") {
		t.Fatalf("fmt import missing: %+v", resp)
	}
}

func TestCodeWatchIsBoundedJSONLAdapter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_watch", "root": root, "index_cache": filepath.Join(root, "cache"),
		"interval_ms": 1, "debounce_ms": 1, "max_cycles": 1, "fsnotify": true,
	})
	if time.Since(started) > 5*time.Second {
		t.Fatal("bounded watch exceeded test deadline")
	}
	if resp["ok"] != true {
		t.Fatalf("watch: %+v", resp)
	}
	if resp["events"] == nil {
		t.Fatalf("watch events missing: %+v", resp)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
