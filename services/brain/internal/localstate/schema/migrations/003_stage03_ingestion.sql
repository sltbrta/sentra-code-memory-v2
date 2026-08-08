-- Stage 03 canonical ingestion metadata. Apply after migration 002 in its own
-- transaction. On failure, roll back the whole version; rollback after release
-- restores the pre-migration database backup rather than dropping canonical rows.
-- Search occurrences and symbol indexes are rebuildable projections and are absent.
CREATE TABLE ingestion_sources (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    configuration_digest TEXT NOT NULL CHECK (
        length(configuration_digest) = 64
        AND configuration_digest NOT GLOB '*[^0-9a-f]*'
    ),
    ignore_policy_digest TEXT NOT NULL CHECK (
        length(ignore_policy_digest) = 64
        AND ignore_policy_digest NOT GLOB '*[^0-9a-f]*'
    ),
    state TEXT NOT NULL CHECK (state IN ('admitted', 'ready', 'reconciling', 'revoked')),
    acl_epoch INTEGER NOT NULL CHECK (acl_epoch >= 0),
    revocation_epoch INTEGER NOT NULL CHECK (revocation_epoch >= 0),
    created_at_ms INTEGER NOT NULL,
    revoked_at_ms INTEGER,
    PRIMARY KEY (tenant_id, brain_id, source_id),
    CHECK (
        (state = 'revoked' AND revoked_at_ms IS NOT NULL)
        OR (state <> 'revoked' AND revoked_at_ms IS NULL)
    )
);
CREATE TABLE ingestion_roots (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    approved_root_id TEXT NOT NULL CHECK (
        length(approved_root_id) = 64
        AND approved_root_id NOT GLOB '*[^0-9a-f]*'
    ),
    symlink_policy TEXT NOT NULL CHECK (symlink_policy = 'record_without_follow'),
    PRIMARY KEY (tenant_id, brain_id, source_id),
    UNIQUE (tenant_id, brain_id, approved_root_id),
    FOREIGN KEY (tenant_id, brain_id, source_id)
    REFERENCES ingestion_sources (tenant_id, brain_id, source_id)
);
CREATE TABLE ingestion_snapshots (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    commit_oid TEXT NOT NULL CHECK (
        length(commit_oid) IN (40, 64)
        AND commit_oid NOT GLOB '*[^0-9a-f]*'
    ),
    tree_oid TEXT NOT NULL CHECK (
        length(tree_oid) IN (40, 64)
        AND tree_oid NOT GLOB '*[^0-9a-f]*'
    ),
    policy_digest TEXT NOT NULL CHECK (
        length(policy_digest) = 64
        AND policy_digest NOT GLOB '*[^0-9a-f]*'
    ),
    path_count INTEGER NOT NULL CHECK (path_count >= 0),
    snapshot_digest TEXT NOT NULL CHECK (
        length(snapshot_digest) = 64
        AND snapshot_digest NOT GLOB '*[^0-9a-f]*'
    ),
    observed_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, brain_id, source_id, snapshot_id),
    UNIQUE (tenant_id, brain_id, source_id, commit_oid, policy_digest),
    FOREIGN KEY (tenant_id, brain_id, source_id)
    REFERENCES ingestion_sources (tenant_id, brain_id, source_id)
);
CREATE TABLE ingestion_source_revisions (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    source_revision_id TEXT NOT NULL,
    source_object_id TEXT NOT NULL,
    path_digest TEXT NOT NULL CHECK (
        length(path_digest) = 64
        AND path_digest NOT GLOB '*[^0-9a-f]*'
    ),
    git_blob_oid TEXT NOT NULL CHECK (
        length(git_blob_oid) IN (40, 64)
        AND git_blob_oid NOT GLOB '*[^0-9a-f]*'
    ),
    content_digest TEXT NOT NULL CHECK (
        length(content_digest) = 64
        AND content_digest NOT GLOB '*[^0-9a-f]*'
    ),
    byte_length INTEGER NOT NULL CHECK (byte_length >= 0),
    entry_kind TEXT NOT NULL CHECK (entry_kind IN ('file', 'symlink')),
    media_type TEXT NOT NULL CHECK (length(media_type) > 0 AND length(media_type) <= 255),
    language TEXT CHECK (
        language IS NULL
        OR language IN ('go', 'typescript', 'python', 'rust', 'java')
    ),
    predecessor_revision_id TEXT,
    deletion_state TEXT NOT NULL CHECK (deletion_state IN ('active', 'tombstoned')),
    acl_epoch INTEGER NOT NULL CHECK (acl_epoch >= 0),
    PRIMARY KEY (tenant_id, brain_id, source_id, source_revision_id),
    UNIQUE (
        tenant_id,
        brain_id,
        source_id,
        source_revision_id,
        source_object_id,
        path_digest
    ),
    FOREIGN KEY (tenant_id, brain_id, source_id, predecessor_revision_id)
    REFERENCES ingestion_source_revisions (tenant_id, brain_id, source_id, source_revision_id)
);
CREATE TABLE ingestion_snapshot_revisions (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    source_revision_id TEXT NOT NULL,
    source_object_id TEXT NOT NULL,
    path_digest TEXT NOT NULL CHECK (
        length(path_digest) = 64
        AND path_digest NOT GLOB '*[^0-9a-f]*'
    ),
    PRIMARY KEY (tenant_id, brain_id, source_id, snapshot_id, source_revision_id),
    UNIQUE (tenant_id, brain_id, source_id, snapshot_id, source_object_id),
    UNIQUE (tenant_id, brain_id, source_id, snapshot_id, path_digest),
    FOREIGN KEY (tenant_id, brain_id, source_id, snapshot_id)
    REFERENCES ingestion_snapshots (tenant_id, brain_id, source_id, snapshot_id),
    FOREIGN KEY (
        tenant_id,
        brain_id,
        source_id,
        source_revision_id,
        source_object_id,
        path_digest
    ) REFERENCES ingestion_source_revisions (
        tenant_id,
        brain_id,
        source_id,
        source_revision_id,
        source_object_id,
        path_digest
    )
);
CREATE TABLE ingestion_generations (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    generation_sequence INTEGER NOT NULL CHECK (generation_sequence > 0),
    snapshot_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('building', 'ready', 'degraded')),
    source_watermark INTEGER NOT NULL CHECK (source_watermark >= 0),
    created_at_ms INTEGER NOT NULL,
    published_at_ms INTEGER,
    PRIMARY KEY (tenant_id, brain_id, source_id, generation_id),
    UNIQUE (tenant_id, brain_id, source_id, generation_sequence),
    FOREIGN KEY (tenant_id, brain_id, source_id, snapshot_id)
    REFERENCES ingestion_snapshots (tenant_id, brain_id, source_id, snapshot_id)
);
CREATE TABLE ingestion_generation_readiness (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    language TEXT NOT NULL CHECK (language IN ('go', 'typescript', 'python', 'rust', 'java')),
    coverage TEXT NOT NULL CHECK (coverage IN ('pending', 'syntax_aware', 'lexical_degraded')),
    reason_code TEXT NOT NULL CHECK (length(reason_code) <= 128),
    PRIMARY KEY (tenant_id, brain_id, source_id, generation_id, language),
    FOREIGN KEY (tenant_id, brain_id, source_id, generation_id)
    REFERENCES ingestion_generations (tenant_id, brain_id, source_id, generation_id)
);
CREATE TABLE ingestion_current_generations (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, brain_id, source_id),
    FOREIGN KEY (tenant_id, brain_id, source_id, generation_id)
    REFERENCES ingestion_generations (tenant_id, brain_id, source_id, generation_id)
);
CREATE TRIGGER ingestion_current_generation_insert_complete
BEFORE INSERT ON ingestion_current_generations
WHEN
    NOT EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = new.tenant_id
            AND generation.brain_id = new.brain_id
            AND generation.source_id = new.source_id
            AND generation.generation_id = new.generation_id
            AND generation.state IN ('ready', 'degraded')
            AND (
                SELECT count(*) FROM ingestion_snapshot_revisions AS membership
                WHERE
                    membership.tenant_id = generation.tenant_id
                    AND membership.brain_id = generation.brain_id
                    AND membership.source_id = generation.source_id
                    AND membership.snapshot_id = generation.snapshot_id
            ) = (
                SELECT snapshot.path_count FROM ingestion_snapshots AS snapshot
                WHERE
                    snapshot.tenant_id = generation.tenant_id
                    AND snapshot.brain_id = generation.brain_id
                    AND snapshot.source_id = generation.source_id
                    AND snapshot.snapshot_id = generation.snapshot_id
            )
            AND (
                SELECT count(*) FROM ingestion_generation_readiness AS readiness
                WHERE
                    readiness.tenant_id = generation.tenant_id
                    AND readiness.brain_id = generation.brain_id
                    AND readiness.source_id = generation.source_id
                    AND readiness.generation_id = generation.generation_id
            ) = 5
            AND NOT EXISTS (
                SELECT 1 FROM ingestion_generation_readiness AS readiness
                WHERE
                    readiness.tenant_id = generation.tenant_id
                    AND readiness.brain_id = generation.brain_id
                    AND readiness.source_id = generation.source_id
                    AND readiness.generation_id = generation.generation_id
                    AND readiness.coverage = 'pending'
            )
    )
