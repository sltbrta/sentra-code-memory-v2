package codeserve

// Canonical Go contract types for the local-first SCM code-memory protocol
// (Phase 0: contracts, fixtures, benchmarks).
//
// The wire protocol stays the map-based JSONL shape in handle.go; these
// structs are the canonical typed view of the same keys. Handlers keep their
// current behavior — the types here codify it, and the conformance tests in
// contract_test.go prove the two agree. The JSONL code-operator verbs are
// represented here as the canonical typed view; handlers preserve the
// existing map-based wire envelope.

import (
	"encoding/json"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contextpack"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
)

// ContractID identifies this revision of the canonical protocol contract.
const ContractID = "sentra-scm.codeserve/v1"

// ErrorCode is a stable machine-readable failure class. The human-readable
// "error" message key is preserved verbatim for existing consumers.
type ErrorCode string

const (
	// ErrInvalidRequest: missing or malformed required fields.
	ErrInvalidRequest ErrorCode = "invalid_request"
	// ErrUnknownVerb: verb not in Catalog().
	ErrUnknownVerb ErrorCode = "unknown_verb"
	// ErrIndexUnavailable: durable index missing, unloadable, or unrefreshable.
	ErrIndexUnavailable ErrorCode = "index_unavailable"
	// ErrInternal: unexpected local failure (e.g. persisting the index).
	ErrInternal ErrorCode = "internal"
	// ErrUnauthorized: local trust policy rejected the caller (missing or
	// wrong bearer token on the HTTP surface).
	ErrUnauthorized ErrorCode = "unauthorized"
	// ErrPathDenied: path policy denied the read (ignored by repoignore or
	// not a durable-index member; explicit opt-in fields can override).
	ErrPathDenied ErrorCode = "path_denied"
)

// ErrorResponse is the canonical failure envelope. OK is always false.
type ErrorResponse struct {
	OK           bool      `json:"ok"`
	Verb         string    `json:"verb"`
	Error        string    `json:"error"`
	ErrorCode    ErrorCode `json:"error_code"`
	Supported    []string  `json:"supported,omitempty"`
	ProductOwned bool      `json:"product_owned"`
}

// IndexSelector locates the durable code index shared by all read verbs.
// Bool fields omit omitempty so explicit false is preserved on the wire.
type IndexSelector struct {
	Root       string `json:"root,omitempty"`
	IndexCache string `json:"index_cache,omitempty"`
	Workers    int    `json:"workers,omitempty"`
	NoRefresh  bool   `json:"no_refresh"`
	Force      bool   `json:"force"`
}

// --- Requests -------------------------------------------------------------

// IndexRequest builds or refreshes the durable index (code_index).
type IndexRequest struct {
	Verb string `json:"verb"`
	IndexSelector
}

// WatchRequest configures the debounced freshness loop. The JSONL verb
// (code_watch) runs bounded cycles (default 1) through the existing
// codecrawl WatchPoll/WatchFS adapters, collecting events and errors
// into a typed WatchResponse. The CLI streaming path is unchanged.
type WatchRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	IntervalMS     int  `json:"interval_ms,omitempty"`
	DebounceMS     int  `json:"debounce_ms,omitempty"`
	QueueSize      int  `json:"queue_size,omitempty"`
	RetryInitialMS int  `json:"retry_initial_ms,omitempty"`
	RetryMaxMS     int  `json:"retry_max_ms,omitempty"`
	MaxCycles      int  `json:"max_cycles,omitempty"`
	FSNotify       bool `json:"fsnotify"`
	TimeoutMS      int  `json:"timeout_ms,omitempty"`
}

// FreshnessRequest probes workspace-vs-index drift (code_freshness).
type FreshnessRequest struct {
	Verb string `json:"verb"`
	IndexSelector
}

// SearchRequest is the lexical search verb (code_search).
type SearchRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Q    string `json:"q"`
	TopK int    `json:"top_k,omitempty"`
}

