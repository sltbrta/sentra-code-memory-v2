// Process tests prove the bounded Stage 02 and Stage 03 local-authority paths
// through real child processes and the pinned Bun TUI. They keep key material
// in the test binary.
package authorityprocess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	"google.golang.org/protobuf/proto"
)

const (
	processHelperEnvironment   = "OUROBOROS_LOCAL_AUTHORITY_PROCESS_HELPER"
	processManifestEnvironment = "OUROBOROS_LOCAL_AUTHORITY_TEST_MANIFEST"
	processDigestEnvironment   = "OUROBOROS_LOCAL_AUTHORITY_TEST_MANIFEST_SHA256"
	// processFactoryStagingDirectory is the optional sibling of the process
	// bootstrap manifest that holds Stage 05 approval-descriptor payloads the
	// process helper stages before the daemon serves. The parent test writes
	// one file per artifact id; the helper prepares them the same way it
	// stages the Stage 02 artifact-a fixture.
	processFactoryStagingDirectory = "factory-descriptor-staging"
	processTimeout                 = 15 * time.Second
)

var processArtifactContent = []byte("stage-02 durable artifact")

// TestLocalAuthorityProcessHelper runs the actual command composition inside a
// child test process. The parent supplies only non-secret bootstrap metadata;
// deterministic test key bytes remain compiled into this test binary.
func TestLocalAuthorityProcessHelper(t *testing.T) {
	if os.Getenv(processHelperEnvironment) != "1" {
		return
	}
	manifest := os.Getenv(processManifestEnvironment)
	digest := os.Getenv(processDigestEnvironment)
	if manifest == "" || digest == "" {
		_, _ = fmt.Fprintln(os.Stderr, "process helper: missing bootstrap")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := RunWithOpener(ctx, []string{
		"--bootstrap", manifest,
		"--bootstrap-sha256", digest,
	}, openProcessTestRuntime); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "process helper: startup rejected")
		os.Exit(2)
	}
}

// TestLocalAuthorityProcessTracer proves the Stage 02 user path through the
// production command, owner-only socket, durable runtime, and actual Bun TUI.
func TestLocalAuthorityProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	bun := requirePinnedBun(t)
	fixture := newProcessFixture(t)
	tui := installProcessTUI(t, bun)

	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "open-session",
	}, "[OK] Local session", "session=session:session-a")
	assertTUISucceeds(t, tui, fixture.socketPath,
		[]string{"status", "--session", "session:session-a"},
		"[OK] Local authority ready", "revocation epoch 3")
	assertTUISucceeds(t, tui, fixture.socketPath,
		processCommandArguments(t, fixture.request(t, "artifact.admit", "admit-1", 3)),
		"[OK] Authority command", "generation=1")
	stopProcessDaemon(t, daemon, fixture.socketPath)
	assertDurableStateClosed(t, fixture.stateRoot)
	assertStateDoesNotContain(t, fixture.stateRoot, processArtifactContent)

	daemon = startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath,
		[]string{"status", "--session", "session:session-a"},
		"[OK] Local authority ready", "revocation epoch 3")
	assertTUISucceeds(t, tui, fixture.socketPath,
		processCommandArguments(t, fixture.request(t, "artifact.read", "read-after-restart", 3)),
		"[OK] Authority command", "artifact=artifact:artifact-a")
	assertTUIDenied(t, tui, fixture.socketPath,
		processCommandArguments(t, fixture.request(t, "artifact.read", "stale-read", 2)))
	assertTUISucceeds(t, tui, fixture.socketPath,
		processCommandArguments(t, fixture.request(t, "artifact.delete", "delete-1", 3)),
		"[OK] Authority command", "artifact=artifact:artifact-a")
	assertNoObjectPayloads(t, filepath.Join(fixture.stateRoot, "objects"))
	assertTUIDenied(t, tui, fixture.socketPath,
		processCommandArguments(t, fixture.request(t, "artifact.read", "read-after-delete", 3)))
	stopProcessDaemon(t, daemon, fixture.socketPath)
	assertDurableStateClosed(t, fixture.stateRoot)
	assertStateDoesNotContain(t, fixture.stateRoot, processArtifactContent)

	fixture.cleanup(t)
	tui.cleanup(t)
}

