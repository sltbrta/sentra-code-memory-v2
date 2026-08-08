// Package-local factory composition: the Stage 05 sealed runner boundary
// exposed to the composing gateway command through plain boundary types. The
// deterministic v1 synthesizer lives here so the runner-internal Synthesizer
// port never leaks across the broker package boundary; Stage 6 model-provider
// execution plugs into the same port without touching this facade.
package localauthority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/effects"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/runner"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Bounded deterministic-synthesizer limits mirroring the frozen edit bounds.
const (
	factoryMaxScriptEdits   = 64
	factoryMaxProbePaths    = 64
	factoryMaxEditFileBytes = 1 << 20
)

// FactoryEditDirective is one deterministic v1 edit instruction resolved from
// the admitted approval descriptor — never from request-body shaping. Content
// derives deterministically from the exact-base bytes and the run identity.
type FactoryEditDirective struct {
	// Op is one of add, modify, delete, rename.
	Op string
	// Path is the normalized repository-relative post-image path.
	Path string
	// OldPath is the normalized pre-image path for rename edits only.
	OldPath string
}

// FactoryLeafScript is the deterministic v1 leaf program: an ordered edit
// directive list plus the leaf's declared forbidden paths, which the
// deterministic leaf probes once each to prove the sealed boundary denies
// them. Scripts derive from ledger state, not from untrusted request bytes.
type FactoryLeafScript struct {
	Edits      []FactoryEditDirective
	ProbePaths []string
}

// FactoryLeafGrant is the runner-facing attenuation of one served plan leaf
// grant: the same lease, fence, scope, base, expiry, and epoch pinned by the
// kernel, narrowed to the sealed file-effect vocabulary.
type FactoryLeafGrant struct {
	GrantID          string
	Initiator        string
	Tenant           string
	TaskID           string
	RunID            string
	LeaseID          string
	LeaseHolder      string
	LeaseFence       uint64
	LeaseExpiresAt   time.Time
	AllowedPaths     []string
	RepositoryGitOID string
	Nonce            string
	RevocationEpoch  uint64
	ExpiresAt        time.Time
	PolicyDigestHex  string
	CommandFence     uint64
}

// FactoryLeafSpec is one bounded leased leaf execution request.
type FactoryLeafSpec struct {
	RunID          string
	NodeID         string
	OwnedPaths     []string
	ForbiddenPaths []string
	BaseGitOID     string
	Identity       Identity
	Grant          FactoryLeafGrant
	ChangeSetID    string
	IdempotencyKey string
	Now            time.Time
}

// FactoryAppliedEdit is one edit the sealed leaf staged and applied, carrying
// its post-image bytes as process-local trace for deterministic gate
// evaluation. Callers must never persist the bytes.
type FactoryAppliedEdit struct {
	Op              string
	Path            string
	OldPath         string
	Language        string
	BeforeDigestHex string
	AfterDigestHex  string
	AfterBytes      []byte
}

// FactoryRollback records the deterministic discard of a rejected candidate.
type FactoryRollback struct {
	ReasonCode         string
	ChangeSetDigestHex string
	DiscardedEdits     int
	FailedEditIndex    int
}

// FactoryDenial is one brokered-effect denial in the leaf trace.
type FactoryDenial struct {
	Action     string
	Path       string
	ReasonCode string
}

// FactoryLeafOutcome is the terminal trace of one sealed leaf execution. It
// never represents canonical state; the kernel consumes the outcome facts
// with fence checks.
type FactoryLeafOutcome struct {
	State    string
	Edits    []FactoryAppliedEdit
	Rollback *FactoryRollback
	Denials  []FactoryDenial
}

// FactoryFenceRegistry resolves the current lease fence state from the
// kernel's durable roster facts. The composing command binds it to the served
// plan; the effect broker queries it at every mutation.
type FactoryFenceRegistry interface {
	CurrentFence(ctx context.Context, leaseID Identifier) (fence uint64, expiresAt time.Time, ok bool)
}

