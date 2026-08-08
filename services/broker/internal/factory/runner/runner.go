// Package runner implements the Stage 05 sealed runner boundary. It executes
// exactly one bounded, leased leaf against an isolated exact-base candidate:
// the leaf receives a sealed effect surface with no dispatch, task-creation,
// shell, network, Git, model, credential, plugin, or instruction authority,
// and every effect is externally brokered with current identity, grant,
// policy, lease, fence, and idempotency reauthorization immediately before
// execution. An escape attempt denies and fails the run closed.
//
// Runner output is trace only: canonical state changes flow through the
// kernel, never through this boundary, and the canonical worktree plus
// complete .git inventory stay byte-identical across success and failure.
//
// The Synthesizer port is the integration seam: v1 executes the deterministic
// fixture-driven synthesizer in process, and a model-provider execution plugs
// in later against the same sealed Effects surface without changing the
// broker, candidate, or failure semantics.
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/effects"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// ErrInvalid is the static rejection for a malformed leaf specification.
var ErrInvalid = errors.New("runner: invalid leaf specification")

// RunState is the bounded terminal state of one sealed leaf execution.
type RunState string

const (
	// RunCompleted means the leaf's candidate applied atomically.
	RunCompleted RunState = "COMPLETED"
	// RunFailed means the run failed closed; any candidate was discarded.
	RunFailed RunState = "FAILED"
)

// LeafSpec is one bounded leaf: exact owned and forbidden scope, fenced
// lease, attenuated grant pinned to the intent Git base, and the idempotency
// identity of its candidate application.
type LeafSpec struct {
	RunID          contracts.Identifier
	NodeID         string
	OwnedPaths     []string
	ForbiddenPaths []string
	BaseGitOID     string
	Identity       contracts.MappedIdentityFact
	Grant          effects.Grant
	ChangeSetID    string
	IdempotencyKey string
	Now            time.Time
}

// Effects is the sealed effect surface handed to one leaf. It is the only
// authority a leaf can reach: every call is brokered, and no dispatch,
// task-creation, shell, network, Git, model, or credential operation exists
// on this surface at all.
type Effects interface {
	// Propose stages one candidate edit after validation and authorization.
	Propose(ctx context.Context, edit changeset.Edit) error
	// ReadFile returns exact-base candidate bytes for one in-scope path.
	ReadFile(ctx context.Context, repositoryPath string) ([]byte, error)
	// RequestEffect asks for one brokered effect by action; anything beyond
	// the sealed file surface denies and fails the run closed.
	RequestEffect(ctx context.Context, action string, repositoryPath string) error
}

// Synthesizer is the leaf execution port. Implementations receive only the
// sealed Effects surface and produce candidate edits through it. The v1
// deterministic synthesizer is fixture-driven; model-provider execution
// integrates here later.
type Synthesizer interface {
	Synthesize(ctx context.Context, leaf LeafSpec, fx Effects) error
}

// DenialRecord is one brokered-effect denial in the run trace.
type DenialRecord struct {
	Action     string
	Path       string
	ReasonCode string
}

// Result is the trace of one sealed leaf execution. It never represents
// canonical state; the kernel consumes the outcome facts with fence checks.
type Result struct {
	State    RunState
	Outcome  *gitcandidate.Outcome
	Rollback *gitcandidate.RollbackReceipt
	Denials  []DenialRecord
}

// Runner executes bounded leaves against the candidate store and broker.
type Runner struct {
	store  *gitcandidate.Store
	broker *effects.Broker
}

// New returns a sealed runner over one candidate store and effect broker.
func New(store *gitcandidate.Store, broker *effects.Broker) (*Runner, error) {
	if store == nil || broker == nil {
		return nil, ErrInvalid
	}
	return &Runner{store: store, broker: broker}, nil
}

