// Process tests prove the bounded Stage 04 grounded-query user path through
// the production command, the owner-only socket, the durable authority, and
// the real checked-in Bun query TUI. The acceptance matrix mirrors the frozen
// grounding fixture: cited answers, absent support, stale support, denied and
// revoked support collapsing to absent_support, provider failure, restart
// rebuild, and idempotent replay of the original disposition.
package authorityprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLocalAuthorityQueryProcessTracer drives the Stage 04 v1 acceptance
// matrix end to end: the real TUI talks to the real production command over
// the owner-only socket, with deterministic synthesis as the default model
// adapter and a hermetic fake provider for the failure and mid-flight-revoke
// cases. No live provider egress ever participates.
func TestLocalAuthorityQueryProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	firstCommit := fixture.prepareStage3Source(t)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "query-open-session",
	}, "[OK] Local session")
	base := []string{"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	add := processIngestionCommand("add", base,
		"--commit", firstCommit, "--configuration-digest", fixture.configDigest,
		"--idempotency", "source-add", "--use-gitignore", "true", "--use-ouroborosignore", "true")
	addOutput := runIngestionTUI(t, tui, fixture.socketPath, add...)
	source, generationOne := processIngestionIdentifiers(t, addOutput)
	secondCommit := fixture.reconcileStage3Source(t, firstCommit)
	reconcile := processIngestionCommand("reconcile", base,
		"--source", source, "--expected-generation", generationOne, "--expected-commit", firstCommit,
		"--target-commit", secondCommit, "--idempotency", "source-reconcile")
	_, generationTwo := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, reconcile...))

	// The query status and sources views answer from the reconciled generation.
	statusOutput, statusCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"status", "--source", source}, base...)...)
	if statusCode != 0 {
		t.Fatalf("query status = %d: %s", statusCode, statusOutput)
	}
	for _, fragment := range []string{
		"[OK]", "projection=ready", generationTwo, "freshness=degraded", "commit=" + secondCommit,
		"readiness=GO:SYNTAX_AWARE,TYPESCRIPT:LEXICAL_DEGRADED,PYTHON:SYNTAX_AWARE,RUST:SYNTAX_AWARE,JAVA:SYNTAX_AWARE",
	} {
		if !strings.Contains(statusOutput, fragment) {
			t.Fatalf("query status output %q omitted %q", statusOutput, fragment)
		}
	}
	sourcesOutput, sourcesCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"sources", "--page-size", "100"}, base...)...)
	if sourcesCode != 0 {
		t.Fatalf("query sources = %d: %s", sourcesCode, sourcesOutput)
	}
	for _, fragment := range []string{"Sources — results=1", source, "state=READY", generationTwo} {
		if !strings.Contains(sourcesOutput, fragment) {
			t.Fatalf("query sources output %q omitted %q", sourcesOutput, fragment)
		}
	}

	// The v1 acceptance ask matrix derives from the frozen grounding fixture:
	// query text, pinned generation, freshness mode, expected status, exact
	// citation ranges, and degraded reasons all come from the checked-in
	// descriptor, and the real engine through the real TUI must reproduce them.
	ask := func(generation, text, freshness, key string) (string, int) {
		// Retry transient connection races under CI load (socket not ready).
		var output string
		var code int
		for attempt := 0; attempt < 4; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*200) * time.Millisecond)
			}
			output, code = runQueryTUI(t, tui, fixture.socketPath, append([]string{
				"ask", "--source", source, "--generation", generation,
				"--query", text, "--freshness", freshness, "--idempotency", key,
				// Hosted runners under load exceed the 3s client default; the
				// tracer proves behavior, not timing, so use the contract maximum.
				"--timeout-ms", "10000",
			}, base...)...)
			// Retry only pure transport races; never mask request-invalid.
			if code == 0 || !strings.Contains(output, "connection-failed") {
				return output, code
			}
		}
		return output, code
	}
	manifest := loadProcessGroundingManifest(t)
	answeredOutput := ""
	for _, queryCase := range manifest.Cases {
		if queryCase.Interference != "none" {
			continue
		}
		generation := generationTwo
		if queryCase.PinnedGeneration == "stale" {
			generation = generationOne
		}
		freshness := mapProcessFreshness(t, queryCase.Freshness)
		output, code := ask(generation, queryCase.Query, freshness, "ask-"+queryCase.CaseID)
		want := groundingExpectation(queryCase)
		if code != want.exitCode {
			t.Fatalf("%s exit = %d, want %d: %s", queryCase.CaseID, code, want.exitCode, output)
		}
		if !strings.Contains(output, want.tag) {
			t.Fatalf("%s output %q omitted %q", queryCase.CaseID, output, want.tag)
		}
		for _, fragment := range want.fragments {
			if !strings.Contains(output, fragment) {
				t.Fatalf("%s output %q omitted %q", queryCase.CaseID, output, fragment)
			}
		}
		if queryCase.CaseID == "answered-go-anchor" {
			answeredOutput = output
			if strings.Contains(output, "degraded=") {
				t.Fatalf("answered output disclosed degraded reasons: %q", output)
			}
		}
	}

	// An exact idempotent replay returns the original disposition byte-for-byte;
	// a conflicting key reuse denies without mutation.
	replayOutput, replayCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-answered-go-anchor")
	if replayCode != 0 || replayOutput != answeredOutput {
		t.Fatalf("idempotent replay = %d:\n%s\nwant byte-identical to:\n%s", replayCode, replayOutput, answeredOutput)
	}
	conflictOutput, conflictCode := ask(generationTwo,
		"Which function lives at src/go/rename-00.go?", "best-effort", "ask-answered-go-anchor")
	if conflictCode != 4 || !strings.Contains(conflictOutput, "[DENIED]") ||
		!strings.Contains(conflictOutput, "not_found_or_denied") {
		t.Fatalf("conflicting idempotency = %d: %s", conflictCode, conflictOutput)
	}

	// History is private, ordered, and shows one user and one active assistant
	// turn per admitted ask; the replay and conflict committed nothing.
	historyOutput, historyCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"history", "--page-size", "100"}, base...)...)
	if historyCode != 0 || !strings.Contains(historyOutput, "History — turns=18") {
		t.Fatalf("history = %d: %s", historyCode, historyOutput)
	}
	for _, fragment := range []string{"role=user status=active", "role=assistant status=active"} {
		if !strings.Contains(historyOutput, fragment) {
			t.Fatalf("history output %q omitted %q", historyOutput, fragment)
		}
	}

	// A mismatched body principal denies at the outer boundary.
	wrongPrincipal := []string{"--principal", "principal:principal-b", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	mismatchOutput, mismatchCode := runQueryTUI(t, tui, fixture.socketPath, append([]string{
		"ask", "--source", source, "--generation", generationTwo,
		"--query", "Which Go function in src/go/modify-00.go returns the stage marker?",
		"--freshness", "best-effort", "--idempotency", "ask-wrong-principal",
	}, wrongPrincipal...)...)
	if mismatchCode != 1 || mismatchOutput != "ERROR request-denied\n" {
		t.Fatalf("identity mismatch = %d: %q", mismatchCode, mismatchOutput)
	}

	// Restart: the projection rebuilds from exact commits, replay returns the
	// original disposition, and history neither duplicates nor loses a turn.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemon(t, fixture)
	restartedReplay, restartedReplayCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-answered-go-anchor")
	if restartedReplayCode != 0 || restartedReplay != answeredOutput {
		t.Fatalf("restart replay = %d:\n%s\nwant byte-identical to:\n%s", restartedReplayCode, restartedReplay, answeredOutput)
	}
	restartedHistory, restartedHistoryCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"history", "--page-size", "100"}, base...)...)
	if restartedHistoryCode != 0 || !strings.Contains(restartedHistory, "History — turns=18") {
		t.Fatalf("restart history = %d: %s", restartedHistoryCode, restartedHistory)
	}
	restartedStatus, restartedStatusCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"status", "--source", source}, base...)...)
	if restartedStatusCode != 0 || !strings.Contains(restartedStatus, "projection=ready") ||
		!strings.Contains(restartedStatus, generationTwo) || !strings.Contains(restartedStatus, "freshness=degraded") {
		t.Fatalf("restart status = %d: %s", restartedStatusCode, restartedStatus)
	}
	freshOutput, freshCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-after-restart")
	if freshCode != 0 || !strings.Contains(freshOutput, "src/go/modify-00.go:3:1-3:53") {
		t.Fatalf("post-restart ask = %d: %s", freshCode, freshOutput)
	}

	// Provider failure: an explicitly configured provider whose endpoint
	// refuses fails closed to synthesis_unavailable, never a silent fallback.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemonWithEnv(t, fixture, []string{
		"OUROBOROS_QUERY_PROVIDER=openai",
		"OPENAI_API_KEY=process-test-static-key",
		"OUROBOROS_OPENAI_BASE_URL=" + refusedProviderAddress(t),
		"OUROBOROS_OPENAI_TIMEOUT_MS=2000",
	})
	failureOutput, failureCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-provider-failure")
	if failureCode != 3 || !strings.Contains(failureOutput, "[ABSTAINED]") ||
		!strings.Contains(failureOutput, "degraded=synthesis_unavailable") {
		t.Fatalf("provider failure = %d: %s", failureCode, failureOutput)
	}
	if strings.Contains(failureOutput, "citation git=") {
		t.Fatalf("provider failure leaked evidence: %q", failureOutput)
	}

	// Crash mid-query: the daemon dies between admission and completion during
	// a black-holed provider call. Restart recovery marks the interrupted
	// assistant turn visibly failed; its replay stays terminal, and the original
	// answered ask still replays its original disposition.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	parked := startBlackHoleProvider(t)
	daemon = startProcessDaemonWithEnv(t, fixture, []string{
		"OUROBOROS_QUERY_PROVIDER=openai",
		"OPENAI_API_KEY=process-test-static-key",
		"OUROBOROS_OPENAI_BASE_URL=http://" + parked.address,
		"OUROBOROS_OPENAI_TIMEOUT_MS=4000",
	})
	crashAsk := startQueryAskProcess(t, tui, fixture.socketPath, append([]string{
		"ask", "--source", source, "--generation", generationTwo,
		"--query", "Which Go function in src/go/modify-00.go returns the stage marker?",
		"--freshness", "best-effort", "--idempotency", "ask-crash-mid-query",
	}, base...))
	time.Sleep(1 * time.Second)
	if err := daemon.command.Process.Kill(); err != nil {
		t.Fatalf("kill daemon mid-query: %v", err)
	}
	<-daemon.done
	crashAskOutput, _ := crashAsk.await(t)
	if strings.Contains(crashAskOutput, "[ANSWERED]") {
		t.Fatalf("crashed ask rendered an answer: %q", crashAskOutput)
	}
	parked.close(t)
	if err := os.Remove(fixture.socketPath); err != nil {
		t.Fatal(err)
	}
	daemon = startProcessDaemon(t, fixture)
	recoveredHistory, recoveredHistoryCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"history", "--page-size", "100"}, base...)...)
	if recoveredHistoryCode != 0 || !strings.Contains(recoveredHistory, "History — turns=24") {
		t.Fatalf("recovered history = %d: %s", recoveredHistoryCode, recoveredHistory)
	}
	for _, fragment := range []string{"role=assistant status=failed", "answer failed — never replayed as fact"} {
		if !strings.Contains(recoveredHistory, fragment) {
			t.Fatalf("recovered history %q omitted %q", recoveredHistory, fragment)
		}
	}
	terminalOutput, terminalCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-crash-mid-query")
	if terminalCode != 4 || !strings.Contains(terminalOutput, "[DENIED]") {
		t.Fatalf("crashed ask replay = %d: %s", terminalCode, terminalOutput)
	}
	originalAgain, originalAgainCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-answered-go-anchor")
	if originalAgainCode != 0 || originalAgain != answeredOutput {
		t.Fatalf("original disposition after crash = %d:\n%s", originalAgainCode, originalAgain)
	}

	// Revoke during query: the slow fake provider keeps synthesis in flight
	// while the source is revoked; the emit reauthorization discards every
	// claim and the answer abstains with exactly absent_support, indistinguishable
	// from denied or genuinely absent support.
	stopProcessDaemon(t, daemon, fixture.socketPath)
	provider := startSlowProvider(t, 2500*time.Millisecond)
	daemon = startProcessDaemonWithEnv(t, fixture, []string{
		"OUROBOROS_QUERY_PROVIDER=openai",
		"OPENAI_API_KEY=process-test-static-key",
		"OUROBOROS_OPENAI_BASE_URL=" + provider.address,
		"OUROBOROS_OPENAI_TIMEOUT_MS=4000",
	})
	midRevoke := startQueryAskProcess(t, tui, fixture.socketPath, append([]string{
		"ask", "--source", source, "--generation", generationTwo,
		"--query", "Which Go function in src/go/modify-00.go returns the stage marker?",
		"--freshness", "best-effort", "--idempotency", "ask-revoke-mid-query", "--timeout-ms", "4500",
	}, base...))
	time.Sleep(300 * time.Millisecond)
	revoke := processIngestionCommand("revoke", base,
		"--source", source, "--expected-generation", generationTwo, "--idempotency", "source-revoke")
	if output := runIngestionTUI(t, tui, fixture.socketPath, revoke...); !strings.Contains(output, "Source revoked") {
		t.Fatalf("revoke output = %q", output)
	}
	midRevokeOutput, midRevokeCode := midRevoke.await(t)
	if midRevokeCode != 3 || !strings.Contains(midRevokeOutput, "[ABSTAINED]") ||
		!strings.Contains(midRevokeOutput, "degraded=absent_support") {
		t.Fatalf("revoke during query = %d: %s", midRevokeCode, midRevokeOutput)
	}
	assertNoExistenceDetail(t, midRevokeOutput)
	provider.close(t)

	// After revocation the same abstention shape serves every new ask, status
	// denies statically, sources list nothing, and history stays readable.
	deniedOutput, deniedCode := ask(generationTwo,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best-effort", "ask-after-revoke")
	if deniedCode != 3 || !strings.Contains(deniedOutput, "[ABSTAINED]") ||
		!strings.Contains(deniedOutput, "degraded=absent_support") {
		t.Fatalf("post-revoke ask = %d: %s", deniedCode, deniedOutput)
	}
	assertNoExistenceDetail(t, deniedOutput)
	revokedStatus, revokedStatusCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"status", "--source", source}, base...)...)
	if revokedStatusCode != 4 || !strings.Contains(revokedStatus, "[DENIED]") {
		t.Fatalf("revoked status = %d: %s", revokedStatusCode, revokedStatus)
	}
	revokedSources, revokedSourcesCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"sources", "--page-size", "100"}, base...)...)
	if revokedSourcesCode != 0 || !strings.Contains(revokedSources, "Sources — results=0") {
		t.Fatalf("revoked sources = %d: %s", revokedSourcesCode, revokedSources)
	}
	revokedHistory, revokedHistoryCode := runQueryTUI(t, tui, fixture.socketPath,
		append([]string{"history", "--page-size", "100"}, base...)...)
	if revokedHistoryCode != 0 || !strings.Contains(revokedHistory, "History — turns=28") {
		t.Fatalf("revoked history = %d: %s", revokedHistoryCode, revokedHistory)
	}

	stopProcessDaemon(t, daemon, fixture.socketPath)
	assertDurableStateClosed(t, fixture.stateRoot)
	// Rendered query text, prose, and cited values live only inside encrypted
	// vault payloads; nothing plaintext persists in the authority state.
	for _, plaintext := range []string{"returns the stage marker", "ouroboros-stage-03", "billing service"} {
		assertStateDoesNotContain(t, fixture.stateRoot, []byte(plaintext))
	}
	fixture.cleanup(t)
	tui.cleanup(t)
}

