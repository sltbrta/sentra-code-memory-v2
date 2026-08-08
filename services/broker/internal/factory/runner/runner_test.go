package runner_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/effects"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/runner"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var testNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// fakePolicy mirrors the Stage 02 evaluator: stale epochs deny, listed
// action|resource pairs allow at the current epoch.
type fakePolicy struct {
	mu      sync.Mutex
	epoch   uint64
	allowed map[string]bool
}

func (f *fakePolicy) Check(_ context.Context, _ contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	decision := contracts.PolicyDecision{
		Allowed:         false,
		RevocationEpoch: f.epoch,
		Receipt:         contracts.Receipt{Status: "rejected", ReasonCode: "not_found_or_denied"},
	}
	if request.RevocationEpoch == f.epoch && f.allowed[request.Action+"|"+request.Resource.Value] {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

// fakeFences is a flippable lease fence registry.
type fakeFences struct {
	mu     sync.Mutex
	fence  uint64
	expiry time.Time
}

func (f *fakeFences) CurrentFence(context.Context, contracts.Identifier) (uint64, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fence, f.expiry, true
}

func (f *fakeFences) bump() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fence++
}

// flipSynthesizer proposes every edit, then revokes the lease fence, proving
// the mutation-time check — not the admission check — catches the stale fence.
type flipSynthesizer struct {
	edits  []changeset.Edit
	fences *fakeFences
}

func (s flipSynthesizer) Synthesize(ctx context.Context, _ runner.LeafSpec, fx runner.Effects) error {
	for _, edit := range s.edits {
		if err := fx.Propose(ctx, edit); err != nil {
			return err
		}
	}
	s.fences.bump()
	return nil
}

func baseFiles() map[string]string {
	files := map[string]string{
		"README.md":           "# fixture\n",
		"src/go/delete-00.go": "package delete00\n",
		"src/go/rename-00.go": "package rename00\n",
		"src/go/add-01.go":    "package add01\n",
	}
	for index := 0; index < 5; index++ {
		files[fmt.Sprintf("src/go/modify-%02d.go", index)] = fmt.Sprintf("package modify%02d\n", index)
	}
	return files
}

type fixture struct {
	canonical     string
	git           string
	base          string
	candidateRoot string
	store         *gitcandidate.Store
	broker        *effects.Broker
	policy        *fakePolicy
	fences        *fakeFences
	runner        *runner.Runner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	canonical := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, git, canonical, "init", "-q", "-b", "main")
	runGit(t, git, canonical, "config", "user.name", "Runner Test")
	runGit(t, git, canonical, "config", "user.email", "runner@example.invalid")
	for name, contents := range baseFiles() {
		target := filepath.Join(canonical, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, git, canonical, "add", "--all")
	runGit(t, git, canonical, "commit", "-q", "-m", "base")
	base := gitOutput(t, git, canonical, "rev-parse", "HEAD")
	candidateRoot := t.TempDir()
	store, err := gitcandidate.NewStore(gitcandidate.Config{
		CanonicalRoot:  canonical,
		CandidateRoot:  candidateRoot,
		GitExecutable:  git,
		CommandTimeout: 10 * time.Second,
		MaxFiles:       1_000,
		MaxFileBytes:   1 << 20,
		MaxTotalBytes:  16 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := &fakePolicy{epoch: 3, allowed: map[string]bool{
		"file.read|repo-1":  true,
		"file.write|repo-1": true,
	}}
	fences := &fakeFences{fence: 7, expiry: testNow.Add(time.Hour)}
	broker, err := effects.NewBroker(policy, fences, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := runner.New(store, broker)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		canonical: canonical, git: git, base: base, candidateRoot: candidateRoot,
		store: store, broker: broker, policy: policy, fences: fences, runner: sealed,
	}
}

func runGit(t *testing.T, git, root string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", root}, args...)
	if output, err := exec.Command(git, cmdArgs...).CombinedOutput(); err != nil {
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
	return strings.TrimSpace(string(output))
}

func (f *fixture) leafSpec() runner.LeafSpec {
	return runner.LeafSpec{
		RunID:          contracts.Identifier{Namespace: "run", Value: "run-1"},
		NodeID:         "leaf-a",
		OwnedPaths:     []string{"src/go"},
		ForbiddenPaths: []string{"src/go/protected"},
		BaseGitOID:     f.base,
		Identity: contracts.MappedIdentityFact{
			Principal: contracts.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    contracts.Identifier{Namespace: "tenant", Value: "t1"},
			Session:   contracts.Identifier{Namespace: "session", Value: "s1"},
		},
		Grant: effects.Grant{
			GrantID:   contracts.Identifier{Namespace: "grant", Value: "g1"},
			Initiator: contracts.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    contracts.Identifier{Namespace: "tenant", Value: "t1"},
			TaskID:    contracts.Identifier{Namespace: "task", Value: "task-1"},
			RunID:     contracts.Identifier{Namespace: "run", Value: "run-1"},
			Lease: effects.Lease{
				LeaseID:   contracts.Identifier{Namespace: "lease", Value: "l1"},
				Holder:    contracts.Identifier{Namespace: "principal", Value: "p1"},
				Fence:     7,
				ExpiresAt: testNow.Add(time.Hour),
			},
			Actions:          []string{effects.ActionFileRead, effects.ActionFileWrite},
			Resources:        []contracts.Identifier{{Namespace: "repository", Value: "repo-1"}},
			RepositoryGitOID: f.base,
			AllowedPaths:     []string{"src/go"},
			Nonce:            "nonce-1",
			RevocationEpoch:  3,
			ExpiresAt:        testNow.Add(time.Hour),
			PolicyDigest:     changeset.DigestBytes([]byte("policy")),
			CommandFence:     1,
		},
		ChangeSetID:    "changeset-1",
		IdempotencyKey: "run-leaf-1",
		Now:            testNow,
	}
}

func modifyEdit(path, before, after string) changeset.Edit {
	return changeset.Edit{
		Path: path, Op: changeset.OpModify, Lang: changeset.LanguageGo,
		BeforeDigest: changeset.DigestBytes([]byte(before)),
		AfterDigest:  changeset.DigestBytes([]byte(after)),
		NewContent:   []byte(after),
	}
}

func addEdit(path, content string) changeset.Edit {
	return changeset.Edit{
		Path: path, Op: changeset.OpAdd, Lang: changeset.LanguageGo,
		AfterDigest: changeset.DigestBytes([]byte(content)),
		NewContent:  []byte(content),
	}
}

func happyEdits() []changeset.Edit {
	return []changeset.Edit{
		modifyEdit("src/go/modify-00.go", "package modify00\n", "package changed00\n"),
		addEdit("src/go/add-00.go", "package add00\n"),
		{
			Path: "src/go/renamed-00.go", OldPath: "src/go/rename-00.go", Op: changeset.OpRename,
			Lang:         changeset.LanguageGo,
			BeforeDigest: changeset.DigestBytes([]byte("package rename00\n")),
			AfterDigest:  changeset.DigestBytes([]byte("package renamed00\n")),
			NewContent:   []byte("package renamed00\n"),
		},
		{
			Path: "src/go/delete-00.go", Op: changeset.OpDelete, Lang: changeset.LanguageGo,
			BeforeDigest: changeset.DigestBytes([]byte("package delete00\n")),
		},
	}
}

func attest(t *testing.T, store *gitcandidate.Store) gitcandidate.Attestation {
	t.Helper()
	attestation, err := store.AttestCanonical()
	if err != nil {
		t.Fatal(err)
	}
	return attestation
}

func candidateDirCount(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func TestHappyPathAppliesAtomicCandidate(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), runner.FixtureSynthesizer(happyEdits()))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunCompleted || result.Outcome == nil {
		t.Fatalf("result = %#v, want COMPLETED with outcome", result)
	}
	if len(result.Denials) != 0 {
		t.Fatalf("denials = %#v, want none", result.Denials)
	}
	outcome := result.Outcome
	if outcome.State != gitcandidate.StateApplied || outcome.PostImageDigest.Hex == "" || outcome.PostImageTreeOID == "" {
		t.Fatalf("outcome = %#v, want APPLIED with post-image facts", outcome)
	}
	if outcome.BaseGitOID != f.base {
		t.Fatalf("candidate base = %q, want intent base %q", outcome.BaseGitOID, f.base)
	}
	kind := gitOutput(t, f.git, filepath.Join(f.candidateRoot, outcome.CandidateID), "cat-file", "-t", outcome.PostImageTreeOID)
	if kind != "tree" {
		t.Fatalf("post-image object kind = %q, want tree", kind)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across a successful run")
	}
}

func TestEscapeAttemptsFailClosed(t *testing.T) {
	cases := map[string]runner.Step{
		"path traversal write":      {Kind: runner.StepEffect, Action: effects.ActionFileWrite, Path: "../outside.go"},
		"absolute path write":       {Kind: runner.StepEffect, Action: effects.ActionFileWrite, Path: "/etc/escape"},
		"write outside owned scope": {Kind: runner.StepEffect, Action: effects.ActionFileWrite, Path: "src/typescript/modify-00.ts"},
		"forbidden path write":      {Kind: runner.StepEffect, Action: effects.ActionFileWrite, Path: "src/go/protected/secret.go"},
		"dispatch attempt":          {Kind: runner.StepEffect, Action: effects.ActionDispatch},
		"task-create attempt":       {Kind: runner.StepEffect, Action: effects.ActionTaskCreate},
		"shell effect attempt":      {Kind: runner.StepEffect, Action: "shell.exec"},
		"network effect attempt":    {Kind: runner.StepEffect, Action: "network.egress"},
		"model effect attempt":      {Kind: runner.StepEffect, Action: "model.invoke"},
		"traversal read attempt":    {Kind: runner.StepRead, Path: "../../etc/passwd"},
	}
	for name, hostile := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			before := attest(t, f.store)
			script := runner.ScriptedSynthesizer{Steps: []runner.Step{
				hostile,
				{Kind: runner.StepPropose, Edit: happyEdits()[0]},
			}}
			result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), script)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != runner.RunFailed {
				t.Fatalf("result state = %q, want FAILED for %s", result.State, name)
			}
			if len(result.Denials) == 0 {
				t.Fatalf("no denial recorded for %s", name)
			}
			if !strings.HasPrefix(result.Denials[0].ReasonCode, "escape_") {
				t.Fatalf("denial reason = %q, want an escape code for %s", result.Denials[0].ReasonCode, name)
			}
			if result.Rollback == nil || result.Rollback.Receipt.Status != "rejected" ||
				result.Rollback.Receipt.ReasonCode != gitcandidate.ReasonEscapeDenied {
				t.Fatalf("rollback = %#v, want rejected escape receipt", result.Rollback)
			}
			if count := candidateDirCount(t, f.candidateRoot); count != 0 {
				t.Fatalf("%d candidate directories remain after escape failure", count)
			}
			if after := attest(t, f.store); after != before {
				t.Fatalf("canonical attestation changed across escape case %s", name)
			}
		})
	}
}

