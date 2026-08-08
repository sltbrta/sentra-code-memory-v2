-- Stage 05 canonical factory kernel metadata. Apply after migration 004 in its
-- own transaction. On failure, roll back the whole version; rollback after
-- release restores the pre-migration database backup rather than dropping
-- canonical rows. Goal prose, admitted intent bytes, candidate preview bytes,
-- mailbox payloads, and finding prose live in the encrypted ArtifactVault; this
-- schema stores only opaque identities, canonical digests, bounded structural
-- facts, and lifecycle state, never prose. Runs, plan nodes, gate roster rows,
-- leases, mailbox messages, findings, idempotency records, and rollback
-- receipts are insert-only facts; update and delete are rejected by trigger.
-- Run, gate, and candidate lifecycle transitions append densely per aggregate
-- and stop at their terminal states, so replay always yields one valid state.
CREATE TABLE factory_runs (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    intent_digest TEXT NOT NULL CHECK (
        length(intent_digest) = 64
        AND intent_digest NOT GLOB '*[^0-9a-f]*'
    ),
    intent_artifact_id TEXT NOT NULL,
    repository_git_oid TEXT NOT NULL CHECK (
        length(repository_git_oid) IN (40, 64)
        AND repository_git_oid NOT GLOB '*[^0-9a-f]*'
    ),
    plan_id TEXT NOT NULL,
    admitted_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id),
    UNIQUE (tenant_id, principal_id, intent_id),
    FOREIGN KEY (tenant_id, principal_id, session_id)
    REFERENCES sessions (tenant_id, principal_id, session_id)
);
CREATE TRIGGER factory_run_update_immutable
BEFORE UPDATE ON factory_runs
BEGIN
    SELECT raise(ABORT, 'factory run is immutable');
END;
CREATE TRIGGER factory_run_delete_immutable
BEFORE DELETE ON factory_runs
BEGIN
    SELECT raise(ABORT, 'factory run is immutable');
END;
CREATE TABLE factory_run_states (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    state TEXT NOT NULL CHECK (state IN (
        'PLANNING', 'READY', 'RUNNING', 'REVIEW', 'CANDIDATE_READY',
        'COMPLETED', 'FAILED', 'CANCELLED'
    )),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_run_state_dense_append
BEFORE INSERT ON factory_run_states
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM factory_run_states AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.run_id = new.run_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'factory run state sequence must append densely');
END;
CREATE TRIGGER factory_run_state_terminal
BEFORE INSERT ON factory_run_states
WHEN
    EXISTS (
        SELECT 1 FROM factory_run_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.run_id = new.run_id
            AND prior.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
    )
BEGIN
    SELECT raise(ABORT, 'factory run is terminal');
END;
CREATE TRIGGER factory_run_state_update_immutable
BEFORE UPDATE ON factory_run_states
BEGIN
    SELECT raise(ABORT, 'factory run state is immutable');
END;
CREATE TRIGGER factory_run_state_delete_immutable
BEFORE DELETE ON factory_run_states
BEGIN
    SELECT raise(ABORT, 'factory run state is immutable');
END;
CREATE TABLE factory_idempotency (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('admit', 'cancel')),
    idempotency_key TEXT NOT NULL CHECK (
        length(idempotency_key) > 0
        AND length(idempotency_key) <= 512
    ),
    request_digest TEXT NOT NULL CHECK (
        length(request_digest) = 64
        AND request_digest NOT GLOB '*[^0-9a-f]*'
    ),
    run_id TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_idempotency_update_immutable
BEFORE UPDATE ON factory_idempotency
BEGIN
    SELECT raise(ABORT, 'factory idempotency record is immutable');
END;
CREATE TRIGGER factory_idempotency_delete_immutable
BEFORE DELETE ON factory_idempotency
BEGIN
    SELECT raise(ABORT, 'factory idempotency record is immutable');