BEGIN
    SELECT raise(ABORT, 'current ingestion generation is incomplete');
END;
CREATE TRIGGER ingestion_current_generation_update_complete
BEFORE UPDATE OF tenant_id, brain_id, source_id, generation_id ON ingestion_current_generations
WHEN
    NOT EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = new.tenant_id
            AND generation.brain_id = new.brain_id
            AND generation.source_id = new.source_id
            AND generation.generation_id = new.generation_id
            AND generation.state IN ('ready', 'degraded')
            AND generation.generation_sequence > (
                SELECT old_generation.generation_sequence
                FROM ingestion_generations AS old_generation
                WHERE
                    old_generation.tenant_id = old.tenant_id
                    AND old_generation.brain_id = old.brain_id
                    AND old_generation.source_id = old.source_id
                    AND old_generation.generation_id = old.generation_id
            )
            AND (
                SELECT count(*) FROM ingestion_snapshot_revisions AS membership
                WHERE
                    membership.tenant_id = generation.tenant_id
                    AND membership.brain_id = generation.brain_id
                    AND membership.source_id = generation.source_id
                    AND membership.snapshot_id = generation.snapshot_id
            ) = (
                SELECT snapshot.path_count FROM ingestion_snapshots AS snapshot
                WHERE
                    snapshot.tenant_id = generation.tenant_id
                    AND snapshot.brain_id = generation.brain_id
                    AND snapshot.source_id = generation.source_id
                    AND snapshot.snapshot_id = generation.snapshot_id
            )
            AND (
                SELECT count(*) FROM ingestion_generation_readiness AS readiness
                WHERE
                    readiness.tenant_id = generation.tenant_id
                    AND readiness.brain_id = generation.brain_id
                    AND readiness.source_id = generation.source_id
                    AND readiness.generation_id = generation.generation_id
            ) = 5
            AND NOT EXISTS (
                SELECT 1 FROM ingestion_generation_readiness AS readiness
                WHERE
                    readiness.tenant_id = generation.tenant_id
                    AND readiness.brain_id = generation.brain_id
                    AND readiness.source_id = generation.source_id
                    AND readiness.generation_id = generation.generation_id
                    AND readiness.coverage = 'pending'
            )
    )
