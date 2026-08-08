-- Stage 04 canonical conversation metadata. Apply after migration 003 in its
-- own transaction. On failure, roll back the whole version; rollback after
-- release restores the pre-migration database backup rather than dropping
-- canonical rows. Rendered turn bytes (query text, answer prose, and claim and
-- citation sets) live in the encrypted ArtifactVault; this schema stores only
-- opaque identities, canonical digests, and lifecycle state, never prose.
-- Turns and idempotency records are insert-only facts with terminal status
-- assigned at commit; update and delete are rejected by trigger. Turn
-- identities are unique per principal so (occurred_at_ms, turn_id) is a total
-- order for history pagination. Assistant completions carry the query
-- idempotency key so completion happens exactly once per admitted query and an
-- idempotent replay can find the original outcome.
CREATE TABLE conversation_turns (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    sequence_in_session INTEGER NOT NULL CHECK (sequence_in_session > 0),
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    status TEXT NOT NULL CHECK (status IN ('active', 'failed')),
    idempotency_key TEXT,
    payload_artifact_id TEXT NOT NULL,
    payload_digest TEXT NOT NULL CHECK (
        length(payload_digest) = 64
        AND payload_digest NOT GLOB '*[^0-9a-f]*'
    ),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, session_id, sequence_in_session),
    UNIQUE (tenant_id, principal_id, session_id, turn_id),
    UNIQUE (tenant_id, principal_id, turn_id),
    CHECK (
        (role = 'user' AND idempotency_key IS NULL)
        OR (
            role = 'assistant'
            AND idempotency_key IS NOT NULL
            AND length(idempotency_key) > 0
            AND length(idempotency_key) <= 512
        )
    ),
    FOREIGN KEY (tenant_id, principal_id, session_id)
    REFERENCES sessions (tenant_id, principal_id, session_id)
);
CREATE INDEX conversation_turns_history ON conversation_turns (tenant_id, principal_id, occurred_at_ms, turn_id);
CREATE UNIQUE INDEX conversation_one_completion_per_query
ON conversation_turns (tenant_id, principal_id, idempotency_key)
WHERE idempotency_key IS NOT NULL;
CREATE TABLE conversation_query_idempotency (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (
        length(request_digest) = 64
        AND request_digest NOT GLOB '*[^0-9a-f]*'
    ),
    session_id TEXT NOT NULL,
    user_turn_id TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, idempotency_key),
    FOREIGN KEY (tenant_id, principal_id, session_id)
    REFERENCES sessions (tenant_id, principal_id, session_id),
    FOREIGN KEY (tenant_id, principal_id, session_id, user_turn_id)
    REFERENCES conversation_turns (tenant_id, principal_id, session_id, turn_id)
);
CREATE TRIGGER conversation_turn_update_immutable
BEFORE UPDATE ON conversation_turns
BEGIN
    SELECT raise(ABORT, 'conversation turn is immutable');
END;
CREATE TRIGGER conversation_turn_delete_immutable
BEFORE DELETE ON conversation_turns
BEGIN
    SELECT raise(ABORT, 'conversation turn is immutable');
END;
CREATE TRIGGER conversation_turn_sequence_dense_append
BEFORE INSERT ON conversation_turns
WHEN
    new.sequence_in_session IS NOT coalesce(
        (
            SELECT max(turn.sequence_in_session) FROM conversation_turns AS turn
            WHERE
                turn.tenant_id = new.tenant_id
                AND turn.principal_id = new.principal_id
                AND turn.session_id = new.session_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'conversation turn sequence must append densely');
END;
CREATE TRIGGER conversation_query_idempotency_user_turn_role
BEFORE INSERT ON conversation_query_idempotency
WHEN
    NOT EXISTS (
        SELECT 1 FROM conversation_turns AS turn
        WHERE
            turn.tenant_id = new.tenant_id
            AND turn.principal_id = new.principal_id
            AND turn.session_id = new.session_id
            AND turn.turn_id = new.user_turn_id
            AND turn.role = 'user'
    )
BEGIN
    SELECT raise(ABORT, 'conversation query idempotency must reference a user turn');
END;
CREATE TRIGGER conversation_query_idempotency_update_immutable
BEFORE UPDATE ON conversation_query_idempotency
BEGIN
    SELECT raise(ABORT, 'conversation query idempotency record is immutable');
END;
CREATE TRIGGER conversation_query_idempotency_delete_immutable
BEFORE DELETE ON conversation_query_idempotency
BEGIN
    SELECT raise(ABORT, 'conversation query idempotency record is immutable');
END;