// assertNoExistenceDetail proves a denied or revoked answer carries no
// protected existence detail: no citations, no claims, no paths, no values.
// The truthful freshness disclosure (generation identity, commit, coverage)
// is contract-frozen and stays.
func assertNoExistenceDetail(t *testing.T, output string) {
	t.Helper()
	for _, forbidden := range []string{
		"citation git=", "claim=", "src/go/", "src/typescript/", "ouroboros-stage-03",
		"tenant-a", "principal-a",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("answer disclosed %q: %q", forbidden, output)
		}
	}
}

// runQueryTUI executes one real query CLI invocation and returns its combined
// output and exit code. Query exit codes 0 (answered/read), 2 (partial),
// 3 (abstained), and 4 (denied) are expected outcomes, not crashes.
func runQueryTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, int) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/query/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	result := tui.Run(t, 15*time.Second, full...)
	if result.err == nil {
		return result.output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(result.err, &exitError) {
		t.Fatalf("query TUI failed to execute: %v: %s", result.err, result.output)
	}
	return result.output, exitError.ExitCode()
}

// queryAskProcess is one in-flight query CLI invocation the test can race
// against daemon lifecycle events. await reaps it exactly once and records
// that fact so the cleanup never waits on an already consumed channel.
type queryAskProcess struct {
	command *exec.Cmd
	output  *boundedProcessOutput
	done    chan error
	awaited bool
}

