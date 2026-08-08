// Process fixtures build strict non-secret bootstrap manifests, real protobuf
// commands, and a private offline copy of the pinned Bun TUI module root for
// the process tracer.
package authorityprocess

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	bootstrap "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// processGrantHorizon bounds issued grant validity to a window derived from
// the test clock instead of a fixed far-future date. It is minted exactly
// once at fixture creation: the broker structurally matches grant expiry
// against the manifest, and the configuration digest pins the exact manifest
// bytes across daemon restarts, so re-minting between phases would break
// digest and restart continuity. The window is sized to outlast any realistic
// pause, and the liveness guard below turns an exhausted window into an
// explicit fixture failure rather than a spurious daemon denial.
const processGrantHorizon = 24 * time.Hour

// processGrantSafetyMargin is the remaining-validity floor that daemon starts
// and request builds require before proceeding.
const processGrantSafetyMargin = time.Minute

const maxProcessSourceBytes = 4 * 1024 * 1024

type processFixture struct {
	manifestRoot   string
	manifestPath   string
	manifestDigest string
	policyDigest   string
	configDigest   string
	stateRoot      string
	sourceRoot     string
	socketPath     string
	grantExpiry    time.Time
	cleaned        bool
	stage3Manifest processStage3DeltaManifest
}

type processStage3DeltaManifest struct {
	SchemaVersion string                        `json:"schemaVersion"`
	PathCounting  string                        `json:"pathCounting"`
	Operations    []processStage3DeltaOperation `json:"operations"`
}

type processStage3DeltaOperation struct {
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	OldPath   string `json:"oldPath"`
	NewPath   string `json:"newPath"`
	Malformed bool   `json:"malformed"`
}

