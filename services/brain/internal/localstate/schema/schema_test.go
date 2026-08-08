// Package schema tests deterministic properties of checked-in SQL migrations.
package schema

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrationIsIdempotentlyVersionedAndRollsBackOnFailure(t *testing.T) {
	db := openMigratedDB(t)
	if err := applyMigration(db, 1, migrationSource(t)); err != nil {
		t.Fatalf("already applied migration must be a no-op: %v", err)
	}
	if err := applyMigration(db, 2, migrationV2Source(t)); err != nil {
		t.Fatalf("already applied migration must be a no-op: %v", err)
	}
	if err := applyMigration(db, 3, migrationV3Source(t)); err != nil {
		t.Fatalf("already applied migration must be a no-op: %v", err)
	}
	if err := applyMigration(db, 4, migrationV4Source(t)); err != nil {
		t.Fatalf("already applied migration must be a no-op: %v", err)
	}
	if err := applyMigration(db, 5, migrationV5Source(t)); err != nil {
		t.Fatalf("already applied migration must be a no-op: %v", err)
	}
	if err := applyMigration(db, 9, "CREATE TABLE transient_rollback (id INTEGER); INVALID SQL;"); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='transient_rollback'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left a partial table")
	}
}

func TestStageThreeMetadataIsTenantBrainScopedWithoutSearchProjections(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO ingestion_sources
		(tenant_id,brain_id,source_id,repository_id,configuration_digest,ignore_policy_digest,
		 state,acl_epoch,revocation_epoch,created_at_ms)
		VALUES ('t1','b1','s1','repo1',printf('%064d',1),printf('%064d',2),'admitted',3,4,1)`)
	assertExecFails(t, db, `INSERT INTO ingestion_sources VALUES
		('t1','b1','bad-digest','repo',replace(printf('%064d',0),'0','g'),printf('%064d',2),'admitted',0,0,1,NULL)`)
	mustExec(t, db, `INSERT INTO ingestion_roots
		(tenant_id,brain_id,source_id,approved_root_id,symlink_policy)
		VALUES ('t1','b1','s1',printf('%064d',20),'record_without_follow')`)
	assertExecFails(t, db, `INSERT INTO ingestion_roots
		(tenant_id,brain_id,source_id,approved_root_id,symlink_policy)
		VALUES ('t1','other','s1',printf('%064d',21),'record_without_follow')`)
	mustExec(t, db, `INSERT INTO ingestion_snapshots
		(tenant_id,brain_id,source_id,snapshot_id,commit_oid,tree_oid,policy_digest,path_count,snapshot_digest,observed_at_ms)
		VALUES ('t1','b1','s1','snap1',printf('%040d',1),printf('%040d',2),printf('%064d',3),1,printf('%064d',4),3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_snapshots VALUES
		('t1','b1','s1','bad-oid',replace(printf('%040d',0),'0','g'),printf('%040d',2),printf('%064d',3),0,printf('%064d',4),3)`)
	mustExec(t, db, `INSERT INTO ingestion_source_revisions
		(tenant_id,brain_id,source_id,source_revision_id,source_object_id,path_digest,
		 git_blob_oid,content_digest,byte_length,entry_kind,media_type,language,predecessor_revision_id,deletion_state,acl_epoch)
		VALUES ('t1','b1','s1','rev1','object1',printf('%064d',8),printf('%040d',9),printf('%064d',10),10,
		'file','text/x-go','go',NULL,'active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','bad-path','object','src/main.go',printf('%040d',9),printf('%064d',10),1,
		'file','text/plain',NULL,NULL,'active',3)`)
	mustExec(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','symlink-rev','symlink-object',printf('%064d',16),printf('%040d',17),printf('%064d',18),8,
		'symlink','inode/symlink',NULL,NULL,'active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','bad-kind','object-kind',printf('%064d',19),printf('%040d',20),printf('%064d',21),1,
		'directory','text/plain',NULL,NULL,'active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','bad-media','object-media',printf('%064d',22),printf('%040d',23),printf('%064d',24),1,
		'file','',NULL,NULL,'active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','bad-language','object-language',printf('%064d',25),printf('%040d',26),printf('%064d',27),1,
		'file','text/plain','ruby',NULL,'active',3)`)
	mustExec(t, db, `INSERT INTO ingestion_snapshot_revisions
		(tenant_id,brain_id,source_id,snapshot_id,source_revision_id,source_object_id,path_digest)
		VALUES ('t1','b1','s1','snap1','rev1','object1',printf('%064d',8))`)
	mustExec(t, db, `INSERT INTO ingestion_snapshots
		(tenant_id,brain_id,source_id,snapshot_id,commit_oid,tree_oid,policy_digest,path_count,snapshot_digest,observed_at_ms)
		VALUES ('t1','b1','s1','snap2',printf('%040d',5),printf('%040d',6),printf('%064d',3),1,printf('%064d',7),4)`)
	mustExec(t, db, `INSERT INTO ingestion_snapshot_revisions
		(tenant_id,brain_id,source_id,snapshot_id,source_revision_id,source_object_id,path_digest)
		VALUES ('t1','b1','s1','snap2','rev1','object1',printf('%064d',8))`)
	assertExecFails(t, db, `INSERT INTO ingestion_snapshot_revisions
		(tenant_id,brain_id,source_id,snapshot_id,source_revision_id,source_object_id,path_digest)
		VALUES ('t1','b1','s1','snap2','rev1','wrong-object',printf('%064d',8))`)
	mustExec(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','rev2','object1',printf('%064d',11),printf('%040d',12),printf('%064d',13),11,
		'file','text/x-go','go','rev1','active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_snapshot_revisions VALUES
		('t1','b1','s1','snap2','rev2','object1',printf('%064d',11))`)
	mustExec(t, db, `INSERT INTO ingestion_source_revisions VALUES
		('t1','b1','s1','rev3','object3',printf('%064d',8),printf('%040d',14),printf('%064d',15),12,
		'file','text/x-go','go',NULL,'active',3)`)
	assertExecFails(t, db, `INSERT INTO ingestion_snapshot_revisions VALUES
		('t1','b1','s1','snap2','rev3','object3',printf('%064d',8))`)
	assertExecFails(t, db, `UPDATE ingestion_source_revisions SET path_digest=printf('%064d',99)
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1' AND source_revision_id='rev1'`)
	assertExecFails(t, db, `UPDATE ingestion_source_revisions SET git_blob_oid=printf('%040d',99)
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1' AND source_revision_id='rev1'`)
	mustExec(t, db, `INSERT INTO ingestion_generations
		(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,source_watermark,created_at_ms)
		VALUES ('t1','b1','s1','gen1',1,'snap1','building',1,4)`)
	assertExecFails(t, db, `INSERT INTO ingestion_current_generations
		(tenant_id,brain_id,source_id,generation_id,updated_at_ms)
		VALUES ('t1','b1','s1','gen1',5)`)
	for _, language := range []string{"go", "typescript", "python", "rust"} {
		mustExec(t, db, `INSERT INTO ingestion_generation_readiness
			(tenant_id,brain_id,source_id,generation_id,language,coverage,reason_code)
			VALUES ('t1','b1','s1','gen1','`+language+`','syntax_aware','')`)
	}
	assertExecFails(t, db, `INSERT INTO ingestion_current_generations
		(tenant_id,brain_id,source_id,generation_id,updated_at_ms)
		VALUES ('t1','b1','s1','gen1',5)`)
	mustExec(t, db, `INSERT INTO ingestion_generation_readiness
		(tenant_id,brain_id,source_id,generation_id,language,coverage,reason_code)
		VALUES ('t1','b1','s1','gen1','java','pending','parser_pending')`)
	assertExecFails(t, db, `INSERT INTO ingestion_current_generations
		(tenant_id,brain_id,source_id,generation_id,updated_at_ms)
		VALUES ('t1','b1','s1','gen1',5)`)
	mustExec(t, db, `UPDATE ingestion_generation_readiness SET coverage='syntax_aware', reason_code=''
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1'
		AND generation_id='gen1' AND language='java'`)
	mustExec(t, db, `UPDATE ingestion_generations SET state='ready', published_at_ms=5
		WHERE generation_id='gen1'`)
	mustExec(t, db, `INSERT INTO ingestion_current_generations
		(tenant_id,brain_id,source_id,generation_id,updated_at_ms)
		VALUES ('t1','b1','s1','gen1',5)`)
	assertExecFails(t, db, `UPDATE ingestion_generation_readiness SET coverage='pending'
		WHERE generation_id='gen1' AND language='go'`)
	assertExecFails(t, db, `DELETE FROM ingestion_generation_readiness
		WHERE generation_id='gen1' AND language='go'`)
	assertExecFails(t, db, `INSERT OR REPLACE INTO ingestion_generation_readiness VALUES
		('t1','b1','s1','gen1','go','lexical_degraded','replacement')`)
	assertExecFails(t, db, `UPDATE ingestion_generations SET state='degraded' WHERE generation_id='gen1'`)
	assertExecFails(t, db, `UPDATE ingestion_generations SET snapshot_id='snap2' WHERE generation_id='gen1'`)
	assertExecFails(t, db, `UPDATE ingestion_snapshots SET path_count=2 WHERE snapshot_id='snap1'`)
	assertExecFails(t, db, `DELETE FROM ingestion_snapshot_revisions
		WHERE snapshot_id='snap1' AND source_revision_id='rev1'`)
	mustExec(t, db, `INSERT INTO ingestion_generations
		(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,source_watermark,created_at_ms)
		VALUES ('t1','b1','s1','gen2',2,'snap1','building',2,6)`)
	for _, language := range []string{"go", "typescript", "python", "rust"} {
		mustExec(t, db, `INSERT INTO ingestion_generation_readiness
			(tenant_id,brain_id,source_id,generation_id,language,coverage,reason_code)
			VALUES ('t1','b1','s1','gen2','`+language+`','syntax_aware','')`)
	}
	assertExecFails(t, db, `UPDATE ingestion_current_generations SET generation_id='gen2'
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1' AND generation_id='gen1'`)
	mustExec(t, db, `INSERT INTO ingestion_generation_readiness
		(tenant_id,brain_id,source_id,generation_id,language,coverage,reason_code)
		VALUES ('t1','b1','s1','gen2','java','lexical_degraded','parser_unavailable')`)
	mustExec(t, db, `UPDATE ingestion_generations SET state='ready', published_at_ms=6
		WHERE generation_id='gen2'`)
	mustExec(t, db, `UPDATE ingestion_current_generations SET generation_id='gen2'
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1' AND generation_id='gen1'`)
	assertExecFails(t, db, `UPDATE ingestion_generation_readiness SET coverage='pending'
		WHERE generation_id='gen1' AND language='go'`)
	assertExecFails(t, db, `DELETE FROM ingestion_snapshot_revisions
		WHERE snapshot_id='snap1' AND source_revision_id='rev1'`)
	assertExecFails(t, db, `UPDATE ingestion_current_generations SET generation_id='gen1'
		WHERE generation_id='gen2'`)
	mustExec(t, db, `INSERT INTO ingestion_tombstones
		(tenant_id,brain_id,source_id,tombstone_id,target_kind,target_revision_id,
		 revocation_epoch,reason_code,recorded_at_ms)
		VALUES ('t1','b1','s1','ts1','source',NULL,4,'source_revoked',7)`)
	assertExecFails(t, db, `UPDATE ingestion_tombstones SET reason_code='changed'
		WHERE tenant_id='t1' AND brain_id='b1' AND source_id='s1' AND tombstone_id='ts1'`)

	for _, table := range []string{"ingestion_search", "ingestion_symbols", "ingestion_occurrences"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rebuildable projection table %q is canonical", table)
		}
	}
}

