// Process tests prove the bounded Stage 05 factory user path through the
// production command, the owner-only socket, the durable authority, the sealed
// runner over isolated exact-base candidates, and the real checked-in Bun
// factory TUI. The acceptance matrix mirrors the frozen factory fixture: one
// approved intent becomes a typed DAG, one atomic multi-file candidate, the
// required gates, and typed review findings — plus stale lease, stale base,
// duplicate message, leaf escape attempt, partial edit failure, failed gate,
// revoke mid-run, and rollback, with the canonical repository byte-identical
// in every outcome.
package authorityprocess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// factoryProcessDescriptor is one staged approval descriptor with its
// digest-bound CLI facts.
type factoryProcessDescriptor struct {
	artifactID       string
	revision         string
	approvalID       string
	approvalExpiry   int64
	approvalRecorded int64
	payload          []byte
	digestHex        string
}

// factoryProcessCase declares one matrix case's descriptor content.
type factoryProcessCase struct {
	name       string
	scopePaths []string
	review     bool
	leaves     []factoryProcessLeaf
	findings   []map[string]string
}

type factoryProcessLeaf struct {
	nodeID         string
	goal           string
	ownedPaths     []string
	forbiddenPaths []string
	edits          []map[string]string
}

var factoryRunIDPattern = regexp.MustCompile(`run=factory-run:([0-9a-f]{64})`)

