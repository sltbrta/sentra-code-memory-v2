package localstorage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// EvidenceRepository persists immutable evidence and lineage metadata under the
// complete tenant, brain, and evidence scope. Tombstones deny reads immediately
// and remove only rebuildable lineage; artifact bytes remain in ArtifactVault.
type EvidenceRepository struct {
	authority *localstate.Store
}

// Put records one immutable evidence reference. Exact retries return false with
// no error; changed duplicates return evidenceledger.ErrConflict.
func (r *EvidenceRepository) Put(ctx context.Context, record evidenceledger.Record) (bool, error) {
	if r == nil || r.authority == nil || !validEvidence(record) {
		return false, evidenceledger.ErrInvalid
	}
	return writeResult(ctx, r.authority, func(writer localstate.MetadataWriter) (bool, error) {
		var status, digest string
		err := writer.QueryRowContext(ctx, `SELECT status,content_digest FROM artifact_manifests
			WHERE tenant_id=? AND artifact_id=? AND generation=?`, record.Tenant.Value,
			record.Artifact.Value, record.Generation).Scan(&status, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			return false, evidenceledger.ErrNotFound
		}
		if err != nil {
			return false, operationError(ctx, "read evidence artifact")
		}
		if status != "published" {
			return false, evidenceledger.ErrNotFound
		}
		if digest != record.Digest.Hex {
			return false, evidenceledger.ErrConflict
		}
		existing, _, err := loadEvidence(ctx, writer, record.Tenant.Value, record.Brain.Value, record.Evidence.Value)
		if err == nil {
			if existing == record {
				return false, nil
			}
			return false, evidenceledger.ErrConflict
		}
		if !errors.Is(err, evidenceledger.ErrNotFound) {
			return false, err
		}
		_, err = writer.ExecContext(ctx, `INSERT INTO evidence_records
		(tenant_id,brain_id,evidence_id,artifact_id,artifact_generation,anchor,
		 digest_algorithm,digest_hex,tombstoned) VALUES (?,?,?,?,?,?,?,?,0)`,
			record.Tenant.Value, record.Brain.Value, record.Evidence.Value, record.Artifact.Value,
			record.Generation, record.Anchor, record.Digest.Algorithm, record.Digest.Hex)
		if err != nil {
			return false, evidenceledger.ErrConflict
		}
		return true, nil
	})
}

// Get returns one exact readable record. Missing, cross-scope, and tombstoned
// records uniformly return evidenceledger.ErrNotFound.
func (r *EvidenceRepository) Get(ctx context.Context, tenant, brain, evidence contracts.Identifier) (evidenceledger.Record, error) {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") || !validID(brain, "brain") ||
		!validID(evidence, "evidence") {
		return evidenceledger.Record{}, evidenceledger.ErrInvalid
	}
	return readResult(ctx, r.authority, func(reader queryer) (evidenceledger.Record, error) {
		record, tombstoned, err := loadEvidence(ctx, reader, tenant.Value, brain.Value, evidence.Value)
		if err != nil {
			return evidenceledger.Record{}, err
		}
		if tombstoned {
			return evidenceledger.Record{}, evidenceledger.ErrNotFound
		}
		return record, nil
	})
}

// PutLineageIfEndpointsReadable atomically verifies both same-scope endpoints and
// inserts a typed edge. Exact retries return false; tombstoned or absent endpoints
// return the uniform evidenceledger.ErrNotFound.
func (r *EvidenceRepository) PutLineageIfEndpointsReadable(ctx context.Context, edge evidenceledger.Lineage) (bool, error) {
	if r == nil || r.authority == nil || !validLineage(edge) {
		return false, evidenceledger.ErrInvalid
	}
	return writeResult(ctx, r.authority, func(writer localstate.MetadataWriter) (bool, error) {
		for _, endpoint := range []contracts.Identifier{edge.Parent, edge.Child} {
			_, tombstoned, err := loadEvidence(ctx, writer, edge.Tenant.Value, edge.Brain.Value, endpoint.Value)
			if err != nil || tombstoned {
				return false, evidenceledger.ErrNotFound
			}
		}
		result, err := writer.ExecContext(ctx, `INSERT OR IGNORE INTO evidence_lineage
		(tenant_id,brain_id,parent_evidence_id,child_evidence_id,relation) VALUES (?,?,?,?,?)`,
			edge.Tenant.Value, edge.Brain.Value, edge.Parent.Value, edge.Child.Value, edge.Relation)
		if err != nil {
			return false, operationError(ctx, "insert evidence lineage")
		}
		created, err := result.RowsAffected()
		if err != nil {
			return false, operationError(ctx, "inspect evidence lineage")
		}
		return created == 1, nil
	})
}

