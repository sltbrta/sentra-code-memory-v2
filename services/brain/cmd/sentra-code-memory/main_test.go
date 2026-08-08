package main

import (
	"bytes"
	"encoding/json"
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
	if code := execute([]string{"serve"}, input, &stdout, &stderr); code != 0 {
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

func TestServeRejectsMalformedJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"serve"}, bytes.NewBufferString("not-json\n"), &stdout, &stderr); code == 0 {
		t.Fatalf("malformed request unexpectedly succeeded: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}
