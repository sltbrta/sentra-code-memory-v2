package authorityprocess

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLocalAuthorityMultimodalProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "multimodal-open-session",
	}, "[OK] Local session")

	base := []string{
		"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}
	textFixture := materializeMultimodalFixture(t, "sample.md")

	admitArgs := processMultimodalCommand("admit", base,
		"--kind", "text",
		"--media-type", "text/markdown",
		"--fixture", textFixture,
		"--brain", "brain:brain-a",
		"--partial", "false",
		"--idempotency", "multimodal-admit-1",
	)
	admitOutput, code := runMultimodalTUI(t, tui, fixture.socketPath, admitArgs...)
	if code != 0 {
		t.Fatalf("admit failed (%d): %s", code, admitOutput)
	}
	sourceID := processMultimodalIdentifier(t, admitOutput)
	if !strings.Contains(admitOutput, "state=ADMITTED") {
		t.Fatalf("admit output = %q", admitOutput)
	}
	// Exact duplicate admit replays.
	replayOutput, replayCode := runMultimodalTUI(t, tui, fixture.socketPath, admitArgs...)
	if replayCode != 0 || processMultimodalIdentifier(t, replayOutput) != sourceID {
		t.Fatalf("admit replay = %q (%d)", replayOutput, replayCode)
	}

	// Media-type mismatch fails closed (JPEG declared as PNG kind).
	pngFixture := materializeMultimodalFixture(t, "sample.png")
	mismatch := processMultimodalCommand("admit", base,
		"--kind", "png",
		"--media-type", "image/jpeg",
		"--fixture", pngFixture,
		"--brain", "brain:brain-a",
		"--idempotency", "multimodal-admit-mismatch",
	)
	mismatchOut, mismatchCode := runMultimodalTUI(t, tui, fixture.socketPath, mismatch...)
	if mismatchCode == 0 {
		t.Fatalf("media-type mismatch accepted: %q", mismatchOut)
	}

	statusArgs := processMultimodalCommand("status", base, "--source", "multimodal-source:"+sourceID)
	statusOut, statusCode := runMultimodalTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusCode != 0 || !strings.Contains(statusOut, "state=READY") {
		t.Fatalf("status = %q (%d)", statusOut, statusCode)
	}

	evidenceArgs := processMultimodalCommand("evidence", base,
		"--source", "multimodal-source:"+sourceID,
		"--page-size", "10",
	)
	evidenceOut, evidenceCode := runMultimodalTUI(t, tui, fixture.socketPath, evidenceArgs...)
	if evidenceCode != 0 || !strings.Contains(evidenceOut, "anchor=") {
		t.Fatalf("evidence = %q (%d)", evidenceOut, evidenceCode)
	}

	// Cross-principal is non-disclosing denial.
	cross := []string{
		"--principal", "principal:principal-b", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a",
	}
	crossOut, crossCode := runMultimodalTUI(t, tui, fixture.socketPath, processMultimodalCommand(
		"status", cross, "--source", "multimodal-source:"+sourceID,
	)...)
	if crossCode == 0 && !strings.Contains(crossOut, "DENIED") {
		t.Fatalf("cross-principal status leaked: %q (%d)", crossOut, crossCode)
	}

	// Partial admit path.
	partialArgs := processMultimodalCommand("admit", base,
		"--kind", "text",
		"--media-type", "text/markdown",
		"--fixture", textFixture,
		"--brain", "brain:brain-a",
		"--partial", "true",
		"--idempotency", "multimodal-admit-partial",
	)
	partialOut, partialCode := runMultimodalTUI(t, tui, fixture.socketPath, partialArgs...)
	if partialCode != 0 {
		t.Fatalf("partial admit = %q (%d)", partialOut, partialCode)
	}
	partialID := processMultimodalIdentifier(t, partialOut)
	partialStatus, partialStatusCode := runMultimodalTUI(t, tui, fixture.socketPath, processMultimodalCommand(
		"status", base, "--source", "multimodal-source:"+partialID,
	)...)
	if partialStatusCode != 0 || !strings.Contains(partialStatus, "PARTIAL_READY") {
		t.Fatalf("partial status = %q (%d)", partialStatus, partialStatusCode)
	}

	// PNG/PDF/WAV happy paths.
	for _, caseSpec := range []struct {
		kind, media, file string
		key               string
	}{
		{"png", "image/png", "sample.png", "multimodal-admit-png"},
		{"pdf", "application/pdf", "sample.pdf", "multimodal-admit-pdf"},
		{"wav", "audio/wav", "sample.wav", "multimodal-admit-wav"},
	} {
		path := materializeMultimodalFixture(t, caseSpec.file)
		var out string
		var code int
		for attempt := 0; attempt < 4; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*250) * time.Millisecond)
			}
			out, code = runMultimodalTUI(t, tui, fixture.socketPath, processMultimodalCommand(
				"admit", base,
				"--kind", caseSpec.kind,
				"--media-type", caseSpec.media,
				"--fixture", path,
				"--brain", "brain:brain-a",
				"--idempotency", caseSpec.key,
			)...)
			if code == 0 || !strings.Contains(out, "request-timeout") {
				break
			}
		}
		if code != 0 {
			t.Fatalf("%s admit failed (%d): %s", caseSpec.kind, code, out)
		}
	}

	// Revoke then status denial.
	revokeArgs := processMultimodalCommand("revoke", base,
		"--source", "multimodal-source:"+sourceID,
		"--idempotency", "multimodal-revoke-1",
	)
	revokeOut, revokeCode := runMultimodalTUI(t, tui, fixture.socketPath, revokeArgs...)
	if revokeCode != 0 || !strings.Contains(revokeOut, "Multimodal revoked") {
		t.Fatalf("revoke = %q (%d)", revokeOut, revokeCode)
	}
	if out, code := runMultimodalTUI(t, tui, fixture.socketPath, revokeArgs...); code != 0 || !strings.Contains(out, "Multimodal revoked") {
		t.Fatalf("revoke replay = %q (%d)", out, code)
	}
	revokedStatus, revokedCode := runMultimodalTUI(t, tui, fixture.socketPath, statusArgs...)
	if revokedCode == 0 && !strings.Contains(revokedStatus, "DENIED") {
		t.Fatalf("revoked status succeeded: %q", revokedStatus)
	}

	// Purge lineage.
	purgeArgs := processMultimodalCommand("purge", base,
		"--source", "multimodal-source:"+sourceID,
		"--idempotency", "multimodal-purge-1",
	)
	purgeOut, purgeCode := runMultimodalTUI(t, tui, fixture.socketPath, purgeArgs...)
	if purgeCode != 0 || !strings.Contains(purgeOut, "Multimodal purged") {
		t.Fatalf("purge = %q (%d)", purgeOut, purgeCode)
	}
	if out, code := runMultimodalTUI(t, tui, fixture.socketPath, purgeArgs...); code != 0 || !strings.Contains(out, "Multimodal purged") {
		t.Fatalf("purge replay = %q (%d)", out, code)
	}
	statusAfterPurge, statusAfterCode := runMultimodalTUI(t, tui, fixture.socketPath, statusArgs...)
	if statusAfterCode == 0 && !strings.Contains(statusAfterPurge, "DENIED") {
		t.Fatalf("purged status succeeded: %q", statusAfterPurge)
	}

	stopProcessDaemon(t, daemon, fixture.socketPath)
	tui.cleanup(t)
	fixture.cleanup(t)
}