// TestLocalAuthorityFactoryProcessTracer drives the Stage 05 v1 acceptance
// matrix end to end: the real TUI talks to the real production command over
// the owner-only socket, approvals stage through the real authorized artifact
// path, leaves execute through the sealed runner against isolated exact-base
// candidates, and the canonical repository stays byte-identical in every
// outcome. No in-process shortcut stands in for the user path.
func TestLocalAuthorityFactoryProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	base := fixture.prepareFactorySource(t)
	canonicalAttestation := factoryCanonicalAttestation(t, fixture.sourceRoot)
	assertCanonical := func() {
		t.Helper()
		if current := factoryCanonicalAttestation(t, fixture.sourceRoot); current != canonicalAttestation {
			t.Fatalf("canonical repository mutated:\nbefore %x\nafter  %x", canonicalAttestation, current)
		}
	}
	// Build and stage descriptor payloads before the daemon starts so the
	// process helper can PrepareArtifact them the same way it stages the
	// Stage 02 artifact-a fixture. The subsequent admit publishes ledger
	// facts over those staged bytes; admission never trusts client payload.
	descriptors := buildFactoryProcessDescriptors(t, fixture)
	writeFactoryDescriptorStaging(t, fixture, descriptors)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "factory-open-session",
	}, "[OK] Local session")
	baseArgs := []string{"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	add := processIngestionCommand("add", baseArgs,
		"--commit", base, "--configuration-digest", fixture.configDigest,
		"--idempotency", "factory-source-add", "--use-gitignore", "true", "--use-ouroborosignore", "true")
	if output := runIngestionTUI(t, tui, fixture.socketPath, add...); !strings.Contains(output, "Source added") {
		t.Fatalf("factory source add = %q", output)
	}

	// Publish every staged approval descriptor through the real authorized
	// artifact.admit path so factory admission revalidates against ledger
	// state, never client-supplied bytes.
	for _, descriptor := range descriptors {
		assertTUISucceeds(t, tui, fixture.socketPath,
			processCommandArguments(t, factoryDescriptorAdmitRequest(t, fixture, descriptor)),
			"[OK] Authority command", "generation=1")
	}

	admit := func(name, idempotencyKey, overrideBase string) (string, int) {
		t.Helper()
		descriptor := descriptors[name]
		intentBase := base
		if overrideBase != "" {
			intentBase = overrideBase
		}
		return runFactoryTUI(t, tui, fixture.socketPath, "change", "admit",
			"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
			"--intent", "intent:"+name,
			"--base", intentBase,
			"--scope-digest", descriptor.digestHex,
			"--evidence", "artifact:"+descriptor.artifactID,
			"--evidence-revision", "revision:"+descriptor.revision,
			"--approval", "approval:"+descriptor.approvalID,
			"--approval-expiry", strconv.FormatInt(descriptor.approvalExpiry, 10),
			"--approval-recorded", strconv.FormatInt(descriptor.approvalRecorded, 10),
			"--idempotency", idempotencyKey,
			"--timeout-ms", "10000")
	}
	read := func(verb, runID string, extra ...string) (string, int) {
		t.Helper()
		arguments := append([]string{"change", verb,
			"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
			"--run", "factory-run:" + runID, "--timeout-ms", "10000"}, extra...)
		return runFactoryTUI(t, tui, fixture.socketPath, arguments...)
	}
	runID := func(output string) string {
		t.Helper()
		matched := factoryRunIDPattern.FindStringSubmatch(output)
		if len(matched) != 2 {
			t.Fatalf("output carried no run identity: %q", output)
		}
		return matched[1]
	}
	assertFragments := func(output string, fragments ...string) {
		t.Helper()
		for _, fragment := range fragments {
			if !strings.Contains(output, fragment) {
				t.Fatalf("output %q omitted %q", output, fragment)
			}
		}
	}
	assertDenied := func(output string, code int) {
		t.Helper()
		if code != 4 || !strings.Contains(output, "[DENIED]") || !strings.Contains(output, "not_found_or_denied") {
			t.Fatalf("expected static denial, got %d: %q", code, output)
		}
		for _, forbidden := range []string{"artifact-factory-approval", "principal-a", "tenant-a", "src/go"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("denial disclosed %q: %q", forbidden, output)
			}
		}
	}

	// Happy path: one approved intent becomes a typed two-leaf DAG, one atomic
	// multi-file candidate, the required gates, and typed review findings.
	happyOutput, happyCode := admit("happy-path", "admit-happy-path", "")
	if happyCode != 0 {
		t.Fatalf("happy admit = %d: %s", happyCode, happyOutput)
	}
	assertFragments(happyOutput, "[ADMITTED]", "state=PLANNING")
	happyRun := runID(happyOutput)
	planOutput, planCode := read("plan", happyRun)
	if planCode != 0 {
		t.Fatalf("happy plan = %d: %s", planCode, planOutput)
	}
	assertFragments(planOutput, "[OK]", "state=PLANNING", "base="+base,
		"node=orchestrator kind=orchestrator", "node=leaf-a kind=leaf scope=[src/go/modify-00.go]",
		"node=leaf-b kind=leaf scope=[src/go/modify-01.go]",
		"node=review kind=review", "edge orchestrator -> leaf-a", "edge orchestrator -> leaf-b",
		"fence=1", "route model=deterministic-v1", "gates=", "BUILD:PENDING", "TEST:PENDING", "DOCS:PENDING", "SECURITY:PENDING")
	candidateOutput, candidateCode := read("candidate", happyRun)
	if candidateCode != 0 {
		t.Fatalf("happy candidate = %d: %s", candidateCode, candidateOutput)
	}
	assertFragments(candidateOutput, "[OK]", "state=VERIFIED", "base="+base,
		"edit MODIFY GO src/go/modify-00.go", "edit MODIFY GO src/go/modify-01.go",
		"obligation GO docs=required tests=required",
		"BUILD:PASSED", "TEST:PASSED", "DOCS:PASSED", "SECURITY:PASSED")
	reviewOutput, reviewCode := read("review", happyRun, "--page-size", "100")
	if reviewCode != 0 {
		t.Fatalf("happy review = %d: %s", reviewCode, reviewOutput)
	}
	assertFragments(reviewOutput, "[OK]", "severity=INFO", "category=DOCS", "disposition=DISMISSED_WITH_EVIDENCE")
	retainedOutput, retainedCode := read("candidate", happyRun)
	if retainedCode != 0 {
		t.Fatalf("retained candidate = %d: %s", retainedCode, retainedOutput)
	}
	assertFragments(retainedOutput, "state=RETAINED", "BUILD:PASSED", "TEST:PASSED", "DOCS:PASSED", "SECURITY:PASSED")
	completedPlan, completedPlanCode := read("plan", happyRun)
	if completedPlanCode != 0 {
		t.Fatalf("completed plan = %d: %s", completedPlanCode, completedPlan)
	}
	assertFragments(completedPlan, "state=COMPLETED")
	assertCanonical()

	// Restart: durable run, plan, candidate, and finding facts survive; the
	// exact admission replay returns the original run without re-executing.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemon(t, fixture)
	restartedPlan, restartedPlanCode := read("plan", happyRun)
	if restartedPlanCode != 0 {
		t.Fatalf("restarted plan = %d: %s", restartedPlanCode, restartedPlan)
	}
	assertFragments(restartedPlan, "state=COMPLETED", "base="+base)
	restartedCandidate, restartedCandidateCode := read("candidate", happyRun)
	if restartedCandidateCode != 0 {
		t.Fatalf("restarted candidate = %d: %s", restartedCandidateCode, restartedCandidate)
	}
	assertFragments(restartedCandidate, "state=RETAINED", "edit MODIFY GO src/go/modify-00.go")
	restartedReview, restartedReviewCode := read("review", happyRun, "--page-size", "100")
	if restartedReviewCode != 0 {
		t.Fatalf("restarted review = %d: %s", restartedReviewCode, restartedReview)
	}
	assertFragments(restartedReview, "severity=INFO", "disposition=DISMISSED_WITH_EVIDENCE")
	replayOutput, replayCode := admit("happy-path", "admit-happy-path", "")
	if replayCode != 0 || runID(replayOutput) != happyRun {
		t.Fatalf("admission replay = %d: %s", replayCode, replayOutput)
	}

	// Stale base: admission revalidates the exact base against the Stage 03
	// catalog; a superseded base denies statically and no run opens.
	staleBase := strings.Repeat("b", 40)
	staleOutput, staleCode := admit("stale-base", "admit-stale-base", staleBase)
	assertDenied(staleOutput, staleCode)
	assertCanonical()

	// Duplicate message: an exact authenticated replay returns the original
	// outcome; a conflicting key reuse denies without mutation.
	firstDuplicate, firstDuplicateCode := admit("duplicate", "admit-duplicate", "")
	if firstDuplicateCode != 0 {
		t.Fatalf("duplicate admit = %d: %s", firstDuplicateCode, firstDuplicate)
	}
	duplicateRun := runID(firstDuplicate)
	secondDuplicate, secondDuplicateCode := admit("duplicate", "admit-duplicate", "")
	if secondDuplicateCode != 0 || runID(secondDuplicate) != duplicateRun {
		t.Fatalf("duplicate replay = %d: %s", secondDuplicateCode, secondDuplicate)
	}
	conflictOutput, conflictCode := admit("duplicate-conflict", "admit-duplicate", "")
	assertDenied(conflictOutput, conflictCode)
	duplicatePlan, duplicatePlanCode := read("plan", duplicateRun)
	if duplicatePlanCode != 0 {
		t.Fatalf("duplicate plan = %d: %s", duplicatePlanCode, duplicatePlan)
	}
	assertFragments(duplicatePlan, "state=PLANNING")
	assertCanonical()

	// Leaf escape attempt: the sealed leaf's forbidden-path probe denies, the
	// security gate fails, the run fails closed, and no candidate exists.
	escapeOutput, escapeCode := admit("escape", "admit-escape", "")
	if escapeCode != 0 {
		t.Fatalf("escape admit = %d: %s", escapeCode, escapeOutput)
	}
	escapeRun := runID(escapeOutput)
	escapeCandidate, escapeCandidateCode := read("candidate", escapeRun)
	assertDenied(escapeCandidate, escapeCandidateCode)
	escapePlan, escapePlanCode := read("plan", escapeRun)
	if escapePlanCode != 0 {
		t.Fatalf("escape plan = %d: %s", escapePlanCode, escapePlan)
	}
	assertFragments(escapePlan, "state=FAILED", "SECURITY:FAILED")
	assertCanonical()

	// Partial edit failure: the add targeting an existing base file fails the
	// atomic application mid-set; the proposed candidate carries its rollback
	// receipt, the run fails, and canonical source is untouched.
	partialOutput, partialCode := admit("partial", "admit-partial", "")
	if partialCode != 0 {
		t.Fatalf("partial admit = %d: %s", partialCode, partialOutput)
	}
	partialRun := runID(partialOutput)
	partialCandidate, partialCandidateCode := read("candidate", partialRun)
	if partialCandidateCode != 0 {
		t.Fatalf("partial candidate = %d: %s", partialCandidateCode, partialCandidate)
	}
	assertFragments(partialCandidate, "state=REJECTED", "rollback receipt=",
		"edit MODIFY GO src/go/modify-00.go", "BUILD:PENDING")
	partialPlan, partialPlanCode := read("plan", partialRun)
	if partialPlanCode != 0 {
		t.Fatalf("partial plan = %d: %s", partialPlanCode, partialPlan)
	}
	assertFragments(partialPlan, "state=FAILED")
	assertCanonical()

	// Failed gate: the deterministic test gate fails the unparseable Go
	// candidate; the verified transition never happens and the candidate is
	// rejected with rollback behind BUILD passed and the rest pending.
	failedGateOutput, failedGateCode := admit("failed-gate", "admit-failed-gate", "")
	if failedGateCode != 0 {
		t.Fatalf("failed-gate admit = %d: %s", failedGateCode, failedGateOutput)
	}
	failedGateRun := runID(failedGateOutput)
	failedGateCandidate, failedGateCandidateCode := read("candidate", failedGateRun)
	if failedGateCandidateCode != 0 {
		t.Fatalf("failed-gate candidate = %d: %s", failedGateCandidateCode, failedGateCandidate)
	}
	assertFragments(failedGateCandidate, "state=REJECTED", "rollback receipt=",
		"BUILD:PASSED", "TEST:FAILED", "DOCS:PENDING", "SECURITY:PENDING")
	assertCanonical()

	// Rollback after review: the fresh review's undisposed blocker rejects the
	// verified candidate through its frozen rollback artifact and receipt.
	rollbackOutput, rollbackCode := admit("rollback", "admit-rollback", "")
	if rollbackCode != 0 {
		t.Fatalf("rollback admit = %d: %s", rollbackCode, rollbackOutput)
	}
	rollbackRun := runID(rollbackOutput)
	verifiedOutput, verifiedCode := read("candidate", rollbackRun)
	if verifiedCode != 0 {
		t.Fatalf("rollback candidate = %d: %s", verifiedCode, verifiedOutput)
	}
	assertFragments(verifiedOutput, "state=VERIFIED", "edit RENAME GO src/go/rename-01.go (from src/go/rename-00.go)")
	rollbackReview, rollbackReviewCode := read("review", rollbackRun, "--page-size", "100")
	if rollbackReviewCode != 0 {
		t.Fatalf("rollback review = %d: %s", rollbackReviewCode, rollbackReview)
	}
	assertFragments(rollbackReview, "severity=BLOCKER", "category=CORRECTNESS", "disposition=OPEN")
	rejectedOutput, rejectedCode := read("candidate", rollbackRun)
	if rejectedCode != 0 {
		t.Fatalf("rejected candidate = %d: %s", rejectedCode, rejectedOutput)
	}
	assertFragments(rejectedOutput, "state=REJECTED", "rollback receipt=")
	rollbackPlan, rollbackPlanCode := read("plan", rollbackRun)
	if rollbackPlanCode != 0 {
		t.Fatalf("rollback plan = %d: %s", rollbackPlanCode, rollbackPlan)
	}
	assertFragments(rollbackPlan, "state=FAILED")
	assertCanonical()

	// Revoke mid-run: cancellation preempts at a safe point, every later read
	// denies statically, and an exact cancel replay returns the original
	// terminal outcome across a restart.
	revokeOutput, revokeCode := admit("revoke", "admit-revoke", "")
	if revokeCode != 0 {
		t.Fatalf("revoke admit = %d: %s", revokeCode, revokeOutput)
	}
	revokeRun := runID(revokeOutput)
	cancelOutput, cancelCode := runFactoryTUI(t, tui, fixture.socketPath, "change", "cancel",
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
		"--run", "factory-run:"+revokeRun, "--idempotency", "cancel-revoke", "--timeout-ms", "10000")
	if cancelCode != 0 {
		t.Fatalf("cancel = %d: %s", cancelCode, cancelOutput)
	}
	assertFragments(cancelOutput, "[CANCELLED]", "state=CANCELLED")
	cancelledPlan, cancelledPlanCode := read("plan", revokeRun)
	assertDenied(cancelledPlan, cancelledPlanCode)
	cancelledCandidate, cancelledCandidateCode := read("candidate", revokeRun)
	assertDenied(cancelledCandidate, cancelledCandidateCode)
	cancelReplay, cancelReplayCode := runFactoryTUI(t, tui, fixture.socketPath, "change", "cancel",
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
		"--run", "factory-run:"+revokeRun, "--idempotency", "cancel-revoke", "--timeout-ms", "10000")
	if cancelReplayCode != 0 {
		t.Fatalf("cancel replay = %d: %s", cancelReplayCode, cancelReplay)
	}
	assertFragments(cancelReplay, "[CANCELLED]", "state=CANCELLED")
	conflictingCancel, conflictingCancelCode := runFactoryTUI(t, tui, fixture.socketPath, "change", "cancel",
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
		"--run", "factory-run:"+revokeRun, "--idempotency", "cancel-revoke-other", "--timeout-ms", "10000")
	assertDenied(conflictingCancel, conflictingCancelCode)
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemon(t, fixture)
	restartedCancel, restartedCancelCode := runFactoryTUI(t, tui, fixture.socketPath, "change", "cancel",
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a",
		"--run", "factory-run:"+revokeRun, "--idempotency", "cancel-revoke", "--timeout-ms", "10000")
	if restartedCancelCode != 0 {
		t.Fatalf("restarted cancel replay = %d: %s", restartedCancelCode, restartedCancel)
	}
	assertFragments(restartedCancel, "[CANCELLED]", "state=CANCELLED")
	restartedCancelledPlan, restartedCancelledPlanCode := read("plan", revokeRun)
	assertDenied(restartedCancelledPlan, restartedCancelledPlanCode)
	assertCanonical()

	// Stale lease: a short-lived lease expires before the candidate read; the
	// leaf execution reauthorizes against the live clock, denies stale, leaves
	// the run alive at its safe point, and never commits a candidate.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemonWithEnv(t, fixture, []string{"OUROBOROS_FACTORY_LEASE_TTL_MS=750"})
	staleLeaseOutput, staleLeaseCode := admit("stale-lease", "admit-stale-lease", "")
	if staleLeaseCode != 0 {
		t.Fatalf("stale-lease admit = %d: %s", staleLeaseCode, staleLeaseOutput)
	}
	staleLeaseRun := runID(staleLeaseOutput)
	time.Sleep(1500 * time.Millisecond)
	staleLeaseCandidate, staleLeaseCandidateCode := read("candidate", staleLeaseRun)
	assertDenied(staleLeaseCandidate, staleLeaseCandidateCode)
	staleLeasePlan, staleLeasePlanCode := read("plan", staleLeaseRun)
	if staleLeasePlanCode != 0 {
		t.Fatalf("stale-lease plan = %d: %s", staleLeasePlanCode, staleLeasePlan)
	}
	assertFragments(staleLeasePlan, "state=RUNNING")
	assertCanonical()

	stopProcessDaemon(t, daemon, fixture.socketPath)
	assertDurableStateClosed(t, fixture.stateRoot)
	// Goal prose, approval descriptors, and finding summaries live only inside
	// encrypted vault payloads; nothing plaintext persists in the authority state.
	for _, plaintext := range []string{"rename the seeded marker accessor", "boundary probe", "fresh review"} {
		assertStateDoesNotContain(t, fixture.stateRoot, []byte(plaintext))
	}
	fixture.cleanup(t)
	tui.cleanup(t)
}