func startQueryAskProcess(t *testing.T, tui *processTUI, socket string, arguments []string) *queryAskProcess {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/query/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket)
	command := exec.Command(tui.bun, full...)
	command.Dir = tui.workdir
	output := &boundedProcessOutput{maximum: 64 * 1024}
	command.Stdout = output
	command.Stderr = output
	command.Env = []string{
		"BUN_INSTALL_CACHE_DIR=" + tui.cache,
		"CI=1", "HOME=" + tui.home, "NO_COLOR=1",
		"PATH=" + filepath.Dir(tui.bun) + ":/usr/bin:/bin", "TMPDIR=" + tui.tmp,
		"XDG_CACHE_HOME=" + tui.cache,
		"XDG_CONFIG_HOME=" + filepath.Join(tui.home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(tui.home, ".local", "share"),
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &queryAskProcess{command: command, output: output, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(func() {
		if !process.awaited {
			_ = command.Process.Kill()
			<-process.done
		}
	})
	return process
}

// await joins one in-flight query CLI invocation and returns its output and
// exit code, reaping the process exactly once.
func (process *queryAskProcess) await(t *testing.T) (string, int) {
	t.Helper()
	defer func() { process.awaited = true }()
	select {
	case err := <-process.done:
		if err == nil {
			return process.output.String(), 0
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return process.output.String(), exitError.ExitCode()
		}
		return process.output.String(), -1
	case <-time.After(20 * time.Second):
		_ = process.command.Process.Kill()
		<-process.done
		t.Fatalf("query TUI did not exit: %s", process.output.String())
		return "", -1
	}
}

// refusedProviderAddress returns a loopback address with no listener so the
// provider client fails fast and deterministically.
func refusedProviderAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// blackHoleProvider accepts connections and never answers them, holding one
// provider call in flight until the daemon dies. Parked connections stay
// referenced so the runtime never closes them early.
type blackHoleProvider struct {
	listener net.Listener
	address  string
	mu       chan struct{}
	parked   []net.Conn
}

func startBlackHoleProvider(t *testing.T) *blackHoleProvider {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	provider := &blackHoleProvider{listener: listener, address: listener.Addr().String(), mu: make(chan struct{}, 1)}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			provider.mu <- struct{}{}
			provider.parked = append(provider.parked, connection)
			<-provider.mu
		}
	}()
	t.Cleanup(func() { provider.close(t) })
	return provider
}