BEGIN
    SELECT raise(ABORT, 'current ingestion generation is incomplete');
END;
CREATE TABLE ingestion_tombstones (
    tenant_id TEXT NOT NULL,
    brain_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    tombstone_id TEXT NOT NULL,
    target_kind TEXT NOT NULL CHECK (target_kind IN ('source', 'source_revision')),
    target_revision_id TEXT,
    revocation_epoch INTEGER NOT NULL CHECK (revocation_epoch >= 0),
    reason_code TEXT NOT NULL CHECK (length(reason_code) > 0 AND length(reason_code) <= 128),
    recorded_at_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, brain_id, source_id, tombstone_id),
    CHECK (
        (target_kind = 'source' AND target_revision_id IS NULL)
        OR (target_kind = 'source_revision' AND target_revision_id IS NOT NULL)
    ),
    FOREIGN KEY (tenant_id, brain_id, source_id)
    REFERENCES ingestion_sources (tenant_id, brain_id, source_id),
    FOREIGN KEY (tenant_id, brain_id, source_id, target_revision_id)
    REFERENCES ingestion_source_revisions (tenant_id, brain_id, source_id, source_revision_id)
);
CREATE TRIGGER ingestion_root_immutable
BEFORE UPDATE ON ingestion_roots
BEGIN
    SELECT raise(ABORT, 'ingestion root metadata is immutable');
