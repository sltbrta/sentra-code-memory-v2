-- Stage 02 canonical authority schema. Forward-only and transaction-safe:
-- apply this complete file inside BEGIN IMMEDIATE; if it fails, ROLLBACK leaves
-- the previous schema intact. No runtime migration executor is supplied here.
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ms INTEGER NOT NULL
);
CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    peer_uid INTEGER NOT NULL CHECK (peer_uid >= 0),
    opened_at_ms INTEGER NOT NULL,
    closed_at_ms INTEGER,
    UNIQUE (tenant_id, principal_id, session_id),
    UNIQUE (tenant_id, session_id)
);
CREATE TABLE commands (
    command_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    command_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    authenticated_digest TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK (fence > 0),
    status TEXT NOT NULL CHECK (status IN ('accepted', 'rejected', 'completed')),
    submitted_at_ms INTEGER NOT NULL,
    UNIQUE (tenant_id, command_id),
    UNIQUE (tenant_id, principal_id, command_type, idempotency_key),
    UNIQUE (
        tenant_id,
        command_id,
        principal_id,
        session_id,
        command_type,
        idempotency_key,
        authenticated_digest,
        fence
    ),
    FOREIGN KEY (tenant_id, principal_id, session_id) REFERENCES sessions (tenant_id, principal_id, session_id)
);
CREATE TABLE command_idempotency (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    command_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    authenticated_digest TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK (fence > 0),
    command_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, command_type, idempotency_key),
    FOREIGN KEY (tenant_id, principal_id, session_id) REFERENCES sessions (tenant_id, principal_id, session_id),
    FOREIGN KEY (
        tenant_id,
        command_id,
        principal_id,
        session_id,
        command_type,
        idempotency_key,
        authenticated_digest,
        fence
    ) REFERENCES commands (
        tenant_id,
        command_id,
        principal_id,
        session_id,
        command_type,
        idempotency_key,
        authenticated_digest,
        fence
    )
);
CREATE TABLE aggregate_versions (
    tenant_id TEXT NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 0),
    PRIMARY KEY (tenant_id, aggregate_type, aggregate_id)
);
CREATE TABLE events (
    sequence INTEGER PRIMARY KEY CHECK (sequence >= 0),
    event_id TEXT NOT NULL UNIQUE,
    tenant_id TEXT NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    aggregate_version INTEGER NOT NULL CHECK (aggregate_version >= 0),
    command_id TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    occurred_at_ms INTEGER NOT NULL,
    UNIQUE (tenant_id, sequence),
    UNIQUE (tenant_id, aggregate_type, aggregate_id, aggregate_version),
    FOREIGN KEY (tenant_id, command_id) REFERENCES commands (tenant_id, command_id)
);
CREATE TABLE receipts (
    receipt_id TEXT PRIMARY KEY,
    command_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('accepted', 'rejected', 'deferred', 'completed', 'partial')),
    reason_code TEXT NOT NULL,
    causal_watermark INTEGER NOT NULL CHECK (causal_watermark >= 0),
    recorded_at_ms INTEGER NOT NULL,
    UNIQUE (tenant_id, receipt_id),
    FOREIGN KEY (tenant_id, command_id) REFERENCES commands (tenant_id, command_id)
);
CREATE TABLE watermarks (
    projection_name TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    watermark INTEGER NOT NULL CHECK (watermark >= 0),
    generation INTEGER NOT NULL CHECK (generation >= 0),
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, projection_name)
);
CREATE TABLE grants (
    grant_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    action_name TEXT NOT NULL,
    revocation_epoch INTEGER NOT NULL CHECK (revocation_epoch >= 0),
    expires_at_ms INTEGER NOT NULL
);
CREATE TABLE revocation_epochs (
    tenant_id TEXT PRIMARY KEY, epoch INTEGER NOT NULL CHECK (epoch >= 0), updated_at_ms INTEGER NOT NULL
);
CREATE TABLE audit_log (
    sequence INTEGER PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    event_digest TEXT NOT NULL,
    previous_digest TEXT,
    recorded_at_ms INTEGER NOT NULL
);
CREATE TABLE outbox (
    outbox_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    event_sequence INTEGER NOT NULL,
    payload_digest TEXT NOT NULL,
    delivered_at_ms INTEGER,
    FOREIGN KEY (tenant_id, event_sequence) REFERENCES events (tenant_id, sequence)
);
CREATE TABLE checkpoints (
    checkpoint_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    event_sequence INTEGER NOT NULL CHECK (event_sequence >= 0),
    audit_digest TEXT NOT NULL,
    key_epoch INTEGER NOT NULL CHECK (key_epoch >= 0),
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY (tenant_id, event_sequence) REFERENCES events (tenant_id, sequence)
);
CREATE TABLE artifact_manifests (
    artifact_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    content_digest TEXT NOT NULL,
    byte_length INTEGER NOT NULL CHECK (byte_length > 0 AND byte_length <= 1099511627776),
    frame_count INTEGER NOT NULL CHECK (frame_count > 0 AND frame_count <= 1048576),
    key_epoch INTEGER NOT NULL CHECK (key_epoch >= 0),
    status TEXT NOT NULL CHECK (status IN ('staged', 'published', 'tombstoned', 'purged', 'quarantined')),
    PRIMARY KEY (tenant_id, artifact_id, generation)
);
CREATE TABLE artifact_frames (
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    frame_index INTEGER NOT NULL CHECK (frame_index >= 0),
    offset_bytes INTEGER NOT NULL CHECK (offset_bytes >= 0),
    length_bytes INTEGER NOT NULL CHECK (length_bytes > 0 AND length_bytes <= 16777216),
    frame_digest TEXT NOT NULL,
    PRIMARY KEY (tenant_id, artifact_id, generation, frame_index),
    FOREIGN KEY (tenant_id, artifact_id, generation) REFERENCES artifact_manifests (tenant_id, artifact_id, generation)
);
CREATE TABLE artifact_generations (
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    current_generation INTEGER NOT NULL CHECK (current_generation > 0),
    PRIMARY KEY (tenant_id, artifact_id),
    FOREIGN KEY (tenant_id, artifact_id, current_generation) REFERENCES artifact_manifests (
        tenant_id, artifact_id, generation
    )
);
CREATE TRIGGER artifact_generations_insert_complete
BEFORE INSERT ON artifact_generations
WHEN
    NOT EXISTS (
        SELECT 1
        FROM artifact_manifests AS manifest
        WHERE
            manifest.tenant_id = new.tenant_id
            AND manifest.artifact_id = new.artifact_id
            AND manifest.generation = new.current_generation
            AND manifest.status = 'published'
            AND (
                SELECT COUNT(*)
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
            ) = manifest.frame_count
            AND (
                SELECT COALESCE(SUM(frame.length_bytes), 0)
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
            ) = manifest.byte_length
            AND NOT EXISTS (
                SELECT 1
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
                    AND (
                        frame.frame_index != (
                            SELECT COUNT(*)
                            FROM artifact_frames AS prior
                            WHERE
                                prior.tenant_id = frame.tenant_id
                                AND prior.artifact_id = frame.artifact_id
                                AND prior.generation = frame.generation
                                AND prior.frame_index < frame.frame_index
                        )
                        OR frame.offset_bytes != (
                            SELECT COALESCE(SUM(prior.length_bytes), 0)
                            FROM artifact_frames AS prior
                            WHERE
                                prior.tenant_id = frame.tenant_id
                                AND prior.artifact_id = frame.artifact_id
                                AND prior.generation = frame.generation
                                AND prior.frame_index < frame.frame_index
                        )
                    )
            )
    )