func TestStaleFenceAtMutationTimeFailsClosed(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)
	edits := happyEdits()
	synthesizer := flipSynthesizer{edits: edits, fences: f.fences}
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), synthesizer)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED under stale fence", result.State)
	}
	found := false
	for _, denial := range result.Denials {
		if denial.ReasonCode == effects.ReasonStaleFence {
			found = true
		}
	}
	if !found {
		t.Fatalf("denials = %#v, want a stale_fence mutation-time denial", result.Denials)
	}
	if result.Rollback == nil || result.Rollback.Receipt.ReasonCode != gitcandidate.ReasonApplyFailed {
		t.Fatalf("rollback = %#v, want apply-failure receipt", result.Rollback)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after stale-fence failure", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across stale-fence failure")
	}
}

func TestPartialEditFailureIsAtomic(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)
	edits := make([]changeset.Edit, 0, 10)
	for index := 0; index < 5; index++ {
		edits = append(edits, modifyEdit(
			fmt.Sprintf("src/go/modify-%02d.go", index),
			fmt.Sprintf("package modify%02d\n", index),
			fmt.Sprintf("package changed%02d\n", index),
		))
	}
	edits = append(edits, addEdit("src/go/add-00.go", "package add00\n"))
	// The seventh edit (index 6) targets a path that already exists in the
	// exact base, so the add fails mid-application.
	edits = append(edits, addEdit("src/go/add-01.go", "package CHANGED\n"))
	for index := 2; index < 5; index++ {
		edits = append(edits, addEdit(fmt.Sprintf("src/go/add-%02d.go", index), fmt.Sprintf("package add%02d\n", index)))
	}
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), runner.FixtureSynthesizer(edits))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED on partial edit failure", result.State)
	}
	rollback := result.Rollback
	if rollback == nil || rollback.Receipt.Status != "rejected" ||
		rollback.Receipt.ReasonCode != gitcandidate.ReasonApplyFailed {
		t.Fatalf("rollback = %#v, want rejected apply-failure receipt", rollback)
	}
	if rollback.FailedEditIndex != 6 || rollback.DiscardedEdits != 6 {
		t.Fatalf("rollback indexes = %d/%d, want 6 discarded and failure at 6",
			rollback.DiscardedEdits, rollback.FailedEditIndex)
	}
	if rollback.BaseGitOID != f.base || rollback.ChangeSetDigest.Hex == "" {
		t.Fatalf("rollback binding = %#v, want base and changeset digest", rollback)
	}
	if result.Outcome == nil || result.Outcome.State != gitcandidate.StateRejected {
		t.Fatalf("outcome = %#v, want REJECTED", result.Outcome)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after partial failure", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across partial edit failure")
	}
}