END;
CREATE TABLE factory_plan_nodes (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('orchestrator', 'leaf', 'review')),
    goal_artifact_id TEXT NOT NULL,
    goal_digest TEXT NOT NULL CHECK (
        length(goal_digest) = 64
        AND goal_digest NOT GLOB '*[^0-9a-f]*'
    ),
    owned_paths TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(owned_paths)),
    forbidden_paths TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(forbidden_paths)),
    route_profile_digest TEXT CHECK (
        route_profile_digest IS NULL
        OR (
            length(route_profile_digest) = 64
            AND route_profile_digest NOT GLOB '*[^0-9a-f]*'
        )
    ),
    route_model_identity TEXT,
    route_rationale_code TEXT,
    grant_actions TEXT CHECK (grant_actions IS NULL OR json_valid(grant_actions)),
    grant_allowed_paths TEXT CHECK (grant_allowed_paths IS NULL OR json_valid(grant_allowed_paths)),
    grant_nonce TEXT,
    grant_expires_at_ms INTEGER,
    grant_revocation_epoch INTEGER,
    grant_command_fence INTEGER,
    grant_policy_digest TEXT CHECK (
        grant_policy_digest IS NULL
        OR (
            length(grant_policy_digest) = 64
            AND grant_policy_digest NOT GLOB '*[^0-9a-f]*'
        )
    ),
    PRIMARY KEY (tenant_id, principal_id, run_id, node_id),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id),
    CHECK (
        (
            kind = 'leaf'
            AND json_array_length(owned_paths) > 0
            AND route_profile_digest IS NOT NULL
            AND route_model_identity IS NOT NULL
            AND route_rationale_code IS NOT NULL
            AND grant_actions IS NOT NULL
            AND grant_allowed_paths IS NOT NULL
            AND grant_nonce IS NOT NULL
            AND grant_expires_at_ms IS NOT NULL
            AND grant_revocation_epoch IS NOT NULL
            AND grant_command_fence IS NOT NULL
            AND grant_policy_digest IS NOT NULL
        )
        OR (
            kind IN ('orchestrator', 'review')
            AND owned_paths = '[]'
            AND forbidden_paths = '[]'
            AND route_profile_digest IS NULL
            AND route_model_identity IS NULL
            AND route_rationale_code IS NULL
            AND grant_actions IS NULL
            AND grant_allowed_paths IS NULL
            AND grant_nonce IS NULL
            AND grant_expires_at_ms IS NULL
            AND grant_revocation_epoch IS NULL
            AND grant_command_fence IS NULL
            AND grant_policy_digest IS NULL
        )
    )
);
CREATE TRIGGER factory_plan_node_update_immutable
BEFORE UPDATE ON factory_plan_nodes
BEGIN
    SELECT raise(ABORT, 'factory plan node is immutable');
END;
CREATE TRIGGER factory_plan_node_delete_immutable
BEFORE DELETE ON factory_plan_nodes
BEGIN
    SELECT raise(ABORT, 'factory plan node is immutable');
END;
CREATE TABLE factory_gates (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('BUILD', 'TEST', 'DOCS', 'SECURITY')),
    required INTEGER NOT NULL CHECK (required IN (0, 1)),
    PRIMARY KEY (tenant_id, principal_id, run_id, gate_id),
    UNIQUE (tenant_id, principal_id, run_id, kind),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_gate_update_immutable
BEFORE UPDATE ON factory_gates
BEGIN
    SELECT raise(ABORT, 'factory gate roster entry is immutable');
END;
CREATE TRIGGER factory_gate_delete_immutable
BEFORE DELETE ON factory_gates
BEGIN
    SELECT raise(ABORT, 'factory gate roster entry is immutable');
END;
CREATE TABLE factory_gate_states (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'PASSED', 'FAILED')),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, gate_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, run_id, gate_id)
    REFERENCES factory_gates (tenant_id, principal_id, run_id, gate_id)
);
CREATE TRIGGER factory_gate_state_dense_append
BEFORE INSERT ON factory_gate_states
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM factory_gate_states AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.run_id = new.run_id
                AND prior.gate_id = new.gate_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'factory gate state sequence must append densely');