BEGIN
    SELECT RAISE(ABORT, 'artifact generation is incomplete');
END;
CREATE TRIGGER artifact_generations_update_complete
BEFORE UPDATE OF tenant_id, artifact_id, current_generation ON artifact_generations
WHEN
    NOT EXISTS (
        SELECT 1
        FROM artifact_manifests AS manifest
        WHERE
            manifest.tenant_id = new.tenant_id
            AND manifest.artifact_id = new.artifact_id
            AND manifest.generation = new.current_generation
            AND manifest.status = 'published'
            AND (
                SELECT COUNT(*)
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
            ) = manifest.frame_count
            AND (
                SELECT COALESCE(SUM(frame.length_bytes), 0)
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
            ) = manifest.byte_length
            AND NOT EXISTS (
                SELECT 1
                FROM artifact_frames AS frame
                WHERE
                    frame.tenant_id = manifest.tenant_id
                    AND frame.artifact_id = manifest.artifact_id
                    AND frame.generation = manifest.generation
                    AND (
                        frame.frame_index != (
                            SELECT COUNT(*)
                            FROM artifact_frames AS prior
                            WHERE
                                prior.tenant_id = frame.tenant_id
                                AND prior.artifact_id = frame.artifact_id
                                AND prior.generation = frame.generation
                                AND prior.frame_index < frame.frame_index
                        )
                        OR frame.offset_bytes != (
                            SELECT COALESCE(SUM(prior.length_bytes), 0)
                            FROM artifact_frames AS prior
                            WHERE
                                prior.tenant_id = frame.tenant_id
                                AND prior.artifact_id = frame.artifact_id
                                AND prior.generation = frame.generation
                                AND prior.frame_index < frame.frame_index
                        )
                    )
            )
    )
BEGIN
    SELECT RAISE(ABORT, 'artifact generation is incomplete');
