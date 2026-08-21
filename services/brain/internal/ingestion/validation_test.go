package ingestion_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

func TestRejectsMissingMalformedAndNonRootInputs(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	valid := testConfig(t, root, git)
	tests := []struct {
		name   string
		mutate func(*ingestion.Config)
		want   error
	}{
		{name: "missing root", mutate: func(config *ingestion.Config) { config.ApprovedRoot = "" }, want: ingestion.ErrInvalidInput},
		{name: "relative root", mutate: func(config *ingestion.Config) { config.ApprovedRoot = "." }, want: ingestion.ErrInvalidInput},
		{name: "malformed digest", mutate: func(config *ingestion.Config) { config.ConfigurationDigest = "xyz" }, want: ingestion.ErrInvalidInput},
		{name: "zero limit", mutate: func(config *ingestion.Config) { config.MaxFiles = 0 }, want: ingestion.ErrInvalidInput},
		{name: "oversized file buffer", mutate: func(config *ingestion.Config) { config.MaxFileBytes = 16<<20 + 1 }, want: ingestion.ErrInvalidInput},
		{name: "oversized total buffer", mutate: func(config *ingestion.Config) { config.MaxTotalBytes = 64<<20 + 1 }, want: ingestion.ErrInvalidInput},
		{name: "oversized tree buffer", mutate: func(config *ingestion.Config) { config.MaxFiles = 100_000 }, want: ingestion.ErrInvalidInput},
		{name: "nested repository path", mutate: func(config *ingestion.Config) { config.ApprovedRoot = filepath.Join(root, "nested") }, want: ingestion.ErrOutOfRoot},
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			_, err := ingestion.New(context.Background(), config)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}

	authority, err := ingestion.New(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []ingestion.Admission{
		{},
		{ExpectedCommitOID: "not-an-oid", IdempotencyKey: "key"},
		{ExpectedCommitOID: gitOutput(t, git, root, "rev-parse", "HEAD")},
	} {
		if _, err := authority.Admit(context.Background(), request); !errors.Is(err, ingestion.ErrInvalidInput) {
			t.Fatalf("malformed admission got %v", err)
		}
	}
}

func TestRejectsOutOfRootWatcherPaths(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/tmp/escape", "../escape", "a/../../escape", "a\\escape", ""} {
		err := authority.ObserveHints([]ingestion.WatchHint{{Kind: ingestion.HintModify, Path: name}})
		if !errors.Is(err, ingestion.ErrOutOfRoot) {
			t.Fatalf("path %q got %v", name, err)
		}
	}
}

func TestApprovedRootRejectsPathReplacement(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	commit := gitOutput(t, git, root, "rev-parse", "HEAD")
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	original := root + "-original"
	if err := os.Rename(root, original); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(git, "clone", "--quiet", original, root).CombinedOutput(); err != nil {
		t.Fatalf("clone replacement root: %v: %s", err, output)
	}
	if _, err := authority.Admit(context.Background(), ingestion.Admission{
		ExpectedCommitOID: commit, IdempotencyKey: "path-replacement",
	}); !errors.Is(err, ingestion.ErrOutOfRoot) {
		t.Fatalf("path replacement admission = %v, want %v", err, ingestion.ErrOutOfRoot)
	}
}