END;
CREATE TRIGGER ingestion_source_identity_immutable
BEFORE UPDATE ON ingestion_sources
WHEN
    old.tenant_id IS NOT new.tenant_id
    OR old.brain_id IS NOT new.brain_id
    OR old.source_id IS NOT new.source_id
    OR old.repository_id IS NOT new.repository_id
    OR old.configuration_digest IS NOT new.configuration_digest
    OR old.ignore_policy_digest IS NOT new.ignore_policy_digest
    OR old.created_at_ms IS NOT new.created_at_ms
BEGIN
    SELECT raise(ABORT, 'ingestion source identity is immutable');
END;
CREATE TRIGGER ingestion_source_lifecycle_forward_only
BEFORE UPDATE ON ingestion_sources
WHEN
    new.acl_epoch < old.acl_epoch
    OR new.revocation_epoch < old.revocation_epoch
    OR (
        old.revoked_at_ms IS NOT NULL
        AND new.revoked_at_ms IS NOT old.revoked_at_ms
    )
    OR NOT (
        new.state = old.state
        OR (old.state = 'admitted' AND new.state IN ('ready', 'reconciling', 'revoked'))
        OR (old.state = 'ready' AND new.state IN ('reconciling', 'revoked'))
        OR (old.state = 'reconciling' AND new.state IN ('ready', 'revoked'))
    )
BEGIN
    SELECT raise(ABORT, 'ingestion source lifecycle cannot regress');
END;
CREATE TRIGGER ingestion_source_revision_immutable
BEFORE UPDATE ON ingestion_source_revisions
WHEN
    old.tenant_id IS NOT new.tenant_id
    OR old.brain_id IS NOT new.brain_id
    OR old.source_id IS NOT new.source_id
    OR old.source_revision_id IS NOT new.source_revision_id
    OR old.source_object_id IS NOT new.source_object_id
    OR old.path_digest IS NOT new.path_digest
    OR old.git_blob_oid IS NOT new.git_blob_oid
    OR old.content_digest IS NOT new.content_digest
    OR old.byte_length IS NOT new.byte_length
    OR old.entry_kind IS NOT new.entry_kind
    OR old.media_type IS NOT new.media_type
    OR old.language IS NOT new.language
    OR old.predecessor_revision_id IS NOT new.predecessor_revision_id
    OR old.acl_epoch IS NOT new.acl_epoch
BEGIN
    SELECT raise(ABORT, 'ingestion source revision identity is immutable');
END;
CREATE TRIGGER ingestion_snapshot_revision_immutable
BEFORE UPDATE ON ingestion_snapshot_revisions
BEGIN
    SELECT raise(ABORT, 'ingestion snapshot revision membership is immutable');
END;
CREATE TRIGGER ingestion_published_snapshot_revision_insert_immutable
BEFORE INSERT ON ingestion_snapshot_revisions
WHEN
    EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = new.tenant_id
            AND generation.brain_id = new.brain_id
            AND generation.source_id = new.source_id
            AND generation.snapshot_id = new.snapshot_id
            AND generation.state IN ('ready', 'degraded')
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion snapshot membership is immutable');
END;
CREATE TRIGGER ingestion_published_snapshot_revision_delete_immutable
BEFORE DELETE ON ingestion_snapshot_revisions
WHEN
    EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = old.tenant_id
            AND generation.brain_id = old.brain_id
            AND generation.source_id = old.source_id
            AND generation.snapshot_id = old.snapshot_id
            AND generation.state IN ('ready', 'degraded')
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion snapshot membership is immutable');
END;
CREATE TRIGGER ingestion_source_revision_tombstone_forward_only
BEFORE UPDATE OF deletion_state ON ingestion_source_revisions
WHEN
    NOT (
        new.deletion_state = old.deletion_state
        OR (old.deletion_state = 'active' AND new.deletion_state = 'tombstoned')
    )
BEGIN
    SELECT raise(ABORT, 'ingestion source revision tombstone cannot be removed');
END;
CREATE TRIGGER ingestion_snapshot_immutable
BEFORE UPDATE ON ingestion_snapshots
BEGIN
    SELECT raise(ABORT, 'ingestion snapshot metadata is immutable');
