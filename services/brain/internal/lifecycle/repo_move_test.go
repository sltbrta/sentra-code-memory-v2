package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

// Hook paths were stored absolute at install time and used verbatim by
// uninstall and status. After `mv repo repo2` every manifest path lay outside
// the new root, so the first confinement check aborted Uninstall before it
// removed a single hook, and Status reported every live hook as missing. The
// only recovery was hand-editing the manifest.
//
// The manifest's stored path is now advisory: the live location is recomputed
// from the current hooks directory.

func installedRepo(t *testing.T) string {
	t.Helper()
	root := withTempRepo(t, false)
	if _, err := Install(Options{Root: root, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("install: %v", err)
	}
	return root
}

func moveRepo(t *testing.T, root string) string {
	t.Helper()
	moved := filepath.Join(t.TempDir(), "moved-repo")
	if err := os.Rename(root, moved); err != nil {
		t.Skipf("rename unavailable: %v", err)
	}
	return moved
}

func TestStatusReportsInstalledHooksAfterTheRepositoryMoves(t *testing.T) {
	moved := moveRepo(t, installedRepo(t))

	report, err := Status(Options{Root: moved, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatalf("status after move: %v", err)
	}
	if len(report.Installed) == 0 {
		t.Fatalf("status reports nothing installed after a move: installed=%v missing=%v",
			report.Installed, report.Missing)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("status reports live hooks as missing after a move: %v", report.Missing)
	}
}

func TestUninstallRemovesHooksAfterTheRepositoryMoves(t *testing.T) {
	moved := moveRepo(t, installedRepo(t))

	if _, err := Uninstall(Options{Root: moved, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("uninstall after move: %v", err)
	}
	hooksDir := filepath.Join(moved, ".sentra", HooksDirName)
	for _, kind := range AllHooks {
		if _, err := os.Stat(filepath.Join(hooksDir, string(kind))); err == nil {
			t.Fatalf("%s survived uninstall after a move", kind)
		}
	}
}
