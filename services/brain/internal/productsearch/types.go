package productsearch

import "time"

// Profile selects which index surface to query.
type Profile string

const (
	ProfileCode   Profile = "code"   // codecrawl multi-crawler (heuristic)
	ProfileLocal  Profile = "local"  // durable FS projection via hosted.OpenLocal
	ProfileMemory Profile = "memory" // legacy alias → local
	ProfileHosted Profile = "hosted" // path2 / product neon from env
	// ProfileCodeExact is Stage P5 exact def/ref/import via codeindex (product facade).
	ProfileCodeExact Profile = "code_exact"
	// ProfileAuto: hosted if Hosted; else local if MemoryDir set; else code if
	// CodeRoot set; else local (fail with clear error if dir missing).
	ProfileAuto Profile = "auto"
)

// Request is one unified search/ask.
type Request struct {
	Profile  Profile
	Question string
	TopK     int
	// CodeRoot is required for ProfileCode (and Auto when no memory dir).
	CodeRoot string
	// MemoryDir + BrainID for local FS product brain (OpenLocal).
	MemoryDir string
	BrainID   string
	// Workers for code crawl.
	Workers int
	// SymbolHop enables stack-graph-style expand on code.
	SymbolHop bool
	// Hosted forces Neon+Qdrant path (env secrets).
	Hosted bool
	// SessionID enables product chat channel on local/hosted Ask (AnswerOpts).
	SessionID string
	// SourceTypes optional ERB source types for authority / prompt modes.
	SourceTypes []string
	// CodeIndexPath optional durable gob path for ProfileCode (default: <CodeRoot>/.ouroboros/code-index.gob).
	CodeIndexPath string
	// ForceFullCode reindexes code from scratch (ignores gob stamps).
	ForceFullCode bool
	// ExactKind filters ProfileCodeExact: "", any, definition, reference, import.
	ExactKind string
}

// Hit is one evidence unit (path or document id).
type Hit struct {
	ID    string  `json:"id"`
	Title string  `json:"title,omitempty"`
	Text  string  `json:"text,omitempty"`
	Score float64 `json:"score"`
	Arm   string  `json:"arm,omitempty"`
}

// Result is the unified response.
type Result struct {
	Answer     string   `json:"answer"`
	Hits       []Hit    `json:"hits"`
	CitedIDs   []string `json:"cited_ids"`
	SearchMode string   `json:"search_mode"`
	Profile    Profile  `json:"profile"`
	// Guarantee states retrieval promise (GAP-PLANE-CODE-SEARCH honesty).
	// Examples: "heuristic_workspace" (codecrawl) vs "exact_p5_codeindex".
	Guarantee            string         `json:"guarantee,omitempty"`
	Failure              string         `json:"failure,omitempty"`
	RetrievalDiagnostics map[string]any `json:"retrieval_diagnostics,omitempty"`
	DurationMs           int64          `json:"duration_ms"`
	ProductOwned         bool           `json:"product_owned"`
}

// Guarantee for each profile — not interchangeable planes.
const (
	GuaranteeHeuristicWorkspace = "heuristic_workspace"  // codecrawl multi-crawl
	GuaranteeExactP5Codeindex   = "exact_p5_codeindex"   // codeindex defs/refs/imports
	GuaranteeResidualCompany    = "residual_company_rag" // OpenLocal / OpenResidual product-owned
	// GuaranteePath2Eval is OpenFromEnv SMF path2 corpus (ERB bench) — never residual_company_rag.
	GuaranteePath2Eval = "path2_eval"
)

// SLAReceipt records one cold/delta measure against performance-targets.yaml.
// Durations are milliseconds in JSON (not raw ns) to match targets.yaml units.
type SLAReceipt struct {
	Operation   string         `json:"operation"`
	TargetMaxMs int64          `json:"target_max_ms"`
	ObservedMs  int64          `json:"observed_ms"`
	Pass        bool           `json:"pass"`
	Detail      map[string]any `json:"detail,omitempty"`
	TargetMax   time.Duration  `json:"-"`
	Observed    time.Duration  `json:"-"`
}

// FillMs populates millisecond fields from Duration fields.
func (r *SLAReceipt) FillMs() {
	if r == nil {
		return
	}
	r.TargetMaxMs = r.TargetMax.Milliseconds()
	r.ObservedMs = r.Observed.Milliseconds()
}