func (provider *blackHoleProvider) close(t *testing.T) {
	t.Helper()
	if provider == nil {
		return
	}
	if provider.listener != nil {
		_ = provider.listener.Close()
		provider.listener = nil
	}
	provider.mu <- struct{}{}
	for _, connection := range provider.parked {
		_ = connection.Close()
	}
	provider.parked = nil
	<-provider.mu
}

// slowProvider answers one OpenAI-shaped completion after a fixed delay with a
// claim whose citation the engine verifies against the canonical pack, keeping
// synthesis deterministically in flight across a concurrent revocation.
type slowProvider struct {
	server  *http.Server
	address string
}

func startSlowProvider(t *testing.T, delay time.Duration) *slowProvider {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	provider := &slowProvider{address: "http://" + listener.Addr().String()}
	proposal := map[string]any{
		"prose": "src/go/modify-00.go returns \"ouroboros-stage-03\".",
		"claims": []any{map[string]any{
			"statement": "src/go/modify-00.go returns \"ouroboros-stage-03\".", "confidence_per_mille": 900,
			"citations": []any{map[string]any{
				"evidence_index": 0, "start_line": 3, "start_column": 1, "end_line": 3, "end_column": 53,
			}},
		}},
	}
	provider.server = &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
		time.Sleep(delay)
		content, _ := json.Marshal(proposal)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}}},
			"usage":   map[string]uint64{"total_tokens": 512},
		})
	})}
	go func() { _ = provider.server.Serve(listener) }()
	t.Cleanup(func() { provider.close(t) })
	return provider
}

