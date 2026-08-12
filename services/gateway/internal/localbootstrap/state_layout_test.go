package localbootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsSafeAbsentAndExistingStateLeaves(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		manifest := validBootstrap(t)
		path, _, digest := writeBootstrap(t, manifest)
		config := mustLoad(t, path, digest)
		assertFixedStatePaths(t, config)
	})
	t.Run("existing", func(t *testing.T) {
		manifest := validBootstrap(t)
		setStateRoot(&manifest, canonicalTempDir(t))
		if err := os.WriteFile(manifest.DatabasePath, []byte("sqlite"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := os.Mkdir(manifest.ObjectRoot, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		createUnixSocket(t, manifest.SocketPath)
		path, _, digest := writeBootstrap(t, manifest)
		config := mustLoad(t, path, digest)
		assertFixedStatePaths(t, config)
	})
}

func TestLoadRejectsUnsafeStateRootAndAncestors(t *testing.T) {
	t.Run("writable root", func(t *testing.T) {
		manifest := validBootstrap(t)
		if err := os.Chmod(manifest.StateRoot, 0o770); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		assertBootstrapError(t, manifest, ErrUnsafeManifest)
	})
	t.Run("writable ancestor", func(t *testing.T) {
		base := canonicalTempDir(t)
		unsafeParent := filepath.Join(base, "unsafe")
		root := filepath.Join(unsafeParent, "state")
		if err := os.Mkdir(unsafeParent, 0o770); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Chmod(unsafeParent, 0o770); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		manifest := validBootstrap(t)
		setStateRoot(&manifest, root)
		assertBootstrapError(t, manifest, ErrUnsafeManifest)
	})
	t.Run("symlink root", func(t *testing.T) {
		base := canonicalTempDir(t)
		realRoot := filepath.Join(base, "real")
		aliasRoot := filepath.Join(base, "alias")
		if err := os.Mkdir(realRoot, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Symlink(realRoot, aliasRoot); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
		manifest := validBootstrap(t)
		setStateRoot(&manifest, aliasRoot)
		assertBootstrapError(t, manifest, ErrUnsafeManifest)
	})
	t.Run("symlink ancestor", func(t *testing.T) {
		base := canonicalTempDir(t)
		realParent := filepath.Join(base, "real")
		aliasParent := filepath.Join(base, "alias")
		realRoot := filepath.Join(realParent, "state")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Mkdir(realRoot, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Symlink(realParent, aliasParent); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
		manifest := validBootstrap(t)
		setStateRoot(&manifest, filepath.Join(aliasParent, "state"))
		assertBootstrapError(t, manifest, ErrUnsafeManifest)
	})
}

func TestLoadRejectsSymlinkApprovedSourceRoot(t *testing.T) {
	manifest := validBootstrap(t)
	realRoot := manifest.ApprovedSourceRoot
	aliasRoot := realRoot + "-alias"
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(aliasRoot) })
	manifest.ApprovedSourceRoot = aliasRoot
	assertBootstrapError(t, manifest, ErrUnsafeManifest)
}

func TestLoadRejectsUnsafeStateLeaves(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, BootstrapV1)
	}{
		{name: "database directory", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.Mkdir(manifest.DatabasePath, 0o700); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
		}},
		{name: "database broad mode", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.WriteFile(manifest.DatabasePath, nil, 0o640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if err := os.Chmod(manifest.DatabasePath, 0o640); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
		}},
		{name: "database read-only mode", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.WriteFile(manifest.DatabasePath, nil, 0o400); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}},
		{name: "object regular file", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.WriteFile(manifest.ObjectRoot, nil, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}},
		{name: "object broad mode", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.Mkdir(manifest.ObjectRoot, 0o750); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			// Make the requested mode explicit; Mkdir is umask-sensitive.
			if err := os.Chmod(manifest.ObjectRoot, 0o750); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
		}},
		{name: "socket regular file", setup: func(t *testing.T, manifest BootstrapV1) {
			if err := os.WriteFile(manifest.SocketPath, nil, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}},
		{name: "symlink leaf", setup: func(t *testing.T, manifest BootstrapV1) {
			target := filepath.Join(filepath.Dir(manifest.StateRoot), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if err := os.Symlink(target, manifest.DatabasePath); err != nil {
				t.Fatalf("Symlink() error = %v", err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validBootstrap(t)
			test.setup(t, manifest)
			assertBootstrapError(t, manifest, ErrUnsafeManifest)
		})
	}
}

func TestLoadRejectsManifestStateOverlapAndAlias(t *testing.T) {
	t.Run("manifest inside state root", func(t *testing.T) {
		manifest := validBootstrap(t)
		payload, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		path := filepath.Join(manifest.StateRoot, "bootstrap.json")
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		digest := sha256.Sum256(payload)
		assertLoadError(t, path, hex.EncodeToString(digest[:]), ErrUnsafeManifest)
	})
	t.Run("database hard link to manifest", func(t *testing.T) {
		manifest := validBootstrap(t)
		path, _, digest := writeBootstrap(t, manifest)
		if err := os.Link(path, manifest.DatabasePath); err != nil {
			t.Fatalf("Link() error = %v", err)
		}
		assertLoadError(t, path, digest, ErrUnsafeManifest)
	})
}

func assertFixedStatePaths(t *testing.T, config *Config) {
	t.Helper()
	if config.DatabasePath() != filepath.Join(config.StateRoot(), databaseLeaf) ||
		config.SocketPath() != filepath.Join(config.StateRoot(), socketLeaf) ||
		config.ObjectRoot() != filepath.Join(config.StateRoot(), objectLeaf) {
		t.Fatal("Config did not return the fixed canonical state layout")
	}
}

func assertBootstrapError(t *testing.T, manifest BootstrapV1, want error) {
	t.Helper()
	path, _, digest := writeBootstrap(t, manifest)
	assertLoadError(t, path, digest, want)
}

func setStateRoot(manifest *BootstrapV1, root string) {
	manifest.StateRoot = root
	manifest.DatabasePath = filepath.Join(root, databaseLeaf)
	manifest.SocketPath = filepath.Join(root, socketLeaf)
	manifest.ObjectRoot = filepath.Join(root, objectLeaf)
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(secureTestDir(t))
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	return root
}

func createUnixSocket(t *testing.T, path string) {
	t.Helper()
	fixturePath := filepath.Join(secureTestDir(t), "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: fixturePath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix() error = %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(fixturePath, 0o600); err != nil {
		_ = listener.Close()
		t.Fatalf("Chmod() error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Rename(fixturePath, path); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
}
