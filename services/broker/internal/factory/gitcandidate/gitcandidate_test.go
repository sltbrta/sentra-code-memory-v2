package gitcandidate_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// allowAll authorizes every mutation; denial-specific tests use denyAt.
type allowAll struct{}

var (
	testTenant    = contracts.Identifier{Namespace: "tenant", Value: "t1"}
	testPrincipal = contracts.Identifier{Namespace: "principal", Value: "p1"}
)

func (allowAll) AuthorizeMutation(context.Context, gitcandidate.Mutation) error { return nil }

// denyAt denies the mutation with the given index, proving authorization is
// re-evaluated at mutation time rather than only at admission.
type denyAt struct {
	index int
	calls int
}

func (d *denyAt) AuthorizeMutation(_ context.Context, mutation gitcandidate.Mutation) error {
	d.calls++
	if mutation.Index == d.index {
		return errors.New("test fence denial")
	}
	return nil
}

func baseFiles() map[string]string {
	files := map[string]string{
		"README.md":           "# fixture\n",
		"src/go/delete-00.go": "package delete00\n",
		"src/go/rename-00.go": "package rename00\n",
	}
	for index := 0; index < 5; index++ {
		files[fmt.Sprintf("src/go/modify-%02d.go", index)] = fmt.Sprintf("package modify%02d\n", index)
	}
	return files
}

