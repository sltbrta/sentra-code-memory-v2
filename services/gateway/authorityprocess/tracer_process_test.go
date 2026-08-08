// Process tests prove the Stage 06 Tracer 001 path through the production
// command, the owner-only socket, and the real checked-in Bun tracer TUI over
// the L1 synthetic fixture digests. Default is deterministic FakeAPI draft-PR
// (no live GitHub). Live dogfood is optional via OUROBOROS_TRACER_LIVE_GITHUB=1
// plus GITHUB_TOKEN; otherwise see docs/stages/stage-06/evidence/live-github-waiver.json.
package authorityprocess

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLocalAuthorityTracerProcessTracer drives the synthetic fixture through
// the composed production path: session → ingest → ask (authorized +
// negatives) → intent → plan → review → draft-pr (idempotent) → outcome, with
// principal B non-disclosing denial and the real Bun tracer TUI.
func TestLocalAuthorityTracerProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	daemon := startProcessDaemon(t, fixture)

	base := []string{
		"--principal", "principal:principal-a",
		"--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
		"--timeout-ms", "10000",
	}

	// Open the product local session so peer mapping is warm (same bootstrap).
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "tracer-open-session",
	}, "[OK] Local session")

	// Session: pin a synthetic tracer run.
	sessionOut, sessionCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "session", "--idempotency", "tracer-session-1",
	}, base...)...)
	if sessionCode != 0 {
		t.Fatalf("tracer session = %d: %s", sessionCode, sessionOut)
	}
	assertTracerFragments(t, sessionOut, "[OK]", "Session opened", "run=run:", "state=MANIFEST_PINNED")
	runID := extractTracerRunID(t, sessionOut)

	// Ingest: admit the pinned L1 synthetic manifest digest.
	ingestOut, ingestCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "ingest",
		"--run", "run:" + runID,
		"--manifest-digest", tracerPinnedManifestDigest,
		"--idempotency", "tracer-ingest-1",
	}, base...)...)
	if ingestCode != 0 {
		t.Fatalf("tracer ingest = %d: %s", ingestCode, ingestOut)
	}
	assertTracerFragments(t, ingestOut, "[OK]", "Fixture ingested", "state=READY")

	// Ask (authorized arm): cited-answer disposition.
	askOut, askCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "ask",
		"--run", "run:" + runID,
		"--query", "What does the supporting span return?",
		"--source", "source:synthetic",
		"--generation", "generation:1",
		"--variant", "authorized",
		"--idempotency", "tracer-ask-authorized",
	}, base...)...)
	if askCode != 0 {
		t.Fatalf("tracer ask authorized = %d: %s", askCode, askOut)
	}
	assertTracerFragments(t, askOut, "[OK]", "Ask complete", "state=ANSWERED")

	// Negative matrix: absent / stale / revoked abstain with stable reason codes.
	for _, variant := range []struct {
		name   string
		reason string
	}{
		{"absent", "SPAN_ABSENT"},
		{"stale", "SPAN_STALE"},
		{"revoked", "SPAN_REVOKED"},
	} {
		out, code := runTracerTUI(t, tui, fixture.socketPath, append([]string{
			"tracer", "ask",
			"--run", "run:" + runID,
			"--query", "What does the supporting span return?",
			"--source", "source:synthetic",
			"--generation", "generation:1",
			"--variant", variant.name,
			"--idempotency", "tracer-ask-" + variant.name,
		}, base...)...)
		if code != 0 {
			t.Fatalf("tracer ask %s = %d: %s", variant.name, code, out)
		}
		assertTracerFragments(t, out, "[OK]", "state=ABSTAINED", "reason="+variant.reason)
	}

	// Unauthorized variant abstains without disclosing span bytes.
	unauthOut, unauthCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "ask",
		"--run", "run:" + runID,
		"--query", "What does the supporting span return?",
		"--source", "source:synthetic",
		"--generation", "generation:1",
		"--variant", "unauthorized",
		"--idempotency", "tracer-ask-unauthorized",
	}, base...)...)
	if unauthCode != 0 {
		t.Fatalf("tracer ask unauthorized = %d: %s", unauthCode, unauthOut)
	}
	assertTracerFragments(t, unauthOut, "state=ABSTAINED", "reason=SPAN_UNAUTHORIZED")

	// Principal B body mismatch: peer is principal-a; body principal-b is
	// request-denied without existence detail (inaccessible ≡ absent envelope).
	denyOut, denyCode := runTracerTUI(t, tui, fixture.socketPath,
		"tracer", "ask",
		"--principal", "principal:principal-b",
		"--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
		"--timeout-ms", "10000",
		"--run", "run:"+runID,
		"--query", "What does the supporting span return?",
		"--source", "source:synthetic",
		"--generation", "generation:1",
		"--idempotency", "tracer-ask-principal-b",
	)
	if denyCode != 1 && denyCode != 4 {
		t.Fatalf("principal-b ask exit = %d: %s", denyCode, denyOut)
	}
	if strings.Contains(denyOut, "ANSWERED") || strings.Contains(denyOut, "supporting span") ||
		strings.Contains(denyOut, runID) {
		t.Fatalf("principal-b denial leaked existence: %q", denyOut)
	}

	// Restore authorized answer so the run is ANSWERED for the intent handoff.
	reaskOut, reaskCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "ask",
		"--run", "run:" + runID,
		"--query", "What does the supporting span return?",
		"--source", "source:synthetic",
		"--generation", "generation:1",
		"--variant", "authorized",
		"--idempotency", "tracer-ask-authorized-restore",
	}, base...)...)
	if reaskCode != 0 {
		t.Fatalf("tracer re-ask = %d: %s", reaskCode, reaskOut)
	}
	assertTracerFragments(t, reaskOut, "state=ANSWERED")

	// Intent → plan → review via L2 compiler.
	scopeDigest := "63746365221513e43aae8513fa289a7808baad21a86c06c4353f155de7dd04c6"
	intentOut, intentCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "intent",
		"--run", "run:" + runID,
		"--base", tracerPinnedBaseGitOID,
		"--scope-digest", scopeDigest,
		"--idempotency", "tracer-intent-1",
	}, base...)...)
	if intentCode != 0 {
		t.Fatalf("tracer intent = %d: %s", intentCode, intentOut)
	}
	assertTracerFragments(t, intentOut, "[OK]", "Intent admitted", "state=INTENT_APPROVED")

	planOut, planCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "plan", "--run", "run:" + runID,
	}, base...)...)
	if planCode != 0 {
		t.Fatalf("tracer plan = %d: %s", planCode, planOut)
	}
	assertTracerFragments(t, planOut, "[OK]", "Plan ready", "state=PLANNED")

	reviewOut, reviewCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "review", "--run", "run:" + runID,
	}, base...)...)
	if reviewCode != 0 {
		t.Fatalf("tracer review = %d: %s", reviewCode, reviewOut)
	}
	assertTracerFragments(t, reviewOut, "[OK]", "Review complete")

	// Draft-PR via FakeAPI (deterministic default).
	effectApproval := strings.Repeat("d", 64)
	changeSet := strings.Repeat("e", 64)
	draftOut, draftCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "draft-pr",
		"--run", "run:" + runID,
		"--effect-approval-digest", effectApproval,
		"--change-set-digest", changeSet,
		"--idempotency", "tracer-draft-1",
	}, base...)...)
	if draftCode != 0 {
		t.Fatalf("tracer draft-pr = %d: %s", draftCode, draftOut)
	}
	assertTracerFragments(t, draftOut, "[OK]", "Draft PR authorized", "draft=true", "head=refs/heads/ouroboros/tracer-001/")
	// Exact idempotent replay converges to the same draft PR.
	replayOut, replayCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "draft-pr",
		"--run", "run:" + runID,
		"--effect-approval-digest", effectApproval,
		"--change-set-digest", changeSet,
		"--idempotency", "tracer-draft-1",
	}, base...)...)
	if replayCode != 0 {
		t.Fatalf("tracer draft-pr replay = %d: %s", replayCode, replayOut)
	}
	assertTracerFragments(t, replayOut, "Draft PR authorized", "draft=true")

	// Outcome reingest with raw-trace separation.
	outcomeOut, outcomeCode := runTracerTUI(t, tui, fixture.socketPath, append([]string{
		"tracer", "outcome",
		"--run", "run:" + runID,
		"--query", "What was the draft PR outcome?",
		"--idempotency", "tracer-outcome-1",
	}, base...)...)
	if outcomeCode != 0 {
		t.Fatalf("tracer outcome = %d: %s", outcomeCode, outcomeOut)
	}
	assertTracerFragments(t, outcomeOut, "[OK]", "Outcome reingested", "raw_trace_separated=true", "state=COMPLETE")

	// Live dogfood is optional; document waiver when token/env absent.
	if os.Getenv("OUROBOROS_TRACER_LIVE_GITHUB") == "1" {
		t.Log("live GitHub dogfood env set; synthetic FakeAPI path already proven — live residual is operator-run")
	}

	_ = daemon
}

func runTracerTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/tracer-001/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 30*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("tracer TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}

func assertTracerFragments(t *testing.T, output string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(output, fragment) {
			t.Fatalf("output %q omitted %q", output, fragment)
		}
	}
}

func extractTracerRunID(t *testing.T, output string) string {
	t.Helper()
	// Lines look like: [OK] Session opened — run=run:<64hex> state=MANIFEST_PINNED
	const marker = "run=run:"
	index := strings.Index(output, marker)
	if index < 0 {
		t.Fatalf("no run id in %q", output)
	}
	rest := output[index+len(marker):]
	end := strings.IndexAny(rest, " \n\t")
	if end < 0 {
		end = len(rest)
	}
	value := strings.TrimSpace(rest[:end])
	if len(value) < 16 {
		t.Fatalf("run id too short in %q", output)
	}
	return value
}