// Lineage returns a bounded, deterministic list of edges touching one exact
// evidence ID. Callers must first verify the record is readable.
func (r *EvidenceRepository) Lineage(ctx context.Context, tenant, brain, evidence contracts.Identifier) ([]evidenceledger.Lineage, error) {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") || !validID(brain, "brain") ||
		!validID(evidence, "evidence") {
		return nil, evidenceledger.ErrInvalid
	}
	return readResult(ctx, r.authority, func(reader queryer) ([]evidenceledger.Lineage, error) {
		rows, err := reader.QueryContext(ctx, `SELECT parent_evidence_id,child_evidence_id,relation
		FROM evidence_lineage WHERE tenant_id=? AND brain_id=?
		AND (parent_evidence_id=? OR child_evidence_id=?)
		ORDER BY parent_evidence_id,child_evidence_id,relation LIMIT 4097`,
			tenant.Value, brain.Value, evidence.Value, evidence.Value)
		if err != nil {
			return nil, operationError(ctx, "read evidence lineage")
		}
		defer rows.Close()
		edges := make([]evidenceledger.Lineage, 0)
		for rows.Next() {
			edge := evidenceledger.Lineage{Tenant: tenant, Brain: brain}
			if err := rows.Scan(&edge.Parent.Value, &edge.Child.Value, &edge.Relation); err != nil {
				return nil, operationError(ctx, "scan evidence lineage")
			}
			edge.Parent.Namespace = "evidence"
			edge.Child.Namespace = "evidence"
			edges = append(edges, edge)
			if len(edges) > 4096 {
				return nil, ErrUnavailable
			}
		}
		if err := rows.Err(); err != nil {
			return nil, operationError(ctx, "iterate evidence lineage")
		}
		return edges, nil
	})
}

// Tombstone atomically denies one exact evidence record and removes its lineage.
// Exact retries succeed; absent and cross-scope records return ErrNotFound.
func (r *EvidenceRepository) Tombstone(ctx context.Context, tenant, brain, evidence contracts.Identifier) error {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") || !validID(brain, "brain") ||
		!validID(evidence, "evidence") {
		return evidenceledger.ErrInvalid
	}
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		_, _, err := loadEvidence(ctx, writer, tenant.Value, brain.Value, evidence.Value)
		if errors.Is(err, evidenceledger.ErrNotFound) {
			return evidenceledger.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := writer.ExecContext(ctx, `UPDATE evidence_records SET tombstoned=1
		WHERE tenant_id=? AND brain_id=? AND evidence_id=?`, tenant.Value, brain.Value, evidence.Value); err != nil {
			return operationError(ctx, "tombstone evidence")
		}
		if _, err := writer.ExecContext(ctx, `DELETE FROM evidence_lineage WHERE tenant_id=? AND brain_id=?
		AND (parent_evidence_id=? OR child_evidence_id=?)`,
			tenant.Value, brain.Value, evidence.Value, evidence.Value); err != nil {
			return operationError(ctx, "remove evidence lineage")
		}
		return nil
	})
}

func loadEvidence(ctx context.Context, query queryer, tenant, brain, evidence string) (evidenceledger.Record, bool, error) {
	var record evidenceledger.Record
	var tombstoned int
	record.Tenant = contracts.Identifier{Namespace: "tenant", Value: tenant}
	record.Brain = contracts.Identifier{Namespace: "brain", Value: brain}
	record.Evidence = contracts.Identifier{Namespace: "evidence", Value: evidence}
	err := query.QueryRowContext(ctx, `SELECT artifact_id,artifact_generation,anchor,
		digest_algorithm,digest_hex,tombstoned FROM evidence_records
		WHERE tenant_id=? AND brain_id=? AND evidence_id=?`, tenant, brain, evidence).
		Scan(&record.Artifact.Value, &record.Generation, &record.Anchor,
			&record.Digest.Algorithm, &record.Digest.Hex, &tombstoned)
	if errors.Is(err, sql.ErrNoRows) {
		return evidenceledger.Record{}, false, evidenceledger.ErrNotFound
	}
	if err != nil || (tombstoned != 0 && tombstoned != 1) {
		return evidenceledger.Record{}, false, operationError(ctx, "read evidence")
	}
	record.Artifact.Namespace = "artifact"
	if !validEvidence(record) {
		return evidenceledger.Record{}, false, operationError(ctx, "validate evidence")
	}
	return record, tombstoned == 1, nil
}