func newRepository(t *testing.T, files map[string]string) (string, string, string) {
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
	runGit(t, git, root, "init", "-q", "-b", "main")
	runGit(t, git, root, "config", "user.name", "Candidate Test")
	runGit(t, git, root, "config", "user.email", "candidate@example.invalid")
	writeFiles(t, root, files)
	runGit(t, git, root, "add", "--all")
	runGit(t, git, root, "commit", "-q", "-m", "base")
	base := gitOutput(t, git, root, "rev-parse", "HEAD")
	return root, git, base
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
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

func newStore(t *testing.T, canonical, git string) *gitcandidate.Store {
	t.Helper()
	store, err := gitcandidate.NewStore(gitcandidate.Config{
		CanonicalRoot:  canonical,
		CandidateRoot:  t.TempDir(),
		GitExecutable:  git,
		CommandTimeout: 10 * time.Second,
		MaxFiles:       1_000,
		MaxFileBytes:   1 << 20,
		MaxTotalBytes:  16 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
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

func attest(t *testing.T, store *gitcandidate.Store) gitcandidate.Attestation {
	t.Helper()
	attestation, err := store.AttestCanonical()
	if err != nil {
		t.Fatal(err)
	}
	return attestation
}

func TestBeginHydratesExactBase(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.BaseGitOID != base {
		t.Fatalf("candidate base = %q, want %q", candidate.BaseGitOID, base)
	}
	for name, want := range baseFiles() {
		content, err := candidate.ReadFile(name)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		if string(content) != want {
			t.Fatalf("candidate %q = %q, want %q", name, content, want)
		}
	}
	head := gitOutput(t, git, candidate.Path, "rev-parse", "HEAD")
	if head != base {
		t.Fatalf("candidate HEAD = %q, want exact base %q", head, base)
	}
}

func TestBeginRejectsWrongBase(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	blob := gitOutput(t, git, canonical, "rev-parse", base+":README.md")
	for name, oid := range map[string]string{
		"blob object":      blob,
		"absent commit":    strings.Repeat("a", 40),
		"malformed object": "not-an-oid",
	} {
		if _, err := store.Begin(context.Background(), oid); err == nil {
			t.Fatalf("Begin(%s) = nil error, want base rejection", name)
		} else if !errors.Is(err, gitcandidate.ErrBase) && !errors.Is(err, gitcandidate.ErrInvalidInput) {
			t.Fatalf("Begin(%s) error = %v, want ErrBase or ErrInvalidInput", name, err)
		}
	}
}

func TestApplyIsAtomicAndLeavesCanonicalByteIdentical(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	before := attest(t, store)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	edits := []changeset.Edit{
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
	outcome, err := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-1", IdempotencyKey: "apply-1", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != gitcandidate.StateApplied || outcome.Rollback != nil {
		t.Fatalf("outcome = %#v, want APPLIED without rollback", outcome)
	}
	if outcome.PostImageDigest.Hex == "" || outcome.PostImageTreeOID == "" {
		t.Fatalf("outcome missing post-image facts: %#v", outcome)
	}
	recomputed, err := store.PostImageDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != outcome.PostImageDigest {
		t.Fatalf("post-image digest %v != recomputed %v", outcome.PostImageDigest, recomputed)
	}
	for name, want := range map[string]string{
		"src/go/modify-00.go":  "package changed00\n",
		"src/go/add-00.go":     "package add00\n",
		"src/go/renamed-00.go": "package renamed00\n",
	} {
		content, err := candidate.ReadFile(name)
		if err != nil || string(content) != want {
			t.Fatalf("candidate %q = %q, %v; want %q", name, content, err, want)
		}
	}
	if _, err := candidate.ReadFile("src/go/delete-00.go"); err == nil {
		t.Fatal("deleted file still readable in candidate")
	}
	if _, err := candidate.ReadFile("src/go/rename-00.go"); err == nil {
		t.Fatal("rename pre-image still readable in candidate")
	}
	if after := attest(t, store); after != before {
		t.Fatalf("canonical attestation changed: before %+v after %+v", before, after)
	}
}

func TestPartialEditFailureDiscardsWholeCandidate(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	before := attest(t, store)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := candidate.Path
	edits := make([]changeset.Edit, 0, 10)
	for index := 0; index < 5; index++ {
		edits = append(edits, modifyEdit(
			fmt.Sprintf("src/go/modify-%02d.go", index),
			fmt.Sprintf("package modify%02d\n", index),
			fmt.Sprintf("package changed%02d\n", index),
		))
	}
	for index := 0; index < 5; index++ {
		edits = append(edits, addEdit(fmt.Sprintf("src/go/add-%02d.go", index), fmt.Sprintf("package add%02d\n", index)))
	}
	authorizer := &denyAt{index: 6}
	outcome, err := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-2", IdempotencyKey: "apply-2", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: authorizer,
	})
	if err == nil || !errors.Is(err, gitcandidate.ErrDenied) {
		t.Fatalf("Apply error = %v, want ErrDenied", err)
	}
	if authorizer.calls != 7 {
		t.Fatalf("authorizer calls = %d, want exactly 7 mutation-time checks", authorizer.calls)
	}
	if outcome == nil || outcome.State != gitcandidate.StateRejected {
		t.Fatalf("outcome = %#v, want REJECTED", outcome)
	}
	rollback := outcome.Rollback
	if rollback == nil {
		t.Fatal("rejected outcome carries no rollback receipt")
	}
	if rollback.Receipt.Status != "rejected" || rollback.Receipt.ReasonCode != gitcandidate.ReasonApplyFailed {
		t.Fatalf("rollback receipt = %#v, want rejected/candidate_apply_failed", rollback.Receipt)
	}
	if rollback.FailedEditIndex != 6 || rollback.DiscardedEdits != 6 {
		t.Fatalf("rollback indexes = %d/%d, want 6 discarded and failure at 6",
			rollback.DiscardedEdits, rollback.FailedEditIndex)
	}
	if rollback.CandidateID != candidate.ID || rollback.BaseGitOID != base || rollback.ChangeSetDigest.Hex == "" {
		t.Fatalf("rollback binding = %#v, want candidate, base, and changeset digest", rollback)
	}
	if _, statErr := os.Stat(candidatePath); !os.IsNotExist(statErr) {
		t.Fatalf("candidate directory survives rejection: %v", statErr)
	}
	if after := attest(t, store); after != before {
		t.Fatalf("canonical attestation changed after partial failure: before %+v after %+v", before, after)
	}
}

func TestStalePreimageDigestRejectsWithRollback(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	before := attest(t, store)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	edits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package WRONG\n", "package changed00\n")}
	outcome, err := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-3", IdempotencyKey: "apply-3", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err == nil || !errors.Is(err, gitcandidate.ErrApply) {
		t.Fatalf("Apply error = %v, want ErrApply", err)
	}
	if outcome.State != gitcandidate.StateRejected || outcome.Rollback == nil {
		t.Fatalf("outcome = %#v, want REJECTED with rollback", outcome)
	}
	if _, statErr := os.Stat(candidate.Path); !os.IsNotExist(statErr) {
		t.Fatal("candidate directory survives stale pre-image rejection")
	}
	if after := attest(t, store); after != before {
		t.Fatal("canonical attestation changed after stale pre-image rejection")
	}
}

func TestDuplicateApplicationReplaysAndConflictingReuseDenies(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	edits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package modify00\n", "package changed00\n")}
	first, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Apply(context.Background(), first, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-4", IdempotencyKey: "apply-4", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil || original.State != gitcandidate.StateApplied {
		t.Fatalf("first apply = %#v, %v", original, err)
	}
	second, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := second.Path
	replayed, err := store.Apply(context.Background(), second, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-4", IdempotencyKey: "apply-4", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil {
		t.Fatalf("exact replay = %v, want original outcome", err)
	}
	if replayed.CandidateID != original.CandidateID || replayed.PostImageDigest != original.PostImageDigest ||
		replayed.State != gitcandidate.StateApplied {
		t.Fatalf("replay = %#v, want original outcome %#v", replayed, original)
	}
	if _, statErr := os.Stat(secondPath); !os.IsNotExist(statErr) {
		t.Fatal("replay left a second candidate directory behind")
	}
	conflictingEdits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package modify00\n", "package OTHER\n")}
	third, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	thirdPath := third.Path
	if _, err := store.Apply(context.Background(), third, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-4", IdempotencyKey: "apply-4", Tenant: testTenant, Principal: testPrincipal, Edits: conflictingEdits, Authorizer: allowAll{},
	}); !errors.Is(err, gitcandidate.ErrConflict) {
		t.Fatalf("conflicting reuse = %v, want ErrConflict", err)
	}
	if _, statErr := os.Stat(thirdPath); !os.IsNotExist(statErr) {
		t.Fatal("conflicting reuse left the fresh candidate directory behind")
	}
}

