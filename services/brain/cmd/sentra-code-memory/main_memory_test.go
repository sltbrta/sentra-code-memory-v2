package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestCLIMemorySessionSavingsOperators exercises the new bounded local
// operators (issue #47) through the direct CLI path, proving the CLI surface
// reaches the same codeserve handlers as JSONL/MCP.
func TestCLIMemorySessionSavingsOperators(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := execute(args, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit=%d stderr=%s", args, code, stderr.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
			t.Fatalf("%v not JSON: %v (%s)", args, err, stdout.String())
		}
		return resp
	}

	// memory_put (admit).
	put := run("memory-put", "--dir", dir, "--principal", "alice",
		"--kind", "fact", "--tier", "stm", "--text", "build uses bazel", "--tags", "build,ci")
	if put["ok"] != true {
		t.Fatalf("memory-put: %v", put)
	}
	entry, _ := put["entry"].(map[string]any)
	if entry == nil || entry["id"] == "" {
		t.Fatalf("memory-put entry: %v", put)
	}

	// memory_search (typed recall).
	search := run("memory-search", "--dir", dir, "--principal", "alice", "--q", "bazel")
	if search["ok"] != true {
		t.Fatalf("memory-search: %v", search)
	}
	if n, _ := search["count"].(float64); n != 1 {
		t.Fatalf("memory-search count=%v want 1: %v", search["count"], search)
	}

	// memory_list.
	list := run("memory-list", "--dir", dir, "--principal", "alice")
	if list["ok"] != true {
		t.Fatalf("memory-list: %v", list)
	}

	// session_continuation on an empty dir still succeeds.
	cont := run("session-continuation", "--dir", dir, "--now", "2026-08-13T12:00:00Z")
	if cont["ok"] != true {
		t.Fatalf("session-continuation: %v", cont)
	}

	// savings_summary on an empty ledger.
	sav := run("savings-summary", "--dir", dir)
	if sav["ok"] != true {
		t.Fatalf("savings-summary: %v", sav)
	}
}

// TestCLIDeferredDisclosure proves a deferred verb returns a structured 501-
// class disclosure through the CLI rather than an opaque unknown-verb error.
func TestCLIDeferredDisclosure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"serve", "--root=/"}, bytes.NewBufferString(
		`{"verb":"session_product"}`+"\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("serve exit=%d stderr=%s", code, stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != false {
		t.Fatalf("deferred verb must not succeed: %v", resp)
	}
	if resp["error_code"] != "deferred" {
		t.Fatalf("error_code=%v want deferred: %v", resp["error_code"], resp)
	}
	if resp["deferred"] != true || resp["decision"] == "" {
		t.Fatalf("missing disclosure fields: %v", resp)
	}
}

// TestCLIAliasesCoverNewVerbs ensures every new operator is reachable by its
// documented CLI alias.
func TestCLIAliasesCoverNewVerbs(t *testing.T) {
	want := map[string]string{
		"memory-put":           "memory_put",
		"memory-search":        "memory_search",
		"memory-list":          "memory_list",
		"memory-promote":       "memory_promote",
		"session-continuation": "session_continuation",
		"savings-summary":      "savings_summary",
	}
	for alias, verb := range want {
		got, ok := aliases[alias]
		if !ok {
			t.Fatalf("alias %q missing", alias)
		}
		if got != verb {
			t.Fatalf("alias %q -> %q want %q", alias, got, verb)
		}
	}
	// Help text advertises the new operators.
	var stdout bytes.Buffer
	writeHelp(&stdout)
	for _, token := range []string{"memory-put", "session-continuation", "savings-summary"} {
		if !strings.Contains(stdout.String(), token) {
			t.Fatalf("help text omits %q", token)
		}
	}
}
