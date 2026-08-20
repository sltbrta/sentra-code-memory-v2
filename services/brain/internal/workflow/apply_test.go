package workflow_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

func TestApplyChangeSetPromotesCompleteCandidate(t *testing.T) {
	root := t.TempDir()
	before := []byte("alpha beta\n")
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	after := []byte("alpha BETA\n")
	base := workflow.Digest(before)
	cs := workflow.ChangeSet{Base: "tree-1", BaseDigests: map[string]string{"a.txt": base}, Edits: []workflow.CandidateEdit{{
		Path: "a.txt", Range: workflow.EditRange{Start: 6, End: 10}, Replacement: "BETA",
		BaseDigest: base, PredictedDigest: workflow.Digest(after),
		// Changed deliberately: the previous form used shell command
		// substitution, which the verification gate no longer provides. This
		// argv form asserts the same thing -- the verifier sees the post-edit
		// content in the staged tree.
	}}, VerificationCommands: []string{"grep -q \"alpha BETA\" a.txt"}}
	receipt, err := workflow.ApplyChangeSet(context.Background(), root, cs, workflow.ApplyOptions{})
	if err != nil || !receipt.Applied {
		t.Fatalf("apply: err=%v receipt=%+v", err, receipt)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(after) {
		t.Fatalf("got %q", got)
	}
	if receipt.Validation.Digest == "" || receipt.BeforeDigest == receipt.AfterDigest {
		t.Fatalf("bad receipt: %+v", receipt)
	}
}

func TestApplyChangeSetFailsClosedAndRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*workflow.ChangeSet)
		opts   workflow.ApplyOptions
	}{
		{name: "stale", mutate: func(cs *workflow.ChangeSet) { cs.BaseDigests["a.txt"] = "stale" }},
		{name: "overlap", mutate: func(cs *workflow.ChangeSet) { cs.Edits = append(cs.Edits, cs.Edits[0]) }},
		{name: "partial", mutate: func(cs *workflow.ChangeSet) { cs.Edits[0].PredictedDigest = "wrong" }},
		{name: "verification", mutate: func(cs *workflow.ChangeSet) { cs.VerificationCommands = []string{"false"} }},
		{name: "staging failure", opts: workflow.ApplyOptions{InjectFailureAt: workflow.FailAfterStage}},
		{name: "promotion failure", opts: workflow.ApplyOptions{InjectFailureAt: workflow.FailDuringPromotion}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			before := []byte("alpha beta\n")
			path := filepath.Join(root, "a.txt")
			_ = os.WriteFile(path, before, 0o644)
			base := workflow.Digest(before)
			after := []byte("alpha BETA\n")
			cs := workflow.ChangeSet{Base: "tree-1", BaseDigests: map[string]string{"a.txt": base}, Edits: []workflow.CandidateEdit{{Path: "a.txt", Range: workflow.EditRange{Start: 6, End: 10}, Replacement: "BETA", BaseDigest: base, PredictedDigest: workflow.Digest(after)}}}
			if tc.mutate != nil {
				tc.mutate(&cs)
			}
			receipt, err := workflow.ApplyChangeSet(context.Background(), root, cs, tc.opts)
			if err == nil || receipt.Applied {
				t.Fatalf("must fail closed: err=%v receipt=%+v", err, receipt)
			}
			got, _ := os.ReadFile(path)
			if string(got) != string(before) {
				t.Fatalf("partial tree: %q", got)
			}
		})
	}
}

func TestApplyChangeSetVerifiesGitHead(t *testing.T) {
	root := t.TempDir()
	before := []byte("alpha beta\n")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), before, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
	base := workflow.Digest(before)
	after := []byte("alpha BETA\n")
	cs := workflow.ChangeSet{Base: head, BaseDigests: map[string]string{"a.txt": base}, Edits: []workflow.CandidateEdit{{Path: "a.txt", Range: workflow.EditRange{Start: 6, End: 10}, Replacement: "BETA", BaseDigest: base, PredictedDigest: workflow.Digest(after)}}}
	if receipt, err := workflow.ApplyChangeSet(context.Background(), root, cs, workflow.ApplyOptions{}); err != nil || !receipt.Applied {
		t.Fatalf("git apply: %v %+v", err, receipt)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != string(after) {
		t.Fatalf("got %q", got)
	}
}

func TestApplyChangeSetRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	_ = os.WriteFile(outside, []byte("safe"), 0o644)
	cs := workflow.ChangeSet{Base: "tree", BaseDigests: map[string]string{"../outside.txt": workflow.Digest([]byte("safe"))}, Edits: []workflow.CandidateEdit{{Path: "../outside.txt", Range: workflow.EditRange{Start: 0, End: 4}, Replacement: "oops", BaseDigest: workflow.Digest([]byte("safe")), PredictedDigest: workflow.Digest([]byte("oops"))}}}
	if _, err := workflow.ApplyChangeSet(context.Background(), root, cs, workflow.ApplyOptions{}); err == nil {
		t.Fatal("escape accepted")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "safe" {
		t.Fatalf("outside changed: %q", got)
	}
}
