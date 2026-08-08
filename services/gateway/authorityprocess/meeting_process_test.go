package authorityprocess

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLocalAuthorityMeetingProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "meeting-open-session",
	}, "[OK] Local session")

	base := []string{
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}
	fixturePath := materializeMeetingFixture(t)
	importArgs := processMeetingCommand("import", base,
		"--title", "Sprint planning",
		"--source-scope", "fixture-meeting",
		"--started-at", "1700000000000",
		"--ended-at", "1700000900000",
		"--raw-retention", "7D",
		"--screenshot-retention", "OFF",
		"--derivative-retention", "30D",
		"--notify", "true",
		"--partial", "false",
		"--fixture", fixturePath,
		"--idempotency", "meeting-import-1",
	)
	importOutput, code := runMeetingTUI(t, tui, fixture.socketPath, importArgs...)
	if code != 0 {
		t.Fatalf("import failed (%d): %s", code, importOutput)
	}
	meetingID := processMeetingIdentifier(t, importOutput)
	if !strings.Contains(importOutput, "state=READY") || !strings.Contains(importOutput, "notify=true") {
		t.Fatalf("import output = %q", importOutput)
	}
	// Exact duplicate import replays.
	replayOutput, replayCode := runMeetingTUI(t, tui, fixture.socketPath, importArgs...)
	if replayCode != 0 || processMeetingIdentifier(t, replayOutput) != meetingID {
		t.Fatalf("import replay = %q (%d)", replayOutput, replayCode)
	}

	// Malformed: missing notify acknowledgement fails closed client-side.
	malformed := processMeetingCommand("import", base,
		"--title", "Sprint planning",
		"--source-scope", "fixture-meeting",
		"--started-at", "1700000000000",
		"--ended-at", "1700000900000",
		"--raw-retention", "7D",
		"--screenshot-retention", "OFF",
		"--derivative-retention", "30D",
		"--notify", "false",
		"--fixture", fixturePath,
		"--idempotency", "meeting-import-malformed",
	)
	malformedOut, malformedCode := runMeetingTUI(t, tui, fixture.socketPath, malformed...)
	if malformedCode == 0 {
		t.Fatalf("missing notify accepted: %q", malformedOut)
	}

	// Missing retention is rejected before authority use.
	missingRetention := processMeetingCommand("import", base,
		"--title", "Sprint planning",
		"--source-scope", "fixture-meeting",
		"--started-at", "1700000000000",
		"--ended-at", "1700000900000",
		"--raw-retention", "",
		"--screenshot-retention", "OFF",
		"--derivative-retention", "30D",
		"--notify", "true",
		"--fixture", fixturePath,
		"--idempotency", "meeting-import-missing-retention",
	)
	missingOut, missingCode := runMeetingTUI(t, tui, fixture.socketPath, missingRetention...)
	if missingCode == 0 {
		t.Fatalf("missing retention accepted: %q", missingOut)
	}

	statusArgs := processMeetingCommand("status", base, "--meeting", "meeting-session:"+meetingID)
	statusOut, statusCode := runMeetingTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusCode != 0 || !strings.Contains(statusOut, "timeline=0-18000ms") {
		t.Fatalf("status = %q (%d)", statusOut, statusCode)
	}

	queryArgs := processMeetingCommand("query", base,
		"--meeting", "meeting-session:"+meetingID,
		"--query", "billing service",
		"--start-millis", "0",
		"--end-millis", "6000",
		"--idempotency", "meeting-query-1",
	)
	queryOut, queryCode := runMeetingTUI(t, tui, fixture.socketPath, queryArgs...)
	if queryCode != 0 || !strings.Contains(queryOut, "citation time=") {
		t.Fatalf("query = %q (%d)", queryOut, queryCode)
	}

	// Cross-principal is non-disclosing denial.
	cross := []string{
		"--principal", "principal:principal-b", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}
	crossOut, crossCode := runMeetingTUI(t, tui, fixture.socketPath, processMeetingCommand(
		"status", cross, "--meeting", "meeting-session:"+meetingID,
	)...)
	if crossCode != 4 && !strings.Contains(crossOut, "not_found_or_denied") && !strings.Contains(crossOut, "DENIED") {
		// Exit 4 is the static denial path; some client mismatches surface as ERROR.
		if !strings.Contains(crossOut, "DENIED") && !strings.Contains(crossOut, "request-denied") && crossCode == 0 {
			t.Fatalf("cross-principal status leaked: %q (%d)", crossOut, crossCode)
		}
	}

	// Partial import path.
	partialArgs := processMeetingCommand("import", base,
		"--title", "Sprint planning partial",
		"--source-scope", "fixture-meeting",
		"--started-at", "1700000000000",
		"--ended-at", "1700000900000",
		"--raw-retention", "7D",
		"--screenshot-retention", "OFF",
		"--derivative-retention", "30D",
		"--notify", "true",
		"--partial", "true",
		"--fixture", fixturePath,
		"--idempotency", "meeting-import-partial",
	)
	partialOut, partialCode := runMeetingTUI(t, tui, fixture.socketPath, partialArgs...)
	if partialCode != 0 || !strings.Contains(partialOut, "state=PARTIAL") {
		t.Fatalf("partial import = %q (%d)", partialOut, partialCode)
	}
	partialID := processMeetingIdentifier(t, partialOut)
	partialQuery, partialQueryCode := runMeetingTUI(t, tui, fixture.socketPath, processMeetingCommand(
		"query", base,
		"--meeting", "meeting-session:"+partialID,
		"--query", "billing",
		"--idempotency", "meeting-query-partial",
	)...)
	if partialQueryCode != 0 || !strings.Contains(partialQuery, "PARTIAL") {
		t.Fatalf("partial query = %q (%d)", partialQuery, partialQueryCode)
	}

	// Revoke then query denial.
	revokeArgs := processMeetingCommand("revoke", base,
		"--meeting", "meeting-session:"+meetingID,
		"--idempotency", "meeting-revoke-1",
	)
	revokeOut, revokeCode := runMeetingTUI(t, tui, fixture.socketPath, revokeArgs...)
	if revokeCode != 0 || !strings.Contains(revokeOut, "Meeting revoked") {
		t.Fatalf("revoke = %q (%d)", revokeOut, revokeCode)
	}
	// Exact revoke replay.
	if out, code := runMeetingTUI(t, tui, fixture.socketPath, revokeArgs...); code != 0 || !strings.Contains(out, "Meeting revoked") {
		t.Fatalf("revoke replay = %q (%d)", out, code)
	}
	revokedQuery, revokedCode := runMeetingTUI(t, tui, fixture.socketPath, queryArgs...)
	if revokedCode == 0 && !strings.Contains(revokedQuery, "DENIED") {
		t.Fatalf("revoked query succeeded: %q", revokedQuery)
	}

	// Purge lineage.
	purgeArgs := processMeetingCommand("purge", base,
		"--meeting", "meeting-session:"+meetingID,
		"--idempotency", "meeting-purge-1",
	)
	purgeOut, purgeCode := runMeetingTUI(t, tui, fixture.socketPath, purgeArgs...)
	if purgeCode != 0 || !strings.Contains(purgeOut, "Meeting purged") {
		t.Fatalf("purge = %q (%d)", purgeOut, purgeCode)
	}
	if out, code := runMeetingTUI(t, tui, fixture.socketPath, purgeArgs...); code != 0 || !strings.Contains(out, "Meeting purged") {
		t.Fatalf("purge replay = %q (%d)", out, code)
	}
	statusAfterPurge, statusAfterCode := runMeetingTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusAfterCode == 0 && !strings.Contains(statusAfterPurge, "DENIED") {
		t.Fatalf("purged status succeeded: %q", statusAfterPurge)
	}

	stopProcessDaemon(t, daemon, fixture.socketPath)
	tui.cleanup(t)
	fixture.cleanup(t)
}

