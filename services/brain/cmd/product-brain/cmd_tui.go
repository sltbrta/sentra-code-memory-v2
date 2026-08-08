// product-brain tui — launches the comprehensive residual Bun TUI shell.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveTUIEntry finds the Bun shell entrypoint outside the monorepo when possible.
// Order:
//  1. OUROBOROS_TUI_ENTRY env
//  2. ~/.ouroboros/tui-entry (single-line path)
//  3. walk cwd → repo apps/tui/packages/shell/src/cli.ts
//  4. binary-adjacent ouroboros-tui.ts / cli.ts
//  5. ~/.ouroboros/tui/packages/shell/src/cli.ts (home install layout)
//  6. apps/tui package.json → bun run tui (cwd-relative)
func resolveTUIEntry() (entry string, bunDir string, mode string) {
	// 1) Explicit env
	if env := strings.TrimSpace(os.Getenv("OUROBOROS_TUI_ENTRY")); env != "" {
		if st, err := os.Stat(env); err == nil && !st.IsDir() {
			return env, filepath.Dir(env), "env"
		}
	}
	// 2) Pointer file
	if home, err := os.UserHomeDir(); err == nil {
		ptr := filepath.Join(home, ".ouroboros", "tui-entry")
		if raw, err := os.ReadFile(ptr); err == nil {
			p := strings.TrimSpace(string(raw))
			if p != "" {
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					return p, filepath.Dir(p), "tui-entry-file"
				}
			}
		}
	}
	// 3) Walk up from cwd for monorepo layout
	wd, _ := os.Getwd()
	for dir := wd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, "apps", "tui", "packages", "shell", "src", "cli.ts")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, filepath.Join(dir, "apps", "tui"), "repo-walk"
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	// 4) Binary-adjacent
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		for _, name := range []string{"ouroboros-tui.ts", "cli.ts", "packages/shell/src/cli.ts"} {
			p := filepath.Join(exeDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, exeDir, "binary-adjacent"
			}
		}
		// When installed as /usr/local/bin/product-brain, look at share layout.
		share := filepath.Join(exeDir, "..", "share", "ouroboros", "tui", "packages", "shell", "src", "cli.ts")
		if st, err := os.Stat(share); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(share)
			return abs, filepath.Dir(abs), "share-layout"
		}
	}
	// 5) Home install layout
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".ouroboros", "tui", "packages", "shell", "src", "cli.ts")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, filepath.Join(home, ".ouroboros", "tui"), "home-install"
		}
	}
	// 6) cwd apps/tui package script (return sentinel for bun run mode)
	if st, err := os.Stat(filepath.Join(wd, "apps", "tui", "package.json")); err == nil && !st.IsDir() {
		return "", filepath.Join(wd, "apps", "tui"), "bun-package"
	}
	return "", "", ""
}

func runTUI(args []string) {
	entry, bunDir, mode := resolveTUIEntry()
	if mode == "bun-package" {
		cmd := exec.Command("bun", "run", "tui")
		cmd.Dir = bunDir
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			fatal("tui: " + err.Error())
		}
		return
	}
	if entry == "" {
		home, _ := os.UserHomeDir()
		fatal(fmt.Sprintf(`tui: shell entry not found

Tried:
  OUROBOROS_TUI_ENTRY
  %s
  monorepo apps/tui/packages/shell/src/cli.ts (walk cwd)
  binary-adjacent ouroboros-tui.ts
  %s

Install options:
  1) Run from the Ouroboros monorepo root
  2) export OUROBOROS_TUI_ENTRY=/abs/path/to/cli.ts
  3) echo /abs/path/to/cli.ts > ~/.ouroboros/tui-entry
  4) Copy apps/tui to ~/.ouroboros/tui and bun install there
`, filepath.Join(home, ".ouroboros", "tui-entry"),
			filepath.Join(home, ".ouroboros", "tui", "packages", "shell", "src", "cli.ts")))
	}

	bun := "bun"
	if b := os.Getenv("OUROBOROS_BUN_BIN"); b != "" {
		bun = b
	}
	cmd := exec.Command(bun, append([]string{"run", entry}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if bunDir != "" {
		// Prefer package node_modules when entry lives under apps/tui tree.
		if st, err := os.Stat(filepath.Join(bunDir, "package.json")); err == nil && !st.IsDir() {
			cmd.Dir = bunDir
		}
	}
	_ = mode // resolution mode available for future diagnostics
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "tui:", err)
		os.Exit(1)
	}
}
