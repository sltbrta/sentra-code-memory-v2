package ingestion_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

func newRepository(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, git, root, "init", "-q")
	if err := os.Mkdir(filepath.Join(root, ".git", "test-hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, root, "config", "core.hooksPath", ".git/test-hooks")
	runGit(t, git, root, "config", "user.name", "Ingestion Test")
	runGit(t, git, root, "config", "user.email", "ingestion@example.invalid")
	writeFiles(t, root, files)
	runGit(t, git, root, "add", "--all")
	runGit(t, git, root, "commit", "-q", "-m", "snapshot")
	return root, git
}

func blockingGit(t *testing.T, git string) (wrapper, block, started, release string) {
	t.Helper()
	directory := t.TempDir()
	wrapper = filepath.Join(directory, "git-wrapper")
	block = filepath.Join(directory, "block")
	started = filepath.Join(directory, "started")
	release = filepath.Join(directory, "release")
	script := fmt.Sprintf(`#!/bin/sh
for argument in "$@"; do
	if [ "$argument" = "cat-file" ] && [ -e %q ]; then
		: > %q
		while [ ! -e %q ]; do /bin/sleep 0.01; done
	fi
done
exec %q "$@"
`, block, started, release, git)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper, block, started, release
}

func waitForFile(t *testing.T, filename string) {
	t.Helper()
	allowance := testAllowance(5 * time.Second)
	// Never wait past the test binary's own budget: a hang should surface as
	// this assertion, with the filename, rather than as a goroutine dump.
	if testDeadline, ok := t.Deadline(); ok {
		if remaining := time.Until(testDeadline) - time.Second; remaining > 0 && remaining < allowance {
			allowance = remaining
		}
	}
	deadline := time.Now().Add(allowance)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filename); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s after %s", filename, allowance)
}

// testAllowance scales a wall-clock allowance for the conditions the suite is
// actually running under.
//
// Every deadline in this package's fixtures is a bound on a git subprocess or
// on another process reaching a rendezvous, and none of them is measuring
// anything: they exist so a genuinely stuck test fails instead of hanging.
// Sized for an idle machine they instead fail on a busy one --
// TestFrozenExactly100ChangeFixture failed once during a full `go test ./...`
// run and passed on every isolated run, at this branch and at the base
// revision, which is the signature of exactly that.
//
// Under the race detector the allowance is multiplied rather than the base
// being raised, so an isolated run still fails fast when something is really
// stuck.
func testAllowance(base time.Duration) time.Duration {
	if raceEnabled {
		return base * 8
	}
	return base * 2
}

func commitFiles(t *testing.T, git, root string, files map[string]string) string {
	t.Helper()
	writeFiles(t, root, files)
	runGit(t, git, root, "add", "--all")
	runGit(t, git, root, "commit", "-q", "-m", "snapshot")
	return gitOutput(t, git, root, "rev-parse", "HEAD")
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if contents == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runGit(t *testing.T, git, root string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", root}, args...)
	cmd := exec.Command(git, cmdArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitOutput(t *testing.T, git, root string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", root}, args...)
	output, err := exec.Command(git, cmdArgs...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output[:len(output)-1])
}

func testConfig(root, git string) ingestion.Config {
	digest := sha256.Sum256([]byte("bootstrap"))
	return ingestion.Config{
		ApprovedRoot:        root,
		GitExecutable:       git,
		TenantID:            "tenant-1",
		BrainID:             "brain-1",
		RepositoryID:        "repository-1",
		ConfigurationDigest: hex.EncodeToString(digest[:]),
		Policy: ingestion.Policy{
			Symlinks: ingestion.RecordWithoutFollow,
		},
		// Bounds every git subprocess the authority spawns. A reconcile over
		// the frozen 100-change fixture runs a lot of them, and this is the
		// deadline that expires first when the suite is under load.
		CommandTimeout:        testAllowance(10 * time.Second),
		MaxFiles:              1_000,
		MaxPathBytes:          4_096,
		MaxFileBytes:          1 << 20,
		MaxTotalBytes:         16 << 20,
		MaxIdempotencyRecords: 256,
	}
}

func admitHead(t *testing.T, authority *ingestion.Authority, git, root string) ingestion.Generation {
	t.Helper()
	commit := gitOutput(t, git, root, "rev-parse", "HEAD")
	generation, err := authority.Admit(context.Background(), ingestion.Admission{
		ExpectedCommitOID: commit,
		IdempotencyKey:    "admit-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}
