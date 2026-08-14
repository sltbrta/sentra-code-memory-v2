package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// runHooks exposes the repo-confined hook lifecycle through the direct CLI.
// The action may appear before or after flags so both documented forms remain
// usable with Go's flag package.
func runHooks(ctx context.Context, args []string, out, errOut io.Writer) int {
	action, remaining := extractAction(args, map[string]bool{
		"install": true, "status": true, "uninstall": true, "run": true,
	})
	if action == "" {
		fmt.Fprintln(errOut, "hooks requires install, status, uninstall, or run")
		return 2
	}
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "", "repository root")
	strategy := fs.String("strategy", "repo-hooks", "repo-hooks or git-common-hooks")
	kinds := fs.String("kinds", "", "comma-separated hook kinds")
	cliPath := fs.String("cli-path", "", "CLI executable path for installed hooks")
	event := fs.String("event", "", "hook event for run")
	unsafeCommon := fs.Bool("allow-unsafe-git-common", false, "allow shared git common hooks")
	if err := fs.Parse(remaining); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "hooks does not accept positional arguments")
		return 2
	}
	req := codeserve.Request{
		"verb":                    "hooks_local",
		"action":                  action,
		"root":                    *root,
		"strategy":                *strategy,
		"kinds":                   *kinds,
		"cli_path":                *cliPath,
		"event":                   *event,
		"allow_unsafe_git_common": *unsafeCommon,
	}
	return writeJSON(out, codeserve.Handle(ctx, req))
}

// runDenseLocal exposes the optional local-only lexical retrieval arm.
func runDenseLocal(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dense-local", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "", "source root")
	scope := fs.String("scope", "", "retrieval identity scope label")
	model := fs.String("model", "bag-of-words:v1", "retrieval model identity label")
	query := fs.String("q", "", "query")
	topK := fs.Int("top-k", 10, "maximum hits (hard ceiling 50)")
	maxCorpus := fs.Int("max-corpus", 8192, "maximum corpus size (tighten only)")
	maxDim := fs.Int("max-dim", 1024, "maximum vector dimension (tighten only)")
	maxQuery := fs.Int("max-query-len", 512, "maximum query length (tighten only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "dense-local does not accept positional arguments")
		return 2
	}
	req := codeserve.Request{
		"verb": "dense_local_search", "root": *root,
		"scope": *scope, "model": *model, "q": *query,
		"top_k": *topK, "max_corpus": *maxCorpus, "max_dim": *maxDim,
		"max_query_len": *maxQuery,
	}
	return writeJSON(out, codeserve.Handle(ctx, req))
}

func extractAction(args []string, actions map[string]bool) (string, []string) {
	for i, arg := range args {
		if actions[arg] {
			remaining := append([]string(nil), args[:i]...)
			remaining = append(remaining, args[i+1:]...)
			return arg, remaining
		}
	}
	return "", args
}