END;
CREATE TRIGGER ingestion_generation_identity_immutable
BEFORE UPDATE ON ingestion_generations
WHEN
    old.tenant_id IS NOT new.tenant_id
    OR old.brain_id IS NOT new.brain_id
    OR old.source_id IS NOT new.source_id
    OR old.generation_id IS NOT new.generation_id
    OR old.generation_sequence IS NOT new.generation_sequence
    OR old.snapshot_id IS NOT new.snapshot_id
    OR old.source_watermark IS NOT new.source_watermark
    OR old.created_at_ms IS NOT new.created_at_ms
    OR (
        old.state IN ('ready', 'degraded')
        AND old.published_at_ms IS NOT new.published_at_ms
    )
BEGIN
    SELECT raise(ABORT, 'ingestion generation identity is immutable');
END;
CREATE TRIGGER ingestion_generation_insert_building
BEFORE INSERT ON ingestion_generations
WHEN new.state <> 'building'
BEGIN
    SELECT raise(ABORT, 'ingestion generation must begin building');
END;
CREATE TRIGGER ingestion_generation_state_forward_only
BEFORE UPDATE OF state ON ingestion_generations
WHEN
    NOT (
        new.state = old.state
        OR (old.state = 'building' AND new.state IN ('ready', 'degraded'))
    )
BEGIN
    SELECT raise(ABORT, 'ingestion generation lifecycle cannot regress');
END;
CREATE TRIGGER ingestion_generation_publish_complete
BEFORE UPDATE OF state ON ingestion_generations
WHEN
    old.state = 'building'
    AND new.state IN ('ready', 'degraded')
    AND (
        (
            SELECT count(*) FROM ingestion_snapshot_revisions AS membership
            WHERE
                membership.tenant_id = new.tenant_id
                AND membership.brain_id = new.brain_id
                AND membership.source_id = new.source_id
                AND membership.snapshot_id = new.snapshot_id
        ) <> (
            SELECT snapshot.path_count FROM ingestion_snapshots AS snapshot
            WHERE
                snapshot.tenant_id = new.tenant_id
                AND snapshot.brain_id = new.brain_id
                AND snapshot.source_id = new.source_id
                AND snapshot.snapshot_id = new.snapshot_id
        )
        OR (
            SELECT count(*) FROM ingestion_generation_readiness AS readiness
            WHERE
                readiness.tenant_id = new.tenant_id
                AND readiness.brain_id = new.brain_id
                AND readiness.source_id = new.source_id
                AND readiness.generation_id = new.generation_id
        ) <> 5
        OR EXISTS (
            SELECT 1 FROM ingestion_generation_readiness AS readiness
            WHERE
                readiness.tenant_id = new.tenant_id
                AND readiness.brain_id = new.brain_id
                AND readiness.source_id = new.source_id
                AND readiness.generation_id = new.generation_id
                AND readiness.coverage = 'pending'
        )
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion generation is incomplete');
END;
CREATE TRIGGER ingestion_published_readiness_insert_immutable
BEFORE INSERT ON ingestion_generation_readiness
WHEN
    EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = new.tenant_id
            AND generation.brain_id = new.brain_id
            AND generation.source_id = new.source_id
            AND generation.generation_id = new.generation_id
            AND generation.state IN ('ready', 'degraded')
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion generation readiness is immutable');
END;
CREATE TRIGGER ingestion_published_readiness_update_immutable
BEFORE UPDATE ON ingestion_generation_readiness
WHEN
    EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = old.tenant_id
            AND generation.brain_id = old.brain_id
            AND generation.source_id = old.source_id
            AND generation.generation_id = old.generation_id
            AND generation.state IN ('ready', 'degraded')
    )
    OR EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = new.tenant_id
            AND generation.brain_id = new.brain_id
            AND generation.source_id = new.source_id
            AND generation.generation_id = new.generation_id
            AND generation.state IN ('ready', 'degraded')
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion generation readiness is immutable');
END;
CREATE TRIGGER ingestion_published_readiness_delete_immutable
BEFORE DELETE ON ingestion_generation_readiness
WHEN
    EXISTS (
        SELECT 1 FROM ingestion_generations AS generation
        WHERE
            generation.tenant_id = old.tenant_id
            AND generation.brain_id = old.brain_id
            AND generation.source_id = old.source_id
            AND generation.generation_id = old.generation_id
            AND generation.state IN ('ready', 'degraded')
    )
BEGIN
    SELECT raise(ABORT, 'published ingestion generation readiness is immutable');
END;
CREATE TRIGGER ingestion_tombstone_immutable
BEFORE UPDATE ON ingestion_tombstones
BEGIN
    SELECT raise(ABORT, 'ingestion tombstone metadata is immutable');
END;
