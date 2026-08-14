package codeserve

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/lifecycle"
)

// handleHooksLocal implements the codeserve side of the local-first hook
// lifecycle (issue #59). It maps JSONL verb parameters to lifecycle.Options
// and serializes the result back. This verb is opt-in and stable; existing
// callers never invoke it unless they specifically opt into the new
// workflow.
//
// Wire shape:
//
//	verb: hooks_local
//	action: install|status|uninstall|run
//	root: repository path
//	strategy: repo-hooks (default) | git-common-hooks
//	kinds: comma-separated hook names (post-commit,post-checkout,...)
//	allow_unsafe_git_common: bool (only honored for git-common-hooks)
//	cli_path: absolute path for installed hooks to invoke
func handleHooksLocal(req Request) Response {
	action, _ := req["action"].(string)
	if action == "" {
		return Response{
			"ok": false, "verb": string(VerbHooksLocal),
			"error":      "action is required (install|status|uninstall|run)",
			"error_code": string(ErrInvalidRequest),
		}
	}
	root, _ := req["root"].(string)
	strategy := lifecycle.StrategyRepoHooks
	if s, ok := req["strategy"].(string); ok {
		strategy = lifecycle.Strategy(s)
	}
	opts := lifecycle.Options{
		Root:                 root,
		Strategy:             strategy,
		AllowUnsafeGitCommon: boolOf(req["allow_unsafe_git_common"]),
	}
	if s, ok := req["cli_path"].(string); ok {
		opts.CLIExecutable = s
	}
	if raw, ok := req["kinds"].(string); ok && raw != "" {
		var kinds []lifecycle.HookKind
		for _, k := range strings.Split(raw, ",") {
			kinds = append(kinds, lifecycle.HookKind(strings.TrimSpace(k)))
		}
		opts.Hooks = kinds
	}
	switch action {
	case "install":
		res, err := lifecycle.Install(opts)
		return lifecycleToResponse(res, err)
	case "status":
		res, err := lifecycle.Status(opts)
		return lifecycleReportToResponse(res, err)
	case "uninstall":
		res, err := lifecycle.Uninstall(opts)
		return lifecycleToResponse(res, err)
	case "run":
		event, _ := req["event"].(string)
		evtRoot, _ := req["root"].(string)
		if err := lifecycle.RunHook(event, evtRoot); err != nil {
			return Response{
				"ok": false, "verb": string(VerbHooksLocal),
				"error":      err.Error(),
				"error_code": string(ErrInternal),
			}
		}
		return okResp(string(VerbHooksLocal), map[string]any{"action": "run"})
	}
	return Response{
		"ok": false, "verb": string(VerbHooksLocal),
		"error":      fmt.Sprintf("unknown action %q", action),
		"error_code": string(ErrInvalidRequest),
	}
}

func lifecycleToResponse(res lifecycle.Result, err error) Response {
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbHooksLocal),
			"error":      err.Error(),
			"error_code": string(ErrInternal),
		}
	}
	out := okResp(string(VerbHooksLocal), map[string]any{
		"action":   res.Action,
		"strategy": res.Strategy,
		"notes":    res.Notes,
	})
	raw, err := json.Marshal(res.Manifest)
	if err == nil {
		out["manifest"] = json.RawMessage(raw)
	}
	return out
}

func lifecycleReportToResponse(rep lifecycle.Report, err error) Response {
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbHooksLocal),
			"error":      err.Error(),
			"error_code": string(ErrInternal),
		}
	}
	return okResp(string(VerbHooksLocal), map[string]any{
		"root":       rep.Root,
		"strategy":   rep.Strategy,
		"hooks_dir":  rep.HooksDir,
		"hooks_path": rep.HooksPath,
		"installed":  rep.Installed,
		"missing":    rep.Missing,
		"unexpected": rep.Unexpected,
		"notes":      rep.Notes,
	})
}

func boolOf(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	}
	return false
}