func newProcessFixture(t *testing.T) *processFixture {
	t.Helper()
	manifestRoot := secureProcessRoot(t, ".ouroboros-process-manifest-")
	stateRoot := secureProcessRoot(t, ".ouroboros-process-state-")
	sourceRoot := secureProcessRoot(t, ".ouroboros-process-source-")
	fixture := &processFixture{
		manifestRoot: manifestRoot,
		manifestPath: filepath.Join(manifestRoot, "bootstrap.json"),
		stateRoot:    stateRoot,
		sourceRoot:   sourceRoot,
		socketPath:   filepath.Join(stateRoot, "authority.sock"),
		// One bounded expiry derived from the test clock keeps the bootstrap
		// manifest and every presented grant identical for digest and replay.
		grantExpiry: time.Now().UTC().Add(processGrantHorizon).Truncate(time.Second),
	}
	for _, directory := range []string{
		filepath.Join(manifestRoot, "home"), filepath.Join(manifestRoot, "cache"),
		filepath.Join(manifestRoot, "tmp"),
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	processGit(t, sourceRoot, "init")
	if err := os.WriteFile(filepath.Join(sourceRoot, "README.md"), []byte("# process fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	processGit(t, sourceRoot, "add", "README.md")
	processGit(t, sourceRoot, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
	t.Cleanup(func() {
		if !fixture.cleaned {
			_ = os.RemoveAll(fixture.manifestRoot)
			_ = os.RemoveAll(fixture.stateRoot)
			_ = os.RemoveAll(fixture.sourceRoot)
		}
	})
	payload := fixture.manifest(t)
	digest := sha256.Sum256(payload)
	fixture.manifestDigest = hex.EncodeToString(digest[:])
	if err := os.WriteFile(fixture.manifestPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := bootstrap.Load(bootstrap.Options{
		ManifestPath: fixture.manifestPath, ExpectedSHA256: fixture.manifestDigest,
		Now: func() time.Time { return time.Unix(1_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("load process bootstrap: %v", err)
	}
	fixture.policyDigest = config.PolicyDigest()
	fixture.configDigest = config.ConfigurationDigest()
	return fixture
}

func processGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("/usr/bin/git", append([]string{"-C", root}, arguments...)...)
	command.Env = sanitizedGitEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (fixture *processFixture) manifest(t *testing.T) []byte {
	t.Helper()
	frameBytes := uint32(8)
	frames := uint64((len(processArtifactContent) + int(frameBytes) - 1) / int(frameBytes))
	relationships := []bootstrap.RelationshipSpec{
		{Object: "brain:brain-a", Relation: "owner", User: "user:principal-a"},
		{Object: "brain:brain-a", Relation: "tenant", User: "tenant:tenant-a"},
		{Object: "evidence:artifact-a", Relation: "brain", User: "brain:brain-a"},
	}
	grants := []bootstrap.GrantSpec{
		{ID: "grant-admit", Action: "artifact.admit", Evidence: bootstrap.EvidenceSpec{Namespace: "evidence", Value: "artifact-a"}, Fence: 7, Nonce: "nonce-admit", ExpiresAt: fixture.grantExpiry.Format(time.RFC3339Nano), RevocationEpoch: 3, Limits: map[string]uint64{"bytes": uint64(len(processArtifactContent)), "frames": frames}},
		{ID: "grant-delete", Action: "artifact.delete", Evidence: bootstrap.EvidenceSpec{Namespace: "evidence", Value: "artifact-a"}, Fence: 7, Nonce: "nonce-delete", ExpiresAt: fixture.grantExpiry.Format(time.RFC3339Nano), RevocationEpoch: 3, Limits: map[string]uint64{}},
		{ID: "grant-read", Action: "artifact.read", Evidence: bootstrap.EvidenceSpec{Namespace: "evidence", Value: "artifact-a"}, Fence: 7, Nonce: "nonce-read", ExpiresAt: fixture.grantExpiry.Format(time.RFC3339Nano), RevocationEpoch: 3, Limits: map[string]uint64{"bytes": 1024}},
	}
	// The Stage 05 approval-descriptor artifacts each carry one admit and one
	// read grant so the factory surface revalidates admissions through the real
	// authorized artifact path, never client bytes.
	for _, artifactID := range processFactoryDescriptorArtifacts() {
		relationships = append(relationships, bootstrap.RelationshipSpec{
			Object: "evidence:" + artifactID, Relation: "brain", User: "brain:brain-a",
		})
		grants = append(grants,
			bootstrap.GrantSpec{
				ID: "grant-factory-admit-" + artifactID, Action: "artifact.admit",
				// Bootstrap grants only admit the evidence namespace; the
				// ArtifactRef itself stays artifact:<id> on the wire.
				Evidence: bootstrap.EvidenceSpec{Namespace: "evidence", Value: artifactID},
				Fence:    7, Nonce: "nonce-factory-admit-" + artifactID,
				ExpiresAt: fixture.grantExpiry.Format(time.RFC3339Nano), RevocationEpoch: 3,
				Limits: map[string]uint64{"bytes": 65536, "frames": 8192},
			},
			bootstrap.GrantSpec{
				ID: "grant-factory-read-" + artifactID, Action: "artifact.read",
				Evidence: bootstrap.EvidenceSpec{Namespace: "evidence", Value: artifactID},
				Fence:    7, Nonce: "nonce-factory-read-" + artifactID,
				ExpiresAt: fixture.grantExpiry.Format(time.RFC3339Nano), RevocationEpoch: 3,
				Limits: map[string]uint64{"bytes": 65536},
			},
		)
	}
	value := bootstrap.BootstrapV1{
		Version: 1, StateRoot: fixture.stateRoot,
		SocketPath:         fixture.socketPath,
		DatabasePath:       filepath.Join(fixture.stateRoot, "authority.sqlite3"),
		ObjectRoot:         filepath.Join(fixture.stateRoot, "objects"),
		ApprovedSourceRoot: fixture.sourceRoot,
		Principal:          "principal-a", Tenant: "tenant-a", Session: "session-a", Brain: "brain-a",
		KeychainService: "ouroboros.process-test", KeyEpoch: 1, KeyReference: "process-test-key",
		// MaxReadBytes is the vault read ceiling, not a grant limit (artifact
		// reads stay grant-bounded at 1024 bytes below). It must cover the
		// Stage 04 conversation payload class: turn payloads hydrate whole
		// through the vault and are bounded by conversation.MaxPayloadBytes
		// (1 MiB), so a smaller ceiling would make history hydration fail
		// closed. 1 MiB is exactly that frozen payload bound.
		MaxConnections: 8, MaxRequests: 64, FrameBytes: frameBytes, MaxReadBytes: 1 << 20,
		RevocationEpoch: 3,
		Relationships:   relationships,
		IssuedGrants:    grants,
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// processFactoryDescriptorArtifacts enumerates the Stage 05 approval
// descriptor artifact identities the process tracer stages through the real
// authorized artifact path before the factory matrix drives.
func processFactoryDescriptorArtifacts() []string {
	return []string{
		"artifact-factory-approval-happy-path",
		"artifact-factory-approval-stale-base",
		"artifact-factory-approval-stale-lease",
		"artifact-factory-approval-duplicate",
		"artifact-factory-approval-duplicate-conflict",
		"artifact-factory-approval-escape",
		"artifact-factory-approval-partial",
		"artifact-factory-approval-failed-gate",
		"artifact-factory-approval-revoke",
		"artifact-factory-approval-rollback",
	}
}

// assertGrantHorizonLive fails with an explicit fixture error when a paused
// run consumed the bounded grant window, so a slow or debugged test never
// surfaces as a spurious daemon denial.
func (fixture *processFixture) assertGrantHorizonLive(t *testing.T) {
	t.Helper()
	if remaining := time.Until(fixture.grantExpiry); remaining < processGrantSafetyMargin {
		t.Fatalf("process fixture grant window exhausted: %s of the bounded %s horizon remains; the run paused past fixture validity", remaining.Round(time.Second), processGrantHorizon)
	}
}

func (fixture *processFixture) request(
	t *testing.T,
	action string,
	idempotency string,
	revocationEpoch uint64,
) *contractsv1.ExecuteAuthorityCommandRequest {
	t.Helper()
	fixture.assertGrantHorizonLive(t)
	now := time.Unix(1_000_000, 0).UTC()
	actor := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
	}
	contentDigest := sha256.Sum256(processArtifactContent)
	artifact := &contractsv1.ArtifactRef{
		ArtifactId: &contractsv1.Identifier{Namespace: "artifact", Value: "artifact-a"},
		TenantId:   &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
		ContentDigest: &contractsv1.Digest{
			Algorithm: "sha256", Hex: hex.EncodeToString(contentDigest[:]),
		},
	}
	grantID := "grant-" + strings.TrimPrefix(action, "artifact.")
	nonce := "nonce-" + strings.TrimPrefix(action, "artifact.")
	request := &contractsv1.ExecuteAuthorityCommandRequest{
		Command: &contractsv1.CommandEnvelope{
			CommandId:   &contractsv1.Identifier{Namespace: "command", Value: idempotency},
			CommandType: action, Actor: actor, SubmittedAt: timestamppb.New(now),
			IdempotencyKey: idempotency,
			PayloadDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)},
			Causal: &contractsv1.CausalContext{
				CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: idempotency},
				CausationId:   &contractsv1.Identifier{Namespace: "cause", Value: idempotency},
				TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: idempotency}, Fence: 7,
			},
		},
		Grant: &contractsv1.CapabilityGrant{
			GrantId: &contractsv1.Identifier{Namespace: "grant", Value: grantID}, Initiator: actor,
			Actions:   []string{action},
			Resources: []*contractsv1.Identifier{{Namespace: "evidence", Value: "artifact-a"}},
			Nonce:     nonce, RevocationEpoch: revocationEpoch,
			ExpiresAt:    timestamppb.New(fixture.grantExpiry),
			PolicyDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: fixture.policyDigest},
			CommandFence: 7,
		},
	}
	switch action {
	case "artifact.admit":
		frames := uint32((len(processArtifactContent) + 7) / 8)
		request.Grant.Limits = []*contractsv1.ResourceLimit{
			{Name: "bytes", Maximum: uint64(len(processArtifactContent))},
			{Name: "frames", Maximum: uint64(frames)},
		}
		request.ArtifactCommand = &contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit{
			ArtifactAdmit: &contractsv1.ArtifactAdmitCommand{
				Artifact: artifact, DeclaredLength: uint64(len(processArtifactContent)), FrameCount: frames,
			},
		}
	case "artifact.read":
		request.Grant.Limits = []*contractsv1.ResourceLimit{{Name: "bytes", Maximum: 1024}}
		request.ArtifactCommand = &contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead{
			ArtifactRead: &contractsv1.ArtifactReadCommand{
				Artifact: artifact, Generation: 1, Length: uint64(len(processArtifactContent)),
			},
		}
	case "artifact.delete":
		request.ArtifactCommand = &contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete{
			ArtifactDelete: &contractsv1.ArtifactDeleteCommand{
				Artifact: artifact, ExpectedGeneration: 1, PurgeAfterTombstone: true,
			},
		}
	default:
		t.Fatalf("unsupported process action %q", action)
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

func (fixture *processFixture) cleanup(t *testing.T) {
	t.Helper()
	for _, path := range []string{fixture.manifestRoot, fixture.stateRoot, fixture.sourceRoot} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("process fixture remained at %s: %v", path, err)
		}
	}
	fixture.cleaned = true
}

func (fixture *processFixture) prepareStage3Source(t *testing.T) string {
	t.Helper()
	root := processRepositoryRoot(t)
	fixture.stage3Manifest = loadProcessStage3DeltaManifest(t, root)
	for index, operation := range fixture.stage3Manifest.Operations {
		if operation.Kind == "add" {
			continue
		}
		writeProcessStage3File(t, fixture.sourceRoot, operation.OldPath, processStage3Contents(t, root, operation, index, "base"))
	}
	for source, destination := range map[string]string{
		"tests/fixtures/stage-03/mixed-p5/go/seed.go":                "src/go/seed.go",
		"tests/fixtures/stage-03/mixed-p5/typescript/seed.ts":        "src/typescript/seed.ts",
		"tests/fixtures/stage-03/mixed-p5/python/seed.py":            "src/python/seed.py",
		"tests/fixtures/stage-03/mixed-p5/rust/seed.rs.fixture":      "src/lib.rs",
		"tests/fixtures/stage-03/mixed-p5/java/Seed.java":            "src/java/Seed.java",
		"tests/fixtures/stage-03/mixed-p5/malformed/unterminated.ts": "src/typescript/unterminated.ts",
	} {
		copyProcessFile(t, filepath.Join(root, source), filepath.Join(fixture.sourceRoot, destination))
	}
	if err := os.WriteFile(filepath.Join(fixture.sourceRoot, "unindexed.txt"), []byte("unindexed source text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("unindexed.txt", filepath.Join(fixture.sourceRoot, "unindexed-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.sourceRoot, "Cargo.toml"), []byte("[package]\nname = \"stage-three-fixture\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	processGit(t, fixture.sourceRoot, "add", "--all")
	processGit(t, fixture.sourceRoot, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid", "commit", "-m", "stage three seed")
	return processGit(t, fixture.sourceRoot, "rev-parse", "HEAD")
}

func (fixture *processFixture) reconcileStage3Source(t *testing.T, firstCommit string) string {
	t.Helper()
	if len(fixture.stage3Manifest.Operations) != 100 {
		t.Fatalf("stage three fixture was not prepared: %d operations", len(fixture.stage3Manifest.Operations))
	}
	root := processRepositoryRoot(t)
	for index, operation := range fixture.stage3Manifest.Operations {
		switch operation.Kind {
		case "add":
			writeProcessStage3File(t, fixture.sourceRoot, operation.NewPath, processStage3Contents(t, root, operation, index, "added"))
		case "modify":
			writeProcessStage3File(t, fixture.sourceRoot, operation.OldPath, processStage3Contents(t, root, operation, index, "modified"))
		case "rename":
			oldPath := filepath.Join(fixture.sourceRoot, filepath.FromSlash(operation.OldPath))
			newPath := filepath.Join(fixture.sourceRoot, filepath.FromSlash(operation.NewPath))
			if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatal(err)
			}
		case "delete":
			if err := os.Remove(filepath.Join(fixture.sourceRoot, filepath.FromSlash(operation.OldPath))); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported stage three operation: %#v", operation)
		}
	}
	processGit(t, fixture.sourceRoot, "add", "--all")
	processGit(t, fixture.sourceRoot, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid", "commit", "-m", "stage three frozen delta")
	secondCommit := processGit(t, fixture.sourceRoot, "rev-parse", "HEAD")
	assertProcessStage3Delta(t, fixture.sourceRoot, firstCommit, secondCommit, fixture.stage3Manifest)
	return secondCommit
}

func loadProcessStage3DeltaManifest(t *testing.T, root string) processStage3DeltaManifest {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(root, "tests", "fixtures", "stage-03", "mixed-p5", "delta-manifest.json"))
	if err != nil {
		t.Fatalf("read stage three delta manifest: %v", err)
	}
	var manifest processStage3DeltaManifest
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode stage three delta manifest: %v", err)
	}
	if manifest.SchemaVersion != "ouroboros.stage03.mixed-p5-delta.v1" || len(manifest.Operations) != 100 {
		t.Fatalf("unexpected stage three delta manifest: version=%q operations=%d", manifest.SchemaVersion, len(manifest.Operations))
	}
	return manifest
}

func processStage3Contents(
	t *testing.T,
	root string,
	operation processStage3DeltaOperation,
	index int,
	phase string,
) string {
	t.Helper()
	seedPath := map[string]string{
		"go": "go/seed.go", "typescript": "typescript/seed.ts", "python": "python/seed.py",
		"rust": "rust/seed.rs.fixture", "java": "java/Seed.java",
	}[operation.Language]
	if operation.Malformed {
		seedPath = "malformed/unterminated.ts"
	}
	if seedPath == "" {
		t.Fatalf("unsupported stage three language: %#v", operation)
	}
	encoded, err := os.ReadFile(filepath.Join(root, "tests", "fixtures", "stage-03", "mixed-p5", seedPath))
	if err != nil {
		t.Fatalf("read stage three seed %q: %v", seedPath, err)
	}
	path := operation.OldPath
	if path == "" {
		path = operation.NewPath
	}
	if operation.Language == "python" {
		contents := strings.Replace(
			string(encoded),
			"Return the cross-language fixture marker.",
			fmt.Sprintf("Return the cross-language fixture marker for %s %03d.", phase, index),
			1,
		)
		if contents == string(encoded) {
			t.Fatalf("stage three Python seed marker was unavailable")
		}
		return contents
	}
	comment := "//"
	contents := fmt.Sprintf("%s\n%s fixture=%s phase=%s index=%03d\n", encoded, comment, path, phase, index)
	if operation.Language == "rust" {
		return contents + fmt.Sprintf("\npub fn fixture_reference_%03d() -> &'static str {\n    anchor()\n}\n", index)
	}
	return contents
}

func writeProcessStage3File(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertProcessStage3Delta(
	t *testing.T,
	repository, firstCommit, secondCommit string,
	manifest processStage3DeltaManifest,
) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(processGit(t, repository, "diff", "--name-status", "-M100%", firstCommit, secondCommit)), "\n")
	if len(lines) != len(manifest.Operations) {
		t.Fatalf("frozen stage three delta records = %d, want %d", len(lines), len(manifest.Operations))
	}
	actual := make([]string, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "A":
			actual = append(actual, processStage3OperationKey(processStage3DeltaOperation{Kind: "add", NewPath: parts[1]}))
		case "M":
			actual = append(actual, processStage3OperationKey(processStage3DeltaOperation{Kind: "modify", OldPath: parts[1]}))
		case "D":
			actual = append(actual, processStage3OperationKey(processStage3DeltaOperation{Kind: "delete", OldPath: parts[1]}))
		case "R100":
			actual = append(actual, processStage3OperationKey(processStage3DeltaOperation{Kind: "rename", OldPath: parts[1], NewPath: parts[2]}))
		default:
			t.Fatalf("unexpected frozen stage three delta record %q", line)
		}
	}
	want := make([]string, 0, len(manifest.Operations))
	for _, operation := range manifest.Operations {
		want = append(want, processStage3OperationKey(operation))
	}
	sort.Strings(actual)
	sort.Strings(want)
	if strings.Join(actual, "\n") != strings.Join(want, "\n") {
		t.Fatal("frozen stage three delta diverged from the checked-in manifest")
	}
}

func processStage3OperationKey(operation processStage3DeltaOperation) string {
	return strings.Join([]string{operation.Kind, operation.OldPath, operation.NewPath}, "\x00")
}

type processTUI struct {
	bun     string
	root    string
	workdir string
	home    string
	cache   string
	tmp     string
	cleaned bool
}

func installProcessTUI(t *testing.T, bun string) *processTUI {
	t.Helper()
	root := secureProcessRoot(t, ".ouroboros-process-tui-")
	workspace := filepath.Join(root, "workspace")
	for _, directory := range []string{workspace, filepath.Join(root, "home"), filepath.Join(root, "cache"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tui := &processTUI{
		bun: bun, root: root, workdir: filepath.Join(workspace, "apps", "tui"),
		home: filepath.Join(root, "home"), cache: filepath.Join(root, "cache"), tmp: filepath.Join(root, "tmp"),
	}
	t.Cleanup(func() {
		if !tui.cleaned {
			_ = os.RemoveAll(tui.root)
		}
	})
	copyProcessTUISources(t, workspace)
	installProcessModuleRoot(t, tui.workdir)
	// Generated contracts live at the repository package boundary, above the
	// TUI workspace. Expose the same pinned module root at that ancestor so Bun
	// resolves generated runtime imports from one materialized tree.
	if err := os.Symlink(
		filepath.Join("apps", "tui", "node_modules"),
		filepath.Join(workspace, "node_modules"),
	); err != nil {
		t.Fatalf("link isolated TUI dependencies: %v", err)
	}
	return tui
}

// processProtobufTarballRunfile is the Bazel-fetched, lockfile-pinned registry
// tarball re-exposed by //apps/tui:bufbuild_protobuf_tarball.
const processProtobufTarballRunfile = "apps/tui/bufbuild-protobuf-2.7.0.tgz"

// installProcessModuleRoot materializes the pinned runtime dependency declared
// by apps/tui/bun.lock without any installer or registry access: the
// Bazel-fetched tarball (integrity-pinned in MODULE.bazel) is re-verified
// against the copied lockfile and extracted into the isolated workspace.
func installProcessModuleRoot(t *testing.T, workdir string) {
	t.Helper()
	tarball := processProtobufTarball(t)
	verifyProcessTarballAgainstLock(t, tarball, filepath.Join(workdir, "bun.lock"))
	extractProcessTarball(t, tarball, filepath.Join(workdir, "node_modules", "@bufbuild", "protobuf"))
	assertProcessModuleRoot(t, filepath.Join(workdir, "node_modules", "@bufbuild", "protobuf"))
}

func processProtobufTarball(t *testing.T) string {
	t.Helper()
	root := processRepositoryRoot(t)
	candidates := []string{filepath.Join(root, filepath.FromSlash(processProtobufTarballRunfile))}
	if os.Getenv("TEST_SRCDIR") == "" {
		// Direct `go test` runs have no runfiles tree; use the built output.
		candidates = append(candidates, filepath.Join(root, "bazel-bin", filepath.FromSlash(processProtobufTarballRunfile)))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	t.Fatalf("pinned protobuf tarball is unavailable; build //apps/tui:bufbuild-protobuf-2.7.0.tgz first")
	return ""
}

// processLockIntegrity structurally parses the Bun lockfile and returns the
// sha512 integrity of one entry in its top-level "packages" map. Isolating
// the map by parsing (not by a greedy text match) guarantees a same-named
// key anywhere else in the file can never satisfy the pin; every step fails
// closed when the format drifts.
func processLockIntegrity(encoded []byte, name string) (string, bool) {
	var lock struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(stripProcessLockTrailingCommas(encoded), &lock); err != nil {
		return "", false
	}
	entry, ok := lock.Packages[name]
	if !ok {
		return "", false
	}
	var fields []json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		return "", false
	}
	for _, field := range fields {
		var text string
		if err := json.Unmarshal(field, &text); err != nil {
			continue
		}
		if integrity, ok := strings.CutPrefix(text, "sha512-"); ok {
			return integrity, true
		}
	}
	return "", false
}

// stripProcessLockTrailingCommas removes the JSONC trailing commas Bun writes
// so the lockfile parses with the strict standard-library decoder. Commas
// inside string literals are preserved.
func stripProcessLockTrailingCommas(encoded []byte) []byte {
	stripped := make([]byte, 0, len(encoded))
	inString := false
	escaped := false
	for index := 0; index < len(encoded); index++ {
		character := encoded[index]
		if inString {
			stripped = append(stripped, character)
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		if character == '"' {
			inString = true
			stripped = append(stripped, character)
			continue
		}
		if character == ',' {
			next := index + 1
			for next < len(encoded) && (encoded[next] == ' ' || encoded[next] == '\t' || encoded[next] == '\n' || encoded[next] == '\r') {
				next++
			}
			if next < len(encoded) && (encoded[next] == '}' || encoded[next] == ']') {
				continue
			}
		}
		stripped = append(stripped, character)
	}
	return stripped
}

func verifyProcessTarballAgainstLock(t *testing.T, tarball, lockfile string) {
	t.Helper()
	encoded, err := os.ReadFile(lockfile)
	if err != nil {
		t.Fatalf("read copied Bun lockfile: %v", err)
	}
	integrity, ok := processLockIntegrity(encoded, "@bufbuild/protobuf")
	if !ok {
		t.Fatal("copied Bun lockfile omitted the @bufbuild/protobuf pin")
	}
	info, err := os.Stat(tarball)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProcessSourceBytes {
		t.Fatalf("unsafe protobuf tarball %q: %v", tarball, err)
	}
	payload, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("read protobuf tarball: %v", err)
	}
	digest := sha512.Sum512(payload)
	if base64.StdEncoding.EncodeToString(digest[:]) != integrity {
		t.Fatal("pinned protobuf tarball diverged from apps/tui/bun.lock")
	}
}

func TestProcessLockIntegrityIsolation(t *testing.T) {
	t.Helper()
	const pin = "qn6tAIZEw5i/wiESBF4nQxZkl86aY4KoO0IkUa2Lh+rya64oTOdJQFlZuMwI1Qz9VBJQrQC4QlSA2DNek5gCOA=="
	const decoy = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	lockfile := func(packagesEntry string) []byte {
		return []byte(`{
  "lockfileVersion": 1,
  "workspaces": {
    "": {
      "name": "fixture",
      "dependencies": {
        "@bufbuild/protobuf": ["@bufbuild/protobuf@9.9.9", "", {}, "sha512-` + decoy + `"],
      },
    },
  },
  "packages": {` + packagesEntry + ` },
}`)
	}
	t.Run("pin comes from the packages section despite a same-named decoy", func(t *testing.T) {
		integrity, ok := processLockIntegrity(lockfile(`
    "@bufbuild/protobuf": ["@bufbuild/protobuf@2.7.0", "", {}, "sha512-`+pin+`"],`), "@bufbuild/protobuf")
		if !ok || integrity != pin {
			t.Fatalf("integrity = %q, %v; want the packages-section pin", integrity, ok)
		}
	})
	t.Run("decoy outside the packages section never satisfies the pin", func(t *testing.T) {
		if integrity, ok := processLockIntegrity(lockfile(`
    "other/package": ["other@1.0.0", "", {}, "sha512-`+decoy+`"],`), "@bufbuild/protobuf"); ok {
			t.Fatalf("decoy satisfied the pin: %q", integrity)
		}
	})
	t.Run("malformed lockfile fails closed", func(t *testing.T) {
		if integrity, ok := processLockIntegrity([]byte(`{"packages": {`), "@bufbuild/protobuf"); ok {
			t.Fatalf("malformed lockfile satisfied the pin: %q", integrity)
		}
	})
}

// extractProcessTarball unpacks the npm tarball under destination, accepting
// only `package/`-prefixed directories and bounded regular files. The gzip
// reader is closed exactly once on every exit path so its trailer finalizes;
// the success path checks the close error.
func extractProcessTarball(t *testing.T, tarball, destination string) {
	t.Helper()
	file, err := os.Open(tarball)
	if err != nil {
		t.Fatalf("open protobuf tarball: %v", err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(io.LimitReader(file, maxProcessSourceBytes))
	if err != nil {
		t.Fatalf("decode protobuf tarball: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = compressed.Close()
		}
	}()
	reader := tar.NewReader(compressed)
	entries := 0
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read protobuf tarball: %v", err)
		}
		name, ok := strings.CutPrefix(header.Name, "package/")
		if !ok {
			t.Fatalf("unsafe protobuf tarball entry %q", header.Name)
		}
		if name == "" {
			// The package root directory entry carries no content.
			continue
		}
		if !filepath.IsLocal(name) {
			t.Fatalf("unsafe protobuf tarball entry %q", header.Name)
		}
		entries++
		if entries > 10_000 {
			t.Fatal("protobuf tarball exceeded the entry bound")
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxProcessSourceBytes {
				t.Fatalf("unsafe protobuf tarball entry size %q", header.Name)
			}
			total += header.Size
			if total > maxProcessSourceBytes {
				t.Fatal("protobuf tarball exceeded the size bound")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, copyErr := io.Copy(output, io.LimitReader(reader, header.Size+1))
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsafe protobuf tarball entry type %q", header.Name)
		}
	}
	if err := compressed.Close(); err != nil {
		t.Fatalf("finalize protobuf tarball: %v", err)
	}
	closed = true
}

func assertProcessModuleRoot(t *testing.T, root string) {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatalf("materialized module root omitted package.json: %v", err)
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &manifest); err != nil || manifest.Name != "@bufbuild/protobuf" {
		t.Fatalf("materialized module root is not @bufbuild/protobuf: %v", err)
	}
}

type tuiResult struct {
	output string
	err    error
}

func (tui *processTUI) Run(t *testing.T, timeout time.Duration, arguments ...string) tuiResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, tui.bun, arguments...)
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
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("Bun command timed out: %v", arguments)
	}
	return tuiResult{output: output.String(), err: err}
}

func (tui *processTUI) cleanup(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(tui.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tui.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated TUI remained: %v", err)
	}
	tui.cleaned = true
}

func assertTUISucceeds(t *testing.T, tui *processTUI, socket string, arguments []string, expected ...string) {
	t.Helper()
	// --no-install turns Bun's default auto-install into a hard failure so the
	// offline module root is enforced rather than incidental.
	full := append([]string{"run", "--no-install", "packages/local-authority/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket, "--timeout-ms", "5000")
	result := tui.Run(t, 10*time.Second, full...)
	if result.err != nil {
		t.Fatalf("TUI request failed: %v: %s", result.err, result.output)
	}
	for _, fragment := range expected {
		if !strings.Contains(result.output, fragment) {
			t.Fatalf("TUI output %q omitted %q", result.output, fragment)
		}
	}
}

func assertTUIDenied(t *testing.T, tui *processTUI, socket string, arguments []string) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/local-authority/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket, "--timeout-ms", "5000")
	result := tui.Run(t, 10*time.Second, full...)
	if result.err != nil {
		t.Fatalf("receipt-bearing TUI denial failed: %v: %q", result.err, result.output)
	}
	for _, expected := range []string{
		"[DENIED] Request denied",
		"[request-denied]",
		"(receipt receipt:",
	} {
		if !strings.Contains(result.output, expected) {
			t.Fatalf("TUI denial %q omitted %q", result.output, expected)
		}
	}
	for _, forbidden := range []string{
		"ERROR", "request-timeout", "socket-unsafe", "response-invalid",
		"artifact-a", "tenant-a", "stage-02",
	} {
		if strings.Contains(result.output, forbidden) {
			t.Fatalf("TUI denial %q disclosed forbidden value %q", result.output, forbidden)
		}
	}
}

func runIngestionTUI(t *testing.T, tui *processTUI, socket string, arguments ...string) string {
	t.Helper()
	output, err := runIngestionTUIResult(t, tui, socket, arguments...)
	if err != nil {
		t.Fatalf("ingestion TUI request failed: %v: %s", err, output)
	}
	return output
}

func runIngestionTUIResult(t *testing.T, tui *processTUI, socket string, arguments ...string) (string, error) {
	t.Helper()
	full := append([]string{"run", "--no-install", "packages/ingestion/src/cli.ts"}, arguments...)
	full = append(full, "--socket", socket, "--timeout-ms", "5000")
	result := tui.Run(t, 10*time.Second, full...)
	return result.output, result.err
}

func processIngestionIdentifiers(t *testing.T, output string) (string, string) {
	t.Helper()
	matched := regexp.MustCompile(`(?s)source=source:([0-9a-f]{64}).*generation=generation:([0-9a-f]{64})`).FindStringSubmatch(output)
	if len(matched) != 3 {
		t.Fatalf("ingestion output did not expose source and generation: %q", output)
	}
	return "source:" + matched[1], "generation:" + matched[2]
}

func requirePinnedBun(t *testing.T) string {
	t.Helper()
	bun := os.Getenv("OUROBOROS_BUN_BIN")
	if !filepath.IsAbs(bun) {
		t.Fatal("OUROBOROS_BUN_BIN must name the pinned Bun executable")
	}
	output, err := exec.Command(bun, "--version").Output()
	if err != nil || strings.TrimSpace(string(output)) != "1.3.14" {
		t.Fatalf("Bun 1.3.14 is required: %v", err)
	}
	return bun
}

func secureProcessRoot(t *testing.T, pattern string) string {
	t.Helper()
	current, err := user.Current()
	if err != nil || !filepath.IsAbs(current.HomeDir) {
		t.Fatalf("resolve current home: %v", err)
	}
	root, err := os.MkdirTemp(current.HomeDir, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	return root
}

func copyProcessTUISources(t *testing.T, destination string) {
	t.Helper()
	root := processRepositoryRoot(t)
	copyDeclaredTUITree(t, root, destination)
	patterns := []string{
		"packages/contracts/package.json",
		"packages/contracts/gen/ts/buf/validate/validate_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/brain_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/common_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/evidence_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/factory_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/ingestion_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/local_authority_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/meetings_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/query_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/security_pb.ts",
	}
	// Stage 08 connector and Stage 11 multimodal bindings are optional so earlier
	// stage process tracers keep working when their data deps omit those files.
	optionalPatterns := []string{
		"packages/contracts/gen/ts/ouroboros/contracts/v1/connectors_pb.ts",
		"packages/contracts/gen/ts/ouroboros/contracts/v1/multimodal_pb.ts",
	}
	var sources []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(matches) == 0 {
			t.Fatalf("resolve TUI source %q: %v", pattern, err)
		}
		sources = append(sources, matches...)
	}
	for _, pattern := range optionalPatterns {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("resolve optional TUI source %q: %v", pattern, err)
		}
		sources = append(sources, matches...)
	}
	sort.Strings(sources)
	for _, source := range sources {
		relative, err := filepath.Rel(root, source)
		if err != nil || strings.HasPrefix(relative, "..") {
			t.Fatalf("unsafe TUI source %q", source)
		}
		copyProcessFile(t, source, filepath.Join(destination, relative))
	}
}

func copyDeclaredTUITree(t *testing.T, root, destination string) {
	t.Helper()
	tuiRoot := filepath.Join(root, "apps", "tui")
	err := filepath.WalkDir(tuiRoot, func(source string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if source != tuiRoot && excludedProcessTUIDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !declaredTUIFile(entry.Name()) {
			return nil
		}
		relative, err := filepath.Rel(root, source)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("unsafe TUI source %q", source)
		}
		copyProcessFile(t, source, filepath.Join(destination, relative))
		return nil
	})
	if err != nil {
		t.Fatalf("copy declared TUI source tree: %v", err)
	}
}