func processMultimodalCommand(kind string, base []string, flags ...string) []string {
	arguments := make([]string, 0, 2+len(base)+len(flags))
	arguments = append(arguments, "multimodal", kind)
	arguments = append(arguments, base...)
	return append(arguments, flags...)
}

func runMultimodalTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/multimodal/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 30*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("multimodal TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}

func processMultimodalIdentifier(t *testing.T, output string) string {
	t.Helper()
	matched := regexp.MustCompile(`source=multimodal-source:([0-9a-f]{64})`).FindStringSubmatch(output)
	if len(matched) != 2 {
		t.Fatalf("multimodal identifier missing from %q", output)
	}
	return matched[1]
}

func materializeMultimodalFixture(t *testing.T, name string) string {
	t.Helper()
	root := processRepositoryRoot(t)
	source := filepath.Join(root, "tests", "fixtures", "stage-11", "multimodal", name)
	encoded, err := os.ReadFile(source)
	if err != nil {
		if os.Getenv("TEST_SRCDIR") != "" {
			source = filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"),
				"tests", "fixtures", "stage-11", "multimodal", name)
			encoded, err = os.ReadFile(source)
		}
		if err != nil {
			t.Fatalf("read multimodal fixture %s: %v", name, err)
		}
	}
	// Short absolute path keeps Identifier value under the 512-byte contract bound.
	destination := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(destination, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return destination
}