func TestDuplicateRunReplaysOriginalOutcome(t *testing.T) {
	f := newFixture(t)
	spec := f.leafSpec()
	first, err := f.runner.RunLeaf(context.Background(), spec, runner.FixtureSynthesizer(happyEdits()))
	if err != nil || first.State != runner.RunCompleted {
		t.Fatalf("first run = %#v, %v", first, err)
	}
	second, err := f.runner.RunLeaf(context.Background(), spec, runner.FixtureSynthesizer(happyEdits()))
	if err != nil {
		t.Fatalf("exact replay = %v, want original outcome", err)
	}
	if second.State != runner.RunCompleted || second.Outcome.CandidateID != first.Outcome.CandidateID ||
		second.Outcome.PostImageDigest != first.Outcome.PostImageDigest {
		t.Fatalf("replay = %#v, want original outcome %#v", second.Outcome, first.Outcome)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 1 {
		t.Fatalf("%d candidate directories after replay, want exactly the original one", count)
	}
	conflicting := runner.FixtureSynthesizer([]changeset.Edit{
		modifyEdit("src/go/modify-01.go", "package modify01\n", "package OTHER\n"),
	})
	if _, err := f.runner.RunLeaf(context.Background(), spec, conflicting); !errors.Is(err, gitcandidate.ErrConflict) {
		t.Fatalf("conflicting reuse = %v, want ErrConflict", err)
	}
}

func TestWrongBaseCandidateFailsClosed(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)

	// The grant pins a different base than the intent: spec validation rejects.
	spec := f.leafSpec()
	spec.Grant.RepositoryGitOID = strings.Repeat("c", 40)
	if _, err := f.runner.RunLeaf(context.Background(), spec, runner.FixtureSynthesizer(happyEdits())); !errors.Is(err, runner.ErrInvalid) {
		t.Fatalf("grant/base mismatch = %v, want ErrInvalid", err)
	}

	// The pinned base is absent from the canonical repository: Begin rejects
	// before any candidate directory exists.
	absent := f.leafSpec()
	absent.BaseGitOID = strings.Repeat("a", 40)
	absent.Grant.RepositoryGitOID = absent.BaseGitOID
	result, err := f.runner.RunLeaf(context.Background(), absent, runner.FixtureSynthesizer(happyEdits()))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed || len(result.Denials) == 0 ||
		result.Denials[0].ReasonCode != effects.ReasonBaseMismatch {
		t.Fatalf("absent base result = %#v, want FAILED with base_mismatch", result)
	}
	if result.Rollback != nil {
		t.Fatalf("rollback = %#v, want none before a candidate exists", result.Rollback)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories after wrong-base failure", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across wrong-base failure")
	}
}

