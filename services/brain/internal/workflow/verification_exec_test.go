package workflow_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

// The verification gate used to run every command through `/bin/sh -c` with
// the parent environment inherited. Combined with the missing dispatch gate
// that made it reachable, that was arbitrary code execution driven by a JSON
// field. The dispatch gate now stands in front of it, but defence in depth
// matters here: an operator who legitimately grants trust to a warm `serve`
// stream is granting "run my project's tests", not "run anything at all".
//
// The contract is now an argv vector against a fixed allowlist: no shell, no
// metacharacters, no inherited environment.

func stagedChangeSet(t *testing.T, root string, commands ...string) workflow.ChangeSet {
	t.Helper()
	before := []byte("alpha BETA\n")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), before, 0o644); err != nil {
		t.Fatal(err)
	}
	after := []byte("alpha GAMMA\n")
	return workflow.ChangeSet{
		Base:        "tree",
		BaseDigests: map[string]string{"a.txt": workflow.Digest(before)},
		Edits: []workflow.CandidateEdit{{
			Path:            "a.txt",
			Range:           workflow.EditRange{Start: 6, End: 10},
			Replacement:     "GAMMA",
			BaseDigest:      workflow.Digest(before),
			PredictedDigest: workflow.Digest(after),
		}},
		VerificationCommands: commands,
	}
}

// TestVerificationRefusesShellMetacharacters is the regression test for the
// proven remote code execution. The command below is meaningless as an argv
// vector and only does anything under a shell.
func TestVerificationRefusesShellMetacharacters(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")

	for _, command := range []string{
		"/usr/bin/touch " + marker,
		"true; touch " + marker,
		"true && touch " + marker,
		"true | touch " + marker,
		"true `touch " + marker + "`",
		"true $(touch " + marker + ")",
		"true > " + marker,
	} {
		t.Run(command, func(t *testing.T) {
			receipt, err := workflow.ApplyChangeSet(context.Background(), root,
				stagedChangeSet(t, root, command), workflow.ApplyOptions{})
			if err == nil {
				t.Fatalf("command %q was accepted; receipt=%+v", command, receipt)
			}
			if receipt.Applied {
				t.Fatalf("command %q applied the change set", command)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("command %q executed: the shell is still reachable", command)
			}
		})
	}
}

// TestVerificationRefusesBinariesOutsideTheAllowlist keeps the gate narrow:
// "run my project's verifier" is not "run any executable on the host".
func TestVerificationRefusesBinariesOutsideTheAllowlist(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		"/usr/bin/touch marker",
		"curl https://example.invalid",
		"sh -c true",
		"bash -c true",
		"env",
		"../../usr/bin/touch marker",
	} {
		t.Run(command, func(t *testing.T) {
			_, err := workflow.ApplyChangeSet(context.Background(), root,
				stagedChangeSet(t, root, command), workflow.ApplyOptions{})
			if err == nil {
				t.Fatalf("command %q was accepted", command)
			}
			if !strings.Contains(err.Error(), "verification") {
				t.Fatalf("error should name the verification gate, got %v", err)
			}
		})
	}
}

// TestVerificationRunsAllowlistedCommandsAgainstTheStagedTree proves the gate
// still does its job: an allowlisted verifier runs, sees the post-edit
// content, and its exit status decides the outcome.
func TestVerificationRunsAllowlistedCommandsAgainstTheStagedTree(t *testing.T) {
	root := t.TempDir()
	receipt, err := workflow.ApplyChangeSet(context.Background(), root,
		stagedChangeSet(t, root, "grep -q GAMMA a.txt"), workflow.ApplyOptions{})
	if err != nil {
		t.Fatalf("allowlisted verification must run: %v (receipt=%+v)", err, receipt)
	}
	if !receipt.Applied {
		t.Fatalf("change set should have applied: %+v", receipt)
	}
	if len(receipt.Verification) != 1 || !receipt.Verification[0].Passed {
		t.Fatalf("verification receipt missing or failed: %+v", receipt.Verification)
	}
}

// TestVerificationFailureBlocksPromotion pins the gate's purpose: a failing
// verifier must leave the working tree untouched.
func TestVerificationFailureBlocksPromotion(t *testing.T) {
	root := t.TempDir()
	cs := stagedChangeSet(t, root, "grep -q NEVER_PRESENT a.txt")
	if _, err := workflow.ApplyChangeSet(context.Background(), root, cs, workflow.ApplyOptions{}); err == nil {
		t.Fatal("a failing verification command must fail the apply")
	}
	body, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "alpha BETA\n" {
		t.Fatalf("working tree mutated despite failed verification: %q", body)
	}
}

// TestVerificationDoesNotInheritTheParentEnvironment stops a verifier from
// reading credentials that happen to be in the operator's shell.
func TestVerificationDoesNotInheritTheParentEnvironment(t *testing.T) {
	t.Setenv("SENTRA_TEST_SECRET_TOKEN", "super-secret-value")
	root := t.TempDir()

	// `printenv NAME` exits non-zero when the variable is unset, so a passing
	// apply would mean the variable was visible to the child.
	_, err := workflow.ApplyChangeSet(context.Background(), root,
		stagedChangeSet(t, root, "printenv SENTRA_TEST_SECRET_TOKEN"), workflow.ApplyOptions{})
	if err == nil {
		t.Fatal("verification child inherited the parent environment")
	}
}
