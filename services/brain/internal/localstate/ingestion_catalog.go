// This file serves the read-only Stage 04 source catalog over the migration
// 003 source, generation, snapshot, and readiness tables. Reads stay scoped to
// one exact tenant/brain/source triple and carry no absolute or
// repository-relative path; the composing gateway authorizes the authenticated
// principal before calling, and unknown scopes fail without existence detail.
package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestionSourceState is the catalog view of one configured source: its
// immutable repository identity, its lifecycle state, and the current complete
// generation pointer when one exists. State is `ready` or `revoked`.
type IngestionSourceState struct {
	Scope               IngestionScope
	RepositoryID        string
	State               string
	CurrentGenerationID string
}

// IngestionGenerationFacts is the immutable catalog record of one published
// complete generation: snapshot identity, policy digest, watermark, and the
// five P5 lane readiness records. Facts never change per generation identity,
// so a superseded or revoked-source generation resolves exactly as published.
type IngestionGenerationFacts struct {
	Scope           IngestionScope
	RepositoryID    string
	GenerationID    string
	Sequence        uint64
	SnapshotID      string
	CommitOID       string
	TreeOID         string
	PolicyDigest    string
	State           string
	SourceWatermark uint64
	Readiness       []IngestionReadiness
}

// LoadIngestionSourceState returns the catalog state of one exact source
// scope. An absent scope returns ErrInvalidInput so the composing layer can
// collapse it to its non-disclosing denial.
func (s *Store) LoadIngestionSourceState(ctx context.Context, scope IngestionScope) (IngestionSourceState, error) {
	if s == nil || ctx == nil || !validIngestionScope(scope) {
		return IngestionSourceState{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return IngestionSourceState{}, ErrInvalidInput
	}
	state := IngestionSourceState{Scope: scope}
	err := s.db.QueryRowContext(ctx, `SELECT source.repository_id,source.state,
		COALESCE((SELECT pointer.generation_id FROM ingestion_current_generations pointer
			WHERE pointer.tenant_id=source.tenant_id AND pointer.brain_id=source.brain_id
			AND pointer.source_id=source.source_id),'')
		FROM ingestion_sources source
		WHERE source.tenant_id=? AND source.brain_id=? AND source.source_id=?`,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID).Scan(
		&state.RepositoryID, &state.State, &state.CurrentGenerationID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestionSourceState{}, ErrInvalidInput
	}
	if err != nil {
		return IngestionSourceState{}, fmt.Errorf("localstate: load ingestion source state: %w", err)
	}
	if !validBoundedID(state.RepositoryID) ||
		(state.State != "ready" && state.State != "admitted" && state.State != "revoked") ||
		(state.State == "revoked" && state.CurrentGenerationID != "") ||
		(state.CurrentGenerationID != "" && !validBoundedID(state.CurrentGenerationID)) {
		return IngestionSourceState{}, ErrIngestionConflict
	}
	return state, nil
}

// LoadIngestionGenerationFacts resolves one published complete generation by
// identity. Facts are immutable per generation, so superseded generations and
// generations of a later-revoked source resolve exactly as published; the
// composing layer owns authorization and revocation-aware denial. An absent
// generation, an incomplete `building` row, or a readiness set that is not the
// complete five-lane publication set returns ErrInvalidInput.
func (s *Store) LoadIngestionGenerationFacts(
	ctx context.Context,
	scope IngestionScope,
	generationID string,
) (IngestionGenerationFacts, error) {
	if s == nil || ctx == nil || !validIngestionScope(scope) || !validBoundedID(generationID) {
		return IngestionGenerationFacts{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return IngestionGenerationFacts{}, ErrInvalidInput
	}
	facts := IngestionGenerationFacts{Scope: scope}
	err := s.db.QueryRowContext(ctx, `SELECT source.repository_id,generation.generation_id,
		generation.generation_sequence,generation.snapshot_id,snapshot.commit_oid,snapshot.tree_oid,
		snapshot.policy_digest,generation.state,generation.source_watermark
		FROM ingestion_sources source
		JOIN ingestion_generations generation USING (tenant_id,brain_id,source_id)
		JOIN ingestion_snapshots snapshot
			ON snapshot.tenant_id=generation.tenant_id AND snapshot.brain_id=generation.brain_id
			AND snapshot.source_id=generation.source_id AND snapshot.snapshot_id=generation.snapshot_id
		WHERE source.tenant_id=? AND source.brain_id=? AND source.source_id=?
		AND generation.generation_id=? AND generation.state IN ('ready','degraded')`,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID, generationID).Scan(
		&facts.RepositoryID, &facts.GenerationID, &facts.Sequence, &facts.SnapshotID,
		&facts.CommitOID, &facts.TreeOID, &facts.PolicyDigest, &facts.State, &facts.SourceWatermark,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestionGenerationFacts{}, ErrInvalidInput
	}
	if err != nil {
		return IngestionGenerationFacts{}, fmt.Errorf("localstate: load ingestion generation facts: %w", err)
	}
	readiness, err := s.loadGenerationReadiness(ctx, scope, generationID)
	if err != nil {
		return IngestionGenerationFacts{}, err
	}
	facts.Readiness = readiness
	if !validBoundedID(facts.RepositoryID) || facts.Sequence == 0 || !validBoundedID(facts.SnapshotID) ||
		!isGitOID(facts.CommitOID) || !isGitOID(facts.TreeOID) || !isSHA256(facts.PolicyDigest) ||
		!completeLanguageSet(facts.Readiness) {
		return IngestionGenerationFacts{}, ErrIngestionConflict
	}
	return facts, nil
}

// loadGenerationReadiness returns the complete five-lane publication readiness
// of one generation in the frozen P5 language order. It runs under the store
// mutex held by the caller.
func (s *Store) loadGenerationReadiness(
	ctx context.Context,
	scope IngestionScope,
	generationID string,
) ([]IngestionReadiness, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT language,coverage,reason_code
		FROM ingestion_generation_readiness
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND generation_id=?
		ORDER BY language`,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID, generationID)
	if err != nil {
		return nil, fmt.Errorf("localstate: read ingestion readiness: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byLanguage := make(map[string]IngestionReadiness, len(p5Languages))
	for rows.Next() {
		var lane IngestionReadiness
		if err := rows.Scan(&lane.Language, &lane.Coverage, &lane.ReasonCode); err != nil {
			return nil, fmt.Errorf("localstate: scan ingestion readiness: %w", err)
		}
		byLanguage[lane.Language] = lane
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localstate: iterate ingestion readiness: %w", err)
	}
	readiness := make([]IngestionReadiness, 0, len(p5Languages))
	for _, language := range p5Languages {
		lane, found := byLanguage[language]
		if !found {
			return nil, ErrIngestionConflict
		}
		readiness = append(readiness, lane)
	}
	return readiness, nil
}