func TestDispatchCarryingGrantIsRejected(t *testing.T) {
	f := newFixture(t)
	spec := f.leafSpec()
	spec.Grant.Actions = append(spec.Grant.Actions, effects.ActionDispatch)
	if _, err := f.runner.RunLeaf(context.Background(), spec, runner.FixtureSynthesizer(happyEdits())); !errors.Is(err, runner.ErrInvalid) {
		t.Fatalf("dispatch-carrying grant = %v, want ErrInvalid", err)
	}
}

func TestSymlinkEscapeThroughProposeFailsClosed(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.canonical, "src", "go", "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.git, f.canonical, "add", "--all")
	runGit(t, f.git, f.canonical, "commit", "-q", "-m", "add symlink")
	f.base = gitOutput(t, f.git, f.canonical, "rev-parse", "HEAD")
	before := attest(t, f.store)
	spec := f.leafSpec()
	spec.BaseGitOID = f.base
	spec.Grant.RepositoryGitOID = f.base
	edit := modifyEdit("src/go/link/escape.go", "x\n", "package escape\n")
	result, err := f.runner.RunLeaf(context.Background(), spec, runner.FixtureSynthesizer([]changeset.Edit{edit}))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed || result.Rollback == nil {
		t.Fatalf("symlink result = %#v, want FAILED with rollback", result)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("escape wrote outside the candidate: entries=%v err=%v", entries, readErr)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories after symlink escape", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across symlink escape")
	}
}

