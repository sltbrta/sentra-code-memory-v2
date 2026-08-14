package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCLISupportsSCIPIngestAndSessionRecall(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\nfunc Anchor() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(t.TempDir(), "index.scip.json")
	if err := os.WriteFile(documentPath, []byte(`{
		"toolName":"scip-test",
		"occurrences":[{"range":[1,5,1,11],"symbol":"scheme pkg m Anchor.","symbolRoles":1}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(input []byte, args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := execute(args, bytes.NewReader(input), &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit=%d stderr=%s", args, code, stderr.String())
		}
		var response map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &response); err != nil {
			t.Fatalf("%v response: %v (%s)", args, err, stdout.String())
		}
		return response
	}

	ingested := run(nil, "ingest-scip", "--root", root, "--path", "source.go", "--language", "go", "--scip", documentPath)
	if ingested["ok"] != true || ingested["authority"] != "scip" {
		t.Fatalf("direct SCIP ingest: %+v", ingested)
	}

	recalled := run(nil, "session-recall", "--root", root, "--q", "anything")
	result, _ := recalled["recall"].(map[string]any)
	if recalled["ok"] != true || result["abstained"] != true {
		t.Fatalf("empty local recall must abstain: %+v", recalled)
	}
}

func TestJSONLServesMalformedSCIPAsStructuredError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"verb": "code_ingest_scip", "root": root, "path": "source.go", "document": "{bad",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"serve"}, bytes.NewReader(append(line, '\n')), &stdout, &stderr); code != 0 {
		t.Fatalf("serve exit=%d stderr=%s", code, stderr.String())
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != false || response["error_code"] != "invalid_request" {
		t.Fatalf("malformed SCIP response: %+v", response)
	}
	if _, claimed := response["authority"]; claimed {
		t.Fatalf("malformed SCIP claimed authority: %+v", response)
	}
}