func TestStageFiveFactoryMetadataEnforcesForeignKeysAndShape(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO factory_runs
		(tenant_id,principal_id,run_id,session_id,intent_id,intent_digest,intent_artifact_id,
		 repository_git_oid,plan_id,admitted_at_ms)
		VALUES ('t1','p1','run-1','sess1','intent-1',printf('%064d',9),'artifact-intent',
		 printf('%040d',7),'plan-1',1)`)
	mustExec(t, db, `INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,
		 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
		VALUES ('t1','p1','run-1','leaf-a','leaf','artifact-goal',printf('%064d',8),'["src/go"]',
		 printf('%064d',2),'model-1','static','["factory.leaf.execute"]','["src/go"]',
		 'nonce-1',100,1,1,printf('%064d',3))`)

	// Idempotency, mailbox, and rollback facts must reference canonical runs
	// and plan nodes; orphaned references reject.
	assertExecFails(t, db, `INSERT INTO factory_idempotency
		VALUES ('t1','p1','admit','key-orphan',printf('%064d',1),'run-missing',1)`)
	mustExec(t, db, `INSERT INTO factory_idempotency
		VALUES ('t1','p1','admit','key-1',printf('%064d',1),'run-1',1)`)
	assertExecFails(t, db, `INSERT INTO factory_mailbox_messages
		(tenant_id,principal_id,run_id,message_id,task_id,kind,sequence,correlation_id,causation_id,
		 sender_principal_id,payload_artifact_id,payload_digest,sent_at_ms)
		VALUES ('t1','p1','run-1','msg-orphan','node-missing','QUESTION',1,'','','p1','artifact-m',printf('%064d',4),1)`)
	mustExec(t, db, `INSERT INTO factory_mailbox_messages
		(tenant_id,principal_id,run_id,message_id,task_id,kind,sequence,correlation_id,causation_id,
		 sender_principal_id,payload_artifact_id,payload_digest,sent_at_ms)
		VALUES ('t1','p1','run-1','msg-1','leaf-a','QUESTION',1,'','','p1','artifact-m',printf('%064d',4),1)`)
	assertExecFails(t, db, `INSERT INTO factory_rollback_receipts
		VALUES ('t1','p1','run-missing','receipt-orphan','candidate_rejected',printf('%064d',5),1)`)
	mustExec(t, db, `INSERT INTO factory_rollback_receipts
		VALUES ('t1','p1','run-1','receipt-1','candidate_rejected',printf('%064d',5),1)`)

	// The leaf shape check rejects scope, route, or grant facts on non-leaf
	// nodes and their absence on leaves.
	assertExecFails(t, db, `INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,
		 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
		VALUES ('t1','p1','run-1','review','review','artifact-goal',printf('%064d',8),'["src/go"]',
		 NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`)
	assertExecFails(t, db, `INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,
		 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
		VALUES ('t1','p1','run-1','leaf-b','leaf','artifact-goal',printf('%064d',8),'[]',
		 NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`)
	mustExec(t, db, `INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest)
		VALUES ('t1','p1','run-1','review','review','artifact-goal',printf('%064d',8))`)

	// Dense fences and terminal states hold under the same connection.
	mustExec(t, db, `INSERT INTO factory_run_states VALUES ('t1','p1','run-1',1,'PLANNING',1)`)
	assertExecFails(t, db, `INSERT INTO factory_run_states VALUES ('t1','p1','run-1',3,'READY',2)`)
	mustExec(t, db, `INSERT INTO factory_run_states VALUES ('t1','p1','run-1',2,'CANCELLED',2)`)
	assertExecFails(t, db, `INSERT INTO factory_run_states VALUES ('t1','p1','run-1',3,'FAILED',3)`)
	mustExec(t, db, `INSERT INTO factory_leases VALUES ('t1','p1','run-1','leaf-a',1,'lease-1','worker-1',1,100)`)
	assertExecFails(t, db, `INSERT INTO factory_leases VALUES ('t1','p1','run-1','leaf-a',3,'lease-3','worker-2',1,100)`)
}

func TestConversationTurnsAreSessionScopedWithBoundedShape(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',1,'user','active','artifact1',printf('%064d',1),2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2',2,'assistant','failed','key2','artifact2',printf('%064d',2),3)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn-k',3,'user','active','keyU','artifact20',printf('%064d',20),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn-a',3,'assistant','active',NULL,'artifact21',printf('%064d',21),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn-b',3,'assistant','active',replace(printf('%0513d',9),' ','x'),'artifact22',printf('%064d',22),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn3',2,'user','active','artifact3',printf('%064d',3),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',3,'user','active','artifact3',printf('%064d',3),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn4',0,'user','active','artifact4',printf('%064d',4),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn5',4,'tool','active','artifact5',printf('%064d',5),5)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn6',4,'user','superseded','artifact6',printf('%064d',6),6)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn7',4,'user','active','artifact7',replace(printf('%064d',0),'0','g'),7)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p2','sess1','turn8',4,'user','active','artifact8',printf('%064d',8),8)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','missing-session','turn9',1,'user','active','artifact9',printf('%064d',9),9)`)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess3','p1','t1',501,10)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess3','turn1',1,'user','active','artifact10',printf('%064d',10),11)`)
	var indexCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type='index' AND name='conversation_turns_history'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatal("principal history index is missing")
	}
}

func TestConversationTurnSequenceAppendsDensely(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess2','p1','t1',501,2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',1,'user','active','artifact1',printf('%064d',1),2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2',2,'assistant','active','key2','artifact2',printf('%064d',2),3)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn3',3,'user','active','artifact3',printf('%064d',3),4)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn5',5,'user','active','artifact5',printf('%064d',5),5)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2-backward',2,'user','active','artifact6',printf('%064d',6),6)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess2','turn4',2,'user','active','artifact4',printf('%064d',4),4)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess2','turn4',1,'user','active','artifact4',printf('%064d',4),4)`)
}