func excludedProcessTUIDirectory(name string) bool {
	switch name {
	case ".cache", ".tmp", "coverage", "node_modules":
		return true
	default:
		return false
	}
}

func declaredTUIFile(name string) bool {
	switch name {
	case "BUILD.bazel", "bun.lock":
		return true
	}
	switch filepath.Ext(name) {
	case ".json", ".md", ".sh", ".toml", ".ts":
		return true
	default:
		return false
	}
}

func processRepositoryRoot(t *testing.T) string {
	t.Helper()
	if runfiles := os.Getenv("TEST_SRCDIR"); runfiles != "" {
		root := filepath.Join(runfiles, os.Getenv("TEST_WORKSPACE"))
		if _, err := os.Stat(filepath.Join(root, "apps", "tui", "package.json")); err == nil {
			return root
		}
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve process test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "apps", "tui", "package.json")); err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func copyProcessFile(t *testing.T, source, destination string) {
	t.Helper()
	// Declared Bazel runfiles may be symlinks into the execution root. Stat
	// follows only that trusted declaration while the private copy is regular.
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProcessSourceBytes {
		t.Fatalf("unsafe TUI source %q: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, io.LimitReader(input, maxProcessSourceBytes))
	closeOutputErr := output.Close()
	closeInputErr := input.Close()
	if err := errors.Join(copyErr, closeOutputErr, closeInputErr); err != nil {
		t.Fatal(err)
	}
}
