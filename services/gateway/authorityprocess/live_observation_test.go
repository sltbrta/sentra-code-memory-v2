// The live-provider observation is opt-in and never part of a required gate.
// With a policy-approved OpenAI credential explicitly present it runs ONE live
// ask through the production command against the deterministic fixture corpus
// and records a quality/token receipt. Without one it skips and records an
// explicit scoped waiver receipt instead. It never weakens a deterministic
// gate and never falls back to another provider or billing identity.
package authorityprocess

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const liveObservationEnvironment = "OUROBOROS_LIVE_QUERY_OBSERVATION"

// liveObservationReceipt records the bounded facts of one live provider ask:
// disposition, citation shape, token usage, latency, and model identity. Cost
// is recorded as the token count the provider billed; the v1 answer contract
// does not split prompt and completion tokens, so dollar cost is derived
// externally from the provider console and never fabricated here.
type liveObservationReceipt struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Observation   string `json:"observation"`
	QueryCase     string `json:"query_case,omitempty"`
	AnswerStatus  string `json:"answer_status,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Claims        int    `json:"claims,omitempty"`
	Citations     int    `json:"citations,omitempty"`
	TotalTokens   uint64 `json:"total_tokens,omitempty"`
	LatencyMillis int64  `json:"latency_ms,omitempty"`
	Deterministic bool   `json:"deterministic_gates_note"`
	Waiver        string `json:"waiver,omitempty"`
	ObservedAtUTC string `json:"observed_at_utc"`
}

// TestLiveQueryProviderObservation runs one live OpenAI ask against the
// deterministic Stage 03 fixture corpus when explicitly enabled, and skips
// with an explicit waiver receipt otherwise. It is tagged manual and belongs
// to no stage gate.
func TestLiveQueryProviderObservation(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	if os.Getenv(liveObservationEnvironment) != "1" || os.Getenv(openAIKeyEnvironment) == "" {
		writeLiveObservationReceipt(t, liveObservationReceipt{
			Schema: "ouroboros.stage04.live-observation.v1", Kind: "waiver",
			Provider: "openai", Observation: "waived",
			Waiver: "no policy-approved provider credential present at observation time; " +
				"deterministic synthesis gates remain the correctness proof and this observation " +
				"never substitutes for them",
			Deterministic: true,
			ObservedAtUTC: time.Now().UTC().Format(time.RFC3339),
		})
		t.Skip("live provider observation waived: no policy-approved credential present")
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	firstCommit := fixture.prepareStage3Source(t)
	model := os.Getenv(openAIModelEnvironment)
	if model == "" {
		model = openAIDefaultModel
	}
	daemon := startProcessDaemonWithEnv(t, fixture, []string{
		"OUROBOROS_QUERY_PROVIDER=openai",
		"OPENAI_API_KEY=" + os.Getenv(openAIKeyEnvironment),
		"OUROBOROS_OPENAI_MODEL=" + model,
		"OUROBOROS_OPENAI_TIMEOUT_MS=4000",
	})
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "live-open-session",
	}, "[OK] Local session")
	base := []string{"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	add := processIngestionCommand("add", base,
		"--commit", firstCommit, "--configuration-digest", fixture.configDigest,
		"--idempotency", "source-add", "--use-gitignore", "true", "--use-ouroborosignore", "true")
	source, generation := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, add...))

	started := time.Now()
	output, code := runQueryTUI(t, tui, fixture.socketPath, append([]string{
		"ask", "--source", source, "--generation", generation,
		"--query", "Which Go function in src/go/modify-00.go returns the stage marker?",
		"--freshness", "best-effort", "--idempotency", "ask-live-observation", "--timeout-ms", "9500",
	}, base...)...)
	latency := time.Since(started).Milliseconds()
	status := liveAnswerStatus(t, output)
	receipt := liveObservationReceipt{
		Schema: "ouroboros.stage04.live-observation.v1", Kind: "observation",
		Provider: "openai", Model: model, Observation: "recorded",
		QueryCase: "answered-go-anchor", AnswerStatus: status, ExitCode: code,
		Claims: strings.Count(output, "claim=claim:"), Citations: strings.Count(output, "citation git="),
		TotalTokens:   liveTokenUsage(t, output),
		LatencyMillis: latency, Deterministic: true,
		ObservedAtUTC: time.Now().UTC().Format(time.RFC3339),
	}
	path := writeLiveObservationReceipt(t, receipt)
	t.Logf("live provider observation receipt: %s", path)
	if code != 0 && code != 2 && code != 3 {
		t.Fatalf("live ask exit = %d: %s", code, output)
	}
	if status == "" {
		t.Fatalf("live ask rendered no disposition: %q", output)
	}
	stopProcessDaemon(t, daemon, fixture.socketPath)
	fixture.cleanup(t)
	tui.cleanup(t)
}

func liveAnswerStatus(t *testing.T, output string) string {
	t.Helper()
	for _, tag := range []string{"[ANSWERED]", "[PARTIAL]", "[ABSTAINED]"} {
		if strings.Contains(output, tag) {
			return strings.Trim(tag, "[]")
		}
	}
	return ""
}

func liveTokenUsage(t *testing.T, output string) uint64 {
	t.Helper()
	matched := regexp.MustCompile(`tokens=([0-9]+)`).FindStringSubmatch(output)
	if len(matched) != 2 {
		return 0
	}
	value, err := strconv.ParseUint(matched[1], 10, 64)
	if err != nil {
		t.Fatalf("parse token usage from %q: %v", output, err)
	}
	return value
}

// writeLiveObservationReceipt persists the receipt beneath the test output
// directory when Bazel provides one, else the test temp dir, and returns the
// path. The Stage 04 evidence directory retains only the sanitized waiver.
func writeLiveObservationReceipt(t *testing.T, receipt liveObservationReceipt) string {
	t.Helper()
	root := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if root == "" {
		root = t.TempDir()
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "live-provider-observation.json")
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