// RunLeaf executes one bounded leaf to a terminal trace. A malformed spec,
// wrong pinned base, any escape attempt, a stale lease or fence at mutation
// time, or any edit failure fails the run closed: the isolated candidate is
// discarded, the rollback receipt is recorded, and canonical Git is never
// touched. An exact idempotent replay returns the recorded outcome.
func (r *Runner) RunLeaf(ctx context.Context, spec LeafSpec, synthesizer Synthesizer) (Result, error) {
	if ctx == nil || synthesizer == nil {
		return Result{}, ErrInvalid
	}
	if err := validateSpec(spec); err != nil {
		return Result{}, err
	}
	leaf := effects.Leaf{
		Identity: spec.Identity,
		Grant:    spec.Grant,
		Scope: effects.Scope{
			OwnedPaths:     spec.OwnedPaths,
			ForbiddenPaths: spec.ForbiddenPaths,
			BaseGitOID:     spec.BaseGitOID,
		},
	}
	// Admission-time current check: grant shape, base binding, lease, fence,
	// epoch, and policy are all revalidated before a candidate exists.
	if err := r.broker.Authorize(ctx, leaf, effects.Request{
		Action:   effects.ActionFileWrite,
		Path:     spec.OwnedPaths[0],
		Resource: spec.Grant.Resources[0],
		Now:      spec.Now,
	}); err != nil {
		return failClosed(nil, []DenialRecord{denial(effects.ActionFileWrite, spec.OwnedPaths[0], err)}), nil
	}
	candidate, err := r.store.Begin(ctx, spec.BaseGitOID)
	if err != nil {
		reason := effects.ReasonPolicyDenied
		if errors.Is(err, gitcandidate.ErrBase) {
			reason = effects.ReasonBaseMismatch
		}
		return failClosed(nil, []DenialRecord{{
			Action:     "candidate.begin",
			Path:       spec.BaseGitOID,
			ReasonCode: reason,
		}}), nil
	}
	surface := &sealedEffects{
		leaf:      leaf,
		broker:    r.broker,
		candidate: candidate,
	}
	synthErr := synthesizer.Synthesize(ctx, spec, surface)
	if synthErr != nil || len(surface.denials) > 0 {
		denials := surface.denials
		if synthErr != nil {
			// Record the failure fact uniformly, whether the synthesizer
			// returned a typed denial or an operational error.
			reason := effects.ReasonCode(synthErr)
			if !errors.Is(synthErr, effects.ErrDenied) {
				reason = effects.ReasonPolicyDenied
			}
			denials = append(denials, DenialRecord{Action: "leaf.synthesize", ReasonCode: reason})
		}
		// Escape denials discard with the escape reason; a plain synthesizer
		// failure or non-escape denial is an ordinary rejection.
		reason := gitcandidate.ReasonRejected
		for _, record := range denials {
			if strings.HasPrefix(record.ReasonCode, "escape_") {
				reason = gitcandidate.ReasonEscapeDenied
				break
			}
		}
		receipt, discardErr := candidate.Discard(reason, stagedDigest(spec, surface))
		return failClosed(receipt, denials), discardErr
	}
	if len(surface.staged) == 0 {
		receipt, discardErr := candidate.Discard(gitcandidate.ReasonRejected, contracts.Digest{})
		return failClosed(receipt, surface.denials), discardErr
	}
	outcome, applyErr := r.store.Apply(ctx, candidate, gitcandidate.ApplyRequest{
		ChangeSetID:    spec.ChangeSetID,
		IdempotencyKey: spec.IdempotencyKey,
		Tenant:         spec.Identity.Tenant,
		Principal:      spec.Identity.Principal,
		Edits:          surface.staged,
		Authorizer:     r.broker.Bind(leaf),
	})
	if applyErr != nil {
		if outcome == nil || outcome.Rollback == nil {
			// A pre-execution rejection leaves no outcome: discard the hydrated
			// candidate and record its rollback receipt before returning the
			// typed cause, so no candidate directory ever leaks.
			receipt, discardErr := candidate.Discard(gitcandidate.ReasonRejected, stagedDigest(spec, surface))
			result := failClosed(receipt, surface.denials)
			if outcome != nil {
				result.Outcome = outcome
			}
			if discardErr != nil {
				return result, discardErr
			}
			return result, applyErr
		}
		denials := surface.denials
		if errors.Is(applyErr, gitcandidate.ErrDenied) {
			denials = append(denials, DenialRecord{
				Action:     effects.ActionFileWrite,
				ReasonCode: effects.ReasonCode(applyErr),
			})
		}
		result := failClosed(outcome.Rollback, denials)
		result.Outcome = outcome
		return result, nil
	}
	if outcome.State != gitcandidate.StateApplied {
		// An exact replay of a rejected application replays the rejection:
		// the run failed closed originally and must stay failed.
		result := failClosed(outcome.Rollback, surface.denials)
		result.Outcome = outcome
		return result, nil
	}
	return Result{
		State:   RunCompleted,
		Outcome: outcome,
		Denials: surface.denials,
	}, nil
}

