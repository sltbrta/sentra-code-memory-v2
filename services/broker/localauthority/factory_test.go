package localauthority

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// factoryTestPolicy is a static current-policy checker for the sealed-runner
// composition tests.
type factoryTestPolicy struct {
	allowed bool
	epoch   uint64
}

func (p factoryTestPolicy) Check(
	_ context.Context, _ shared.MappedIdentityFact, _ shared.PolicyRequest,
) (shared.PolicyDecision, error) {
	return shared.PolicyDecision{Allowed: p.allowed, RevocationEpoch: p.epoch}, nil
}

// factoryTestFences pins the lease fence state the effect broker resolves.
type factoryTestFences struct {
	fence     uint64
	expiresAt time.Time
	ok        bool
}

func (f factoryTestFences) CurrentFence(
	_ context.Context, _ Identifier,
) (uint64, time.Time, bool) {
	return f.fence, f.expiresAt, f.ok
}

func factoryTestLeafSpec(t *testing.T, base string) FactoryLeafSpec {
	t.Helper()
	expires := time.Now().UTC().Add(time.Minute)
	return FactoryLeafSpec{
		RunID:          "run-1",
		NodeID:         "leaf-a",
		OwnedPaths:     []string{"src/go/modify-00.go"},
		ForbiddenPaths: []string{"src/typescript"},
		BaseGitOID:     base,
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "p"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "t"},
			Session:   shared.Identifier{Namespace: "session", Value: "s"},
		},
		Grant: FactoryLeafGrant{
			GrantID:          "grant-1",
			Initiator:        "p",
			Tenant:           "t",
			TaskID:           "leaf-a",
			RunID:            "run-1",
			LeaseID:          "lease-1",
			LeaseHolder:      "p",
			LeaseFence:       1,
			LeaseExpiresAt:   expires,
			AllowedPaths:     []string{"src/go/modify-00.go"},
			RepositoryGitOID: base,
			Nonce:            "nonce-1",
			RevocationEpoch:  3,
			ExpiresAt:        expires,
			PolicyDigestHex:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			CommandFence:     1,
		},
		ChangeSetID:    "changeset-1",
		IdempotencyKey: "leaf-1",
		Now:            time.Now().UTC(),
	}
}