// TestLocalAuthorityIngestionProcessTracer proves committed-Git admission,
// P5 search, frozen reconciliation, restart, replay, and revocation through the
// production command and checked-in Bun TUI.
func TestLocalAuthorityIngestionProcessTracer(t *testing.T) {
	if os.Getenv(processHelperEnvironment) == "1" {
		return
	}
	tui := installProcessTUI(t, requirePinnedBun(t))
	fixture := newProcessFixture(t)
	firstCommit := fixture.prepareStage3Source(t)
	daemon := startProcessDaemon(t, fixture)
	assertTUISucceeds(t, tui, fixture.socketPath, []string{
		"session", "--principal", "principal:principal-a", "--tenant", "tenant:tenant-a",
		"--session", "session:session-a", "--idempotency", "source-open-session",
	}, "[OK] Local session")
	base := []string{"--principal", "principal:principal-a", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	add := processIngestionCommand("add", base,
		"--commit", firstCommit, "--configuration-digest", fixture.configDigest,
		"--idempotency", "source-add", "--use-gitignore", "true", "--use-ouroborosignore", "true")
	addOutput := runIngestionTUI(t, tui, fixture.socketPath, add...)
	source, generation := processIngestionIdentifiers(t, addOutput)
	replaySource, replayGeneration := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, add...))
	if replaySource != source || replayGeneration != generation {
		t.Fatalf("source add replay = %s/%s, want %s/%s", replaySource, replayGeneration, source, generation)
	}
	status := processIngestionCommand("status", base, "--source", source)
	assertProcessIngestionStatus(t, runIngestionTUI(t, tui, fixture.socketPath, status...), generation)
	assertProcessIngestionSearchMatrix(t, tui, fixture.socketPath, base, source, generation)
	malformedExact := processIngestionCommand("search", base,
		"--source", source, "--generation", generation, "--query", "malformed", "--kind", "exact", "--page-size", "100")
	if output := runIngestionTUI(t, tui, fixture.socketPath, malformedExact...); !strings.Contains(output, "src/typescript/modify-00.ts") {
		t.Fatalf("malformed TypeScript exact search output = %q", output)
	}

	secondCommit := fixture.reconcileStage3Source(t, firstCommit)
	conflictingAdd := processIngestionCommand("add", base,
		"--commit", secondCommit, "--configuration-digest", fixture.configDigest,
		"--idempotency", "source-add", "--use-gitignore", "true", "--use-ouroborosignore", "true")
	assertProcessIngestionDenied(t, tui, fixture.socketPath, conflictingAdd...)

	unknownSource := "source:" + strings.Repeat("0", 64)
	assertProcessIngestionDenied(t, tui, fixture.socketPath, processIngestionCommand("status", base, "--source", unknownSource)...)
	wrongPrincipal := []string{"--principal", "principal:principal-b", "--tenant", "tenant:tenant-a", "--session", "session:session-a"}
	assertProcessIngestionRequestDenied(t, tui, fixture.socketPath, processIngestionCommand("status", wrongPrincipal, "--source", source)...)

	reconcile := processIngestionCommand("reconcile", base,
		"--source", source, "--expected-generation", generation, "--expected-commit", firstCommit,
		"--target-commit", secondCommit, "--idempotency", "source-reconcile")
	_, nextGeneration := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, reconcile...))
	if nextGeneration == generation {
		t.Fatal("reconcile did not advance generation")
	}
	_, reconciledReplayGeneration := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, reconcile...))
	if reconciledReplayGeneration != nextGeneration {
		t.Fatalf("reconcile replay generation = %s, want %s", reconciledReplayGeneration, nextGeneration)
	}
	staleReconcile := processIngestionCommand("reconcile", base,
		"--source", source, "--expected-generation", generation, "--expected-commit", firstCommit,
		"--target-commit", secondCommit, "--idempotency", "source-reconcile-stale")
	assertProcessIngestionDenied(t, tui, fixture.socketPath, staleReconcile...)

	reconciledSearch := processIngestionCommand("search", base,
		"--source", source, "--generation", nextGeneration, "--query", "Anchor", "--kind", "symbol", "--page-size", "100")
	beforeRestartSearch := runIngestionTUI(t, tui, fixture.socketPath, reconciledSearch...)
	if !strings.Contains(beforeRestartSearch, "src/go/") {
		t.Fatalf("reconciled search output = %q", beforeRestartSearch)
	}
	stopProcessDaemon(t, daemon, fixture.socketPath)
	daemon = startProcessDaemon(t, fixture)
	assertProcessIngestionStatus(t, runIngestionTUI(t, tui, fixture.socketPath, status...), nextGeneration)
	afterRestartSearch := runIngestionTUI(t, tui, fixture.socketPath, reconciledSearch...)
	if afterRestartSearch != beforeRestartSearch {
		t.Fatalf("reconciled search changed across restart:\n before=%q\n after=%q", beforeRestartSearch, afterRestartSearch)
	}
	_, restartReplayGeneration := processIngestionIdentifiers(t, runIngestionTUI(t, tui, fixture.socketPath, reconcile...))
	if restartReplayGeneration != nextGeneration {
		t.Fatalf("reconcile replay after restart generation = %s, want %s", restartReplayGeneration, nextGeneration)
	}

	revoke := processIngestionCommand("revoke", base,
		"--source", source, "--expected-generation", nextGeneration, "--idempotency", "source-revoke")
	for replay := 0; replay < 2; replay++ {
		if output := runIngestionTUI(t, tui, fixture.socketPath, revoke...); !strings.Contains(output, "Source revoked") {
			t.Fatalf("revoke replay %d output = %q", replay, output)
		}
	}
	assertProcessIngestionDenied(t, tui, fixture.socketPath, status...)
	postRevokeSearch := processIngestionCommand("search", base,
		"--source", source, "--generation", nextGeneration, "--query", "Anchor", "--kind", "symbol", "--page-size", "100")
	assertProcessIngestionDenied(t, tui, fixture.socketPath, postRevokeSearch...)
	postRevokeReconcile := processIngestionCommand("reconcile", base,
		"--source", source, "--expected-generation", nextGeneration, "--expected-commit", secondCommit,
		"--target-commit", firstCommit, "--idempotency", "source-reconcile-revoked")
	assertProcessIngestionDenied(t, tui, fixture.socketPath, postRevokeReconcile...)
	stopProcessDaemon(t, daemon, fixture.socketPath)
	tui.cleanup(t)
	fixture.cleanup(t)
}

