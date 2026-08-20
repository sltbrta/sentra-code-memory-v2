package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// The verification gate runs a change set's declared checks against the staged
// tree. It used to hand each command string to `/bin/sh -c` with the parent
// environment inherited, which made an ordinary JSON field on a request into
// arbitrary code execution as the serving user.
//
// The gate is now an argv vector: the command is split without any shell
// semantics, its first token must name an allowlisted verifier, and the child
// runs with a constructed environment. An operator who grants trust to a warm
// stream is saying "run this project's tests", not "run anything".

// verificationAllowlist is the set of program names a change set may name.
// Every entry is a build, test, or inspection tool. Deliberately absent: any
// shell (sh, bash, zsh, fish), anything that fetches from the network (curl,
// wget), and anything whose whole purpose is running another program (env,
// xargs, nice, time, sudo, ssh).
var verificationAllowlist = map[string]bool{
	// Go
	"go": true, "gofmt": true, "golangci-lint": true, "staticcheck": true,
	// Task runners
	"just": true, "make": true,
	// JavaScript and TypeScript
	"npm": true, "npx": true, "pnpm": true, "yarn": true, "node": true,
	"bun": true, "tsc": true, "eslint": true, "vitest": true, "jest": true,
	// Rust
	"cargo": true, "rustc": true,
	// Python
	"python": true, "python3": true, "pytest": true, "ruff": true, "mypy": true,
	// Ruby
	"ruby": true, "rake": true, "rspec": true, "bundle": true,
	// Small, side-effect-free inspection tools useful as assertions
	"grep": true, "test": true, "true": true, "false": true, "diff": true, "cmp": true,
}

// ErrVerificationCommand reports a command the gate refuses to run. It is a
// distinct sentinel so a caller can tell "your command is not allowed" from
// "your command failed".
type verificationCommandError struct{ reason string }

func (e *verificationCommandError) Error() string {
	return "verification command rejected: " + e.reason
}

// parseVerificationCommand splits a command into an argv vector and checks it
// against the allowlist. It implements no shell semantics whatsoever: quoting
// groups tokens, and nothing else is interpreted. A metacharacter is therefore
// not "escaped", it is simply an ordinary character in an argument -- which is
// why the check below rejects them outright rather than trying to quote them.
// Silently passing `; rm -rf /` through as a literal argument would be safe but
// baffling; refusing it says what happened.
func parseVerificationCommand(command string) ([]string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil, &verificationCommandError{reason: "empty"}
	}
	if len(trimmed) > applyMaxCommandLen {
		return nil, &verificationCommandError{
			reason: fmt.Sprintf("longer than %d bytes", applyMaxCommandLen),
		}
	}
	for _, r := range trimmed {
		if r == 0 || (unicode.IsControl(r) && r != '\t') {
			return nil, &verificationCommandError{reason: "contains a control character"}
		}
	}
	// Refused before splitting so the message names the real problem. These are
	// inert here -- there is no shell to interpret them -- but a command
	// containing them was written for one, and running it with a different
	// meaning than its author intended is its own kind of bug.
	if idx := strings.IndexAny(trimmed, "|&;<>()$`\\!*?[]{}~\n\r"); idx >= 0 {
		return nil, &verificationCommandError{
			reason: fmt.Sprintf("contains shell metacharacter %q; commands run as an argv vector, not through a shell", trimmed[idx]),
		}
	}

	argv, err := splitVerificationArgs(trimmed)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, &verificationCommandError{reason: "empty"}
	}
	program := argv[0]
	if strings.ContainsAny(program, `/\`) {
		return nil, &verificationCommandError{
			reason: fmt.Sprintf("%q names a path; use a bare program name resolved on PATH", program),
		}
	}
	if !verificationAllowlist[program] {
		return nil, &verificationCommandError{
			reason: fmt.Sprintf("%q is not an allowed verifier (allowed: %s)", program, allowedVerifiers()),
		}
	}
	if _, err := exec.LookPath(program); err != nil {
		return nil, &verificationCommandError{
			reason: fmt.Sprintf("%q is not installed", program),
		}
	}
	return argv, nil
}

// splitVerificationArgs splits on whitespace, honouring single and double
// quotes as grouping only. No escape sequences, no expansion, no substitution.
func splitVerificationArgs(command string) ([]string, error) {
	var (
		argv    []string
		current strings.Builder
		quote   rune
		open    bool
	)
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			open = true
		case unicode.IsSpace(r):
			if current.Len() > 0 || open {
				argv = append(argv, current.String())
				current.Reset()
				open = false
			}
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, &verificationCommandError{reason: "unterminated quote"}
	}
	if current.Len() > 0 || open {
		argv = append(argv, current.String())
	}
	return argv, nil
}

func allowedVerifiers() string {
	names := make([]string, 0, len(verificationAllowlist))
	for name := range verificationAllowlist {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// verificationEnv builds the child environment. The parent's is not inherited:
// a verifier does not need the operator's API keys, and a change set author
// should not be able to read them by running `printenv`.
//
// PATH is carried because a verifier must be findable and toolchains shell out
// to their own subcommands. HOME points at the stage, so a tool that writes a
// cache does so inside the sandbox rather than the operator's home directory.
func verificationEnv(stage string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + stage,
		"TMPDIR=" + filepath.Join(stage, ".sentra-tmp"),
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		// Keep toolchains offline and non-interactive: a verification gate that
		// can reach the network is a verification gate that can exfiltrate.
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GONOSUMDB=*",
		"GOTOOLCHAIN=local",
		"CARGO_NET_OFFLINE=true",
		"NPM_CONFIG_OFFLINE=true",
		"PIP_NO_INDEX=1",
		"GIT_TERMINAL_PROMPT=0",
	}
	// A Go verifier needs a module and build cache; without them `go test`
	// fails for a reason unrelated to the change set being verified.
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