func TestSymlinkEscapeCannotLeaveCandidate(t *testing.T) {
	canonical, git, _ := newRepository(t, baseFiles())
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(canonical, "src", "go", "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, canonical, "add", "--all")
	runGit(t, git, canonical, "commit", "-q", "-m", "add symlink")
	base := gitOutput(t, git, canonical, "rev-parse", "HEAD")
	store := newStore(t, canonical, git)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	// A pre-image resolved through the symlink must fail: the store resolves
	// every path component and refuses to follow it outside the candidate.
	edits := []changeset.Edit{modifyEdit("src/go/link/escape.go", "x\n", "package escape\n")}
	outcome, err := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-5", IdempotencyKey: "apply-5", Tenant: testTenant, Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err == nil {
		t.Fatalf("symlink pre-image apply = %#v, want rejection", outcome)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("escape wrote outside the candidate: entries=%v err=%v", entries, readErr)
	}
	if _, statErr := os.Stat(candidate.Path); !os.IsNotExist(statErr) {
		t.Fatal("candidate directory survives symlink rejection")
	}
}

// blockingAuthorizer holds the first mutation until released, giving tests a
// deterministic in-flight window.
type blockingAuthorizer struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingAuthorizer) AuthorizeMutation(_ context.Context, mutation gitcandidate.Mutation) error {
	if mutation.Index == 0 {
		close(b.started)
		<-b.release
	}
	return nil
}

func TestCrossPrincipalSameKeyComputesOwnCandidate(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	edits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package modify00\n", "package changed00\n")}
	second := contracts.Identifier{Namespace: "principal", Value: "p2"}

	first, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Apply(context.Background(), first, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-6", IdempotencyKey: "shared-key", Tenant: testTenant,
		Principal: testPrincipal, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil || original.State != gitcandidate.StateApplied {
		t.Fatalf("first apply = %#v, %v", original, err)
	}
	other, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := store.Apply(context.Background(), other, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-6", IdempotencyKey: "shared-key", Tenant: testTenant,
		Principal: second, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil {
		t.Fatalf("cross-principal same-key apply = %v, want an independent scope", err)
	}
	if independent.State != gitcandidate.StateApplied || independent.CandidateID == original.CandidateID {
		t.Fatalf("cross-principal outcome = %#v, want its own APPLIED candidate", independent)
	}
	if independent.PostImageDigest != original.PostImageDigest {
		t.Fatal("identical edits must produce identical post-image digests across scopes")
	}
	// Each principal's exact replay returns its own recorded outcome.
	replay, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Apply(context.Background(), replay, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-6", IdempotencyKey: "shared-key", Tenant: testTenant,
		Principal: second, Edits: edits, Authorizer: allowAll{},
	})
	if err != nil || replayed.CandidateID != independent.CandidateID {
		t.Fatalf("second principal replay = %#v, %v; want its own original outcome", replayed, err)
	}
}

func TestConcurrentInvalidApplyNeverRemovesActiveCandidate(t *testing.T) {
	canonical, git, base := newRepository(t, baseFiles())
	store := newStore(t, canonical, git)
	candidate, err := store.Begin(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	edits := []changeset.Edit{modifyEdit("src/go/modify-00.go", "package modify00\n", "package changed00\n")}
	authorizer := &blockingAuthorizer{started: make(chan struct{}), release: make(chan struct{})}
	validResult := make(chan error, 1)
	go func() {
		_, err := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
			ChangeSetID: "changeset-7", IdempotencyKey: "race-key", Tenant: testTenant,
			Principal: testPrincipal, Edits: edits, Authorizer: authorizer,
		})
		validResult <- err
	}()
	<-authorizer.started
	duplicate := addEdit("src/go/add-00.go", "package add00\n")
	invalid := []changeset.Edit{duplicate, duplicate}
	_, invalidErr := store.Apply(context.Background(), candidate, gitcandidate.ApplyRequest{
		ChangeSetID: "changeset-7", IdempotencyKey: "race-key", Tenant: testTenant,
		Principal: testPrincipal, Edits: invalid, Authorizer: allowAll{},
	})
	close(authorizer.release)
	if err := <-validResult; err != nil {
		t.Fatalf("valid in-flight apply = %v, want success", err)
	}
	if !errors.Is(invalidErr, gitcandidate.ErrConflict) {
		t.Fatalf("concurrent invalid apply = %v, want ErrConflict, never a removal", invalidErr)
	}
	if _, statErr := os.Stat(candidate.Path); statErr != nil {
		t.Fatalf("active candidate removed by a concurrent invalid apply: %v", statErr)
	}
	content, err := candidate.ReadFile("src/go/modify-00.go")
	if err != nil || string(content) != "package changed00\n" {
		t.Fatalf("candidate content = %q, %v; want the applied edit intact", content, err)
	}
}