// failClosed builds the terminal failed-run trace. The candidate is already
// discarded by the caller; canonical Git was never touched.
func failClosed(receipt *gitcandidate.RollbackReceipt, denials []DenialRecord) Result {
	if denials == nil {
		denials = []DenialRecord{}
	}
	return Result{State: RunFailed, Rollback: receipt, Denials: denials}
}

func denial(action, path string, err error) DenialRecord {
	return DenialRecord{Action: action, Path: path, ReasonCode: effects.ReasonCode(err)}
}

// stagedDigest binds a rollback receipt to the staged edit set. When nothing
// was staged, every discard path records the same canonical empty digest.
func stagedDigest(spec LeafSpec, surface *sealedEffects) contracts.Digest {
	if len(surface.staged) == 0 {
		return contracts.Digest{}
	}
	return gitcandidate.ChangeSetDigest(spec.BaseGitOID, surface.staged)
}

// sealedEffects is the runner-owned implementation of the leaf surface.
type sealedEffects struct {
	leaf      effects.Leaf
	broker    *effects.Broker
	candidate *gitcandidate.Candidate
	staged    []changeset.Edit
	denials   []DenialRecord
}

// Propose validates and authorizes one candidate edit, then stages it. Any
// validation or authorization failure is recorded and returned; the runner
// fails the run closed on any denial.
func (s *sealedEffects) Propose(ctx context.Context, edit changeset.Edit) error {
	if err := changeset.ValidateEdit(edit); err != nil {
		reason := effects.ReasonInvalidEdit
		if changeset.ValidatePath(edit.Path) != nil || changeset.ValidatePath(edit.OldPath) != nil && edit.Op == changeset.OpRename {
			reason = effects.ReasonEscapePathShape
		}
		s.denials = append(s.denials, DenialRecord{
			Action:     effects.ActionFileWrite,
			Path:       edit.Path,
			ReasonCode: reason,
		})
		return fmt.Errorf("%w: %v", effects.ErrDenied, err)
	}
	bound := s.broker.Bind(s.leaf)
	if err := bound.AuthorizeMutation(ctx, gitcandidate.Mutation{Index: len(s.staged), Edit: edit}); err != nil {
		s.denials = append(s.denials, denial(effects.ActionFileWrite, edit.Path, err))
		return err
	}
	s.staged = append(s.staged, edit)
	return nil
}

// ReadFile authorizes and performs one exact-base candidate read. The request
// carries no pinned time, so the broker reauthorizes against the live clock:
// a grant or lease expiring mid-run denies the read.
func (s *sealedEffects) ReadFile(ctx context.Context, repositoryPath string) ([]byte, error) {
	if err := s.broker.Authorize(ctx, s.leaf, effects.Request{
		Action:   effects.ActionFileRead,
		Path:     repositoryPath,
		Resource: s.leaf.Grant.Resources[0],
	}); err != nil {
		s.denials = append(s.denials, denial(effects.ActionFileRead, repositoryPath, err))
		return nil, err
	}
	return s.candidate.ReadFile(repositoryPath)
}