// failingSynthesizer returns a non-denial operational error.
type failingSynthesizer struct{ err error }

func (s failingSynthesizer) Synthesize(context.Context, runner.LeafSpec, runner.Effects) error {
	return s.err
}

// expirySynthesizer advances the shared clock past the grant expiry after
// staging its edits, then attempts one brokered read.
type expirySynthesizer struct {
	edits []changeset.Edit
	now   *time.Time
}

func (s expirySynthesizer) Synthesize(ctx context.Context, _ runner.LeafSpec, fx runner.Effects) error {
	for _, edit := range s.edits {
		if err := fx.Propose(ctx, edit); err != nil {
			return err
		}
	}
	*s.now = s.now.Add(2 * time.Hour)
	_, err := fx.ReadFile(ctx, "src/go/modify-00.go")
	return err
}

func TestSetLevelEditViolationDiscardsCandidate(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)
	// Both edits pass per-edit validation and authorization, so the duplicate
	// post-image path is only visible to set-level validation inside Apply.
	duplicate := addEdit("src/go/add-00.go", "package add00\n")
	script := runner.ScriptedSynthesizer{Steps: []runner.Step{
		{Kind: runner.StepPropose, Edit: duplicate},
		{Kind: runner.StepPropose, Edit: duplicate},
	}}
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), script)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED for a set-level violation", result.State)
	}
	if result.Rollback == nil || result.Rollback.Receipt.Status != "rejected" ||
		result.Rollback.Receipt.ReasonCode != gitcandidate.ReasonRejected {
		t.Fatalf("rollback = %#v, want rejected candidate_rejected receipt", result.Rollback)
	}
	if result.Outcome == nil || result.Outcome.State != gitcandidate.StateRejected {
		t.Fatalf("outcome = %#v, want REJECTED", result.Outcome)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after a set-level violation", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across a set-level violation")
	}
}

func TestReplayOfFailedRunStaysFailed(t *testing.T) {
	f := newFixture(t)
	badEdits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package WRONG\n", "package changed00\n")}
	first, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), runner.FixtureSynthesizer(badEdits))
	if err != nil {
		t.Fatal(err)
	}
	if first.State != runner.RunFailed || first.Outcome == nil || first.Outcome.State != gitcandidate.StateRejected {
		t.Fatalf("first run = %#v, want FAILED with REJECTED outcome", first)
	}
	second, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), runner.FixtureSynthesizer(badEdits))
	if err != nil {
		t.Fatalf("replay = %v, want the recorded rejection", err)
	}
	if second.State != runner.RunFailed {
		t.Fatalf("replayed run state = %q, want FAILED, never COMPLETED", second.State)
	}
	if second.Rollback == nil || second.Rollback.Receipt.ReasonCode != gitcandidate.ReasonApplyFailed {
		t.Fatalf("replayed rollback = %#v, want the original apply-failure receipt", second.Rollback)
	}
	if second.Outcome == nil || second.Outcome.State != gitcandidate.StateRejected ||
		second.Outcome.CandidateID != first.Outcome.CandidateID {
		t.Fatalf("replayed outcome = %#v, want the original rejection %#v", second.Outcome, first.Outcome)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after a rejection replay", count)
	}
}