func newFactoryTestRunner(t *testing.T, repository string, fences FactoryFenceRegistry) *FactoryRunner {
	t.Helper()
	composed, err := OpenFactoryRunner(FactoryRunnerConfig{
		CanonicalRoot:  repository,
		CandidateRoot:  filepath.Join(t.TempDir(), "candidates"),
		GitExecutable:  "/usr/bin/git",
		CommandTimeout: 5 * time.Second,
		MaxFiles:       1_000,
		MaxFileBytes:   1 << 20,
		MaxTotalBytes:  4 << 20,
		Policy:         factoryTestPolicy{allowed: true, epoch: 3},
		Fences:         fences,
		Clock:          func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return composed
}

func writeFactoryTestRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repository, "src", "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "go", "modify-00.go"),
		[]byte("package main\n\nfunc marker() string { return \"stage\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The add target exists at base so an add directive against it fails the
	// atomic application mid-set.
	if err := os.WriteFile(filepath.Join(repository, "src", "go", "add-00.go"),
		[]byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(arguments ...string) string {
		t.Helper()
		output, err := runFactoryTestGit(repository, arguments...)
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	git("init")
	git("add", "--all")
	git("-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid", "commit", "-m", "seed")
	return repository, git("rev-parse", "HEAD")
}

// runFactoryTestGit executes one git invocation against the test repository
// and returns its trimmed output.
func runFactoryTestGit(repository string, arguments ...string) (string, error) {
	command := exec.Command("/usr/bin/git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func factoryTestAttestation(t *testing.T, repository string) string {
	t.Helper()
	status, err := runFactoryTestGit(repository, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	head, err := runFactoryTestGit(repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(repository, "src", "go", "modify-00.go"))
	if err != nil {
		t.Fatal(err)
	}
	return status + "\x00" + head + "\x00" + string(content)
}

func TestOpenFactoryRunnerValidatesConfiguration(t *testing.T) {
	if _, err := OpenFactoryRunner(FactoryRunnerConfig{}); err == nil {
		t.Fatal("empty configuration opened")
	}
}

func TestFactoryRunnerExecutesDeterministicLeaf(t *testing.T) {
	repository, base := writeFactoryTestRepository(t)
	runner := newFactoryTestRunner(t, repository, factoryTestFences{
		fence: 1, expiresAt: time.Now().UTC().Add(time.Minute), ok: true,
	})
	before := factoryTestAttestation(t, repository)
	outcome, err := runner.ExecuteLeaf(context.Background(), factoryTestLeafSpec(t, base), FactoryLeafScript{
		Edits: []FactoryEditDirective{{Op: "modify", Path: "src/go/modify-00.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != "COMPLETED" || len(outcome.Edits) != 1 || outcome.Rollback != nil || len(outcome.Denials) != 0 {
		t.Fatalf("outcome = %#v", outcome)
	}
	edit := outcome.Edits[0]
	if edit.Op != "modify" || edit.Path != "src/go/modify-00.go" || edit.Language != "go" ||
		len(edit.AfterBytes) == 0 || len(edit.AfterDigestHex) != 64 || len(edit.BeforeDigestHex) != 64 {
		t.Fatalf("edit = %#v", edit)
	}
	if after := factoryTestAttestation(t, repository); after != before {
		t.Fatalf("canonical repository mutated:\nbefore %q\nafter  %q", before, after)
	}
	// An exact replay returns the original outcome without re-executing.
	replayed, err := runner.ExecuteLeaf(context.Background(), factoryTestLeafSpec(t, base), FactoryLeafScript{
		Edits: []FactoryEditDirective{{Op: "modify", Path: "src/go/modify-00.go"}},
	})
	if err != nil || replayed.State != "COMPLETED" || len(replayed.Edits) != 1 {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	if after := factoryTestAttestation(t, repository); after != before {
		t.Fatal("canonical repository mutated on replay")
	}
}

func TestFactoryRunnerEscapeProbeFailsClosed(t *testing.T) {
	repository, base := writeFactoryTestRepository(t)
	runner := newFactoryTestRunner(t, repository, factoryTestFences{
		fence: 1, expiresAt: time.Now().UTC().Add(time.Minute), ok: true,
	})
	before := factoryTestAttestation(t, repository)
	spec := factoryTestLeafSpec(t, base)
	outcome, err := runner.ExecuteLeaf(context.Background(), spec, FactoryLeafScript{
		Edits:      []FactoryEditDirective{{Op: "modify", Path: "src/go/modify-00.go"}},
		ProbePaths: []string{"src/typescript"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != "FAILED" || outcome.Rollback == nil || len(outcome.Denials) == 0 {
		t.Fatalf("escape outcome = %#v", outcome)
	}
	escape := false
	for _, denial := range outcome.Denials {
		if denial.ReasonCode == "escape_path_scope" || denial.ReasonCode == "escape_forbidden_path" {
			escape = true
		}
	}
	if !escape {
		t.Fatalf("escape denial missing: %#v", outcome.Denials)
	}
	if after := factoryTestAttestation(t, repository); after != before {
		t.Fatal("canonical repository mutated by escape attempt")
	}
}

func TestFactoryRunnerAtomicApplicationFailureRollsBack(t *testing.T) {
	repository, base := writeFactoryTestRepository(t)
	runner := newFactoryTestRunner(t, repository, factoryTestFences{
		fence: 1, expiresAt: time.Now().UTC().Add(time.Minute), ok: true,
	})
	before := factoryTestAttestation(t, repository)
	spec := factoryTestLeafSpec(t, base)
	spec.OwnedPaths = []string{"src/go/modify-00.go", "src/go/add-00.go"}
	spec.Grant.AllowedPaths = spec.OwnedPaths
	outcome, err := runner.ExecuteLeaf(context.Background(), spec, FactoryLeafScript{
		Edits: []FactoryEditDirective{
			{Op: "modify", Path: "src/go/modify-00.go"},
			// The add targets a path that already exists at base: the atomic
			// application fails mid-set and the whole candidate is discarded.
			{Op: "add", Path: "src/go/add-00.go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != "FAILED" || outcome.Rollback == nil || outcome.Rollback.ChangeSetDigestHex == "" {
		t.Fatalf("failure outcome = %#v", outcome)
	}
	if after := factoryTestAttestation(t, repository); after != before {
		t.Fatal("canonical repository mutated by failed application")
	}
}

func TestFactoryRunnerStaleLeaseDenies(t *testing.T) {
	repository, base := writeFactoryTestRepository(t)
	runner := newFactoryTestRunner(t, repository, factoryTestFences{
		fence: 1, expiresAt: time.Now().UTC().Add(-time.Minute), ok: true,
	})
	outcome, err := runner.ExecuteLeaf(context.Background(), factoryTestLeafSpec(t, base), FactoryLeafScript{
		Edits: []FactoryEditDirective{{Op: "modify", Path: "src/go/modify-00.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != "FAILED" || len(outcome.Denials) == 0 || outcome.Denials[0].ReasonCode != "stale_lease" {
		t.Fatalf("stale outcome = %#v", outcome)
	}
}

func TestFactoryRunnerRejectsMalformedScript(t *testing.T) {
	repository, base := writeFactoryTestRepository(t)
	runner := newFactoryTestRunner(t, repository, factoryTestFences{
		fence: 1, expiresAt: time.Now().UTC().Add(time.Minute), ok: true,
	})
	if _, err := runner.ExecuteLeaf(context.Background(), factoryTestLeafSpec(t, base), FactoryLeafScript{}); err == nil {
		t.Fatal("empty script executed")
	}
	if _, err := runner.ExecuteLeaf(context.Background(), factoryTestLeafSpec(t, base), FactoryLeafScript{
		Edits: []FactoryEditDirective{{Op: "modify", Path: "src/go/modify-00.go", OldPath: "src/go/other.go"}},
	}); err == nil {
		t.Fatal("malformed directive executed")
	}
}