// prepareFactorySource seeds the canonical fixture repository the factory
// matrix runs against: valid Go lanes for the happy and partial paths, one
// add target pre-existing at base for the atomicity failure, one unparseable
// Go file for the failed gate, and one rename pre-image for the rollback path.
func (fixture *processFixture) prepareFactorySource(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"src/go/modify-00.go": "package main\n\n// Marker returns the seeded stage marker.\nfunc Marker() string { return \"stage-five\" }\n",
		"src/go/modify-01.go": "package main\n\n// Second returns the second seeded marker.\nfunc Second() string { return \"stage-five-second\" }\n",
		"src/go/modify-02.go": "package main\n\n// Third returns the third seeded marker.\nfunc Third() string { return \"stage-five-third\" }\n",
		"src/go/modify-03.go": "package main\n\n// Fourth returns the fourth seeded marker.\nfunc Fourth() string { return \"stage-five-fourth\" }\n",
		"src/go/modify-04.go": "package main\n\n// Fifth returns the fifth seeded marker.\nfunc Fifth() string { return \"stage-five-fifth\" }\n",
		"src/go/add-01.go":    "package main\n",
		"src/go/broken-00.go": "package main\n\nfunc Broken( {\n",
		"src/go/rename-00.go": "package main\n\n// Renamed returns the rename pre-image marker.\nfunc Renamed() string { return \"stage-five-rename\" }\n",
		"README.md":           "# stage five process fixture\n",
	}
	for relative, contents := range files {
		path := filepath.Join(fixture.sourceRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	processGit(t, fixture.sourceRoot, "add", "--all")
	processGit(t, fixture.sourceRoot, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid",
		"commit", "-m", "stage five factory fixture")
	return processGit(t, fixture.sourceRoot, "rev-parse", "HEAD")
}

// factoryCanonicalAttestation digests the complete canonical repository
// inventory — worktree plus every .git file, mode, and byte — so the matrix
// proves byte-identical canonical state across success and every failure.
func factoryCanonicalAttestation(t *testing.T, root string) [32]byte {
	t.Helper()
	type record struct {
		path string
		mode string
		sum  [32]byte
	}
	records := make([]record, 0, 64)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular canonical entry: %s", path)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		records = append(records, record{
			path: filepath.ToSlash(relative), mode: info.Mode().String(), sum: sha256.Sum256(contents),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(records); index++ {
		for position := index; position > 0 && records[position-1].path > records[position].path; position-- {
			records[position-1], records[position] = records[position], records[position-1]
		}
	}
	hasher := sha256.New()
	for _, entry := range records {
		hasher.Write([]byte(entry.path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(entry.mode))
		hasher.Write([]byte{0})
		hasher.Write(entry.sum[:])
	}
	return [32]byte(hasher.Sum(nil))
}

// buildFactoryProcessDescriptors authors the approval descriptors for the
// matrix and returns them keyed by case name. Approval expiry pins the bounded
// fixture horizon so a paused run fails explicitly, never silently.
func buildFactoryProcessDescriptors(t *testing.T, fixture *processFixture) map[string]factoryProcessDescriptor {
	t.Helper()
	recorded := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Unix()
	expiry := fixture.grantExpiry.Unix()
	cases := map[string]factoryProcessCase{
		"happy-path": {
			name:       "happy-path",
			scopePaths: []string{"src/go/modify-00.go", "src/go/modify-01.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-00.go",
					ownedPaths: []string{"src/go/modify-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
				{nodeID: "leaf-b", goal: "rename the seeded marker accessor in src/go/modify-01.go",
					ownedPaths: []string{"src/go/modify-01.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-01.go"}}},
			},
			findings: []map[string]string{{
				"severity": "INFO", "category": "DOCS", "summary": "fresh review note: docs obligation verified",
				"disposition": "DISMISSED_WITH_EVIDENCE",
			}},
		},
		"stale-base": {
			name:       "stale-base",
			scopePaths: []string{"src/go/modify-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-00.go",
					ownedPaths: []string{"src/go/modify-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
			},
		},
		"stale-lease": {
			name:       "stale-lease",
			scopePaths: []string{"src/go/modify-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-00.go",
					ownedPaths: []string{"src/go/modify-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
			},
		},
		"duplicate": {
			name:       "duplicate",
			scopePaths: []string{"src/go/modify-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-00.go",
					ownedPaths: []string{"src/go/modify-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
			},
		},
		"duplicate-conflict": {
			name:       "duplicate-conflict",
			scopePaths: []string{"src/go/modify-01.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-01.go",
					ownedPaths: []string{"src/go/modify-01.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-01.go"}}},
			},
		},
		"escape": {
			name:       "escape",
			scopePaths: []string{"src/go/modify-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor and probe the sealed boundary",
					ownedPaths: []string{"src/go/modify-00.go"}, forbiddenPaths: []string{"src/typescript"},
					edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
			},
		},
		"partial": {
			name: "partial",
			scopePaths: []string{
				"src/go/modify-00.go", "src/go/modify-01.go", "src/go/modify-02.go", "src/go/modify-03.go",
				"src/go/modify-04.go", "src/go/add-00.go", "src/go/add-01.go", "src/go/add-02.go",
				"src/go/add-03.go", "src/go/add-04.go",
			},
			review: true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessors in five files",
					ownedPaths: []string{
						"src/go/modify-00.go", "src/go/modify-01.go", "src/go/modify-02.go",
						"src/go/modify-03.go", "src/go/modify-04.go",
					},
					edits: []map[string]string{
						{"op": "modify", "path": "src/go/modify-00.go"}, {"op": "modify", "path": "src/go/modify-01.go"},
						{"op": "modify", "path": "src/go/modify-02.go"}, {"op": "modify", "path": "src/go/modify-03.go"},
						{"op": "modify", "path": "src/go/modify-04.go"},
					}},
				{nodeID: "leaf-b", goal: "add the five seeded marker test files",
					ownedPaths: []string{
						"src/go/add-00.go", "src/go/add-01.go", "src/go/add-02.go",
						"src/go/add-03.go", "src/go/add-04.go",
					},
					edits: []map[string]string{
						{"op": "add", "path": "src/go/add-00.go"}, {"op": "add", "path": "src/go/add-01.go"},
						{"op": "add", "path": "src/go/add-02.go"}, {"op": "add", "path": "src/go/add-03.go"},
						{"op": "add", "path": "src/go/add-04.go"},
					}},
			},
		},
		"failed-gate": {
			name:       "failed-gate",
			scopePaths: []string{"src/go/broken-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in the unparseable file",
					ownedPaths: []string{"src/go/broken-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/broken-00.go"}}},
			},
		},
		"revoke": {
			name:       "revoke",
			scopePaths: []string{"src/go/modify-00.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker accessor in src/go/modify-00.go",
					ownedPaths: []string{"src/go/modify-00.go"}, edits: []map[string]string{{"op": "modify", "path": "src/go/modify-00.go"}}},
			},
		},
		"rollback": {
			name:       "rollback",
			scopePaths: []string{"src/go/rename-00.go", "src/go/rename-01.go", "src/go/modify-01.go"},
			review:     true,
			leaves: []factoryProcessLeaf{
				{nodeID: "leaf-a", goal: "rename the seeded marker file and update its test",
					ownedPaths: []string{"src/go/rename-00.go", "src/go/rename-01.go", "src/go/modify-01.go"},
					edits: []map[string]string{
						{"op": "rename", "path": "src/go/rename-01.go", "oldPath": "src/go/rename-00.go"},
						{"op": "modify", "path": "src/go/modify-01.go"},
					}},
			},
			findings: []map[string]string{{
				"severity": "BLOCKER", "category": "CORRECTNESS",
				"summary":     "fresh review: the rename drops the marker contract and must not be retained",
				"disposition": "OPEN",
			}},
		},
	}
	descriptors := make(map[string]factoryProcessDescriptor, len(cases))
	for name, descriptorCase := range cases {
		leaves := make([]map[string]any, 0, len(descriptorCase.leaves))
		for _, leaf := range descriptorCase.leaves {
			leaves = append(leaves, map[string]any{
				"nodeId": leaf.nodeID, "goal": leaf.goal, "ownedPaths": leaf.ownedPaths,
				"forbiddenPaths": leaf.forbiddenPaths, "edits": leaf.edits,
			})
		}
		payload, err := json.Marshal(map[string]any{
			"version":          "ouroboros.stage05.factory-approval.v1",
			"intentId":         name,
			"evidenceRevision": "revision-" + name,
			"approval": map[string]any{
				"approvalId":            "approval-" + name,
				"expiresAtUnixSeconds":  expiry,
				"recordedAtUnixSeconds": recorded,
			},
			"scopePaths": descriptorCase.scopePaths,
			"review":     descriptorCase.review,
			"leaves":     leaves,
			"findings":   descriptorCase.findings,
		})
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		descriptors[name] = factoryProcessDescriptor{
			artifactID:       "artifact-factory-approval-" + name,
			revision:         "revision-" + name,
			approvalID:       "approval-" + name,
			approvalExpiry:   expiry,
			approvalRecorded: recorded,
			payload:          payload,
			digestHex:        hex.EncodeToString(digest[:]),
		}
	}
	return descriptors
}