// FindRelevantRequest is the lean agent-facing top-k retrieval
// (code_find_relevant). Preview defaults to true on the wire when omitted;
// omitempty is deliberately absent so explicit false survives marshaling.
// MaxBytes/MaxTokens/Render opt into bounded context packing (contextpack);
// when all are unset the response shape is exactly the legacy payload.
// Session shares a dedup/handle registry across bounded calls.
type FindRelevantRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Q         string `json:"q"`
	TopK      int    `json:"top_k,omitempty"`
	Preview   bool   `json:"preview"`
	MaxBytes  int    `json:"max_bytes,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Render    string `json:"render,omitempty"`
	Session   string `json:"session,omitempty"`
}

// ExpandRequest returns defs+refs neighborhoods of a seed (code_expand).
type ExpandRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Seed string `json:"seed"`
}

// ReadRequest reads a bounded source region (code_read). By default the
// read is constrained by the repository ignore policy and — when a durable
// index exists — by index membership; AllowIgnored / AllowUnindexed are the
// explicit, typed opt-ins around those two gates.
type ReadRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Path           string `json:"path"`
	StartLine      int    `json:"start_line,omitempty"`
	MaxLines       int    `json:"max_lines,omitempty"`
	AllowIgnored   bool   `json:"allow_ignored"`
	AllowUnindexed bool   `json:"allow_unindexed"`
}

// ImpactRequest computes the heuristic impact closure (code_impact).
type ImpactRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Seed     string `json:"seed"`
	MaxDepth int    `json:"max_depth,omitempty"`
	MaxFiles int    `json:"max_files,omitempty"`
}

// FindRouteRequest finds bridge files between two seeds (code_find_route).
type FindRouteRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	From       string `json:"from"`
	To         string `json:"to"`
	MaxBridges int    `json:"max_bridges,omitempty"`
}

// IngestPathsRequest incrementally re-indexes explicit paths
// (code_ingest_paths). The wire also accepts a comma-separated string.
type IngestPathsRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Paths []string `json:"paths"`
}

// ExactKind selects the exact-match lane for code_exact.
type ExactKind string

const (
	ExactKindAny        ExactKind = "any"
	ExactKindDefinition ExactKind = "definition"
	ExactKindReference  ExactKind = "reference"
	ExactKindImport     ExactKind = "import"
)

// PingRequest is the liveness probe (ping).
type PingRequest struct {
	Verb string `json:"verb"`
}

// PingResponse is a liveness acknowledgment.
type PingResponse struct {
	ResponseMeta
}

// ExactRequest is the precise defs/refs/imports lane. code_defs and
// code_refs are code_exact with Kind pinned; code_imports is the
// same pinning for ExactKindImport.
type ExactRequest struct {
	Verb string    `json:"verb"`
	Root string    `json:"root"`
	Q    string    `json:"q"`
	Kind ExactKind `json:"kind,omitempty"`
	TopK int       `json:"top_k,omitempty"`
}

// MemoryAskRequest queries the company-doc residual lane (memory_ask).
type MemoryAskRequest struct {
	Verb    string `json:"verb"`
	Dir     string `json:"dir"`
	Q       string `json:"q"`
	Session string `json:"session,omitempty"`
	TopK    int    `json:"top_k,omitempty"`
}

// --- Responses ------------------------------------------------------------

// ResponseMeta is the envelope prefix every success response carries.
type ResponseMeta struct {
	OK           bool   `json:"ok"`
	Verb         string `json:"verb"`
	ProductOwned bool   `json:"product_owned"`
}

// IndexResponse reports an index build/refresh cycle.
type IndexResponse struct {
	ResponseMeta
	Root           string `json:"root"`
	GobPath        string `json:"gob_path"`
	Wrote          bool   `json:"wrote"`
	Changed        int    `json:"changed"`
	Unchanged      int    `json:"unchanged"`
	SkippedByStamp int    `json:"skipped_by_stamp"`
	DurationMS     int64  `json:"duration_ms"`
	GitHead        string `json:"git_head"`
	SearchBackend  string `json:"search_backend"`
}

// WatchEvent is one line of the CLI watch event stream (event="refresh" or
// "refresh_error").
type WatchEvent struct {
	Event          string `json:"event"`
	Root           string `json:"root,omitempty"`
	GobPath        string `json:"gob_path,omitempty"`
	FilesIndexed   int    `json:"files_indexed,omitempty"`
	Changed        int    `json:"changed,omitempty"`
	Unchanged      int    `json:"unchanged,omitempty"`
	SkippedByStamp int    `json:"skipped_by_stamp,omitempty"`
	DurationMS     int64  `json:"duration_ms,omitempty"`
	Workers        int    `json:"workers,omitempty"`
	Wrote          bool   `json:"wrote,omitempty"`
	QueueDepth     int    `json:"queue_depth,omitempty"`
	RetryCount     int    `json:"retry_count,omitempty"`
	FullRescan     bool   `json:"full_rescan,omitempty"`
	Error          string `json:"error,omitempty"`
}

// FreshnessResponse wraps codecrawl.FreshnessReport.
type FreshnessResponse struct {
	ResponseMeta
	Report     codecrawl.FreshnessReport `json:"report"`
	DurationMS int64                     `json:"duration_ms"`
}

// SearchResponse wraps ranked lexical hits.
type SearchResponse struct {
	ResponseMeta
	Root          string          `json:"root"`
	GobPath       string          `json:"gob_path"`
	Q             string          `json:"q"`
	Hits          []codecrawl.Hit `json:"hits"`
	DurationMS    int64           `json:"duration_ms"`
	SearchBackend string          `json:"search_backend"`
}

// FindRelevantResponse wraps the lean agent payload. Context is present
// only when the request opted into bounded packing (max_bytes, max_tokens,
// or render).
type FindRelevantResponse struct {
	ResponseMeta
	Payload       codecrawl.AgentPayload `json:"payload"`
	Context       *contextpack.Result    `json:"context,omitempty"`
	DurationMS    int64                  `json:"duration_ms"`
	SearchBackend string                 `json:"search_backend"`
}

// ExpandResponse is the heuristic defs+refs neighborhood of a seed.
type ExpandResponse struct {
	ResponseMeta
	Seed          string   `json:"seed"`
	Defs          []string `json:"defs"`
	Refs          []string `json:"refs"`
	Authority     string   `json:"authority"`
	SearchBackend string   `json:"search_backend"`
}

// ReadResponse is the bounded source-region read.
type ReadResponse struct {
	ResponseMeta
	Path      string `json:"path"`
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Truncated bool   `json:"truncated"`
}

// ImpactResponse wraps codecrawl.ImpactReceipt (authority "heuristic").
type ImpactResponse struct {
	ResponseMeta
	Receipt       codecrawl.ImpactReceipt `json:"receipt"`
	SearchBackend string                  `json:"search_backend"`
}

// FindRouteResponse wraps codecrawl.RouteReceipt.
type FindRouteResponse struct {
	ResponseMeta
	Receipt       codecrawl.RouteReceipt `json:"receipt"`
	SearchBackend string                 `json:"search_backend"`
}

// IngestPathsResponse reports an incremental ingest.
type IngestPathsResponse struct {
	ResponseMeta
	Changed int      `json:"changed"`
	Paths   []string `json:"paths"`
	GobPath string   `json:"gob_path"`
	Root    string   `json:"root"`
	GitHead string   `json:"git_head"`
}

// ExactResponse wraps the precise codeindex result.
type ExactResponse struct {
	ResponseMeta
	Result        productsearch.Result `json:"result"`
	DurationMS    int64                `json:"duration_ms"`
	SearchBackend string               `json:"search_backend"`
}

// WatchResponse wraps a bounded code_watch JSONL cycle (non-streaming).
// Events collect refresh cycles and errors; the verb returns after max_cycles
// (default 1) or context cancellation instead of running forever.
type WatchResponse struct {
	ResponseMeta
	Root            string       `json:"root"`
	GobPath         string       `json:"gob_path"`
	DurationMS      int64        `json:"duration_ms"`
	Events          []WatchEvent `json:"events"`
	Cancelled       bool         `json:"cancelled,omitempty"`
	EventsTruncated bool         `json:"events_truncated,omitempty"`
}

// DecodeResponse binds a wire response to its canonical typed form. It is
// the conformance seam: handlers stay map-based, callers and tests get
// typed values with zero behavior change.
func DecodeResponse(resp Response, v any) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// --- Catalog metadata -----------------------------------------------------

// VerbStatus marks contract stability.
type VerbStatus string

const (
	// StatusStable verbs are live and covered by conformance tests.
	StatusStable VerbStatus = "stable"
	// StatusPlanned verbs are typed here but have no handler yet.
	StatusPlanned VerbStatus = "planned"
)

// VerbSpec is the canonical catalog metadata for one verb: surface,
// stability, field contract, and compatibility aliases.
type VerbSpec struct {
	Name     string     `json:"name"`
	Status   VerbStatus `json:"status"`
	Surface  string     `json:"surface"` // jsonl | cli
	Summary  string     `json:"summary"`
	Required []string   `json:"required,omitempty"`
	Optional []string   `json:"optional,omitempty"`
	Aliases  []string   `json:"aliases,omitempty"`
}

// CatalogMetadata is the typed companion to Catalog(): every live JSONL
// verb plus planned verbs whose contracts are fixed in this package.
// Aliases preserve existing entry points (CLI subcommands and the
// back-compat "find_route" serve alias); no alias is a new behavior.
func CatalogMetadata() []VerbSpec {
	return []VerbSpec{
		{Name: string(VerbPing), Status: StatusStable, Surface: "jsonl",
			Summary: "liveness probe"},
		{Name: string(VerbCatalog), Status: StatusStable, Surface: "jsonl",
			Summary: "lean verb discovery; detailed specs are opt-in"},
		{Name: string(VerbCodeIndex), Status: StatusStable, Surface: "jsonl",
			Summary:  "build or refresh the durable code index",
			Required: []string{"root"},
			Optional: []string{"index_cache", "workers", "force"},
			Aliases:  []string{"index", "code-index"}},
		{Name: string(VerbCodeWatch), Status: StatusStable, Surface: "jsonl",
			Summary:  "bounded freshness loop (default 1 cycle); collects refresh events and errors",
			Required: []string{"root"},
			Optional: []string{"index_cache", "workers", "interval_ms", "debounce_ms",
				"queue_size", "retry_initial_ms", "retry_max_ms", "max_cycles", "fsnotify", "timeout_ms"},
			Aliases: []string{"watch"}},
		{Name: string(VerbCodeSearch), Status: StatusStable, Surface: "jsonl",
			Summary:  "lexical ranked search over the durable index",
			Required: []string{"q"},
			Optional: []string{"root", "index_cache", "top_k", "no_refresh", "force", "workers"},
			Aliases:  []string{"search", "code-search"}},
		{Name: string(VerbFindRelevant), Status: StatusStable, Surface: "jsonl",
			Summary:  "lean top-k agent payload with optional source previews and opt-in bounded context packing",
			Required: []string{"q"},
			Optional: []string{"root", "index_cache", "top_k", "preview", "no_refresh",
				"max_bytes", "max_tokens", "render", "session"},
			Aliases: []string{"relevant"}},
		{Name: string(VerbExpand), Status: StatusStable, Surface: "jsonl",
			Summary:  "heuristic defs+refs neighborhood of a seed symbol",
			Required: []string{"seed"},
			Optional: []string{"root", "index_cache", "no_refresh"},
			Aliases:  []string{"expand"}},
		{Name: string(VerbCodeRead), Status: StatusStable, Surface: "jsonl",
			Summary:  "bounded source-region read constrained by repoignore/index membership (start_line default 1, max_lines default 200 cap 1000)",
			Required: []string{"root", "path"},
			Optional: []string{"start_line", "max_lines", "index_cache", "allow_ignored", "allow_unindexed"},
			Aliases:  []string{"read"}},
		{Name: string(VerbImpact), Status: StatusStable, Surface: "jsonl",
			Summary:  "heuristic impact closure over defs/refs/imports",
			Required: []string{"seed"},
			Optional: []string{"root", "index_cache", "max_depth", "max_files", "no_refresh"},
			Aliases:  []string{"impact"}},
		{Name: string(VerbFindRoute), Status: StatusStable, Surface: "jsonl",
			Summary:  "bridge files between two seeds via shared names",
			Required: []string{"from", "to"},
			Optional: []string{"root", "index_cache", "max_bridges", "no_refresh"},
			Aliases:  []string{"route", "find_route"}},
		{Name: string(VerbFreshness), Status: StatusStable, Surface: "jsonl",
			Summary:  "workspace vs index drift probe",
			Optional: []string{"root", "index_cache"},
			Aliases:  []string{"freshness"}},
		{Name: string(VerbIngestPaths), Status: StatusStable, Surface: "jsonl",
			Summary:  "incrementally re-index explicit relative paths",
			Required: []string{"root", "paths"},
			Optional: []string{"index_cache"},
			Aliases:  []string{"ingest"}},
		{Name: string(VerbCodeExact), Status: StatusStable, Surface: "jsonl",
			Summary:  "precise exact-match lane over the code index",
			Required: []string{"root", "q"},
			Optional: []string{"kind", "top_k"},
			Aliases:  []string{"exact"}},
		{Name: string(VerbCodeDefs), Status: StatusStable, Surface: "jsonl",
			Summary:  "code_exact pinned to definitions",
			Required: []string{"root", "q"},
			Optional: []string{"top_k"},
			Aliases:  []string{"defs"}},
		{Name: string(VerbCodeRefs), Status: StatusStable, Surface: "jsonl",
			Summary:  "code_exact pinned to references",
			Required: []string{"root", "q"},
			Optional: []string{"top_k"},
			Aliases:  []string{"refs"}},
		{Name: string(VerbCodeImports), Status: StatusStable, Surface: "jsonl",
			Summary:  "code_exact pinned to imports (equivalent to code_exact kind=import)",
			Required: []string{"root", "q"},
			Optional: []string{"top_k"},
			Aliases:  []string{"imports"}},
		{Name: string(VerbMemoryAsk), Status: StatusStable, Surface: "jsonl",
			Summary:  "company-doc residual lane (not code)",
			Required: []string{"dir", "q"},
			Optional: []string{"session", "top_k"},
			Aliases:  []string{"memory-ask"}},
	}
}