// RequestEffect is the generic brokered-effect escape valve. Anything beyond
// the sealed file surface — dispatch, task creation, shell, network, Git, or
// model effects — denies and is recorded as an escape attempt. Like reads,
// requests reauthorize against the live broker clock.
func (s *sealedEffects) RequestEffect(ctx context.Context, action string, repositoryPath string) error {
	err := s.broker.Authorize(ctx, s.leaf, effects.Request{
		Action:   action,
		Path:     repositoryPath,
		Resource: s.leaf.Grant.Resources[0],
	})
	if err != nil {
		s.denials = append(s.denials, denial(action, repositoryPath, err))
		return err
	}
	return nil
}

// StepKind is one deterministic synthesizer step kind.
type StepKind string

const (
	// StepPropose stages one edit through the sealed surface.
	StepPropose StepKind = "propose"
	// StepEffect requests one raw brokered effect.
	StepEffect StepKind = "effect"
	// StepRead performs one brokered candidate read.
	StepRead StepKind = "read"
)

// Step is one deterministic synthesizer step.
type Step struct {
	Kind   StepKind
	Edit   changeset.Edit
	Action string
	Path   string
}

// ScriptedSynthesizer is the deterministic fixture-driven v1 leaf. It plays
// an exact step list through the sealed surface, continuing after broker
// denials so hostile fixtures exercise every escape attempt; any
// non-authorization failure stops execution.
type ScriptedSynthesizer struct {
	Steps []Step
}

// Synthesize plays the frozen script through the sealed effect surface.
func (s ScriptedSynthesizer) Synthesize(ctx context.Context, _ LeafSpec, fx Effects) error {
	for _, step := range s.Steps {
		var err error
		switch step.Kind {
		case StepPropose:
			err = fx.Propose(ctx, step.Edit)
		case StepEffect:
			err = fx.RequestEffect(ctx, step.Action, step.Path)
		case StepRead:
			_, err = fx.ReadFile(ctx, step.Path)
		default:
			err = fmt.Errorf("runner: unknown script step %q", step.Kind)
		}
		if err != nil && !errors.Is(err, effects.ErrDenied) {
			return err
		}
	}
	return nil
}

// FixtureSynthesizer returns the deterministic v1 leaf that proposes exactly
// the given edits in order, mirroring the frozen factory fixture cases.
func FixtureSynthesizer(edits []changeset.Edit) ScriptedSynthesizer {
	steps := make([]Step, 0, len(edits))
	for _, edit := range edits {
		steps = append(steps, Step{Kind: StepPropose, Edit: edit})
	}
	return ScriptedSynthesizer{Steps: steps}
}

// validateSpec enforces the bounded leaf shape before any candidate exists:
// identities, normalized non-empty owned scope, normalized forbidden scope,
// an exact pinned base, grant validity without dispatch authority, and the
// base binding between spec and grant.
func validateSpec(spec LeafSpec) error {
	if spec.RunID.Namespace != "run" || spec.RunID.Value == "" ||
		spec.NodeID == "" || len(spec.NodeID) > 64 ||
		spec.ChangeSetID == "" || spec.IdempotencyKey == "" || spec.Now.IsZero() ||
		len(spec.OwnedPaths) == 0 || len(spec.OwnedPaths) > 64 || len(spec.ForbiddenPaths) > 64 ||
		(len(spec.BaseGitOID) != 40 && len(spec.BaseGitOID) != 64) {
		return ErrInvalid
	}
	for _, owned := range spec.OwnedPaths {
		if err := changeset.ValidatePath(owned); err != nil {
			return ErrInvalid
		}
	}
	for _, forbidden := range spec.ForbiddenPaths {
		if err := changeset.ValidatePath(forbidden); err != nil {
			return ErrInvalid
		}
	}
	if err := effects.ValidateGrant(spec.Grant, spec.Now); err != nil {
		return ErrInvalid
	}
	if spec.Grant.RepositoryGitOID != spec.BaseGitOID {
		return ErrInvalid
	}
	return nil
}