END;
CREATE TRIGGER factory_gate_state_terminal
BEFORE INSERT ON factory_gate_states
WHEN
    EXISTS (
        SELECT 1 FROM factory_gate_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.run_id = new.run_id
            AND prior.gate_id = new.gate_id
            AND prior.status IN ('PASSED', 'FAILED')
    )
BEGIN
    SELECT raise(ABORT, 'factory gate evaluation is terminal');
END;
CREATE TRIGGER factory_gate_state_update_immutable
BEFORE UPDATE ON factory_gate_states
BEGIN
    SELECT raise(ABORT, 'factory gate state is immutable');
END;
CREATE TRIGGER factory_gate_state_delete_immutable
BEFORE DELETE ON factory_gate_states
BEGIN
    SELECT raise(ABORT, 'factory gate state is immutable');
END;
CREATE TABLE factory_leases (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK (fence > 0),
    lease_id TEXT NOT NULL,
    holder_principal_id TEXT NOT NULL,
    issued_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, node_id, fence),
    UNIQUE (tenant_id, principal_id, run_id, node_id, lease_id),
    FOREIGN KEY (tenant_id, principal_id, run_id, node_id)
    REFERENCES factory_plan_nodes (tenant_id, principal_id, run_id, node_id)
);
CREATE TRIGGER factory_lease_fence_dense_append
BEFORE INSERT ON factory_leases
WHEN
    new.fence IS NOT coalesce(
        (
            SELECT max(prior.fence) FROM factory_leases AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.run_id = new.run_id
                AND prior.node_id = new.node_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'factory lease fence must advance densely');
END;
CREATE TRIGGER factory_lease_update_immutable
BEFORE UPDATE ON factory_leases
BEGIN
    SELECT raise(ABORT, 'factory lease is immutable');
END;
CREATE TRIGGER factory_lease_delete_immutable
BEFORE DELETE ON factory_leases
BEGIN
    SELECT raise(ABORT, 'factory lease is immutable');
END;
CREATE TABLE factory_leaf_results (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK (fence > 0),
    result_artifact_id TEXT NOT NULL,
    result_digest TEXT NOT NULL CHECK (
        length(result_digest) = 64
        AND result_digest NOT GLOB '*[^0-9a-f]*'
    ),
    committed_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, node_id),
    FOREIGN KEY (tenant_id, principal_id, run_id, node_id)
    REFERENCES factory_plan_nodes (tenant_id, principal_id, run_id, node_id)
);
CREATE TRIGGER factory_leaf_result_update_immutable
BEFORE UPDATE ON factory_leaf_results
BEGIN
    SELECT raise(ABORT, 'factory leaf result is immutable');
END;
CREATE TRIGGER factory_leaf_result_delete_immutable
BEFORE DELETE ON factory_leaf_results
BEGIN
    SELECT raise(ABORT, 'factory leaf result is immutable');
END;
CREATE TABLE factory_mailbox_messages (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN (
        'QUESTION', 'ANSWER', 'FINDING', 'EVIDENCE', 'DEPENDENCY_READY',
        'BLOCKED', 'HANDOVER', 'REVIEW_REQUEST', 'REVIEW_RESULT', 'CORRECTION',
        'CANCELLATION'
    )),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    correlation_id TEXT NOT NULL,
    causation_id TEXT NOT NULL,
    sender_principal_id TEXT NOT NULL,
    payload_artifact_id TEXT NOT NULL,
    payload_digest TEXT NOT NULL CHECK (
        length(payload_digest) = 64
        AND payload_digest NOT GLOB '*[^0-9a-f]*'
    ),
    expires_at_ms INTEGER,
    sent_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, message_id),
    UNIQUE (tenant_id, principal_id, run_id, task_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id),
    FOREIGN KEY (tenant_id, principal_id, run_id, task_id)
    REFERENCES factory_plan_nodes (tenant_id, principal_id, run_id, node_id)
);
CREATE TRIGGER factory_mailbox_sequence_dense_append
BEFORE INSERT ON factory_mailbox_messages
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM factory_mailbox_messages AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.run_id = new.run_id
                AND prior.task_id = new.task_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'factory mailbox sequence must append densely');
