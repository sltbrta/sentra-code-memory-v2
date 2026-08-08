-- Stage 11 canonical multimodal source metadata. Apply after migration 006 in
-- its own transaction. On failure, roll back the whole version; rollback after
-- release restores the pre-migration database backup rather than dropping
-- canonical rows. Original bytes and derived evidence live in the encrypted
-- ArtifactVault; this schema stores only opaque identities, canonical digests,
-- bounded structural facts, lane readiness codes, and lifecycle state, never
-- payload bytes. Source rows and idempotency records are insert-only facts;
-- lifecycle state appends densely and stops at terminal PURGED. Update and
-- delete of source identity rows are rejected by trigger.
CREATE TABLE multimodal_sources (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    source_revision_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN ('TEXT_MARKDOWN', 'PDF', 'PNG', 'PCM_WAV')
    ),
    media_type TEXT NOT NULL CHECK (
        length(media_type) > 0
        AND length(media_type) <= 128
    ),
    byte_length INTEGER NOT NULL CHECK (byte_length > 0 AND byte_length <= 262144000),
    content_digest TEXT NOT NULL CHECK (
        length(content_digest) = 64
        AND content_digest NOT GLOB '*[^0-9a-f]*'
    ),
    payload_artifact_id TEXT NOT NULL,
    evidence_artifact_id TEXT NOT NULL,
    extractor_identity_hex TEXT NOT NULL CHECK (
        length(extractor_identity_hex) = 64
        AND extractor_identity_hex NOT GLOB '*[^0-9a-f]*'
    ),
    brain_id TEXT NOT NULL CHECK (
        length(brain_id) > 0
        AND length(brain_id) <= 512
    ),
    source_object_ns TEXT NOT NULL CHECK (
        length(source_object_ns) > 0
        AND length(source_object_ns) <= 64
    ),
    source_object_val TEXT NOT NULL CHECK (
        length(source_object_val) > 0
        AND length(source_object_val) <= 512
    ),
    bounds_json TEXT NOT NULL CHECK (length(bounds_json) > 0 AND length(bounds_json) <= 4096),
    partial INTEGER NOT NULL CHECK (partial IN (0, 1)),
    admitted_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, source_id),
    FOREIGN KEY (tenant_id, principal_id, session_id)
    REFERENCES sessions (tenant_id, principal_id, session_id)
);
CREATE TRIGGER multimodal_source_update_immutable
BEFORE UPDATE ON multimodal_sources
BEGIN
    SELECT raise(ABORT, 'multimodal source is immutable');
END;
CREATE TRIGGER multimodal_source_delete_immutable
BEFORE DELETE ON multimodal_sources
BEGIN
    SELECT raise(ABORT, 'multimodal source is immutable');
END;
CREATE TABLE multimodal_source_states (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    state TEXT NOT NULL CHECK (
        state IN (
            'ADMITTED',
            'EXTRACTING',
            'PARTIAL_READY',
            'READY',
            'FAILED',
            'QUARANTINED',
            'REVOKED',
            'PURGED'
        )
    ),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, source_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, source_id)
    REFERENCES multimodal_sources (tenant_id, principal_id, source_id)
);
CREATE TRIGGER multimodal_state_dense_append
BEFORE INSERT ON multimodal_source_states
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM multimodal_source_states AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.source_id = new.source_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'multimodal state sequence must append densely');
END;
CREATE TRIGGER multimodal_state_terminal
BEFORE INSERT ON multimodal_source_states
WHEN
    EXISTS (
        SELECT 1 FROM multimodal_source_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.source_id = new.source_id
            AND prior.state IN ('PURGED')
    )
BEGIN
    SELECT raise(ABORT, 'multimodal state is terminal');
END;
CREATE TRIGGER multimodal_state_revoked_only_to_purged
BEFORE INSERT ON multimodal_source_states
WHEN
    new.state NOT IN ('PURGED')
    AND EXISTS (
        SELECT 1 FROM multimodal_source_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.source_id = new.source_id
            AND prior.sequence = (
                SELECT max(latest.sequence) FROM multimodal_source_states AS latest
                WHERE
                    latest.tenant_id = new.tenant_id
                    AND latest.principal_id = new.principal_id
                    AND latest.source_id = new.source_id
            )
            AND prior.state = 'REVOKED'
    )
BEGIN
    SELECT raise(ABORT, 'revoked multimodal source may only transition to purged');
END;
CREATE TABLE multimodal_idempotency (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('admit', 'revoke', 'purge')),
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (
        length(request_digest) = 64
        AND request_digest NOT GLOB '*[^0-9a-f]*'
    ),
    source_id TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, principal_id, source_id)
    REFERENCES multimodal_sources (tenant_id, principal_id, source_id)
);
CREATE TRIGGER multimodal_idempotency_update_immutable
BEFORE UPDATE ON multimodal_idempotency
BEGIN
    SELECT raise(ABORT, 'multimodal idempotency is immutable');
END;
CREATE TRIGGER multimodal_idempotency_delete_immutable
BEFORE DELETE ON multimodal_idempotency
BEGIN
    SELECT raise(ABORT, 'multimodal idempotency is immutable');
END;