func TestReadReauthorizesAgainstLiveClock(t *testing.T) {
	f := newFixture(t)
	now := testNow
	policy := &fakePolicy{epoch: 3, allowed: map[string]bool{
		"file.read|repo-1":  true,
		"file.write|repo-1": true,
	}}
	fences := &fakeFences{fence: 7, expiry: testNow.Add(3 * time.Hour)}
	broker, err := effects.NewBroker(policy, fences, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := runner.New(f.store, broker)
	if err != nil {
		t.Fatal(err)
	}
	edits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package modify00\n", "package changed00\n")}
	result, err := sealed.RunLeaf(context.Background(), f.leafSpec(), expirySynthesizer{edits: edits, now: &now})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED after mid-run grant expiry", result.State)
	}
	found := false
	for _, denial := range result.Denials {
		if denial.Action == effects.ActionFileRead && denial.ReasonCode == effects.ReasonGrantMalformed {
			found = true
		}
	}
	if !found {
		t.Fatalf("denials = %#v, want a live-clock file.read expiry denial", result.Denials)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after mid-run expiry", count)
	}
}

func TestSynthesizerFailureUsesRejectionReason(t *testing.T) {
	f := newFixture(t)
	before := attest(t, f.store)
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), failingSynthesizer{err: errors.New("synthesizer crashed")})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED for a synthesizer failure", result.State)
	}
	if result.Rollback == nil || result.Rollback.Receipt.ReasonCode != gitcandidate.ReasonRejected {
		t.Fatalf("rollback = %#v, want candidate_rejected, never the escape reason", result.Rollback)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after a synthesizer failure", count)
	}
	if after := attest(t, f.store); after != before {
		t.Fatal("canonical attestation changed across a synthesizer failure")
	}
}

// denyingSynthesizer returns a typed broker denial directly, without going
// through the sealed effect surface.
type denyingSynthesizer struct{ err error }

func (s denyingSynthesizer) Synthesize(context.Context, runner.LeafSpec, runner.Effects) error {
	return s.err
}

func TestSynthesizerReturnedDenialIsRecordedInTrace(t *testing.T) {
	f := newFixture(t)
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(),
		denyingSynthesizer{err: &effects.Denial{Reason: effects.ReasonStaleFence}})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed {
		t.Fatalf("result state = %q, want FAILED for a returned denial", result.State)
	}
	found := false
	for _, denial := range result.Denials {
		if denial.Action == "leaf.synthesize" && denial.ReasonCode == effects.ReasonStaleFence {
			found = true
		}
	}
	if !found {
		t.Fatalf("denials = %#v, want the returned denial recorded as leaf.synthesize/stale_fence", result.Denials)
	}
	if result.Rollback == nil || result.Rollback.ChangeSetDigest.Hex != "" {
		t.Fatalf("rollback = %#v, want the canonical empty-set digest with nothing staged", result.Rollback)
	}
	if count := candidateDirCount(t, f.candidateRoot); count != 0 {
		t.Fatalf("%d candidate directories remain after a returned denial", count)
	}
}

func TestRollbackDigestBindsStagedEdits(t *testing.T) {
	f := newFixture(t)
	staged := happyEdits()[0]
	script := runner.ScriptedSynthesizer{Steps: []runner.Step{
		{Kind: runner.StepPropose, Edit: staged},
		{Kind: runner.StepEffect, Action: effects.ActionDispatch},
	}}
	result, err := f.runner.RunLeaf(context.Background(), f.leafSpec(), script)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runner.RunFailed || result.Rollback == nil {
		t.Fatalf("result = %#v, want FAILED with rollback", result)
	}
	want := gitcandidate.ChangeSetDigest(f.base, []changeset.Edit{staged})
	if result.Rollback.ChangeSetDigest != want || want.Hex == "" {
		t.Fatalf("rollback digest = %v, want the staged set digest %v", result.Rollback.ChangeSetDigest, want)
	}
	if result.Rollback.Receipt.ReasonCode != gitcandidate.ReasonEscapeDenied {
		t.Fatalf("rollback reason = %q, want the escape reason", result.Rollback.Receipt.ReasonCode)
	}
}
