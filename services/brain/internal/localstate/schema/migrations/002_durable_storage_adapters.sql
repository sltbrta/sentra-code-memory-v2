-- Stage 02 durable storage adapter metadata. Apply only after migration 001 in
-- its own transaction. Rollback removes the whole version without changing v1.
CREATE TABLE artifact_reservation_fences (
    fence INTEGER PRIMARY KEY AUTOINCREMENT
);
CREATE TABLE artifact_reservations (
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    locator TEXT NOT NULL CHECK (length(locator) > 0 AND length(locator) <= 1024),
    reservation_fence INTEGER NOT NULL UNIQUE CHECK (reservation_fence > 0),
    PRIMARY KEY (tenant_id, artifact_id, generation),
    FOREIGN KEY (tenant_id, artifact_id, generation)
    REFERENCES artifact_manifests (tenant_id, artifact_id, generation) ON DELETE CASCADE,
    FOREIGN KEY (reservation_fence) REFERENCES artifact_reservation_fences (fence)
);
CREATE TRIGGER artifact_reservation_immutable
BEFORE UPDATE ON artifact_reservations
BEGIN
    SELECT raise(ABORT, 'artifact reservation is immutable');
END;
CREATE UNIQUE INDEX one_current_key_epoch_per_tenant
ON key_epochs (tenant_id)
WHERE state = 'current';
CREATE TABLE evidence_records (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    artifact_generation INTEGER NOT NULL CHECK (artifact_generation > 0),
    anchor TEXT NOT NULL CHECK (length(anchor) > 0 AND length(anchor) <= 4096),
    digest_algorithm TEXT NOT NULL CHECK (digest_algorithm = 'sha256'),
    digest_hex TEXT NOT NULL,
    tombstoned INTEGER NOT NULL DEFAULT 0 CHECK (tombstoned IN (0, 1)),
    PRIMARY KEY (tenant_id, brain_id, evidence_id)
);
CREATE TABLE evidence_lineage (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    parent_evidence_id TEXT NOT NULL,
    child_evidence_id TEXT NOT NULL,
    relation TEXT NOT NULL CHECK (length(relation) > 0 AND length(relation) <= 128),
    PRIMARY KEY (tenant_id, brain_id, parent_evidence_id, child_evidence_id, relation),
    FOREIGN KEY (tenant_id, brain_id, parent_evidence_id)
    REFERENCES evidence_records (tenant_id, brain_id, evidence_id),
    FOREIGN KEY (tenant_id, brain_id, child_evidence_id)
    REFERENCES evidence_records (tenant_id, brain_id, evidence_id),
    CHECK (parent_evidence_id != child_evidence_id)
);
CREATE INDEX evidence_lineage_parent
ON evidence_lineage (tenant_id, brain_id, parent_evidence_id);
CREATE INDEX evidence_lineage_child
ON evidence_lineage (tenant_id, brain_id, child_evidence_id);
CREATE TRIGGER evidence_record_immutable
BEFORE UPDATE ON evidence_records
WHEN
    old.tenant_id IS NOT new.tenant_id
    OR old.brain_id IS NOT new.brain_id
    OR old.evidence_id IS NOT new.evidence_id
    OR old.artifact_id IS NOT new.artifact_id
    OR old.artifact_generation IS NOT new.artifact_generation
    OR old.anchor IS NOT new.anchor
    OR old.digest_algorithm IS NOT new.digest_algorithm
    OR old.digest_hex IS NOT new.digest_hex
BEGIN
    SELECT raise(ABORT, 'evidence record is immutable');
END;
CREATE TRIGGER evidence_tombstone_forward_only
BEFORE UPDATE OF tombstoned ON evidence_records
WHEN NOT (new.tombstoned = old.tombstoned OR (old.tombstoned = 0 AND new.tombstoned = 1))
BEGIN
    SELECT raise(ABORT, 'evidence tombstone cannot be removed');
END;
CREATE TRIGGER evidence_lineage_immutable
BEFORE UPDATE ON evidence_lineage
BEGIN
    SELECT raise(ABORT, 'evidence lineage is immutable');
END;
CREATE TABLE artifact_storage_tombstones (
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    request_reason_code TEXT NOT NULL CHECK (
        length(request_reason_code) > 0 AND length(request_reason_code) <= 128
    ),
    receipt_operation_namespace TEXT NOT NULL CHECK (receipt_operation_namespace = 'artifact-operation'),
    receipt_operation_value TEXT NOT NULL,
    receipt_status TEXT NOT NULL CHECK (receipt_status = 'tombstoned'),
    receipt_reason_code TEXT NOT NULL CHECK (receipt_reason_code = 'OURO-ARTIFACT-TOMBSTONED'),
    receipt_watermark INTEGER NOT NULL CHECK (
        receipt_watermark > 0 AND receipt_watermark = generation
    ),
    PRIMARY KEY (tenant_id, artifact_id, generation),
    FOREIGN KEY (tenant_id, artifact_id, generation)
    REFERENCES artifact_manifests (tenant_id, artifact_id, generation)
);
CREATE TRIGGER artifact_storage_tombstone_immutable
BEFORE UPDATE ON artifact_storage_tombstones
BEGIN
    SELECT raise(ABORT, 'artifact storage tombstone is immutable');
END;