// FactoryRunnerConfig binds the sealed runner composition: the canonical
// approved root, the isolated candidate parent directory (never inside the
// canonical worktree), the pinned Git executable, bounded operation limits,
// the current-policy check port, the kernel-backed fence registry, and the
// live clock every reauthorization evaluates.
type FactoryRunnerConfig struct {
	CanonicalRoot  string
	CandidateRoot  string
	GitExecutable  string
	CommandTimeout time.Duration
	MaxFiles       int
	MaxFileBytes   int64
	MaxTotalBytes  int64
	Policy         shared.PolicyCheck
	Fences         FactoryFenceRegistry
	Clock          func() time.Time
}

// FactoryRunner executes bounded leased leaves against isolated exact-base
// candidates with current-policy reauthorization on every mutation.
type FactoryRunner struct {
	runner *runner.Runner
}

// OpenFactoryRunner composes the exact-base candidate store, the current-policy
// effect broker, and the sealed one-leaf runner. Every field is required; a
// misconfigured composition fails at construction, never at execution time.
func OpenFactoryRunner(config FactoryRunnerConfig) (*FactoryRunner, error) {
	if config.CanonicalRoot == "" || config.CandidateRoot == "" || config.GitExecutable == "" ||
		config.CommandTimeout <= 0 || config.MaxFiles <= 0 || config.MaxFileBytes <= 0 ||
		config.MaxTotalBytes <= 0 || config.Policy == nil || config.Fences == nil || config.Clock == nil {
		return nil, ErrInvalidConfig
	}
	store, err := gitcandidate.NewStore(gitcandidate.Config{
		CanonicalRoot:  config.CanonicalRoot,
		CandidateRoot:  config.CandidateRoot,
		GitExecutable:  config.GitExecutable,
		CommandTimeout: config.CommandTimeout,
		MaxFiles:       config.MaxFiles,
		MaxFileBytes:   config.MaxFileBytes,
		MaxTotalBytes:  config.MaxTotalBytes,
	})
	if err != nil {
		return nil, ErrInvalidConfig
	}
	broker, err := effects.NewBroker(config.Policy, fenceRegistryAdapter{config.Fences}, config.Clock)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	sealed, err := runner.New(store, broker)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	return &FactoryRunner{runner: sealed}, nil
}

// ExecuteLeaf runs one bounded leased leaf with the deterministic v1
// synthesizer to a terminal trace. A malformed spec or script, a wrong pinned
// base, any escape attempt, a stale lease or fence at mutation time, or any
// edit failure fails the leaf closed: the isolated candidate is discarded with
// its rollback receipt and canonical Git is never touched.
func (r *FactoryRunner) ExecuteLeaf(
	ctx context.Context, spec FactoryLeafSpec, script FactoryLeafScript,
) (FactoryLeafOutcome, error) {
	if r == nil || r.runner == nil || ctx == nil {
		return FactoryLeafOutcome{}, ErrDenied
	}
	inner, err := runnerSpec(spec)
	if err != nil {
		return FactoryLeafOutcome{}, err
	}
	synthesizer, err := newFactoryDeterministicSynthesizer(spec, script)
	if err != nil {
		return FactoryLeafOutcome{}, err
	}
	result, err := r.runner.RunLeaf(ctx, inner, synthesizer)
	if err != nil {
		return FactoryLeafOutcome{}, fmt.Errorf("%w: %v", ErrDenied, staticFactoryError(err))
	}
	outcome := FactoryLeafOutcome{
		State:   string(result.State),
		Edits:   synthesizer.staged,
		Denials: make([]FactoryDenial, 0, len(result.Denials)),
	}
	for _, record := range result.Denials {
		outcome.Denials = append(outcome.Denials, FactoryDenial{
			Action: record.Action, Path: record.Path, ReasonCode: record.ReasonCode,
		})
	}
	if result.Rollback != nil {
		outcome.Rollback = &FactoryRollback{
			ReasonCode:         result.Rollback.Receipt.ReasonCode,
			ChangeSetDigestHex: result.Rollback.ChangeSetDigest.Hex,
			DiscardedEdits:     result.Rollback.DiscardedEdits,
			FailedEditIndex:    result.Rollback.FailedEditIndex,
		}
	}
	return outcome, nil
}