func processIngestionCommand(kind string, base []string, flags ...string) []string {
	arguments := make([]string, 0, 2+len(base)+len(flags))
	arguments = append(arguments, "source", kind)
	arguments = append(arguments, base...)
	return append(arguments, flags...)
}

func assertProcessIngestionStatus(t *testing.T, output, generation string) {
	t.Helper()
	for _, expected := range []string{
		generation,
		"state=ADMITTED",
		"GO:SYNTAX_AWARE",
		"TYPESCRIPT:LEXICAL_DEGRADED",
		"PYTHON:SYNTAX_AWARE",
		"RUST:SYNTAX_AWARE",
		"JAVA:SYNTAX_AWARE",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("status output %q omitted %q", output, expected)
		}
	}
}

func assertProcessIngestionSearchMatrix(
	t *testing.T,
	tui *processTUI,
	socket string,
	base []string,
	source, generation string,
) {
	t.Helper()
	for _, lane := range []struct {
		name      string
		query     string
		reference string
		pathPart  string
	}{
		{name: "go", query: "Anchor", reference: "string", pathPart: "src/go/"},
		{name: "typescript", query: "anchor", reference: "string", pathPart: "src/typescript/"},
		{name: "python", query: "anchor", reference: "str", pathPart: "src/python/"},
		{name: "rust", query: "anchor", reference: "anchor", pathPart: "src/rust/"},
		{name: "java", query: "anchor", reference: "String", pathPart: "src/java/"},
	} {
		for _, kind := range []string{"exact", "symbol", "reference"} {
			query := lane.query
			if kind == "reference" {
				query = lane.reference
			}
			command := processIngestionCommand("search", base,
				"--source", source, "--generation", generation, "--query", query,
				"--kind", kind, "--page-size", "100")
			output, err := runIngestionTUIResult(t, tui, socket, command...)
			if err != nil {
				t.Fatalf("%s %s search failed: %v: %q", lane.name, kind, err, output)
			}
			if !strings.Contains(output, lane.pathPart) {
				t.Fatalf("%s %s search output = %q", lane.name, kind, output)
			}
		}
	}
}

func assertProcessIngestionDenied(t *testing.T, tui *processTUI, socket string, arguments ...string) {
	t.Helper()
	output, err := runIngestionTUIResult(t, tui, socket, arguments...)
	if err != nil || !strings.Contains(output, "not_found_or_denied") {
		t.Fatalf("ingestion denial response = %q", output)
	}
	for _, forbidden := range []string{"source:", "generation:", "tenant-a", "principal-a", "principal-b", "src/", "ERROR request-timeout", "response-invalid"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("ingestion denial disclosed %q: %q", forbidden, output)
		}
	}
}

func assertProcessIngestionRequestDenied(t *testing.T, tui *processTUI, socket string, arguments ...string) {
	t.Helper()
	output, err := runIngestionTUIResult(t, tui, socket, arguments...)
	if err == nil || output != "ERROR request-denied\n" {
		t.Fatalf("identity mismatch response = %q", output)
	}
}

