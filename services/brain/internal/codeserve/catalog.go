package codeserve

// Verb is a stable multi-verb protocol name (snake_case).
type Verb string

// Supported code verbs (SCM operator parity; session latent memory out of scope).
const (
	VerbPing         Verb = "ping"
	VerbCatalog      Verb = "catalog"
	VerbCodeIndex    Verb = "code_index"
	VerbCodeSearch   Verb = "code_search"
	VerbFindRelevant Verb = "code_find_relevant"
	VerbExpand       Verb = "code_expand"
	VerbImpact       Verb = "code_impact"
	VerbFindRoute    Verb = "code_find_route"
	VerbFreshness    Verb = "code_freshness"
	VerbIngestPaths  Verb = "code_ingest_paths"
	VerbCodeExact    Verb = "code_exact"
	VerbCodeDefs     Verb = "code_defs"
	VerbCodeRefs     Verb = "code_refs"
	VerbCodeRead     Verb = "code_read"
	VerbCodeImports  Verb = "code_imports"
	VerbCodeWatch    Verb = "code_watch"
	VerbMemoryAsk    Verb = "memory_ask" // residual company-doc; not code

	// Typed local agent-memory operators (issue #47). These expose the
	// bounded, local, projection-only memory.Store tier operations that the
	// standalone product owns. They are not the company-doc residual lane.
	VerbMemoryPut     Verb = "memory_put"
	VerbMemorySearch  Verb = "memory_search"
	VerbMemoryList    Verb = "memory_list"
	VerbMemoryPromote Verb = "memory_promote"

	// Bounded local session operator (issue #47). A single safe composite over
	// the repo-local sessionlog: read the event stream and build a budgeted
	// continuation packet. This is not the full SCM session product.
	VerbSessionContinuation Verb = "session_continuation"

	// Local token-savings read (issue #47).
	VerbSavingsSummary Verb = "savings_summary"

	// Deferred / non-goal verbs (issue #47). These are catalogued so the gap is
	// discoverable, but every call returns a structured deferred disclosure
	// (error_code=deferred) instead of a silent unknown-verb error. They are
	// never wired to a handler.
	VerbLifecycleInstall Verb = "lifecycle_install"
	VerbSessionProduct   Verb = "session_product"
	VerbCodeDenseRerank  Verb = "code_dense_rerank"
	VerbHostedTenancy    Verb = "hosted_tenancy"
	VerbQueryAdvanced    Verb = "query_advanced"
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
		string(VerbMemoryAsk),
		string(VerbMemoryPut),
		string(VerbMemorySearch),
		string(VerbMemoryList),
		string(VerbMemoryPromote),
		string(VerbSessionContinuation),
		string(VerbSavingsSummary),
		string(VerbLifecycleInstall),
		string(VerbSessionProduct),
		string(VerbCodeDenseRerank),
		string(VerbHostedTenancy),
		string(VerbQueryAdvanced),
	}
}
