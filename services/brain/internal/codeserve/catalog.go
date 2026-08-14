package codeserve

// Verb is a stable multi-verb protocol name (snake_case).
type Verb string

// Supported code verbs (SCM operator parity; session latent memory out of scope).
const (
	VerbPing                Verb = "ping"
	VerbCatalog             Verb = "catalog"
	VerbCodeIndex           Verb = "code_index"
	VerbCodeSearch          Verb = "code_search"
	VerbFindRelevant        Verb = "code_find_relevant"
	VerbExpand              Verb = "code_expand"
	VerbImpact              Verb = "code_impact"
	VerbFindRoute           Verb = "code_find_route"
	VerbFreshness           Verb = "code_freshness"
	VerbIngestPaths         Verb = "code_ingest_paths"
	VerbIngestSCIP          Verb = "code_ingest_scip"
	VerbCodeExact           Verb = "code_exact"
	VerbCodeDefs            Verb = "code_defs"
	VerbCodeRefs            Verb = "code_refs"
	VerbCodeRead            Verb = "code_read"
	VerbCodeImports         Verb = "code_imports"
	VerbCodeWatch           Verb = "code_watch"
	VerbRepoMap             Verb = "code_repo_map"
	VerbStructuralSearch    Verb = "code_structural_search"
	VerbDiagnostics         Verb = "code_diagnostics"
	VerbApplyChangeSet      Verb = "code_apply_changeset"
	VerbMemoryAsk           Verb = "memory_ask" // residual company-doc; not code
	VerbMemoryPut           Verb = "memory_put"
	VerbMemorySearch        Verb = "memory_search"
	VerbMemoryList          Verb = "memory_list"
	VerbMemoryPromote       Verb = "memory_promote"
	VerbSessionContinuation Verb = "session_continuation"
	VerbSessionRecall       Verb = "session_recall"
	VerbSavingsSummary      Verb = "savings_summary"
	VerbLifecycleInstall    Verb = "lifecycle_install"
	VerbSessionProduct      Verb = "session_product"
	VerbCodeDenseRerank     Verb = "code_dense_rerank"
	VerbHostedTenancy       Verb = "hosted_tenancy"
	VerbQueryAdvanced       Verb = "query_advanced"
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
		string(VerbIngestSCIP),
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
		string(VerbMemoryPut),
		string(VerbMemorySearch),
		string(VerbMemoryList),
		string(VerbMemoryPromote),
		string(VerbSessionContinuation),
		string(VerbSessionRecall),
		string(VerbSavingsSummary),
		string(VerbLifecycleInstall),
		string(VerbSessionProduct),
		string(VerbCodeDenseRerank),
		string(VerbHostedTenancy),
		string(VerbQueryAdvanced),
	}
}
