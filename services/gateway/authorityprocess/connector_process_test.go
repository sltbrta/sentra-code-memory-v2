package authorityprocess

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLocalAuthorityConnectorProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "connector-open-session",
	}, "[OK] Local session")

	base := []string{
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}

	connectArgs := processConnectorCommand("connect", base,
		"--owner", "ouroboros-dogfood",
		"--repo", "sample-repo",
		"--source-scope", "github.com/ouroboros-dogfood/sample-repo",
		"--idempotency", "connector-connect-1",
	)
	connectOutput, code := runConnectionTUI(t, tui, fixture.socketPath, connectArgs...)
	if code != 0 {
		t.Fatalf("connect failed (%d): %s", code, connectOutput)
	}
	connectionID := processConnectionIdentifier(t, connectOutput)
	if !strings.Contains(connectOutput, "state=READY") || !strings.Contains(connectOutput, "provider=github") {
		t.Fatalf("connect output = %q", connectOutput)
	}
	// Exact duplicate connect replays.
	replayOutput, replayCode := runConnectionTUI(t, tui, fixture.socketPath, connectArgs...)
	if replayCode != 0 || processConnectionIdentifier(t, replayOutput) != connectionID {
		t.Fatalf("connect replay = %q (%d)", replayOutput, replayCode)
	}

	// Malformed: owner/repo scope mismatch fails closed client-side or as denial.
	malformed := processConnectorCommand("connect", base,
		"--owner", "ouroboros-dogfood",
		"--repo", "sample-repo",
		"--source-scope", "github.com/wrong/scope",
		"--idempotency", "connector-connect-malformed",
	)
	malformedOut, malformedCode := runConnectionTUI(t, tui, fixture.socketPath, malformed...)
	if malformedCode == 0 {
		t.Fatalf("scope mismatch accepted: %q", malformedOut)
	}

	statusArgs := processConnectorCommand("status", base, "--connection", "connection:"+connectionID)
	statusOut, statusCode := runConnectionTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusCode != 0 || !strings.Contains(statusOut, "cursor=cursor-v1") {
		t.Fatalf("status = %q (%d)", statusOut, statusCode)
	}

	queryArgs := processConnectorCommand("query", base,
		"--connection", "connection:"+connectionID,
		"--query", "billing",
		"--idempotency", "connector-query-1",
	)
	queryOut, queryCode := runConnectionTUI(t, tui, fixture.socketPath, queryArgs...)
	if queryCode != 0 || !strings.Contains(queryOut, "citation kind=") {
		t.Fatalf("query = %q (%d)", queryOut, queryCode)
	}

	// Incremental reconcile admits delta.
	reconcileArgs := processConnectorCommand("reconcile", base,
		"--connection", "connection:"+connectionID,
		"--cursor", "cursor-v1",
		"--reason", "manual",
		"--idempotency", "connector-reconcile-1",
	)
	reconcileOut, reconcileCode := runConnectionTUI(t, tui, fixture.socketPath, reconcileArgs...)
	if reconcileCode != 0 || !strings.Contains(reconcileOut, "cursor-v2") {
		t.Fatalf("reconcile = %q (%d)", reconcileOut, reconcileCode)
	}

	// Cross-principal is non-disclosing denial.
	cross := []string{
		"--principal", "principal:principal-b", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}
	crossOut, crossCode := runConnectionTUI(t, tui, fixture.socketPath, processConnectorCommand(
		"status", cross, "--connection", "connection:"+connectionID,
	)...)
	if crossCode == 0 && !strings.Contains(crossOut, "DENIED") && !strings.Contains(crossOut, "not_found_or_denied") {
		t.Fatalf("cross-principal status leaked: %q (%d)", crossOut, crossCode)
	}

	// Revoke then query denial.
	revokeArgs := processConnectorCommand("revoke", base,
		"--connection", "connection:"+connectionID,
		"--idempotency", "connector-revoke-1",
	)
	revokeOut, revokeCode := runConnectionTUI(t, tui, fixture.socketPath, revokeArgs...)
	if revokeCode != 0 || !strings.Contains(revokeOut, "Connection revoked") {
		t.Fatalf("revoke = %q (%d)", revokeOut, revokeCode)
	}
	if out, code := runConnectionTUI(t, tui, fixture.socketPath, revokeArgs...); code != 0 || !strings.Contains(out, "Connection revoked") {
		t.Fatalf("revoke replay = %q (%d)", out, code)
	}
	revokedQuery, revokedCode := runConnectionTUI(t, tui, fixture.socketPath, queryArgs...)
	if revokedCode == 0 && !strings.Contains(revokedQuery, "DENIED") {
		t.Fatalf("revoked query succeeded: %q", revokedQuery)
	}

	// Purge lineage.
	purgeArgs := processConnectorCommand("purge", base,
		"--connection", "connection:"+connectionID,
		"--idempotency", "connector-purge-1",
	)
	purgeOut, purgeCode := runConnectionTUI(t, tui, fixture.socketPath, purgeArgs...)
	if purgeCode != 0 || !strings.Contains(purgeOut, "Connection purged") {
		t.Fatalf("purge = %q (%d)", purgeOut, purgeCode)
	}
	statusAfterPurge, statusAfterCode := runConnectionTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusAfterCode == 0 && !strings.Contains(statusAfterPurge, "DENIED") {
		t.Fatalf("purged status succeeded: %q", statusAfterPurge)
	}

	stopProcessDaemon(t, daemon, fixture.socketPath)
	tui.cleanup(t)
	fixture.cleanup(t)
}

func processConnectorCommand(kind string, base []string, flags ...string) []string {
	arguments := make([]string, 0, 2+len(base)+len(flags))
	arguments = append(arguments, "connection", kind)
	arguments = append(arguments, base...)
	return append(arguments, flags...)
}

func runConnectionTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/connections/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 30*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("connection TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}

func processConnectionIdentifier(t *testing.T, output string) string {
	t.Helper()
	matched := regexp.MustCompile(`connection=connection:([0-9a-f]{64})`).FindStringSubmatch(output)
	if len(matched) != 2 {
		t.Fatalf("connection identifier missing from %q", output)
	}
	return matched[1]
}
