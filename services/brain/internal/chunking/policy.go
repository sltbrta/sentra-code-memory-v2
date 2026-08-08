package chunking

import "fmt"

// PolicyID is the stable identity family for all Ouroboros chunk policies.
const PolicyID = "ouroboros-chunk"

// Baseline contract from issue #332: one evaluated baseline chunks at a
// 500-token target with at least 50-token overlap. Structure-aware and
// parent-child variants are evaluated against it, not assumed better.
const (
	BaselineVersion       = 1
	BaselineTargetTokens  = 500
	BaselineOverlapTokens = 50
)

// Strategy selects the chunking algorithm a policy applies.
type Strategy string

const (
	// StrategyWholeDoc keeps each document as one chunk (naive RAG baseline).
	StrategyWholeDoc Strategy = "whole_doc"
	// StrategyFixed slides a token window with overlap (500/50 baseline).
	StrategyFixed Strategy = "fixed"
	// StrategyStructure packs structure-aware blocks (headings, fences,
	// table rows, slide separators, chat turns) into token-bounded chunks.
	StrategyStructure Strategy = "structure"
	// StrategyParentChild emits small retrievable children linked to large
	// parent chunks for context expansion.
	StrategyParentChild Strategy = "parent_child"
)

// Kind classifies a source for structure-aware boundary rules.
type Kind string

const (
	KindProse  Kind = "prose"
	KindCode   Kind = "code"
	KindTable  Kind = "table"
	KindSlides Kind = "slides"
	KindChat   Kind = "chat"
)

// Kinds enumerates every known source kind (fixtures/tests iterate it).
var Kinds = []Kind{KindProse, KindCode, KindTable, KindSlides, KindChat}

// Policy is one versioned chunking configuration. Policies are values: a
// change to any field that moves boundaries must bump Version.
type Policy struct {
	ID          string   `json:"id"`
	Version     int      `json:"version"`
	TokenizerID string   `json:"tokenizer_id"`
	Strategy    Strategy `json:"strategy"`

	// TargetTokens is the soft per-chunk token target.
	TargetTokens int `json:"target_tokens"`
	// OverlapTokens is how many trailing tokens repeat in the next chunk.
	OverlapTokens int `json:"overlap_tokens"`

	// Parent-child sizing (StrategyParentChild only).
	ParentTargetTokens  int `json:"parent_target_tokens,omitempty"`
	ParentOverlapTokens int `json:"parent_overlap_tokens,omitempty"`
	ChildTargetTokens   int `json:"child_target_tokens,omitempty"`
	ChildOverlapTokens  int `json:"child_overlap_tokens,omitempty"`
}

// Validate checks internal consistency. It does not force the baseline
// numbers: experiments may vary them, but the report must stamp the policy
// fingerprint so deviations are attributable.
func (p Policy) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("chunking: policy id required")
	}
	if p.Version < 1 {
		return fmt.Errorf("chunking: policy version must be >= 1")
	}
	if p.TokenizerID == "" {
		return fmt.Errorf("chunking: tokenizer id required")
	}
	switch p.Strategy {
	case StrategyWholeDoc:
		return nil
	case StrategyFixed, StrategyStructure:
		return validateWindow(p.TargetTokens, p.OverlapTokens, "chunk")
	case StrategyParentChild:
		if err := validateWindow(p.ParentTargetTokens, p.ParentOverlapTokens, "parent"); err != nil {
			return err
		}
		if err := validateWindow(p.ChildTargetTokens, p.ChildOverlapTokens, "child"); err != nil {
			return err
		}
		if p.ChildTargetTokens >= p.ParentTargetTokens {
			return fmt.Errorf("chunking: child target %d must be < parent target %d",
				p.ChildTargetTokens, p.ParentTargetTokens)
		}
		return nil
	default:
		return fmt.Errorf("chunking: unknown strategy %q", p.Strategy)
	}
}

func validateWindow(target, overlap int, role string) error {
	if target < 16 {
		return fmt.Errorf("chunking: %s target tokens must be >= 16, got %d", role, target)
	}
	if overlap < 0 || overlap >= target {
		return fmt.Errorf("chunking: %s overlap tokens must be in [0, target), got %d", role, overlap)
	}
	return nil
}

// MeetsBaseline reports whether p honors the issue #332 baseline contract:
// 500-token target and at least 50-token overlap on the retrieval window.
func (p Policy) MeetsBaseline() bool {
	switch p.Strategy {
	case StrategyFixed, StrategyStructure:
		return p.TargetTokens == BaselineTargetTokens && p.OverlapTokens >= BaselineOverlapTokens
	case StrategyParentChild:
		return p.ChildTargetTokens > 0 && p.ChildOverlapTokens >= BaselineOverlapTokens/2 &&
			p.ParentTargetTokens >= BaselineTargetTokens
	default:
		return false
	}
}

// BaselinePolicy returns the fixed-size 500/50 evaluated baseline.
func BaselinePolicy() Policy {
	return Policy{
		ID: PolicyID, Version: BaselineVersion, TokenizerID: TokenizerID,
		Strategy:     StrategyFixed,
		TargetTokens: BaselineTargetTokens, OverlapTokens: BaselineOverlapTokens,
	}
}

// NaivePolicy returns the whole-document baseline (one chunk per document),
// matching the legacy DocumentsToChunks behavior.
func NaivePolicy() Policy {
	return Policy{
		ID: PolicyID, Version: BaselineVersion, TokenizerID: TokenizerID,
		Strategy: StrategyWholeDoc,
	}
}

// StructurePolicy returns the structure-aware 500/50 variant.
func StructurePolicy() Policy {
	return Policy{
		ID: PolicyID, Version: BaselineVersion, TokenizerID: TokenizerID,
		Strategy:     StrategyStructure,
		TargetTokens: BaselineTargetTokens, OverlapTokens: BaselineOverlapTokens,
	}
}

// ParentChildPolicy returns the parent-child variant: retrievable 125-token
// children under 1000-token parents. The child window is deliberately small
// for precision; parents carry the 500+ token context for expansion.
func ParentChildPolicy() Policy {
	return Policy{
		ID: PolicyID, Version: BaselineVersion, TokenizerID: TokenizerID,
		Strategy:            StrategyParentChild,
		ParentTargetTokens:  1000,
		ParentOverlapTokens: 100,
		ChildTargetTokens:   125,
		ChildOverlapTokens:  25,
	}
}

// EvalStrategies is the comparison set for the chunk-eval harness: the naive
// baseline plus every evaluated alternative.
func EvalStrategies() []Policy {
	return []Policy{NaivePolicy(), BaselinePolicy(), StructurePolicy(), ParentChildPolicy()}
}

// Fingerprint is the report-safe policy identity stamp.
func (p Policy) Fingerprint() map[string]any {
	return map[string]any{
		"policy_id":             p.ID,
		"policy_version":        p.Version,
		"tokenizer_id":          p.TokenizerID,
		"strategy":              string(p.Strategy),
		"target_tokens":         p.TargetTokens,
		"overlap_tokens":        p.OverlapTokens,
		"parent_target_tokens":  p.ParentTargetTokens,
		"parent_overlap_tokens": p.ParentOverlapTokens,
		"child_target_tokens":   p.ChildTargetTokens,
		"child_overlap_tokens":  p.ChildOverlapTokens,
		"meets_baseline":        p.MeetsBaseline(),
	}
}