END;
CREATE TRIGGER factory_mailbox_update_immutable
BEFORE UPDATE ON factory_mailbox_messages
BEGIN
    SELECT raise(ABORT, 'factory mailbox message is immutable');
END;
CREATE TRIGGER factory_mailbox_delete_immutable
BEFORE DELETE ON factory_mailbox_messages
BEGIN
    SELECT raise(ABORT, 'factory mailbox message is immutable');
END;
CREATE TABLE factory_mailbox_acks (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    acked_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, message_id),
    FOREIGN KEY (tenant_id, principal_id, run_id, message_id)
    REFERENCES factory_mailbox_messages (tenant_id, principal_id, run_id, message_id)
);
CREATE TRIGGER factory_mailbox_ack_update_immutable
BEFORE UPDATE ON factory_mailbox_acks
BEGIN
    SELECT raise(ABORT, 'factory mailbox acknowledgement is immutable');
END;
CREATE TRIGGER factory_mailbox_ack_delete_immutable
BEFORE DELETE ON factory_mailbox_acks
BEGIN
    SELECT raise(ABORT, 'factory mailbox acknowledgement is immutable');
END;
CREATE TABLE factory_candidates (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    change_set_id TEXT NOT NULL,
    candidate_artifact_id TEXT NOT NULL,
    candidate_digest TEXT NOT NULL CHECK (
        length(candidate_digest) = 64
        AND candidate_digest NOT GLOB '*[^0-9a-f]*'
    ),
    proposed_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_candidate_update_immutable
BEFORE UPDATE ON factory_candidates
BEGIN
    SELECT raise(ABORT, 'factory candidate is immutable');
END;
CREATE TRIGGER factory_candidate_delete_immutable
BEFORE DELETE ON factory_candidates
BEGIN
    SELECT raise(ABORT, 'factory candidate is immutable');
END;
CREATE TABLE factory_candidate_states (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    state TEXT NOT NULL CHECK (state IN (
        'PROPOSED', 'APPLIED', 'VERIFIED', 'REVIEWED', 'RETAINED', 'REJECTED'
    )),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, sequence),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_candidate_state_dense_append
BEFORE INSERT ON factory_candidate_states
WHEN
    new.sequence IS NOT coalesce(
        (
            SELECT max(prior.sequence) FROM factory_candidate_states AS prior
            WHERE
                prior.tenant_id = new.tenant_id
                AND prior.principal_id = new.principal_id
                AND prior.run_id = new.run_id
        ),
        0
    ) + 1
BEGIN
    SELECT raise(ABORT, 'factory candidate state sequence must append densely');
END;
CREATE TRIGGER factory_candidate_state_terminal
BEFORE INSERT ON factory_candidate_states
WHEN
    EXISTS (
        SELECT 1 FROM factory_candidate_states AS prior
        WHERE
            prior.tenant_id = new.tenant_id
            AND prior.principal_id = new.principal_id
            AND prior.run_id = new.run_id
            AND prior.state IN ('RETAINED', 'REJECTED')
    )
BEGIN
    SELECT raise(ABORT, 'factory candidate is terminal');
END;
CREATE TRIGGER factory_candidate_state_update_immutable
BEFORE UPDATE ON factory_candidate_states
BEGIN
    SELECT raise(ABORT, 'factory candidate state is immutable');
END;
CREATE TRIGGER factory_candidate_state_delete_immutable
BEFORE DELETE ON factory_candidate_states
BEGIN
    SELECT raise(ABORT, 'factory candidate state is immutable');
END;
CREATE TABLE factory_rollback_receipts (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    rollback_artifact_digest TEXT NOT NULL CHECK (
        length(rollback_artifact_digest) = 64
        AND rollback_artifact_digest NOT GLOB '*[^0-9a-f]*'
    ),
    recorded_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE TRIGGER factory_rollback_receipt_update_immutable
BEFORE UPDATE ON factory_rollback_receipts
BEGIN
    SELECT raise(ABORT, 'factory rollback receipt is immutable');
END;
CREATE TRIGGER factory_rollback_receipt_delete_immutable
BEFORE DELETE ON factory_rollback_receipts
BEGIN
    SELECT raise(ABORT, 'factory rollback receipt is immutable');
END;
CREATE TABLE factory_findings (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    finding_id TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('INFO', 'MINOR', 'MAJOR', 'BLOCKER')),
    category TEXT NOT NULL CHECK (category IN (
        'CORRECTNESS', 'SECURITY', 'DATA_INTEGRITY', 'DOCS', 'TESTS'
    )),
    reviewer_principal_id TEXT NOT NULL,
    reviewer_session_id TEXT NOT NULL,
    reviewer_family TEXT NOT NULL,
    payload_artifact_id TEXT NOT NULL,
    payload_digest TEXT NOT NULL CHECK (
        length(payload_digest) = 64
        AND payload_digest NOT GLOB '*[^0-9a-f]*'
    ),
    evidence_count INTEGER NOT NULL CHECK (evidence_count >= 0),
    occurred_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, finding_id),
    FOREIGN KEY (tenant_id, principal_id, run_id)
    REFERENCES factory_runs (tenant_id, principal_id, run_id)
);
CREATE INDEX factory_findings_page
ON factory_findings (tenant_id, principal_id, run_id, occurred_at_ms, finding_id);
CREATE TRIGGER factory_finding_update_immutable
BEFORE UPDATE ON factory_findings
BEGIN
    SELECT raise(ABORT, 'factory finding is immutable');