END;
CREATE TRIGGER published_artifact_manifest_immutable
BEFORE UPDATE ON artifact_manifests
WHEN (
    old.tenant_id IS NOT new.tenant_id
    OR old.artifact_id IS NOT new.artifact_id
    OR old.generation IS NOT new.generation
    OR old.content_digest IS NOT new.content_digest
    OR old.byte_length IS NOT new.byte_length
    OR old.frame_count IS NOT new.frame_count
    OR old.key_epoch IS NOT new.key_epoch
)
AND old.status != 'staged'
BEGIN
    SELECT RAISE(ABORT, 'published artifact manifest is immutable');
END;
CREATE TRIGGER artifact_manifest_status_forward_only
BEFORE UPDATE OF status ON artifact_manifests
WHEN
    NOT (
        new.status = old.status
        OR (old.status = 'staged' AND new.status IN ('published', 'quarantined'))
        OR (old.status = 'published' AND new.status IN ('tombstoned', 'quarantined'))
        OR (old.status = 'tombstoned' AND new.status IN ('purged', 'quarantined'))
    )
BEGIN
    SELECT RAISE(ABORT, 'artifact manifest lifecycle cannot move backward');
END;
CREATE TRIGGER published_artifact_frame_insert_immutable
BEFORE INSERT ON artifact_frames
WHEN
    EXISTS (
        SELECT 1 FROM artifact_manifests AS manifest
        WHERE
            manifest.tenant_id = new.tenant_id
            AND manifest.artifact_id = new.artifact_id
            AND manifest.generation = new.generation
            AND manifest.status IN ('published', 'tombstoned', 'quarantined')
    )
BEGIN
    SELECT RAISE(ABORT, 'published artifact frames are immutable');
END;
CREATE TRIGGER published_artifact_frame_update_immutable
BEFORE UPDATE ON artifact_frames
WHEN
    EXISTS (
        SELECT 1 FROM artifact_manifests AS manifest
        WHERE
            manifest.status IN ('published', 'tombstoned', 'quarantined')
            AND (
                (
                    manifest.tenant_id = old.tenant_id
                    AND manifest.artifact_id = old.artifact_id
                    AND manifest.generation = old.generation
                )
                OR (
                    manifest.tenant_id = new.tenant_id
                    AND manifest.artifact_id = new.artifact_id
                    AND manifest.generation = new.generation
                )
            )
    )
BEGIN
    SELECT RAISE(ABORT, 'published artifact frames are immutable');
END;
CREATE TRIGGER published_artifact_frame_delete_immutable
BEFORE DELETE ON artifact_frames
WHEN
    EXISTS (
        SELECT 1 FROM artifact_manifests AS manifest
        WHERE
            manifest.tenant_id = old.tenant_id
            AND manifest.artifact_id = old.artifact_id
            AND manifest.generation = old.generation
            AND manifest.status IN ('published', 'tombstoned', 'quarantined')
    )
BEGIN
    SELECT RAISE(ABORT, 'published artifact frames are immutable');
END;
CREATE TABLE key_epochs (
    tenant_id TEXT NOT NULL,
    key_epoch INTEGER NOT NULL CHECK (key_epoch >= 0),
    key_reference TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('current', 'historical', 'legacy', 'unreadable')),
    PRIMARY KEY (tenant_id, key_epoch)
);
CREATE TABLE artifact_quarantine (
    quarantine_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    reason_code TEXT NOT NULL,
    observed_at_ms INTEGER NOT NULL
);
CREATE TABLE legacy_markers (
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    legacy_key_reference TEXT NOT NULL,
    migrated_at_ms INTEGER,
    PRIMARY KEY (tenant_id, artifact_id, generation)
);
CREATE TABLE artifact_lineage (
    tenant_id TEXT NOT NULL,
    parent_artifact_id TEXT NOT NULL,
    child_artifact_id TEXT NOT NULL,
    relation TEXT NOT NULL,
    PRIMARY KEY (tenant_id, parent_artifact_id, child_artifact_id, relation)
);
CREATE TABLE tombstones (
    tombstone_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    receipt_id TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    tombstoned_at_ms INTEGER NOT NULL,
    UNIQUE (tenant_id, tombstone_id),
    UNIQUE (tenant_id, artifact_id, generation),
    UNIQUE (tenant_id, artifact_id, generation, tombstone_id),
    FOREIGN KEY (tenant_id, receipt_id) REFERENCES receipts (tenant_id, receipt_id)
);
CREATE TABLE purge_jobs (
    purge_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    tombstone_id TEXT NOT NULL,
    key_epoch INTEGER NOT NULL CHECK (key_epoch >= 0),
    status TEXT NOT NULL CHECK (status IN ('scheduled', 'completed', 'quarantined')),
    completed_at_ms INTEGER,
    UNIQUE (tenant_id, artifact_id, generation),
    FOREIGN KEY (
        tenant_id,
        artifact_id,
        generation,
        tombstone_id
    ) REFERENCES tombstones (
        tenant_id,
        artifact_id,
        generation,
        tombstone_id
    )
);
