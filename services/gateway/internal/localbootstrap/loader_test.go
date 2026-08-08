package localbootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)

func TestLoadReturnsNormalizedImmutableConfiguration(t *testing.T) {
	manifest := validBootstrap(t)
	path, payload, digest := writeBootstrap(t, manifest)

	config, err := Load(Options{ManifestPath: path, ExpectedSHA256: digest, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.ConfigurationDigest() != digest || config.StateRoot() != manifest.StateRoot ||
		config.SocketPath() != manifest.SocketPath ||
		config.DatabasePath() != manifest.DatabasePath || config.ObjectRoot() != manifest.ObjectRoot ||
		config.ApprovedSourceRoot() != manifest.ApprovedSourceRoot ||
		config.Principal() != manifest.Principal || config.Tenant() != manifest.Tenant ||
		config.Session() != manifest.Session || config.Brain() != manifest.Brain ||
		config.KeychainService() != manifest.KeychainService || config.KeyEpoch() != manifest.KeyEpoch ||
		config.KeyReference() != manifest.KeyReference || config.MaxConnections() != manifest.MaxConnections ||
		config.MaxRequests() != manifest.MaxRequests || config.FrameBytes() != manifest.FrameBytes ||
		config.MaxReadBytes() != manifest.MaxReadBytes || config.RevocationEpoch() != manifest.RevocationEpoch {
		t.Fatal("Load() did not preserve normalized scalar fields")
	}
	actualPayloadDigest := sha256.Sum256(payload)
	if config.ConfigurationDigest() != hex.EncodeToString(actualPayloadDigest[:]) {
		t.Fatal("ConfigurationDigest() does not bind exact accepted bytes")
	}
	relationships := config.Relationships()
	if len(relationships) != 3 || relationships[0].Object != "brain:brain-a" ||
		relationships[1].Object != "brain:brain-a" || relationships[2].Object != "evidence:evidence-a" {
		t.Fatalf("Relationships() not canonical: %#v", relationships)
	}
	grants := config.IssuedGrants()
	if len(grants) != 3 || grants[0].ID != "grant-admit" || grants[1].ID != "grant-delete" ||
		grants[2].ID != "grant-read" {
		t.Fatalf("IssuedGrants() not canonical: %#v", grants)
	}
	relationships[0].Object = "mutated"
	grants[0].Limits["bytes"] = 1
	if config.Relationships()[0].Object == "mutated" || config.IssuedGrants()[0].Limits["bytes"] == 1 {
		t.Fatal("Config accessors exposed mutable internal collections")
	}
}

func TestLoadDigestsAreDeterministicAcrossReloadAndRelationshipOrder(t *testing.T) {
	manifest := validBootstrap(t)
	path, _, digest := writeBootstrap(t, manifest)
	first := mustLoad(t, path, digest)
	second := mustLoad(t, path, digest)
	if first.ConfigurationDigest() != second.ConfigurationDigest() || first.PolicyDigest() != second.PolicyDigest() {
		t.Fatal("reloading identical bytes changed a digest")
	}
	const expectedPolicyDigest = "c98e0984d5317e0eca1be20ea7917dcb8f46b01828fa550ca6d0934320df1876"
	if first.PolicyDigest() != expectedPolicyDigest {
		t.Fatalf("PolicyDigest() = %q, want golden %q", first.PolicyDigest(), expectedPolicyDigest)
	}

	manifest.Relationships[0], manifest.Relationships[2] = manifest.Relationships[2], manifest.Relationships[0]
	reorderedPath, _, reorderedDigest := writeBootstrap(t, manifest)
	reordered := mustLoad(t, reorderedPath, reorderedDigest)
	if first.ConfigurationDigest() == reordered.ConfigurationDigest() {
		t.Fatal("configuration digest did not bind exact JSON order")
	}
	if first.PolicyDigest() != reordered.PolicyDigest() {
		t.Fatal("policy digest depends on relationship input order")
	}
}

func TestLoadRejectsInvalidDigestPins(t *testing.T) {
	path, _, digest := writeBootstrap(t, validBootstrap(t))
	tests := []struct {
		name string
		pin  string
		want error
	}{
		{name: "missing", pin: "", want: ErrInvalidOptions},
		{name: "malformed", pin: "abc", want: ErrInvalidOptions},
		{name: "uppercase", pin: strings.ToUpper(digest), want: ErrInvalidOptions},
		{name: "wrong", pin: strings.Repeat("0", 64), want: ErrDigestMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(Options{ManifestPath: path, ExpectedSHA256: test.pin, Now: func() time.Time { return fixedNow }})
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLoadRejectsStrictJSONViolations(t *testing.T) {
	_, payload, _ := writeBootstrap(t, validBootstrap(t))
	unknown := append([]byte(`{"unknown":true,`), payload[1:]...)
	trailing := append(append([]byte(nil), payload...), []byte(` {}`)...)
	invalidUTF8 := bytes.Replace(payload, []byte("principal-a"), []byte{'p', 0xff}, 1)
	for name, candidate := range map[string][]byte{
		"unknown": unknown, "trailing": trailing, "invalid UTF-8": invalidUTF8,
	} {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, candidate, 0o600)
			_, err := Load(Options{ManifestPath: path, ExpectedSHA256: digest, Now: func() time.Time { return fixedNow }})
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Load() error = %v, want %v", err, ErrInvalidManifest)
			}
		})
	}
}

func TestLoadRejectsUnsafeFilesystemEntries(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		path, _, digest := writeBootstrap(t, validBootstrap(t))
		alias := filepath.Join(filepath.Dir(path), "alias.json")
		if err := os.Symlink(path, alias); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
		assertLoadError(t, alias, digest, ErrUnsafeManifest)
	})
	t.Run("file mode", func(t *testing.T) {
		path, _, digest := writeBootstrap(t, validBootstrap(t))
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		assertLoadError(t, path, digest, ErrUnsafeManifest)
	})
	t.Run("parent mode", func(t *testing.T) {
		path, _, digest := writeBootstrap(t, validBootstrap(t))
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		assertLoadError(t, path, digest, ErrUnsafeManifest)
	})
}