func openProcessTestRuntime(ctx context.Context, config brain.DarwinConfig) (*brain.Runtime, error) {
	_, statErr := os.Lstat(config.Durable.DatabasePath)
	restarting := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, brain.ErrInvalid
	}
	resolver := processKeyResolver{config: config.Durable}
	runtime, err := brain.OpenDurable(ctx, config.Durable, resolver)
	if err != nil {
		return nil, err
	}
	if restarting {
		return runtime, nil
	}
	digest := sha256.Sum256(processArtifactContent)
	frameBytes := uint64(config.Durable.Storage.FrameBytes)
	if frameBytes == 0 {
		_ = runtime.Close()
		return nil, brain.ErrInvalid
	}
	artifact := brain.Artifact{
		ID:                 brain.Identifier{Namespace: "artifact", Value: "artifact-a"},
		Tenant:             config.Durable.Tenant,
		Digest:             brain.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digest[:])},
		Generation:         1,
		ExpectedGeneration: 0,
		KeyEpoch:           config.Durable.CurrentKeyReference.Epoch,
		Length:             uint64(len(processArtifactContent)),
		FrameCount:         uint32((uint64(len(processArtifactContent)) + frameBytes - 1) / frameBytes),
	}
	if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(processArtifactContent)); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	// Stage 05 process proofs may leave approval-descriptor payloads beside
	// the bootstrap manifest. Each file name is the artifact id; contents are
	// staged through PrepareArtifact so the subsequent authorized admit
	// publishes real ledger state instead of client-supplied bytes.
	if err := prepareFactoryDescriptorStaging(ctx, runtime, config, frameBytes); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

// prepareFactoryDescriptorStaging stages every approval-descriptor payload
// written next to the process bootstrap manifest. Missing or empty staging
// directories are a no-op so Stage 02–04 process tracers stay unaffected.
func prepareFactoryDescriptorStaging(
	ctx context.Context, runtime *brain.Runtime, config brain.DarwinConfig, frameBytes uint64,
) error {
	manifestPath := os.Getenv(processManifestEnvironment)
	if manifestPath == "" || frameBytes == 0 {
		return nil
	}
	stagingRoot := filepath.Join(filepath.Dir(manifestPath), processFactoryStagingDirectory)
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return brain.ErrInvalid
		}
		artifactID := entry.Name()
		if artifactID == "" || strings.Contains(artifactID, string(filepath.Separator)) {
			return brain.ErrInvalid
		}
		content, err := os.ReadFile(filepath.Join(stagingRoot, artifactID))
		if err != nil || len(content) == 0 {
			return brain.ErrInvalid
		}
		digest := sha256.Sum256(content)
		artifact := brain.Artifact{
			ID:                 brain.Identifier{Namespace: "artifact", Value: artifactID},
			Tenant:             config.Durable.Tenant,
			Digest:             brain.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digest[:])},
			Generation:         1,
			ExpectedGeneration: 0,
			KeyEpoch:           config.Durable.CurrentKeyReference.Epoch,
			Length:             uint64(len(content)),
			FrameCount:         uint32((uint64(len(content)) + frameBytes - 1) / frameBytes),
		}
		if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(content)); err != nil {
			return err
		}
	}
	return nil
}

type processKeyResolver struct{ config brain.DurableConfig }

func (resolver processKeyResolver) Current(_ context.Context, tenant brain.Identifier) (brain.KeyMaterial, error) {
	if tenant != resolver.config.Tenant {
		return brain.KeyMaterial{}, brain.ErrInvalid
	}
	return resolver.material(), nil
}

func (resolver processKeyResolver) Resolve(
	_ context.Context,
	tenant brain.Identifier,
	epoch uint64,
) (brain.KeyMaterial, error) {
	if tenant != resolver.config.Tenant || epoch != resolver.config.CurrentKeyReference.Epoch {
		return brain.KeyMaterial{}, brain.ErrInvalid
	}
	return resolver.material(), nil
}

func (resolver processKeyResolver) material() brain.KeyMaterial {
	return brain.KeyMaterial{
		Reference: resolver.config.CurrentKeyReference,
		RootKey:   bytes.Repeat([]byte{0x5a}, brain.RootKeyBytes),
	}
}

type processDaemon struct {
	command *exec.Cmd
	output  *boundedProcessOutput
	done    chan struct{}
	waitErr error
}

