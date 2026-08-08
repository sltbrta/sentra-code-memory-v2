// product-brain planes — dual-plane honesty surface (GAP-PLANE-*).
package main

import (
	"flag"
)

// runPlanes emits the residual vs authority vs code plane inventory.
// Finishes GAP-PLANE by making by-design forks user-reachable without merging engines.
func runPlanes(args []string) {
	fs := flag.NewFlagSet("planes", flag.ExitOnError)
	_ = fs.Parse(args)

	planes := []map[string]any{
		{
			"id":     "GAP-PLANE-ASK",
			"status": "by-design",
			"fork":   "residual ask vs authority query.Engine vs path2 SMF eval",
			"residual": map[string]any{
				// residual_company_rag only for product-owned stores (OpenLocal / OpenResidual).
				// search --profile hosted via OpenFromEnv is path2_eval — not residual.
				"entry":     "product-brain ask | search --profile local | OpenResidual (fs|memory|product_neon)",
				"guarantee": "residual_company_rag",
				"not":       []string{"authority_query", "path2_smf_corpus"},
				"diag_keys": []string{"plane=residual", "product_owned=true", "not_authority_query", "not_path2_smf"},
			},
			"path2_eval": map[string]any{
				"entry":     "product-brain search|ask --profile hosted → OpenFromEnv (ERB SMF path2)",
				"guarantee": "path2_eval",
				"not":       []string{"residual_company_rag", "product_owned residual write path"},
				"diag_keys": []string{"plane=path2_eval", "product_owned=false", "path2_smf"},
			},
			"authority": map[string]any{
				"entry":     "product-brain authority + query package over Unix socket",
				"guarantee": "acl_git_fail_closed",
			},
		},
		{
			"id":     "GAP-PLANE-CODE",
			"status": "by-design",
			"fork":   "code operator working-tree vs company evidence vault",
			"code_operator": map[string]any{
				"entry":     "product-brain code-search | code-*",
				"guarantee": "heuristic_workspace",
				"not":       []string{"company_chunk_store", "artifact_vault"},
			},
			"company_residual": map[string]any{
				"entry": "product-brain create|ingest|ask",
			},
		},
		{
			"id":     "GAP-PLANE-SESSION",
			"status": "by-design",
			"fork":   "three distinct 'session' vocabularies — never one store",
			"vocab": []map[string]string{
				{"name": "productsec_seal", "meaning": "residual ask security profile seal (single_user|multi_principal)"},
				{"name": "conversation_vault", "meaning": "authority encrypted principal session turns (conversation package)"},
				{"name": "scm_session", "meaning": "coding-agent continuation product — deferred NG-SCM-010 / SCM-SESSION-PRODUCT.md"},
			},
			"refuse": "Do not pass residual SessionID expecting authority vault semantics.",
		},
		{
			"id":     "GAP-PLANE-CODE-SEARCH",
			"status": "by-design",
			"fork":   "codecrawl heuristic vs codeindex P5 exact — different guarantees",
			"profiles": []map[string]string{
				{"profile": "code", "guarantee": "heuristic_workspace", "entry": "search --profile code | code-search"},
				{"profile": "code_exact", "guarantee": "exact_p5_codeindex", "entry": "search --profile code_exact | code-exact"},
			},
			"honesty": "Results include guarantee field; never claim SCIP product without precise lane (NG-SCM-SCIP).",
		},
		{
			"id":             "GAP-PLANE-GRAPH",
			"status":         "by-design",
			"fork":           "memory edges (truth) vs structureIndex (projection) vs ontology packs",
			"residual_truth": "memory_edges",
			"projections":    []string{"structureIndex", "ontology/projections"},
			"diag_keys":      []string{"graph_truth=memory_edges", "structure_index=projection"},
		},
		{
			"id":     "GAP-PLANE-DENSE",
			"status": "by-design",
			"fork":   "residual dense substrates vs path2 Qdrant eval ANN",
			"residual": map[string]any{
				"substrates": []string{"sqlite", "postgres", "qdrant", "faiss", "memory"},
				"bind":       "OpenResidual dense=… fail-closed for qdrant without QDRANT_URL+KEY",
			},
			"path2_eval": map[string]any{
				"label": "path2_qdrant",
				"when":  "OpenFromEnv / ERB full-bench only",
				"not":   "residual product dense ownership",
			},
		},
	}

	emitJSON(map[string]any{
		"event":         "planes",
		"product_owned": true,
		"doctrine":      "ADR 0020–0024 one product binary; planes are composed not merged",
		"planes":        planes,
		"cohesive_residual": []string{
			"create|ingest|watch → residual IR retrieval_ready",
			"gardener enrich + cortex + lifecycle",
			"ask|memory|search --profile local (or hosted only when productOwned)",
		},
		"not_residual": []string{
			"OpenFromEnv path2 SMF — guarantee path2_eval",
			"authority query.Engine",
			"code_operator / code_exact",
		},
	})
}
