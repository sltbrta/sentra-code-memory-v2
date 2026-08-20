package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogAndJSONLServe(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"catalog"}, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("catalog exit=%d stderr=%s", code, stderr.String())
	}
	var catalog map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("catalog is not JSON: %v (%s)", err, stdout.String())
	}
	if catalog["ok"] != true {
		t.Fatalf("catalog response: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	input := bytes.NewBufferString(`{"verb":"ping"}
{"verb":"unknown"}
`)
	if code := execute([]string{"serve", "--root=/"}, input, &stdout, &stderr); code != 0 {
		t.Fatalf("serve exit=%d stderr=%s", code, stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("expected two JSONL responses, got %d: %s", len(lines), stdout.String())
	}
	var ping map[string]any
	if err := json.Unmarshal(lines[0], &ping); err != nil || ping["ok"] != true {
		t.Fatalf("ping response: %s", lines[0])
	}
	var denied map[string]any
	if err := json.Unmarshal(lines[1], &denied); err != nil || denied["ok"] != false {
		t.Fatalf("unknown response: %s", lines[1])
	}
}

func TestMLXEnvUsesRecommendedTinyModel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"mlx", "env"}, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("mlx env exit=%d stderr=%s", code, stderr.String())
	}
	for _, model := range []string{"LFM2.5-VL-1.6B-8bit", "gemma-4-e2b-it-4bit", "Qwen3-VL-Embedding-2B-4bit", "Qwen3-VL-Reranker-2B-4bit"} {
		if !bytes.Contains(stdout.Bytes(), []byte(model)) {
			t.Fatalf("mlx env omitted recommended model %q: %s", model, stdout.String())
		}
	}
}

func TestWatchRunsOneRefreshAndReportsQueueMetrics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"watch", "--root", root, "--max-cycles", "1", "--debounce", "1ms"}, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("watch exit=%d stderr=%s", code, stderr.String())
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &event); err != nil {
		t.Fatalf("watch event is not JSON: %v (%s)", err, stdout.String())
	}
	if event["event"] != "refresh" {
		t.Fatalf("watch event=%s", stdout.String())
	}
}

func TestServeRejectsMalformedJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"serve", "--root=/"}, bytes.NewBufferString("not-json\n"), &stdout, &stderr); code == 0 {
		t.Fatalf("malformed request unexpectedly succeeded: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}