// staticFactoryError collapses runner vocabulary to a non-disclosing reason
// code; upstream error text never crosses the composition boundary.
func staticFactoryError(err error) string {
	switch {
	case errors.Is(err, runner.ErrInvalid):
		return "invalid_leaf"
	case errors.Is(err, gitcandidate.ErrBase):
		return "base_mismatch"
	case errors.Is(err, gitcandidate.ErrDenied):
		return "effect_denied"
	default:
		return "execution_failed"
	}
}

// runnerSpec attenuates the boundary spec into the runner-internal leaf shape:
// the served factory identities map onto the runner's grant/lease namespaces,
// and the plan-level factory.leaf.execute authority narrows to the sealed
// file.read/file.write effect vocabulary inside the identical scope, lease,
// fence, base, expiry, and epoch.
func runnerSpec(spec FactoryLeafSpec) (runner.LeafSpec, error) {
	if spec.RunID == "" || spec.NodeID == "" || len(spec.OwnedPaths) == 0 ||
		len(spec.OwnedPaths) > factoryMaxScriptEdits || len(spec.ForbiddenPaths) > factoryMaxProbePaths ||
		spec.BaseGitOID == "" || spec.ChangeSetID == "" || spec.IdempotencyKey == "" || spec.Now.IsZero() {
		return runner.LeafSpec{}, ErrInvalidConfig
	}
	grant := spec.Grant
	lease := effects.Lease{
		LeaseID:   shared.Identifier{Namespace: "lease", Value: grant.LeaseID},
		Holder:    shared.Identifier{Namespace: "principal", Value: grant.LeaseHolder},
		Fence:     grant.LeaseFence,
		ExpiresAt: grant.LeaseExpiresAt,
	}
	return runner.LeafSpec{
		RunID:          shared.Identifier{Namespace: "run", Value: spec.RunID},
		NodeID:         spec.NodeID,
		OwnedPaths:     append([]string(nil), spec.OwnedPaths...),
		ForbiddenPaths: append([]string(nil), spec.ForbiddenPaths...),
		BaseGitOID:     spec.BaseGitOID,
		Identity:       spec.Identity,
		Grant: effects.Grant{
			GrantID:          shared.Identifier{Namespace: "grant", Value: grant.GrantID},
			Initiator:        shared.Identifier{Namespace: "principal", Value: grant.Initiator},
			Tenant:           shared.Identifier{Namespace: "tenant", Value: grant.Tenant},
			TaskID:           shared.Identifier{Namespace: "factory-task", Value: grant.TaskID},
			RunID:            shared.Identifier{Namespace: "factory-run", Value: grant.RunID},
			Lease:            lease,
			Actions:          []string{effects.ActionFileRead, effects.ActionFileWrite},
			Resources:        []shared.Identifier{{Namespace: "tenant", Value: grant.Tenant}},
			RepositoryGitOID: grant.RepositoryGitOID,
			AllowedPaths:     append([]string(nil), grant.AllowedPaths...),
			Nonce:            grant.Nonce,
			RevocationEpoch:  grant.RevocationEpoch,
			ExpiresAt:        grant.ExpiresAt,
			PolicyDigest:     shared.Digest{Algorithm: "sha256", Hex: grant.PolicyDigestHex},
			CommandFence:     grant.CommandFence,
		},
		ChangeSetID:    spec.ChangeSetID,
		IdempotencyKey: spec.IdempotencyKey,
		Now:            spec.Now,
	}, nil
}