func (provider *slowProvider) close(t *testing.T) {
	t.Helper()
	if provider != nil && provider.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = provider.server.Shutdown(ctx)
		provider.server = nil
	}
}

// processGroundingCase mirrors one frozen Stage 04 grounding case descriptor.
type processGroundingCase struct {
	CaseID            string `json:"caseId"`
	Category          string `json:"category"`
	Query             string `json:"query"`
	PinnedGeneration  string `json:"pinnedGeneration"`
	Freshness         string `json:"freshness"`
	Interference      string `json:"interference"`
	ExpectedStatus    string `json:"expectedStatus"`
	ExpectedCitations []struct {
		Path        string `json:"path"`
		StartLine   int    `json:"startLine"`
		StartColumn int    `json:"startColumn"`
		EndLine     int    `json:"endLine"`
		EndColumn   int    `json:"endColumn"`
	} `json:"expectedCitations"`
	ExpectedReasons []string `json:"expectedReasons"`
	Note            string   `json:"note"`
}

type processGroundingManifest struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Corpus        string                 `json:"corpus"`
	Cases         []processGroundingCase `json:"cases"`
}

// loadProcessGroundingManifest reads the checked-in frozen grounding fixture
// so the process matrix can never drift from the conformance descriptor.
func loadProcessGroundingManifest(t *testing.T) processGroundingManifest {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(
		processRepositoryRoot(t), "tests", "fixtures", "stage-04", "grounding", "query-cases.json"))
	if err != nil {
		t.Fatalf("read grounding fixture: %v", err)
	}
	var manifest processGroundingManifest
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode grounding fixture: %v", err)
	}
	if manifest.SchemaVersion != "ouroboros.stage04.grounding-cases.v1" || len(manifest.Cases) != 12 {
		t.Fatalf("unexpected grounding fixture: version=%q cases=%d", manifest.SchemaVersion, len(manifest.Cases))
	}
	return manifest
}

