package localauthority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestDurableRuntimePreparesAdmitsReadsAndRecoversAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config, keys := durableTestConfig(root)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("durable encrypted content"))
	if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatalf("prepare artifact: %v", err)
	}
	admit := testRequest(identity, artifact, "artifact.admit", "durable-admit", "durable-admit")
	if _, err := runtime.Execute(ctx, admit); err != nil {
		t.Fatalf("admit artifact: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}

	runtime, err = OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatalf("restart durable runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	read := testRequest(identity, artifact, "artifact.read", "durable-read", "durable-read")
	read.Length = uint64(len(content))
	result, err := runtime.Execute(ctx, read)
	if err != nil || !bytes.Equal(result.Bytes, content) {
		t.Fatalf("read after restart = %q, %v", result.Bytes, err)
	}
	clear(result.Bytes)
	assertObjectsEncrypted(t, config.ObjectRoot, content)
}

func TestDurableRuntimeRejectsChangedMaterialForCommittedReference(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := db.QueryRow(`SELECT key_reference FROM key_epochs
		WHERE tenant_id=? AND key_epoch=?`, config.Tenant.Value, config.CurrentKeyReference.Epoch).
		Scan(&persisted); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(persisted, keyCommitmentPrefix) ||
		persisted == config.CurrentKeyReference.KeyID.Value {
		t.Fatalf("persisted key binding = %q", persisted)
	}

	changed := durableTestResolver(config, 8)
	if _, err := OpenDurable(ctx, config, changed); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("changed key material restart = %v", err)
	}
}

func TestDurableRuntimeRejectsLiveMaterialReplacementForCommittedEpoch(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	first, firstContent := testArtifact([]byte("first committed material"))
	if err := runtime.PrepareArtifact(ctx, first, bytes.NewReader(firstContent)); err != nil {
		t.Fatalf("prepare before replacement: %v", err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	admit := testRequest(identity, first, "artifact.admit", "commitment-admit", "commitment-admit")
	if _, err := runtime.Execute(ctx, admit); err != nil {
		t.Fatalf("admit before replacement: %v", err)
	}
	replacement := bytes.Repeat([]byte{8}, keyring.RootKeyBytes)
	keys.Add(config.Tenant, keyring.Material{
		Reference: config.CurrentKeyReference, RootKey: replacement,
	}, keyring.Current)
	clear(replacement)

	next, nextContent := testArtifact([]byte("replacement must not share epoch"))
	if err := runtime.PrepareArtifact(ctx, next, bytes.NewReader(nextContent)); !errors.Is(err, ErrDenied) {
		t.Fatalf("prepare after live material replacement = %v", err)
	}
	read := testRequest(identity, first, "artifact.read", "commitment-read", "commitment-read")
	read.Length = uint64(len(firstContent))
	if _, err := runtime.Execute(ctx, read); !errors.Is(err, ErrDenied) {
		t.Fatalf("read after live material replacement = %v", err)
	}
}

func TestDurableRuntimeResolvesKeyBeforeInstallingReference(t *testing.T) {
	ctx := context.Background()
	failed, _ := durableTestConfig(t.TempDir())
	failed.CurrentKeyReference.KeyID.Value = "missing-first-boot-key"
	if _, err := OpenDurable(ctx, failed, keyring.NewMemory()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing first-boot key = %v", err)
	}
	db, err := sql.Open("sqlite", failed.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM key_epochs").Scan(&rows); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("failed first boot installed %d key rows", rows)
	}
	corrected := failed
	corrected.CurrentKeyReference.KeyID.Value = "corrected-first-boot-key"
	runtime, err := OpenDurable(ctx, corrected, durableTestResolver(corrected, 7))
	if err != nil {
		t.Fatalf("corrected first boot = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableOpenRejectsSecondOwnerAndReleasesPartialOpen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config, keys := durableTestConfig(root)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDurable(ctx, config, keys); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second owner = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	invalidObjects := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(invalidObjects, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ObjectRoot = invalidObjects
	if _, err := OpenDurable(ctx, config, keys); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial open = %v", err)
	}
	if err := os.Remove(invalidObjects); err != nil {
		t.Fatal(err)
	}
	config.ObjectRoot = filepath.Join(root, "recovered-objects")
	recovered, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatalf("owner lock leaked after partial open: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareArtifactValidatesAndDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	artifact, content := testArtifact([]byte("trusted staging only"))
	if err := runtime.PrepareArtifact(ctx, artifact, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil reader = %v", err)
	}
	wrongTenant := artifact
	wrongTenant.Tenant.Value = "other"
	if err := runtime.PrepareArtifact(ctx, wrongTenant, bytes.NewReader(content)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-tenant staging = %v", err)
	}
	wrongEpoch := artifact
	wrongEpoch.KeyEpoch++
	if err := runtime.PrepareArtifact(ctx, wrongEpoch, bytes.NewReader(content)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-current epoch staging = %v", err)
	}
	changed := append([]byte(nil), content...)
	changed[0] ^= 0xff
	if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(changed)); !errors.Is(err, ErrDenied) {
		t.Fatalf("digest mismatch = %v", err)
	}
	if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	read := testRequest(identity, artifact, "artifact.read", "before-admit", "before-admit")
	read.Length = uint64(len(content))
	rejected, err := runtime.Execute(ctx, read)
	if err != nil || rejected.Receipt.Status != "rejected" ||
		rejected.Receipt.ReasonCode != "not_found_or_denied" || rejected.Authorization.Allowed ||
		rejected.Authorization.ReasonCode != "not_found_or_denied" || len(rejected.Bytes) != 0 {
		t.Fatalf("staged artifact rejection = %#v, %v", rejected, err)
	}
}

func TestRuntimeCloseHandlesNilAndPartialValues(t *testing.T) {
	var nilRuntime *Runtime
	if err := nilRuntime.Close(); err != nil {
		t.Fatalf("nil close = %v", err)
	}
	partial := &Runtime{}
	if err := partial.Close(); err != nil {
		t.Fatalf("partial close = %v", err)
	}
	if err := partial.Close(); err != nil {
		t.Fatalf("partial close retry = %v", err)
	}
}

func TestRuntimeOperationsDenyAfterClose(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("closed runtime"))
	request := testRequest(identity, artifact, "artifact.admit", "closed-admit", "closed-admit")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.OpenSession(ctx, identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("OpenSession after Close = %v", err)
	}
	if _, err := runtime.ReadStatus(ctx, identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("ReadStatus after Close = %v", err)
	}
	if _, err := runtime.Execute(ctx, request); !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute after Close = %v", err)
	}
	if err := runtime.PrepareArtifact(ctx, artifact, bytes.NewReader(content)); !errors.Is(err, ErrDenied) {
		t.Fatalf("PrepareArtifact after Close = %v", err)
	}
}

func TestRuntimeCloseAndReadStatusAreRaceSafe(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, 65)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for range 8 {
				_, statusErr := runtime.ReadStatus(ctx, identity)
				if statusErr != nil && !errors.Is(statusErr, ErrDenied) {
					errorsSeen <- statusErr
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		if closeErr := runtime.Close(); closeErr != nil {
			errorsSeen <- closeErr
		}
	}()
	close(start)
	wait.Wait()
	close(errorsSeen)
	for operationErr := range errorsSeen {
		t.Fatalf("concurrent lifecycle error = %v", operationErr)
	}
	if _, err := runtime.ReadStatus(ctx, identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("final status after Close = %v", err)
	}
}

func TestLegacyRuntimeCannotPrepareArtifactWithoutDurableScope(t *testing.T) {
	root := t.TempDir()
	runtime := openTestRuntime(
		t,
		filepath.Join(root, "authority.db"),
		newTestDependencies(filepath.Join(root, "objects")),
	)
	artifact, content := testArtifact([]byte("legacy staging denied"))
	if err := runtime.PrepareArtifact(context.Background(), artifact, bytes.NewReader(content)); !errors.Is(err, ErrDenied) {
		t.Fatalf("legacy PrepareArtifact = %v", err)
	}
}

func durableTestConfig(root string) (DurableConfig, *keyring.Memory) {
	tenant := Identifier{Namespace: "tenant", Value: "t"}
	reference := shared.KeyReference{
		Root:  Identifier{Namespace: "key-root", Value: "t"},
		KeyID: Identifier{Namespace: "key", Value: "durable-test-key"}, Epoch: 1,
	}
	config := DurableConfig{
		DatabasePath: filepath.Join(root, "authority.db"), ObjectRoot: filepath.Join(root, "objects"),
		Tenant: tenant, CurrentKeyReference: reference,
		Brain:               Identifier{Namespace: "brain", Value: "b"},
		ConfigurationDigest: digest([]byte("durable-test-config")),
		Clock:               fixedClock{millis: 1_000_000},
		Storage:             StorageOptions{FrameBytes: 4, MaxReadBytes: 1024},
	}
	return config, durableTestResolver(config, 7)
}

func durableTestResolver(config DurableConfig, fill byte) *keyring.Memory {
	keys := keyring.NewMemory()
	keyBytes := bytes.Repeat([]byte{fill}, keyring.RootKeyBytes)
	keys.Add(config.Tenant, keyring.Material{
		Reference: config.CurrentKeyReference, RootKey: keyBytes,
	}, keyring.Current)
	clear(keyBytes)
	return keys
}