// fenceRegistryAdapter binds the boundary fence registry to the effect
// broker's internal port.
type fenceRegistryAdapter struct {
	registry FactoryFenceRegistry
}

func (adapter fenceRegistryAdapter) CurrentFence(
	ctx context.Context, leaseID shared.Identifier,
) (uint64, time.Time, bool) {
	if adapter.registry == nil {
		return 0, time.Time{}, false
	}
	return adapter.registry.CurrentFence(ctx, leaseID)
}

// factoryDeterministicSynthesizer is the deterministic v1 leaf: it replays the
// admitted descriptor's edit directives through the sealed effect surface,
// deriving content deterministically from exact-base bytes and the run
// identity, and probes every declared forbidden path once so a hostile or
// mistaken boundary regression fails the run closed. It is the documented
// Stage 6 integration seam: model-provider execution implements the same
// runner.Synthesizer port against the identical sealed surface.
type factoryDeterministicSynthesizer struct {
	spec   FactoryLeafSpec
	script FactoryLeafScript
	staged []FactoryAppliedEdit
}

func newFactoryDeterministicSynthesizer(
	spec FactoryLeafSpec, script FactoryLeafScript,
) (*factoryDeterministicSynthesizer, error) {
	if len(script.Edits) == 0 && len(script.ProbePaths) == 0 {
		return nil, ErrInvalidConfig
	}
	if len(script.Edits) > factoryMaxScriptEdits || len(script.ProbePaths) > factoryMaxProbePaths {
		return nil, ErrInvalidConfig
	}
	for _, directive := range script.Edits {
		switch directive.Op {
		case "add", "delete":
			if directive.OldPath != "" {
				return nil, ErrInvalidConfig
			}
		case "modify":
			if directive.OldPath != "" {
				return nil, ErrInvalidConfig
			}
		case "rename":
			if directive.OldPath == "" || directive.OldPath == directive.Path {
				return nil, ErrInvalidConfig
			}
		default:
			return nil, ErrInvalidConfig
		}
		if factoryLanguageForPath(directive.Path) == "" {
			return nil, ErrInvalidConfig
		}
	}
	return &factoryDeterministicSynthesizer{spec: spec, script: script}, nil
}

// Synthesize plays the deterministic script through the sealed surface. Every
// broker denial is recorded by the surface and fails the run closed; any
// operational failure stops execution.
func (s *factoryDeterministicSynthesizer) Synthesize(
	ctx context.Context, _ runner.LeafSpec, fx runner.Effects,
) error {
	for _, probe := range s.script.ProbePaths {
		// The deterministic v1 leaf proves its sealed boundary by attempting one
		// write per declared forbidden path; the broker must deny every attempt.
		// The probe's language lane is irrelevant to the path authorization, so
		// it pins Go and lets a malformed probe path surface as an escape-shaped
		// denial through the same fail-closed path.
		err := fx.Propose(ctx, changeset.Edit{
			Path:         probe,
			Op:           changeset.OpModify,
			Lang:         changeset.LanguageGo,
			BeforeDigest: changeset.DigestBytes(nil),
			AfterDigest:  changeset.DigestBytes([]byte("ouroboros-factory-boundary-probe\n")),
			NewContent:   []byte("ouroboros-factory-boundary-probe\n"),
		})
		if err != nil && !errors.Is(err, effects.ErrDenied) {
			return err
		}
	}
	for _, directive := range s.script.Edits {
		edit, err := s.buildEdit(ctx, fx, directive)
		if err != nil {
			return err
		}
		if err := fx.Propose(ctx, edit); err != nil {
			return err
		}
		s.staged = append(s.staged, FactoryAppliedEdit{
			Op:              string(edit.Op),
			Path:            edit.Path,
			OldPath:         edit.OldPath,
			Language:        string(edit.Lang),
			BeforeDigestHex: edit.BeforeDigest.Hex,
			AfterDigestHex:  edit.AfterDigest.Hex,
			AfterBytes:      edit.NewContent,
		})
	}
	return nil
}

