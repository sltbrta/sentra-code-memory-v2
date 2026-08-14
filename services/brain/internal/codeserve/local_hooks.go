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
// workflow. The mutating actions (install/uninstall/run) carry the
// VerbSpec.RequiresOperatorTrust flag: adapters are responsible for gating
// them, but codeserve itself never inspects the flag, so direct CLI and
// explicit JSONL operator pipelines are unaffected (issue #59 + #63).
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

// OperatorTrustActions lists the mutating verbs+actions whose dispatch
// from a model-facing adapter (HTTP /dispatch, MCP tools/call) requires
// an explicit operator opt-in. Status and other read-only verbs on the
// same surface are intentionally NOT included so the gates fail closed
// only on actions that leave host state behind.
var OperatorTrustActions = map[Verb]map[string]bool{
	VerbHooksLocal: {
		"install":   true,
		"uninstall": true,
		"run":       true,
	},
}

// IsOperatorTrustAction reports whether the (verb, action) pair would
// leave host state behind and therefore requires an explicit operator
// opt-in to dispatch from a model-facing adapter. Adapters call this so
// codeserve stays the single source of truth for the gate. The verb must
// already be recognized by Handle; unknown actions on a recognized verb
// are treated as non-trusting so a typo in `action=` is never silently
// admitted as "trusted".
func IsOperatorTrustAction(verb string, action string) bool {
	trust, ok := OperatorTrustActions[Verb(verb)]
	if !ok {
		return false
	}
	return trust[strings.TrimSpace(action)]
}

// VerbRequiresOperatorTrust reports whether the verb's catalog metadata
// marks any of its actions as requiring an explicit operator opt-in from
// model-facing surfaces. It is metadata-driven so the surface contract
// and the dispatch gate stay in sync.
func VerbRequiresOperatorTrust(verb string) bool {
	for _, s := range CatalogMetadata() {
		if s.Name == verb {
			return s.RequiresOperatorTrust
		}
	}
	return false
}

// OperatorTrustError returns the canonical structured response produced
// when an adapter refuses a gated dispatch without an explicit opt-in.
// Keeping the envelope here lets codeserve own the contract; adapters
// just forward the map.
func OperatorTrustError(verb, action, surface string) Response {
	return Response{
		"ok": false, "verb": verb, "action": action,
		"error": "operator trust required for " + verb + " action=" + action +
			" on " + surface + "; set the explicit operator opt-in (HTTP " +
			"X-Sentra-Operator-Trust header or ?operator_trust=1 query; " +
			"MCP arguments._operator_trust=true) or run the direct CLI",
		"error_code":    string(ErrOperatorTrust),
		"product_owned": true,
		"trust_required": map[string]any{
			"verb":         verb,
			"action":       action,
			"surface":      surface,
			"opt_in_field": "_operator_trust",
		},
	}
}