func TestRejectsUnsupportedIgnoreRules(t *testing.T) {
	tests := []string{"!included.go\n", "**/*.go\n", "/absolute\n", "a/../b\n", "[ab].go\n"}
	for _, rule := range tests {
		t.Run(rule, func(t *testing.T) {
			root, git := newRepository(t, map[string]string{
				".gitignore": rule,
				"main.go":    "package main\n",
			})
			config := testConfig(t, root, git)
			config.Policy.UseGitIgnore = true
			authority, err := ingestion.New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			commit := gitOutput(t, git, root, "rev-parse", "HEAD")
			_, err = authority.Admit(context.Background(), ingestion.Admission{ExpectedCommitOID: commit, IdempotencyKey: "admit"})
			if !errors.Is(err, ingestion.ErrUnsupportedPolicy) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRejectsNestedIgnoreFileAndSubmodule(t *testing.T) {
	t.Run("nested ignore", func(t *testing.T) {
		root, git := newRepository(t, map[string]string{
			"nested/.gitignore": "ignored.go\n",
			"main.go":           "package main\n",
		})
		authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
		if err != nil {
			t.Fatal(err)
		}
		_, err = authority.Admit(context.Background(), ingestion.Admission{
			ExpectedCommitOID: gitOutput(t, git, root, "rev-parse", "HEAD"),
			IdempotencyKey:    "nested-ignore",
		})
		if !errors.Is(err, ingestion.ErrUnsupportedPolicy) {
			t.Fatalf("nested ignore got %v", err)
		}
	})
	t.Run("submodule gitlink", func(t *testing.T) {
		root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
		commit := gitOutput(t, git, root, "rev-parse", "HEAD")
		runGit(t, git, root, "update-index", "--add", "--cacheinfo", "160000,"+commit+",vendor/dependency")
		runGit(t, git, root, "commit", "-q", "-m", "gitlink")
		authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
		if err != nil {
			t.Fatal(err)
		}
		_, err = authority.Admit(context.Background(), ingestion.Admission{
			ExpectedCommitOID: gitOutput(t, git, root, "rev-parse", "HEAD"),
			IdempotencyKey:    "gitlink",
		})
		if !errors.Is(err, ingestion.ErrUnsupportedPolicy) {
			t.Fatalf("submodule got %v", err)
		}
	})
}

func TestAppliesFrozenIgnoreSubset(t *testing.T) {
	root, git := newRepository(t, map[string]string{
		".gitignore":        "generated/\nsrc/*.tmp\nexact.txt\n",
		".ouroborosignore":  "private/?ey.txt\n",
		"generated/a.go":    "ignored\n",
		"src/a.tmp":         "ignored\n",
		"src/nested/a.tmp":  "kept\n",
		"exact.txt":         "ignored\n",
		"private/key.txt":   "ignored\n",
		"private/other.txt": "kept\n",
	})
	config := testConfig(t, root, git)
	config.Policy.UseGitIgnore = true
	config.Policy.UseOuroborosIgnore = true
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	paths := make(map[string]bool)
	for _, file := range generation.Manifest.Files {
		paths[file.Path] = true
	}
	for _, ignored := range []string{"generated/a.go", "src/a.tmp", "exact.txt", "private/key.txt"} {
		if paths[ignored] {
			t.Fatalf("ignored path admitted: %s", ignored)
		}
	}
	for _, included := range []string{".gitignore", ".ouroborosignore", "src/nested/a.tmp", "private/other.txt"} {
		if !paths[included] {
			t.Fatalf("included path missing: %s", included)
		}
	}
}

func TestRecordsSymlinkWithoutFollowing(t *testing.T) {
	root, git := newRepository(t, map[string]string{"target.txt": "committed\n"})
	external := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(external, []byte("uncommitted secret bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, root, "add", "link")
	runGit(t, git, root, "commit", "-q", "-m", "symlink")
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	for _, file := range generation.Manifest.Files {
		if file.Path != "link" {
			continue
		}
		if file.Kind != ingestion.EntrySymlink || file.SizeBytes != int64(len(external)) {
			t.Fatalf("symlink was not recorded as link blob: %#v", file)
		}
		sum := sha256.Sum256([]byte(external))
		if file.ContentDigest != hex.EncodeToString(sum[:]) {
			t.Fatalf("symlink content was followed: %#v", file)
		}
		return
	}
	t.Fatal("symlink entry missing")
}

func TestReadsCommittedBytesUnderHostileEnvironment(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "committed\n"})
	commit := gitOutput(t, git, root, "rev-parse", "HEAD")
	writeFiles(t, root, map[string]string{"main.go": "dirty working tree bytes\n"})
	hostile, _ := newRepository(t, map[string]string{"wrong.go": "wrong repository\n"})
	t.Setenv("GIT_DIR", filepath.Join(hostile, ".git"))
	t.Setenv("GIT_WORK_TREE", hostile)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(hostile, ".git", "index"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(hostile, "hostile.gitconfig"))
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := authority.Admit(context.Background(), ingestion.Admission{
		ExpectedCommitOID: commit,
		IdempotencyKey:    "hostile-environment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.Manifest.Files) != 1 || generation.Manifest.Files[0].Path != "main.go" {
		t.Fatalf("hostile environment redirected Git: %#v", generation.Manifest.Files)
	}
	sum := sha256.Sum256([]byte("committed\n"))
	if generation.Manifest.Files[0].ContentDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("dirty bytes were admitted: %#v", generation.Manifest.Files[0])
	}
}
