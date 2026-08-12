package codeserve

// Verb is a stable multi-verb protocol name (snake_case).
type Verb string

// Supported code verbs (SCM operator parity; session latent memory out of scope).
const (
	VerbPing             Verb = "ping"
	VerbCatalog          Verb = "catalog"
	VerbCodeIndex        Verb = "code_index"
	VerbCodeSearch       Verb = "code_search"
	VerbFindRelevant     Verb = "code_find_relevant"
	VerbExpand           Verb = "code_expand"
	VerbImpact           Verb = "code_impact"
	VerbFindRoute        Verb = "code_find_route"
	VerbFreshness        Verb = "code_freshness"
	VerbIngestPaths      Verb = "code_ingest_paths"
	VerbCodeExact        Verb = "code_exact"
	VerbCodeDefs         Verb = "code_defs"
	VerbCodeRefs         Verb = "code_refs"
	VerbCodeRead         Verb = "code_read"
	VerbCodeImports      Verb = "code_imports"
	VerbCodeWatch        Verb = "code_watch"
	VerbRepoMap          Verb = "code_repo_map"
	VerbStructuralSearch Verb = "code_structural_search"
	VerbDiagnostics      Verb = "code_diagnostics"
	VerbApplyChangeSet   Verb = "code_apply_changeset"
	VerbMemoryAsk        Verb = "memory_ask" // residual company-doc; not code
)

// Catalog lists protocol verbs for discovery (POL integration).
func Catalog() []string {
	return []string{
		string(VerbPing),
		string(VerbCatalog),
		string(VerbCodeIndex),
		string(VerbCodeSearch),
		string(VerbFindRelevant),
		string(VerbExpand),
		string(VerbImpact),
		string(VerbFindRoute),
		string(VerbFreshness),
		string(VerbIngestPaths),
		string(VerbCodeExact),
		string(VerbCodeDefs),
		string(VerbCodeRefs),
		string(VerbCodeRead),
		string(VerbCodeImports),
		string(VerbCodeWatch),
		string(VerbRepoMap),
		string(VerbStructuralSearch),
		string(VerbDiagnostics),
		string(VerbApplyChangeSet),
		string(VerbMemoryAsk),
	}
}
