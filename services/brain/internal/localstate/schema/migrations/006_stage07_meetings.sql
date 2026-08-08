-- Stage 07 canonical meeting-transcript metadata. Apply after migration 005 in
-- its own transaction. On failure, roll back the whole version; rollback after
-- release restores the pre-migration database backup rather than dropping
-- canonical rows. Transcript prose and segment text live in the encrypted
-- ArtifactVault; this schema stores only opaque identities, canonical digests,
-- bounded structural facts, retention codes, and lifecycle state, never prose.
-- Meeting rows and idempotency records are insert-only facts; lifecycle state
-- appends densely and stops at terminal REVOKED or PURGED. Update and delete
-- of meeting identity rows are rejected by trigger.
CREATE TABLE meeting_sessions (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    meeting_session_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    title_digest TEXT NOT NULL CHECK (
        length(title_digest) = 64
        AND title_digest NOT GLOB '*[^0-9a-f]*'
    ),
    source_scope TEXT NOT NULL CHECK (
        length(source_scope) > 0
        AND length(source_scope) <= 128
    ),
    started_at_ms INTEGER NOT NULL,
    ended_at_ms INTEGER NOT NULL CHECK (ended_at_ms >= started_at_ms),
    timeline_start_millis INTEGER NOT NULL CHECK (timeline_start_millis >= 0),
    timeline_end_millis INTEGER NOT NULL CHECK (timeline_end_millis > timeline_start_millis),
    segment_count INTEGER NOT NULL CHECK (segment_count > 0 AND segment_count <= 500),
    raw_media_retention TEXT NOT NULL CHECK (
        raw_media_retention IN ('PROCESSING_PLUS_24H', '7D', '30D', '90D')
    ),
    screenshot_retention TEXT NOT NULL CHECK (
        screenshot_retention IN ('OFF', 'PROCESSING_PLUS_24H', '7D', '30D')
    ),
    derivative_retention TEXT NOT NULL CHECK (
        derivative_retention IN ('30D', '90D', '365D', 'UNTIL_DELETED')
    ),
    notify_reminder_recorded INTEGER NOT NULL CHECK (notify_reminder_recorded = 1),
    partial INTEGER NOT NULL CHECK (partial IN (0, 1)),
    payload_artifact_id TEXT NOT NULL,
    payload_digest TEXT NOT NULL CHECK (
        length(payload_digest) = 64
        AND payload_digest NOT GLOB '*[^0-9a-f]*'
    ),
    admitted_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, meeting_session_id),
    FOREIGN KEY (tenant_id, principal_id, session_id)
    REFERENCES sessions (tenant_id, principal_id, session_id)
);
CREATE TRIGGER meeting_session_update_immutable
BEFORE UPDATE ON meeting_sessions
BEGIN
    SELECT raise(ABORT, 'meeting session is immutable');
END;
CREATE TRIGGER meeting_session_delete_immutable
BEFORE DELETE ON meeting_sessions
BEGIN
    SELECT raise(ABORT, 'meeting session is immutable');
END;
CREATE TABLE meeting_session_states (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    meeting_session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    state TEXT NOT NULL CHECK (state IN ('READY', 'PARTIAL', 'REVOKED', 'PURGED')),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, meeting_session_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, meeting_session_id)
    REFERENCES meeting_sessions (tenant_id, principal_id, meeting_session_id)
);
CREATE TRIGGER meeting_state_dense_append
BEFORE INSERT ON meeting_session_states
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM meeting_session_states AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.meeting_session_id = new.meeting_session_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'meeting state sequence must append densely');
END;
CREATE TRIGGER meeting_state_terminal
BEFORE INSERT ON meeting_session_states
WHEN
    EXISTS (
        SELECT 1 FROM meeting_session_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.meeting_session_id = new.meeting_session_id
            AND prior.state IN ('PURGED')
    )
BEGIN
    SELECT raise(ABORT, 'meeting state is terminal');
END;
CREATE TRIGGER meeting_state_revoked_only_to_purged
BEFORE INSERT ON meeting_session_states
WHEN
    new.state NOT IN ('PURGED')
    AND EXISTS (
        SELECT 1 FROM meeting_session_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.meeting_session_id = new.meeting_session_id
            AND prior.sequence = (
                SELECT max(latest.sequence) FROM meeting_session_states AS latest
                WHERE
                    latest.tenant_id = new.tenant_id
                    AND latest.principal_id = new.principal_id
                    AND latest.meeting_session_id = new.meeting_session_id
            )
            AND prior.state = 'REVOKED'
    )
BEGIN
    SELECT raise(ABORT, 'revoked meeting may only transition to purged');
END;
CREATE TABLE meeting_idempotency (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('import', 'revoke', 'purge', 'query')),
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (
        length(request_digest) = 64
        AND request_digest NOT GLOB '*[^0-9a-f]*'
    ),
    meeting_session_id TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, principal_id, meeting_session_id)
    REFERENCES meeting_sessions (tenant_id, principal_id, meeting_session_id)
);
CREATE TRIGGER meeting_idempotency_update_immutable
BEFORE UPDATE ON meeting_idempotency
BEGIN
    SELECT raise(ABORT, 'meeting idempotency is immutable');
END;
CREATE TRIGGER meeting_idempotency_delete_immutable
BEFORE DELETE ON meeting_idempotency
BEGIN
    SELECT raise(ABORT, 'meeting idempotency is immutable');
END;