func TestConversationCompletionIsExactlyOncePerQuery(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess2','p2','t1',502,2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',1,'user','active','artifact1',printf('%064d',1),2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2',2,'assistant','active','key1','artifact2',printf('%064d',2),3)`)
	assertExecFails(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn3',3,'assistant','failed','key1','artifact3',printf('%064d',3),4)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p2','sess2','turn1',1,'user','active','artifact4',printf('%064d',4),2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p2','sess2','turn2',2,'assistant','active','key1','artifact5',printf('%064d',5),3)`)
}

func TestConversationTurnsAndIdempotencyAreImmutable(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',1,'user','active','artifact1',printf('%064d',1),2)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2',2,'assistant','failed','key2','artifact2',printf('%064d',2),3)`)
	mustExec(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key1',printf('%064d',10),'sess1','turn1',2)`)
	for _, statement := range []string{
		`UPDATE conversation_turns SET status='failed' WHERE turn_id='turn1'`,
		`UPDATE conversation_turns SET status='active' WHERE turn_id='turn2'`,
		`UPDATE conversation_turns SET payload_digest=printf('%064d',99) WHERE turn_id='turn1'`,
		`UPDATE conversation_turns SET occurred_at_ms=99 WHERE turn_id='turn1'`,
		`DELETE FROM conversation_turns WHERE turn_id='turn1'`,
		`DELETE FROM conversation_turns`,
		`UPDATE conversation_query_idempotency SET request_digest=printf('%064d',11)
			WHERE tenant_id='t1' AND principal_id='p1' AND idempotency_key='key1'`,
		`UPDATE conversation_query_idempotency SET user_turn_id='turn2'
			WHERE tenant_id='t1' AND principal_id='p1' AND idempotency_key='key1'`,
		`DELETE FROM conversation_query_idempotency
			WHERE tenant_id='t1' AND principal_id='p1' AND idempotency_key='key1'`,
	} {
		assertExecFails(t, db, statement)
	}
	var turnCount, idempotencyCount int
	if err := db.QueryRow(`SELECT count(*) FROM conversation_turns`).Scan(&turnCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM conversation_query_idempotency`).Scan(&idempotencyCount); err != nil {
		t.Fatal(err)
	}
	if turnCount != 2 || idempotencyCount != 1 {
		t.Fatalf("immutable tables lost rows: turns=%d idempotency=%d", turnCount, idempotencyCount)
	}
	var status, digest string
	if err := db.QueryRow(`SELECT status, payload_digest FROM conversation_turns WHERE turn_id='turn1'`).
		Scan(&status, &digest); err != nil {
		t.Fatal(err)
	}
	if status != "active" || digest != "0000000000000000000000000000000000000000000000000000000000000001" {
		t.Fatalf("failed updates mutated a turn: status=%q digest=%q", status, digest)
	}
}

func TestConversationQueryIdempotencyDistinguishesRetriesAndConflicts(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess1','p1','t1',501,1)`)
	mustExec(t, db, `INSERT INTO sessions (session_id,principal_id,tenant_id,peer_uid,opened_at_ms)
		VALUES ('sess2','p2','t1',502,1)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn1',1,'user','active','artifact1',printf('%064d',1),2)`)
	mustExec(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key1',printf('%064d',10),'sess1','turn1',2)`)
	assertExecFails(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key1',printf('%064d',11),'sess1','turn1',3)`)
	assertExecFails(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key2',replace(printf('%064d',0),'0','g'),'sess1','turn1',3)`)
	assertExecFails(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key3',printf('%064d',12),'sess1','missing-turn',3)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','turn2',2,'assistant','active','key-a','artifact5',printf('%064d',5),3)`)
	assertExecFails(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key4',printf('%064d',13),'sess1','turn2',3)`)
	mustExec(t, db, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p2','sess2','turn1',1,'user','active','artifact4',printf('%064d',4),2)`)
	mustExec(t, db, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p2','key1',printf('%064d',10),'sess2','turn1',2)`)
}

func TestIngestionGenerationLifecycleIsForwardOnly(t *testing.T) {
	db := openMigratedDB(t)
	insertIngestionSourceAndSnapshot(t, db, "s1")
	mustExec(t, db, `INSERT INTO ingestion_generations
		(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,
		 source_watermark,created_at_ms)
		VALUES ('t','b','s1','ready',1,'snap','building',1,1)`)
	insertCompleteReadiness(t, db, "ready")
	mustExec(t, db, `UPDATE ingestion_generations SET state='ready', published_at_ms=2
		WHERE tenant_id='t' AND brain_id='b' AND source_id='s1' AND generation_id='ready'`)
	mustExec(t, db, `INSERT INTO ingestion_current_generations VALUES ('t','b','s1','ready',2)`)
	for _, statement := range []string{
		`UPDATE ingestion_generations SET state='building' WHERE generation_id='ready'`,
		`UPDATE ingestion_generations SET state='degraded' WHERE generation_id='ready'`,
		`UPDATE ingestion_generations SET source_watermark=2 WHERE generation_id='ready'`,
		`UPDATE ingestion_generations SET published_at_ms=NULL WHERE generation_id='ready'`,
	} {
		assertExecFails(t, db, statement)
	}
	mustExec(t, db, `INSERT INTO ingestion_snapshots VALUES
		('t','b','s1','mismatch',printf('%040d',5),printf('%040d',6),printf('%064d',3),1,printf('%064d',7),2)`)
	mustExec(t, db, `INSERT INTO ingestion_generations
		(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,
		 source_watermark,created_at_ms)
		VALUES ('t','b','s1','degraded',2,'mismatch','building',1,1)`)
	insertCompleteReadiness(t, db, "degraded")
	assertExecFails(t, db, `UPDATE ingestion_generations SET state='degraded', published_at_ms=2
		WHERE generation_id='degraded'`)
	assertExecFails(t, db, `UPDATE ingestion_current_generations SET generation_id='degraded'
		WHERE source_id='s1'`)
	assertExecFails(t, db, `UPDATE ingestion_generations SET state='ready' WHERE generation_id='degraded'`)
}

func TestIngestionSourceLifecycleAndEpochsAreForwardOnly(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO ingestion_sources
		(tenant_id,brain_id,source_id,repository_id,configuration_digest,ignore_policy_digest,
		 state,acl_epoch,revocation_epoch,created_at_ms)
		VALUES ('t','b','s','repo',printf('%064d',1),printf('%064d',2),'admitted',2,3,1)`)
	assertExecFails(t, db, `INSERT INTO ingestion_sources VALUES
		('t','b','bad1','repo',printf('%064d',1),printf('%064d',2),'revoked',2,3,1,NULL)`)
	assertExecFails(t, db, `INSERT INTO ingestion_sources VALUES
		('t','b','bad2','repo',printf('%064d',1),printf('%064d',2),'ready',2,3,1,4)`)
	mustExec(t, db, `UPDATE ingestion_sources SET state='ready' WHERE source_id='s'`)
	assertExecFails(t, db, `UPDATE ingestion_sources SET state='admitted' WHERE source_id='s'`)
	assertExecFails(t, db, `UPDATE ingestion_sources SET acl_epoch=1 WHERE source_id='s'`)
	assertExecFails(t, db, `UPDATE ingestion_sources SET revocation_epoch=2 WHERE source_id='s'`)
	for _, statement := range []string{
		`UPDATE ingestion_sources SET tenant_id='other' WHERE source_id='s'`,
		`UPDATE ingestion_sources SET brain_id='other' WHERE source_id='s'`,
		`UPDATE ingestion_sources SET source_id='other' WHERE source_id='s'`,
		`UPDATE ingestion_sources SET repository_id='changed' WHERE source_id='s'`,
		`UPDATE ingestion_sources SET configuration_digest=printf('%064d',9) WHERE source_id='s'`,
		`UPDATE ingestion_sources SET ignore_policy_digest=printf('%064d',9) WHERE source_id='s'`,
		`UPDATE ingestion_sources SET created_at_ms=2 WHERE source_id='s'`,
	} {
		assertExecFails(t, db, statement)
	}
	mustExec(t, db, `UPDATE ingestion_sources SET state='reconciling' WHERE source_id='s'`)
	mustExec(t, db, `UPDATE ingestion_sources SET state='ready' WHERE source_id='s'`)
	mustExec(t, db, `UPDATE ingestion_sources
		SET state='revoked', revocation_epoch=4, revoked_at_ms=5 WHERE source_id='s'`)
	for _, state := range []string{"admitted", "ready", "reconciling"} {
		assertExecFails(t, db, `UPDATE ingestion_sources SET state='`+state+`' WHERE source_id='s'`)
	}
	assertExecFails(t, db, `UPDATE ingestion_sources SET revoked_at_ms=NULL WHERE source_id='s'`)
	mustExec(t, db, `UPDATE ingestion_sources SET acl_epoch=3, revocation_epoch=5 WHERE source_id='s'`)
}

func TestTenantScopedAuthorityForeignKeysRejectCrossTenantLinks(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, "INSERT INTO sessions VALUES ('s1','p1','t1',501,1,NULL)")
	mustExec(t, db, "INSERT INTO sessions VALUES ('s2','p2','t2',502,1,NULL)")

	assertExecFails(t, db, "INSERT INTO commands VALUES ('bad-session','t2','p1','s1','artifact.admit','k','d',1,'accepted',1)")
	mustExec(t, db, "INSERT INTO commands VALUES ('c1','t1','p1','s1','artifact.admit','k','d',1,'accepted',1)")
	assertExecFails(t, db, "INSERT INTO events VALUES (1,'e-cross','t2','artifact','a',1,'c1','d',1)")
	assertExecFails(t, db, "INSERT INTO receipts VALUES ('r-cross','c1','t2','accepted','ok',1,1)")

	mustExec(t, db, "INSERT INTO events VALUES (1,'e1','t1','artifact','a',1,'c1','d',1)")
	mustExec(t, db, "INSERT INTO receipts VALUES ('r1','c1','t1','accepted','ok',1,1)")
	assertExecFails(t, db, "INSERT INTO outbox VALUES ('o-cross','t2',1,'d',NULL)")
	assertExecFails(t, db, "INSERT INTO checkpoints VALUES ('cp-cross','t2',1,'d',1,1)")
	assertExecFails(t, db, "INSERT INTO tombstones VALUES ('ts-cross','t2','a',1,'r1','delete',1)")

	mustExec(t, db, "INSERT INTO tombstones VALUES ('ts1','t1','a',1,'r1','delete',1)")
	assertExecFails(t, db, "INSERT INTO purge_jobs VALUES ('p-cross','t2','a',1,'ts1',1,'scheduled',NULL)")
	assertExecFails(t, db, "INSERT INTO purge_jobs VALUES ('p-artifact','t1','different-artifact',1,'ts1',1,'scheduled',NULL)")
	assertExecFails(t, db, "INSERT INTO purge_jobs VALUES ('p-generation','t1','a',2,'ts1',1,'scheduled',NULL)")
	mustExec(t, db, "INSERT INTO purge_jobs VALUES ('p1','t1','a',1,'ts1',1,'scheduled',NULL)")
}

func TestCommandIdempotencyScopeDistinguishesRetriesAndConflicts(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, "INSERT INTO sessions VALUES ('s1','p1','t1',501,1,NULL)")
	mustExec(t, db, "INSERT INTO sessions VALUES ('s2','p2','t1',502,1,NULL)")
	mustExec(t, db, "INSERT INTO sessions VALUES ('s3','p3','t2',503,1,NULL)")
	mustExec(t, db, "INSERT INTO commands VALUES ('c1','t1','p1','s1','artifact.admit','same-key','digest-1',7,'accepted',1)")
	mustExec(t, db, "INSERT INTO command_idempotency VALUES ('t1','p1','s1','artifact.admit','same-key','digest-1',7,'c1')")

	assertExecFails(t, db, "INSERT INTO commands VALUES ('c-conflict','t1','p1','s1','artifact.admit','same-key','digest-2',8,'accepted',1)")
	assertExecFails(t, db, "INSERT INTO command_idempotency VALUES ('t1','p1','s1','artifact.admit','same-key','digest-2',8,'c1')")
	mustExec(t, db, "INSERT INTO commands VALUES ('c-mapping','t1','p1','s1','artifact.admit','command-key','digest-5',9,'accepted',1)")
	assertExecFails(t, db, "INSERT INTO command_idempotency VALUES ('t1','p1','s1','artifact.read','mapping-key','different-digest',7,'c-mapping')")
	mustExec(t, db, "INSERT INTO commands VALUES ('c2','t1','p1','s1','artifact.read','same-key','digest-2',7,'accepted',1)")
	mustExec(t, db, "INSERT INTO commands VALUES ('c3','t1','p2','s2','artifact.admit','same-key','digest-3',7,'accepted',1)")
	mustExec(t, db, "INSERT INTO commands VALUES ('c4','t2','p3','s3','artifact.admit','same-key','digest-4',7,'accepted',1)")
}

func TestCurrentArtifactRequiresCompletePublishedFrames(t *testing.T) {
	db := openMigratedDB(t)
	assertExecFails(t, db, `INSERT INTO artifact_manifests
		(artifact_id,tenant_id,generation,content_digest,byte_length,frame_count,key_epoch,status)
		VALUES ('zero','t',1,'d',1,0,1,'published')`)

	insertManifest(t, db, "staged", 1, 1, "staged")
	insertFrame(t, db, "staged", 1, 0, 0, 1)
	assertCurrentFails(t, db, "staged", 1)

	insertManifest(t, db, "missing", 1, 1, "staged")
	publishManifest(t, db, "missing", 1)
	assertCurrentFails(t, db, "missing", 1)

	insertManifest(t, db, "gapped", 3, 2, "staged")
	insertFrame(t, db, "gapped", 1, 0, 0, 1)
	insertFrame(t, db, "gapped", 1, 1, 2, 2)
	publishManifest(t, db, "gapped", 1)
	assertCurrentFails(t, db, "gapped", 1)

	insertManifest(t, db, "overlap", 3, 2, "staged")
	insertFrame(t, db, "overlap", 1, 0, 0, 2)
	insertFrame(t, db, "overlap", 1, 1, 1, 1)
	publishManifest(t, db, "overlap", 1)
	assertCurrentFails(t, db, "overlap", 1)

	insertManifest(t, db, "truncated", 2, 1, "staged")
	insertFrame(t, db, "truncated", 1, 0, 0, 1)
	publishManifest(t, db, "truncated", 1)
	assertCurrentFails(t, db, "truncated", 1)

	insertManifest(t, db, "count-mismatch", 2, 2, "staged")
	insertFrame(t, db, "count-mismatch", 1, 0, 0, 2)
	publishManifest(t, db, "count-mismatch", 1)
	assertCurrentFails(t, db, "count-mismatch", 1)

	insertManifest(t, db, "complete", 1, 1, "staged")
	insertFrame(t, db, "complete", 1, 0, 0, 1)
	publishManifest(t, db, "complete", 1)
	mustExec(t, db, "INSERT INTO artifact_generations VALUES ('t','complete',1)")
	assertExecFails(t, db, "UPDATE artifact_frames SET length_bytes=2 WHERE tenant_id='t' AND artifact_id='complete'")

	insertManifestGeneration(t, db, "complete", 2, 1, 1, "staged")
	insertFrame(t, db, "complete", 2, 0, 0, 1)
	assertExecFails(t, db, "UPDATE artifact_generations SET current_generation=2 WHERE tenant_id='t' AND artifact_id='complete'")
	mustExec(t, db, "UPDATE artifact_manifests SET status='published' WHERE tenant_id='t' AND artifact_id='complete' AND generation=2")
	mustExec(t, db, "UPDATE artifact_generations SET current_generation=2 WHERE tenant_id='t' AND artifact_id='complete'")
	assertExecFails(t, db, "UPDATE artifact_manifests SET content_digest='changed' WHERE tenant_id='t' AND artifact_id='complete' AND generation=1")
	assertExecFails(t, db, "UPDATE artifact_frames SET frame_digest='changed' WHERE tenant_id='t' AND artifact_id='complete' AND generation=1")
	mustExec(t, db, "UPDATE artifact_manifests SET status='tombstoned' WHERE tenant_id='t' AND artifact_id='complete' AND generation=1")
	assertExecFails(t, db, "UPDATE artifact_manifests SET status='published' WHERE tenant_id='t' AND artifact_id='complete' AND generation=1")
}

func TestDurableAdapterMetadataIsScopedAndForwardOnly(t *testing.T) {
	db := openMigratedDB(t)
	mustExec(t, db, `INSERT INTO key_epochs VALUES ('t1',1,'key-1','current')`)
	assertExecFails(t, db, `INSERT INTO key_epochs VALUES ('t1',2,'key-2','current')`)
	mustExec(t, db, `INSERT INTO key_epochs VALUES ('t2',1,'key-other','current')`)

	mustExec(t, db, `INSERT INTO evidence_records
		(tenant_id,brain_id,evidence_id,artifact_id,artifact_generation,anchor,
		 digest_algorithm,digest_hex,tombstoned)
		VALUES ('t1','b1','e1','a1',1,'full','sha256','d',0)`)
	mustExec(t, db, `INSERT INTO evidence_records
		(tenant_id,brain_id,evidence_id,artifact_id,artifact_generation,anchor,
		 digest_algorithm,digest_hex,tombstoned)
		VALUES ('t1','b1','e2','a2',1,'full','sha256','d',0)`)
	mustExec(t, db, `INSERT INTO evidence_records
		(tenant_id,brain_id,evidence_id,artifact_id,artifact_generation,anchor,
		 digest_algorithm,digest_hex,tombstoned)
		VALUES ('t2','b1','e3','a3',1,'full','sha256','d',0)`)
	mustExec(t, db, `INSERT INTO evidence_lineage VALUES ('t1','b1','e1','e2','derived-from')`)
	assertExecFails(t, db, `INSERT INTO evidence_lineage VALUES ('t1','b1','e1','e3','cross-tenant')`)
	assertExecFails(t, db, `UPDATE evidence_records SET anchor='changed'
		WHERE tenant_id='t1' AND brain_id='b1' AND evidence_id='e1'`)
	mustExec(t, db, `UPDATE evidence_records SET tombstoned=1
		WHERE tenant_id='t1' AND brain_id='b1' AND evidence_id='e1'`)
	assertExecFails(t, db, `UPDATE evidence_records SET tombstoned=0
		WHERE tenant_id='t1' AND brain_id='b1' AND evidence_id='e1'`)

	insertManifest(t, db, "reserved", 1, 1, "staged")
	mustExec(t, db, `INSERT INTO artifact_reservation_fences DEFAULT VALUES`)
	mustExec(t, db, `INSERT INTO artifact_reservations VALUES ('t','reserved',1,'locator',1)`)
	assertExecFails(t, db, `UPDATE artifact_reservations SET locator='changed'
		WHERE tenant_id='t' AND artifact_id='reserved' AND generation=1`)
}

func TestVersionOneUpgradeRetainsCanonicalRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := applyMigration(db, 1, migrationSource(t)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO sessions VALUES ('s1','p1','t1',501,1,NULL)")
	if err := applyMigration(db, 2, migrationV2Source(t)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, 2, migrationV2Source(t)); err != nil {
		t.Fatalf("v2 retry = %v", err)
	}
	var principal string
	if err := db.QueryRow("SELECT principal_id FROM sessions WHERE session_id='s1'").Scan(&principal); err != nil || principal != "p1" {
		t.Fatalf("retained principal=%q err=%v", principal, err)
	}
	var versions int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version IN (1,2)").Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("versions=%d err=%v", versions, err)
	}
}

func openMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyMigration(db, 1, migrationSource(t)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, 2, migrationV2Source(t)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, 3, migrationV3Source(t)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, 4, migrationV4Source(t)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, 5, migrationV5Source(t)); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertIngestionSourceAndSnapshot(t *testing.T, db *sql.DB, sourceID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ingestion_sources
		(tenant_id,brain_id,source_id,repository_id,configuration_digest,ignore_policy_digest,
		 state,acl_epoch,revocation_epoch,created_at_ms)
		VALUES ('t','b',?,'repo',printf('%064d',1),printf('%064d',2),'admitted',1,1,1)`, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO ingestion_snapshots
		(tenant_id,brain_id,source_id,snapshot_id,commit_oid,tree_oid,policy_digest,path_count,
		 snapshot_digest,observed_at_ms)
		VALUES ('t','b',?,'snap',printf('%040d',1),printf('%040d',2),printf('%064d',3),0,
		 printf('%064d',4),1)`, sourceID)
	if err != nil {
		t.Fatal(err)
	}
}

func insertCompleteReadiness(t *testing.T, db *sql.DB, generationID string) {
	t.Helper()
	for _, language := range []string{"go", "typescript", "python", "rust", "java"} {
		_, err := db.Exec(`INSERT INTO ingestion_generation_readiness VALUES
			('t','b','s1',?,?,'syntax_aware','')`, generationID, language)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func migrationSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("migrations", "001_stage02_authority.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func migrationV2Source(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("migrations", "002_durable_storage_adapters.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func migrationV3Source(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("migrations", "003_stage03_ingestion.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func migrationV4Source(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("migrations", "004_stage04_conversation.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func migrationV5Source(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("migrations", "005_stage05_factory.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func applyMigration(db *sql.DB, version int, source string) error {
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY CHECK(version > 0), applied_at_ms INTEGER NOT NULL)"); err != nil {
		return err
	}
	var found int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version = ?", version).Scan(&found); err != nil {
		return err
	}
	if found != 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(source); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations (version, applied_at_ms) VALUES (?, 0)", version); err != nil {
		return err
	}
	return tx.Commit()
}

func insertManifest(t *testing.T, db *sql.DB, artifact string, length, frames int, status string) {
	t.Helper()
	insertManifestGeneration(t, db, artifact, 1, length, frames, status)
}

func insertManifestGeneration(t *testing.T, db *sql.DB, artifact string, generation, length, frames int, status string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO artifact_manifests
		(artifact_id,tenant_id,generation,content_digest,byte_length,frame_count,key_epoch,status)
		VALUES (?, 't', ?, 'd', ?, ?, 1, ?)`, artifact, generation, length, frames, status)
	if err != nil {
		t.Fatal(err)
	}
}

func insertFrame(t *testing.T, db *sql.DB, artifact string, generation, index, offset, length int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO artifact_frames
		(tenant_id,artifact_id,generation,frame_index,offset_bytes,length_bytes,frame_digest)
		VALUES ('t', ?, ?, ?, ?, ?, 'd')`,
		artifact, generation, index, offset, length)
	if err != nil {
		t.Fatal(err)
	}
}

func publishManifest(t *testing.T, db *sql.DB, artifact string, generation int) {
	t.Helper()
	if _, err := db.Exec(
		"UPDATE artifact_manifests SET status='published' WHERE tenant_id='t' AND artifact_id=? AND generation=?",
		artifact,
		generation,
	); err != nil {
		t.Fatal(err)
	}
}

func assertCurrentFails(t *testing.T, db *sql.DB, artifact string, generation int) {
	t.Helper()
	_, err := db.Exec("INSERT INTO artifact_generations VALUES ('t', ?, ?)", artifact, generation)
	if err == nil {
		t.Fatalf("incomplete artifact %q became current", artifact)
	}
}

func mustExec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func assertExecFails(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err == nil {
		t.Fatalf("statement unexpectedly succeeded: %s", statement)
	}
}