// buildEdit derives one digest-bound edit from the exact-base candidate bytes.
// Modify, delete, and rename read their pre-image through the brokered
// exact-base read; add derives fresh deterministic content.
func (s *factoryDeterministicSynthesizer) buildEdit(
	ctx context.Context, fx runner.Effects, directive FactoryEditDirective,
) (changeset.Edit, error) {
	language := changeset.Language(factoryLanguageForPath(directive.Path))
	switch directive.Op {
	case "add":
		content := s.deriveAdd(directive.Path)
		return changeset.Edit{
			Path: directive.Path, Op: changeset.OpAdd, Lang: language,
			AfterDigest: changeset.DigestBytes(content), NewContent: content,
		}, nil
	case "delete":
		base, err := fx.ReadFile(ctx, directive.Path)
		if err != nil {
			return changeset.Edit{}, err
		}
		return changeset.Edit{
			Path: directive.Path, Op: changeset.OpDelete, Lang: language,
			BeforeDigest: changeset.DigestBytes(base),
		}, nil
	case "modify":
		base, err := fx.ReadFile(ctx, directive.Path)
		if err != nil {
			return changeset.Edit{}, err
		}
		content := s.deriveModify(directive.Path, base)
		return changeset.Edit{
			Path: directive.Path, Op: changeset.OpModify, Lang: language,
			BeforeDigest: changeset.DigestBytes(base),
			AfterDigest:  changeset.DigestBytes(content), NewContent: content,
		}, nil
	case "rename":
		base, err := fx.ReadFile(ctx, directive.OldPath)
		if err != nil {
			return changeset.Edit{}, err
		}
		content := s.deriveModify(directive.Path, base)
		return changeset.Edit{
			Path: directive.Path, OldPath: directive.OldPath, Op: changeset.OpRename, Lang: language,
			BeforeDigest: changeset.DigestBytes(base),
			AfterDigest:  changeset.DigestBytes(content), NewContent: content,
		}, nil
	default:
		return changeset.Edit{}, ErrInvalidConfig
	}
}

// deriveModify appends the deterministic run-bound marker comment; the change
// is exactly reproducible from the run identity and the base bytes.
func (s *factoryDeterministicSynthesizer) deriveModify(path string, base []byte) []byte {
	return append(append([]byte(nil), base...), []byte(s.marker(path))...)
}

// deriveAdd authors the deterministic new-file content for one added path.
func (s *factoryDeterministicSynthesizer) deriveAdd(path string) []byte {
	return []byte(s.marker(path) + factoryPackageClause(path))
}

// marker binds the edit to its run, node, and path so identical runs replay
// byte-identical content and differing runs never collide.
func (s *factoryDeterministicSynthesizer) marker(path string) string {
	comment := "//"
	if strings.HasSuffix(path, ".py") {
		comment = "#"
	}
	return fmt.Sprintf("%s ouroboros-factory run=%s node=%s path=%s\n",
		comment, s.spec.RunID, s.spec.NodeID, path)
}

// factoryPackageClause keeps derived Go sources parseable for the
// deterministic test gate; other lanes need no clause.
func factoryPackageClause(path string) string {
	if strings.HasSuffix(path, ".go") {
		return "package main\n"
	}
	return ""
}

// factoryLanguageForPath maps one repository-relative path to its P5 lane.
func factoryLanguageForPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return string(changeset.LanguageGo)
	case strings.HasSuffix(path, ".ts"):
		return string(changeset.LanguageTypeScript)
	case strings.HasSuffix(path, ".py"):
		return string(changeset.LanguagePython)
	case strings.HasSuffix(path, ".rs"):
		return string(changeset.LanguageRust)
	case strings.HasSuffix(path, ".java"):
		return string(changeset.LanguageJava)
	default:
		return ""
	}
}