END;
CREATE TRIGGER factory_finding_delete_immutable
BEFORE DELETE ON factory_findings
BEGIN
    SELECT raise(ABORT, 'factory finding is immutable');
END;
CREATE TABLE factory_finding_dispositions (
    tenant_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    finding_id TEXT NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN (
        'FIXED', 'DISMISSED_WITH_EVIDENCE', 'DEFERRED', 'BLOCKING'
    )),
    receipt_id TEXT NOT NULL,
    receipt_reason_code TEXT NOT NULL,
    dispositioned_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, run_id, finding_id),
    FOREIGN KEY (tenant_id, principal_id, run_id, finding_id)
    REFERENCES factory_findings (tenant_id, principal_id, run_id, finding_id)
);
CREATE TRIGGER factory_finding_disposition_evidence
BEFORE INSERT ON factory_finding_dispositions
WHEN
    new.disposition = 'DISMISSED_WITH_EVIDENCE'
    AND (
        SELECT finding.evidence_count FROM factory_findings AS finding
        WHERE
            finding.tenant_id = new.tenant_id
            AND finding.principal_id = new.principal_id
            AND finding.run_id = new.run_id
            AND finding.finding_id = new.finding_id
    ) <= 0
BEGIN
    SELECT raise(ABORT, 'factory finding dismissal requires evidence');
END;
CREATE TRIGGER factory_finding_disposition_update_immutable
BEFORE UPDATE ON factory_finding_dispositions
BEGIN
    SELECT raise(ABORT, 'factory finding disposition is immutable');
END;
CREATE TRIGGER factory_finding_disposition_delete_immutable
BEFORE DELETE ON factory_finding_dispositions
BEGIN
    SELECT raise(ABORT, 'factory finding disposition is immutable');
END;
