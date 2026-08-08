package localbootstrap

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsInvalidOptions(t *testing.T) {
	path, _, digest := writeBootstrap(t, validBootstrap(t))
	tests := []struct {
		name   string
		path   string
		digest string
		now    func() time.Time
	}{
		{name: "missing path", digest: digest, now: func() time.Time { return fixedNow }},
		{name: "relative path", path: "bootstrap.json", digest: digest, now: func() time.Time { return fixedNow }},
		{name: "unclean manifest path", path: filepath.Dir(path) + "/../" + filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path), digest: digest, now: func() time.Time { return fixedNow }},
		{name: "missing clock", path: path, digest: digest},
		{name: "zero clock", path: path, digest: digest, now: func() time.Time { return time.Time{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(Options{ManifestPath: test.path, ExpectedSHA256: test.digest, Now: test.now})
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("Load() error = %v, want %v", err, ErrInvalidOptions)
			}
		})
	}
}

func TestLoadRejectsInvalidManifestFacts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BootstrapV1)
	}{
		{name: "relative state path", mutate: func(value *BootstrapV1) { value.SocketPath = "runtime.sock" }},
		{name: "root state path", mutate: func(value *BootstrapV1) { value.ObjectRoot = "/" }},
		{name: "unclean state path", mutate: func(value *BootstrapV1) { value.DatabasePath = "/tmp/a/../state.db" }},
		{name: "alternative database leaf", mutate: func(value *BootstrapV1) { value.DatabasePath = filepath.Join(value.StateRoot, "state.db") }},
		{name: "deep object leaf", mutate: func(value *BootstrapV1) { value.ObjectRoot = filepath.Join(value.StateRoot, "data", objectLeaf) }},
		{name: "relative approved source root", mutate: func(value *BootstrapV1) { value.ApprovedSourceRoot = "repository" }},
		{name: "filesystem approved source root", mutate: func(value *BootstrapV1) { value.ApprovedSourceRoot = "/" }},
		{name: "state approved source root", mutate: func(value *BootstrapV1) { value.ApprovedSourceRoot = value.StateRoot }},
		{name: "duplicate state path", mutate: func(value *BootstrapV1) { value.DatabasePath = value.SocketPath }},
		{name: "object root contains socket", mutate: func(value *BootstrapV1) { value.ObjectRoot = filepath.Dir(value.SocketPath) }},
		{name: "socket path contains object root", mutate: func(value *BootstrapV1) { value.ObjectRoot = filepath.Join(value.SocketPath, "objects") }},
		{name: "duplicate relationship", mutate: func(value *BootstrapV1) { value.Relationships = append(value.Relationships, value.Relationships[0]) }},
		{name: "wildcard relationship", mutate: func(value *BootstrapV1) { value.Relationships[0].User = "brain:*" }},
		{name: "delegating relationship", mutate: func(value *BootstrapV1) { value.Relationships[0].Relation = "delegate" }},
		{name: "unknown relationship", mutate: func(value *BootstrapV1) { value.Relationships[0].Relation = "reader" }},
		{name: "duplicate grant", mutate: func(value *BootstrapV1) { value.IssuedGrants = append(value.IssuedGrants, value.IssuedGrants[0]) }},
		{name: "expired grant", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].ExpiresAt = fixedNow.Format(time.RFC3339) }},
		{name: "non UTC grant", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].ExpiresAt = "2040-01-02T03:04:05+01:00" }},
		{name: "stale grant", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].RevocationEpoch-- }},
		{name: "wildcard grant", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Evidence.Value = "*" }},
		{name: "delegating grant", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].ID = "delegate-child" }},
		{name: "bad action", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Action = "artifact.write" }},
		{name: "bad namespace", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Evidence.Namespace = "artifact" }},
		{name: "missing limits", mutate: func(value *BootstrapV1) { value.IssuedGrants[2].Limits = nil }},
		{name: "control character", mutate: func(value *BootstrapV1) { value.Principal = "principal\nadmin" }},
		{name: "empty relationships", mutate: func(value *BootstrapV1) { value.Relationships = nil }},
		{name: "empty grants", mutate: func(value *BootstrapV1) { value.IssuedGrants = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validBootstrap(t)
			test.mutate(&manifest)
			path, _, digest := writeBootstrap(t, manifest)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func TestLoadRejectsActionSpecificLimitViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BootstrapV1)
	}{
		{name: "admit missing frames", mutate: func(value *BootstrapV1) { value.IssuedGrants[1].Limits = map[string]uint64{"bytes": 1} }},
		{name: "admit unknown key", mutate: func(value *BootstrapV1) { value.IssuedGrants[1].Limits = map[string]uint64{"bytes": 1, "other": 1} }},
		{name: "admit zero", mutate: func(value *BootstrapV1) { value.IssuedGrants[1].Limits["frames"] = 0 }},
		{name: "admit oversized bytes", mutate: func(value *BootstrapV1) { value.IssuedGrants[1].Limits["bytes"] = maxAdmitBytes + 1 }},
		{name: "admit oversized frames", mutate: func(value *BootstrapV1) { value.IssuedGrants[1].Limits["frames"] = maxAdmitFrames + 1 }},
		{name: "read zero", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Limits["bytes"] = 0 }},
		{name: "read oversized", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Limits["bytes"] = value.MaxReadBytes + 1 }},
		{name: "read unknown key", mutate: func(value *BootstrapV1) { value.IssuedGrants[0].Limits = map[string]uint64{"other": 1} }},
		{name: "delete metered", mutate: func(value *BootstrapV1) { value.IssuedGrants[2].Limits["bytes"] = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validBootstrap(t)
			test.mutate(&manifest)
			path, _, digest := writeBootstrap(t, manifest)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func TestLoadRejectsMalformedTopLevelJSON(t *testing.T) {
	for name, payload := range map[string][]byte{
		"array":     []byte(`[]`),
		"null":      []byte(`null`),
		"truncated": []byte(`{"version":1`),
		"wrong type": []byte(strings.Replace(
			`{"version":1}`, `1`, `"one"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, payload, 0o600)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}