func startProcessDaemon(t *testing.T, fixture *processFixture) *processDaemon {
	t.Helper()
	return startProcessDaemonWithEnv(t, fixture, nil)
}

// startProcessDaemonWithEnv starts the production command with optional extra
// environment (for example an explicitly configured query provider). The extra
// values are fully test-controlled; the ambient environment never leaks in.
func startProcessDaemonWithEnv(t *testing.T, fixture *processFixture, extraEnv []string) *processDaemon {
	t.Helper()
	// The daemon validates manifest grant expiry against its own startup and
	// request wall clocks; fail explicitly if a paused run exhausted the window.
	fixture.assertGrantHorizonLive(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), processTimeout)
	defer cancel()
	command := exec.Command(executable, "-test.run=^TestLocalAuthorityProcessHelper$")
	command.Env = append([]string{
		"HOME=" + filepath.Join(fixture.manifestRoot, "home"),
		"PATH=/usr/bin:/bin",
		"TMPDIR=" + filepath.Join(fixture.manifestRoot, "tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(fixture.manifestRoot, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(fixture.manifestRoot, "home", ".config"),
		"XDG_DATA_HOME=" + filepath.Join(fixture.manifestRoot, "home", ".local", "share"),
		processHelperEnvironment + "=1",
		processManifestEnvironment + "=" + fixture.manifestPath,
		processDigestEnvironment + "=" + fixture.manifestDigest,
	}, extraEnv...)
	output := &boundedProcessOutput{maximum: 16 * 1024}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	daemon := &processDaemon{command: command, output: output, done: make(chan struct{})}
	go func() {
		daemon.waitErr = command.Wait()
		close(daemon.done)
	}()
	t.Cleanup(func() {
		select {
		case <-daemon.done:
			return
		default:
			_ = command.Process.Kill()
			<-daemon.done
		}
	})
	waitForProcessSocket(t, ctx, daemon, fixture.socketPath)
	return daemon
}

func waitForProcessSocket(t *testing.T, ctx context.Context, daemon *processDaemon, socketPath string) {
	t.Helper()
	for {
		info, err := os.Lstat(socketPath)
		if err == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == 0o600 {
			return
		}
		select {
		case <-daemon.done:
			t.Fatalf("daemon exited before readiness: %v: %s", daemon.waitErr, daemon.output.String())
		case <-ctx.Done():
			_ = daemon.command.Process.Kill()
			<-daemon.done
			t.Fatalf("daemon readiness timed out: %s", daemon.output.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func stopProcessDaemon(t *testing.T, daemon *processDaemon, socketPath string) {
	t.Helper()
	if err := daemon.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
	select {
	case <-daemon.done:
		if daemon.waitErr != nil {
			t.Fatalf("daemon shutdown: %v: %s", daemon.waitErr, daemon.output.String())
		}
	case <-time.After(5 * time.Second):
		_ = daemon.command.Process.Kill()
		<-daemon.done
		t.Fatalf("daemon did not stop: %s", daemon.output.String())
	}
	if daemon.command.ProcessState == nil || !daemon.command.ProcessState.Exited() {
		t.Fatal("daemon process was not reaped")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remained after shutdown: %v", err)
	}
}

func processCommandArguments(t *testing.T, request *contractsv1.ExecuteAuthorityCommandRequest) []string {
	t.Helper()
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"command", "--request-base64", base64.StdEncoding.EncodeToString(payload)}
}

func assertDurableStateClosed(t *testing.T, root string) {
	t.Helper()
	database, err := os.Lstat(filepath.Join(root, "authority.sqlite3"))
	if err != nil {
		t.Fatalf("inspect authority database: %v", err)
	}
	if !database.Mode().IsRegular() || database.Mode().Perm() != 0o600 {
		t.Fatalf("authority database is not owner-only: mode=%v", database.Mode().Perm())
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(filepath.Join(root, "authority.sqlite3") + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SQLite sidecar %q remained: %v", suffix, err)
		}
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasSuffix(entry.Name(), ".part") {
			return fmt.Errorf("partial artifact remained: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertStateDoesNotContain(t *testing.T, root string, plaintext []byte) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, plaintext) {
			return fmt.Errorf("plaintext persisted at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoObjectPayloads(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return fmt.Errorf("purged object payload remained: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type boundedProcessOutput struct {
	contents bytes.Buffer
	maximum  int
	mu       sync.Mutex
}

func (output *boundedProcessOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.maximum - output.contents.Len()
	if remaining > 0 {
		_, _ = output.contents.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}

func (output *boundedProcessOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.contents.String()
}