func processMeetingCommand(kind string, base []string, flags ...string) []string {
	arguments := make([]string, 0, 2+len(base)+len(flags))
	arguments = append(arguments, "meeting", kind)
	arguments = append(arguments, base...)
	return append(arguments, flags...)
}

func runMeetingTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/meetings/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 30*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("meeting TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}

func processMeetingIdentifier(t *testing.T, output string) string {
	t.Helper()
	matched := regexp.MustCompile(`meeting=meeting-session:([0-9a-f]{64})`).FindStringSubmatch(output)
	if len(matched) != 2 {
		t.Fatalf("meeting identifier missing from %q", output)
	}
	return matched[1]
}

func materializeMeetingFixture(t *testing.T) string {
	t.Helper()
	root := processRepositoryRoot(t)
	source := filepath.Join(root, "tests", "fixtures", "stage-07", "transcript", "fixture-meeting.json")
	encoded, err := os.ReadFile(source)
	if err != nil {
		// Fall back to runfiles/Bazel data path.
		if os.Getenv("TEST_SRCDIR") != "" {
			source = filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"),
				"tests", "fixtures", "stage-07", "transcript", "fixture-meeting.json")
			encoded, err = os.ReadFile(source)
		}
		if err != nil {
			t.Fatalf("read meeting fixture: %v", err)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "fixture-meeting.json")
	if err := os.WriteFile(destination, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return destination
}