func TestLoadRejectsEmptyAndOversizedDocuments(t *testing.T) {
	for name, payload := range map[string][]byte{
		"empty":    {},
		"oversize": []byte(strings.Repeat("x", MaxManifestBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, payload, 0o600)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func mustLoad(t *testing.T, path, digest string) *Config {
	t.Helper()
	config, err := Load(Options{ManifestPath: path, ExpectedSHA256: digest, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return config
}

func assertLoadError(t *testing.T, path, digest string, want error) {
	t.Helper()
	_, err := Load(Options{ManifestPath: path, ExpectedSHA256: digest, Now: func() time.Time { return fixedNow }})
	if !errors.Is(err, want) {
		t.Fatalf("Load() error = %v, want %v", err, want)
	}
}

func writeBootstrap(t *testing.T, manifest BootstrapV1) (string, []byte, string) {
	t.Helper()
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	path, digest := writeRaw(t, payload, 0o600)
	return path, payload, digest
}

func writeRaw(t *testing.T, payload []byte, mode os.FileMode) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(secureTestDir(t))
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	parent := filepath.Join(root, "manifest")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	path := filepath.Join(parent, "bootstrap.json")
	if err := os.WriteFile(path, payload, mode); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	digest := sha256.Sum256(payload)
	return path, hex.EncodeToString(digest[:])
}

func validBootstrap(t *testing.T) BootstrapV1 {
	t.Helper()
	root, err := filepath.EvalSymlinks(secureTestDir(t))
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	return BootstrapV1{
		Version: 1, StateRoot: root, SocketPath: filepath.Join(root, socketLeaf),
		DatabasePath: filepath.Join(root, databaseLeaf), ObjectRoot: filepath.Join(root, objectLeaf),
		ApprovedSourceRoot: canonicalTempDir(t),
		Principal:          "principal-a", Tenant: "tenant-a",
		Session: "session-a", Brain: "brain-a", KeychainService: "ai.ouroboros.local",
		KeyEpoch: 1, KeyReference: "key-a", MaxConnections: 8, MaxRequests: 64,
		FrameBytes: 64 * 1024, MaxReadBytes: 1024 * 1024, RevocationEpoch: 3,
		Relationships: []RelationshipSpec{
			{Object: "evidence:evidence-a", Relation: "brain", User: "brain:brain-a"},
			{Object: "brain:brain-a", Relation: "owner", User: "user:principal-a"},
			{Object: "brain:brain-a", Relation: "tenant", User: "tenant:tenant-a"},
		},
		IssuedGrants: []GrantSpec{
			{ID: "grant-read", Action: "artifact.read", Evidence: EvidenceSpec{Namespace: "evidence", Value: "evidence-a"}, Fence: 7, Nonce: "nonce-read", ExpiresAt: "2040-01-02T03:04:05Z", RevocationEpoch: 3, Limits: map[string]uint64{"bytes": 1024}},
			{ID: "grant-admit", Action: "artifact.admit", Evidence: EvidenceSpec{Namespace: "evidence", Value: "evidence-a"}, Fence: 7, Nonce: "nonce-admit", ExpiresAt: "2040-01-02T03:04:05Z", RevocationEpoch: 3, Limits: map[string]uint64{"bytes": 4096, "frames": 1}},
			{ID: "grant-delete", Action: "artifact.delete", Evidence: EvidenceSpec{Namespace: "evidence", Value: "evidence-a"}, Fence: 7, Nonce: "nonce-delete", ExpiresAt: "2040-01-02T03:04:05Z", RevocationEpoch: 3, Limits: map[string]uint64{}},
		},
	}
}

// secureTestDir creates authority fixtures outside Bazel's world-writable
// TEST_TMPDIR so production ancestor checks remain identical in tests and CI.
func secureTestDir(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	if err != nil || !filepath.IsAbs(current.HomeDir) {
		t.Fatalf("current user home unavailable")
	}
	root, err := os.MkdirTemp(current.HomeDir, ".ouroboros-bootstrap-test-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("RemoveAll() error = %v", err)
		}
	})
	return root
}