// mapProcessFreshness converts the frozen freshness vocabulary to the CLI's.
func mapProcessFreshness(t *testing.T, freshness string) string {
	t.Helper()
	switch freshness {
	case "best_effort", "complete_generation", "abstain_if_stale":
		return strings.ReplaceAll(freshness, "_", "-")
	default:
		t.Fatalf("unknown frozen freshness %q", freshness)
		return ""
	}
}

type processGroundingExpectation struct {
	exitCode  int
	tag       string
	fragments []string
}

// groundingExpectation maps one frozen case to its process-visible exit code,
// row tag, and required output fragments: exact citation ranges from the
// descriptor and the first frozen degraded reason. Over the Stage 03 process
// corpus, genuinely absent answers additionally disclose the truthful
// partial_coverage of canonical unindexed revisions, so the degraded fragment
// is asserted as a prefix, never widened.
func groundingExpectation(queryCase processGroundingCase) processGroundingExpectation {
	expectation := processGroundingExpectation{}
	switch queryCase.ExpectedStatus {
	case "answered":
		expectation.exitCode = 0
		expectation.tag = "[ANSWERED]"
	case "partial":
		expectation.exitCode = 2
		expectation.tag = "[PARTIAL]"
	case "abstained":
		expectation.exitCode = 3
		expectation.tag = "[ABSTAINED]"
	}
	for _, citation := range queryCase.ExpectedCitations {
		expectation.fragments = append(expectation.fragments, fmt.Sprintf("%s:%d:%d-%d:%d",
			citation.Path, citation.StartLine, citation.StartColumn, citation.EndLine, citation.EndColumn))
	}
	if len(queryCase.ExpectedReasons) > 0 {
		expectation.fragments = append(expectation.fragments, "degraded="+queryCase.ExpectedReasons[0])
	}
	if queryCase.ExpectedStatus == "partial" && containsProcessString(queryCase.ExpectedReasons, "stale_support") {
		expectation.fragments = append(expectation.fragments, "freshness=stale_disclosed")
	}
	return expectation
}

func containsProcessString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