// writeFactoryDescriptorStaging drops one owner-only payload file per
// approval descriptor next to the process bootstrap manifest. The process
// helper stages each file through PrepareArtifact before serving so admit
// publishes real vault content.
func writeFactoryDescriptorStaging(
	t *testing.T, fixture *processFixture, descriptors map[string]factoryProcessDescriptor,
) {
	t.Helper()
	stagingRoot := filepath.Join(fixture.manifestRoot, processFactoryStagingDirectory)
	if err := os.Mkdir(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range descriptors {
		path := filepath.Join(stagingRoot, descriptor.artifactID)
		if err := os.WriteFile(path, descriptor.payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// factoryDescriptorAdmitRequest builds the real authorized artifact-admit
// command that publishes one previously staged approval descriptor through
// the Stage 02 path.
func factoryDescriptorAdmitRequest(
	t *testing.T, fixture *processFixture, descriptor factoryProcessDescriptor,
) *contractsv1.ExecuteAuthorityCommandRequest {
	t.Helper()
	fixture.assertGrantHorizonLive(t)
	content := descriptor.payload
	contentDigest := sha256.Sum256(content)
	frames := uint32((len(content) + 7) / 8)
	actor := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
	}
	grantID := "grant-factory-admit-" + descriptor.artifactID
	idempotency := "factory-descriptor-admit-" + descriptor.artifactID
	request := &contractsv1.ExecuteAuthorityCommandRequest{
		Command: &contractsv1.CommandEnvelope{
			CommandId:      &contractsv1.Identifier{Namespace: "command", Value: idempotency},
			CommandType:    "artifact.admit",
			Actor:          actor,
			SubmittedAt:    timestamppb.New(time.Unix(1_000_000, 0).UTC()),
			IdempotencyKey: idempotency,
			PayloadDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)},
			Causal: &contractsv1.CausalContext{
				CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: idempotency},
				CausationId:   &contractsv1.Identifier{Namespace: "cause", Value: idempotency},
				TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: idempotency},
				Fence:         7,
			},
		},
		Grant: &contractsv1.CapabilityGrant{
			GrantId:   &contractsv1.Identifier{Namespace: "grant", Value: grantID},
			Initiator: actor,
			Actions:   []string{"artifact.admit"},
			// Grant resources match the bootstrap evidence namespace; the
			// ArtifactRef below keeps the artifact: prefix.
			Resources: []*contractsv1.Identifier{{Namespace: "evidence", Value: descriptor.artifactID}},
			Limits: []*contractsv1.ResourceLimit{
				{Name: "bytes", Maximum: 65536},
				{Name: "frames", Maximum: 8192},
			},
			Nonce:           "nonce-factory-admit-" + descriptor.artifactID,
			RevocationEpoch: 3,
			ExpiresAt:       timestamppb.New(fixture.grantExpiry),
			PolicyDigest:    &contractsv1.Digest{Algorithm: "sha256", Hex: fixture.policyDigest},
			CommandFence:    7,
		},
		ArtifactCommand: &contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit{
			ArtifactAdmit: &contractsv1.ArtifactAdmitCommand{
				Artifact: &contractsv1.ArtifactRef{
					ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: descriptor.artifactID},
					TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
					ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(contentDigest[:])},
				},
				DeclaredLength: uint64(len(content)),
				FrameCount:     frames,
			},
		},
	}
	identity := shared.MappedIdentityFact{
		Principal: shared.Identifier{Namespace: "principal", Value: "principal-a"},
		Tenant:    shared.Identifier{Namespace: "tenant", Value: "tenant-a"},
		Session:   shared.Identifier{Namespace: "session", Value: "session-a"},
	}
	fingerprint, err := OperationFingerprint(identity, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Command.PayloadDigest = &contractsv1.Digest{Algorithm: fingerprint.Algorithm, Hex: fingerprint.Hex}
	return request
}

// runFactoryTUI executes one real factory CLI invocation and returns its
// combined output and exit code. Exit codes 0 (admitted, read, cancelled) and
// 4 (the static not_found_or_denied denial) are expected outcomes.
func runFactoryTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/factory/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 30*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("factory TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}
