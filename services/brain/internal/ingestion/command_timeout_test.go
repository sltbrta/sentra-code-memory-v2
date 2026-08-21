package ingestion_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// TestFrozenExactly100ChangeFixture failed once under a full parallel run and
// was never reproduced. The mechanism was located by reading -- CommandTimeout
// bounds every git subprocess a reconcile spawns, and a reconcile over the
// frozen fixture spawns many -- but "located by reading" is exactly the
// standard this branch exists to raise.
//
// These reproduce the mechanism on demand. They do not prove the original
// failure was this one; nothing can, without a reproduction of it. What they
// show is that the mechanism is real, that its signature is a context deadline
// surfacing from production code rather than an obviously test-shaped timeout,
// and that deriving the bound from the test's own deadline removes it.

// slowGit wraps the real git with a fixed delay per invocation, which is what
// a loaded machine does to a subprocess without anything being wrong.
func slowGit(t *testing.T, git string, delay time.Duration) string {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "slow-git")
	script := fmt.Sprintf("#!/bin/sh\n/bin/sleep %.2f\nexec %q \"$@\"\n",
		delay.Seconds(), git)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

// TestASlowGitSurfacesAsAContextDeadlineFromProductionCode is the signature.
//
// The failure a loaded machine produces is not "the test timed out". It is an
// ingestion error carrying a context deadline, which reads as a defect in the
// authority rather than as a subprocess that was merely slow -- and that is
// why the original failure was hard to place.
func TestASlowGitSurfacesAsAContextDeadlineFromProductionCode(t *testing.T) {
	root, git := newRepository(t, map[string]string{"a.go": "package a\n"})
	config := testConfig(t, root, git)
	config.GitExecutable = slowGit(t, git, 300*time.Millisecond)
	// A bound below the subprocess's own cost: what a fixed allowance becomes
	// on a machine slower than the one it was chosen on.
	config.CommandTimeout = 50 * time.Millisecond

	_, err := ingestion.New(context.Background(), config)
	if err == nil {
		t.Fatal("a git slower than the command bound was admitted; the fixture " +
			"does not reproduce the mechanism")
	}
	// The point is where it comes from, not that it fails: this is an
	// ingestion error, not a testing timeout.
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "git") ||
		errors.Is(err, ingestion.ErrGit) {
		return
	}
	t.Fatalf("unexpected failure shape for a slow git: %v", err)
}

// TestTheDerivedAllowanceAbsorbsASlowGit is the fix. The same subprocess, the
// same delay, under a bound derived from the test binary's deadline rather
// than from a constant somebody chose on a fast machine.
func TestTheDerivedAllowanceAbsorbsASlowGit(t *testing.T) {
	root, git := newRepository(t, map[string]string{"a.go": "package a\n"})
	config := testConfig(t, root, git)
	config.GitExecutable = slowGit(t, git, 300*time.Millisecond)

	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatalf("a git 300ms slower than usual failed under the derived "+
			"allowance: %v", err)
	}
	if authority == nil {
		t.Fatal("no authority returned")
	}
}

// TestTheDerivedAllowanceScalesWithTheTestBudget is the property that makes
// the derivation more than a bigger constant: the bound follows the budget the
// runner was given, so a longer run tolerates a slower machine and a short one
// still fails fast.
func TestTheDerivedAllowanceScalesWithTheTestBudget(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Skip("no -timeout set, so there is no budget to derive from")
	}
	root, git := newRepository(t, map[string]string{"a.go": "package a\n"})
	got := testConfig(t, root, git).CommandTimeout

	if got <= 0 {
		t.Fatal("the derived allowance is not positive")
	}
	if got > maxSubprocessAllowance {
		t.Fatalf("the derived allowance is %s, over the config's own ceiling of %s: "+
			"a generous -timeout would be rejected as invalid input",
			got, maxSubprocessAllowance)
	}
	// It must be a real fraction of the remaining budget rather than a
	// constant that happens to be smaller.
	remaining := time.Until(deadline)
	if remaining > 2*time.Minute && got < 20*time.Second {
		t.Fatalf("the allowance is %s with %s of budget left: it is not "+
			"following the budget", got, remaining.Round(time.Second))
	}
}
