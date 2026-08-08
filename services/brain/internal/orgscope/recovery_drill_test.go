package orgscope_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/orgscope"
)

const recoveryConfigDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type recoveryScenario struct {
	request orgscope.RecoveryDrillRequest
	target  *orgscope.Store
	source  *fixture
}

func newRecoveryScenario(t *testing.T) recoveryScenario {
	t.Helper()
	source := newFixture(t)
	backupAt := time.Date(2026, 8, 4, 11, 50, 0, 0, time.UTC)
	source.now = backupAt
	source.provision("alice", "bob")
	if _, err := source.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	teamScope := orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}
	if _, err := source.auth.Grant(orgscope.Grant{Subject: "group:eng", Scope: teamScope}); err != nil {
		t.Fatal(err)
	}
	erased := orgscope.Item{
		ID: "erased", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "payroll erased payload",
	}
	kept := orgscope.Item{
		ID: "kept", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "payroll retained payload",
	}
	source.put(erased, kept)
	// Populate cache and session projections in the source before backup. The
	// recovery target must derive, not trust, those projections.
	if _, err := source.store.Query(principal("alice"), "payroll"); err != nil {
		t.Fatal(err)
	}
	backup, err := source.store.CreateRecoveryBackup(orgscope.RecoveryBackupPin{
		GenerationID: "generation-42", ConfigDigest: recoveryConfigDigest,
		CreatedAt: backupAt, QueueSequence: 40,
	})
	if err != nil {
		t.Fatal(err)
	}

	erasedAt := time.Date(2026, 8, 4, 11, 58, 0, 0, time.UTC)
	source.now = erasedAt
	erasure, err := source.store.Erase("gdpr-current-overlay", erased.ID)
	if err != nil || !erasure.Complete {
		t.Fatalf("source erasure = %+v, %v", erasure, err)
	}
	tombstone := source.store.Tombstones()[0]
	late := orgscope.Item{
		ID: "late", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "payroll queued payload",
	}
	lateAt := time.Date(2026, 8, 4, 11, 59, 0, 0, time.UTC)
	source.now = lateAt
	if err := source.store.Put(late); err != nil {
		t.Fatal(err)
	}
	// The current ACL overlay revokes Bob after the backup. Restoring only
	// backup-era ACLs would incorrectly preserve his group access.
	if _, err := source.dir.RemoveMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	acl, err := source.auth.CreateACLSnapshot("generation-42", recoveryConfigDigest, lateAt)
	if err != nil {
		t.Fatal(err)
	}

	verifiedAt := time.Date(2026, 8, 4, 12, 20, 0, 0, time.UTC)
	targetDir, err := orgscope.NewDirectory("acme", func() time.Time { return verifiedAt })
	if err != nil {
		t.Fatal(err)
	}
	target := orgscope.NewStore(orgscope.NewAuthority(targetDir))
	request := orgscope.RecoveryDrillRequest{
		DrillID: "drill-312", TenantID: "acme", GenerationID: "generation-42",
		ConfigDigest: recoveryConfigDigest,
		IncidentAt:   time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		StartedAt:    time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		Backup:       backup,
		Queue: []orgscope.RecoveryQueueEntry{
			{Sequence: 41, GenerationID: "generation-42", At: erasedAt, Kind: orgscope.RecoveryQueueErase, Tombstone: &tombstone},
			{Sequence: 42, GenerationID: "generation-42", At: lateAt, Kind: orgscope.RecoveryQueuePut, Item: &late},
		},
		CurrentACL: acl,
		ACLProbes: []orgscope.RecoveryACLProbe{
			{Principal: principal("alice"), Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Allowed: true},
			{Principal: principal("bob"), Scope: teamScope, Allowed: false},
		},
		RepresentativeQuery: orgscope.RecoveryQueryProbe{Principal: principal("alice"), Query: "payroll"},
		ExpectedTombstones:  []orgscope.Tombstone{tombstone},
		Substrates:          orgscope.NewHermeticRecoverySubstrateMatrix(),
	}
	return recoveryScenario{request: request, target: target, source: source}
}

func TestRecoveryDrillRestoresPinnedGenerationQueueACLAndTombstones(t *testing.T) {
	scenario := newRecoveryScenario(t)
	receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "passed" || !receipt.Verified || receipt.ProductionCertified {
		t.Fatalf("receipt status = %+v", receipt)
	}
	if receipt.GenerationID != "generation-42" || receipt.ConfigDigest != recoveryConfigDigest ||
		receipt.BackupDigest == "" || receipt.ACLDigest == "" || receipt.QueueDigest == "" {
		t.Fatalf("receipt pins = %+v", receipt)
	}
	if receipt.RPOMillis != time.Minute.Milliseconds() || receipt.RTOMillis != (20*time.Minute).Milliseconds() ||
		!receipt.RPOWithinObjective || !receipt.RTOWithinObjective {
		t.Fatalf("recovery objectives = %+v", receipt)
	}
	if receipt.QueueFromSequence != 41 || receipt.QueueThroughSequence != 42 ||
		receipt.QueueEntriesReplayed != 2 || !receipt.QueueReplayComplete {
		t.Fatalf("queue receipt = %+v", receipt)
	}
	if !receipt.ProjectionRebuilt || !receipt.ACLRestored || receipt.ACLProbesPassed != 2 ||
		!receipt.TombstonesComplete || receipt.TombstonesExpected != 1 || receipt.TombstonesVerified != 1 {
		t.Fatalf("verification receipt = %+v", receipt)
	}
	if receipt.TombstoneDigest == "" {
		t.Fatal("missing canonical tombstone digest")
	}
	wantHermeticChecks := []string{
		"primary", "search_index", "query_cache", "session_history",
		"backup_manifest", "pre_erasure_restore", "reingest_guard",
	}
	if len(receipt.HermeticStoreChecks) != len(wantHermeticChecks) {
		t.Fatalf("hermetic checks = %+v", receipt.HermeticStoreChecks)
	}
	for i, check := range receipt.HermeticStoreChecks {
		if check.Name != wantHermeticChecks[i] || !check.Passed || check.TombstonesChecked != 1 {
			t.Fatalf("hermetic check[%d] = %+v", i, check)
		}
	}
	wantSubstrates := orgscope.RequiredRecoverySubstrates()
	if len(receipt.Substrates) != len(wantSubstrates) {
		t.Fatalf("substrates = %+v", receipt.Substrates)
	}
	gotSubstrates := make(map[orgscope.RecoverySubstrateKind]bool, len(receipt.Substrates))
	for _, substrate := range receipt.Substrates {
		if !substrate.Passed || substrate.ProviderBoundary != orgscope.RecoveryBoundaryHermeticFake ||
			substrate.RestoreCandidates != 3 || substrate.TombstonesChecked != 1 || substrate.RepresentativeRecords != 2 {
			t.Fatalf("substrate = %+v", substrate)
		}
		gotSubstrates[substrate.Kind] = true
	}
	for _, kind := range wantSubstrates {
		if !gotSubstrates[kind] {
			t.Fatalf("missing substrate receipt %q", kind)
		}
	}
	if !receipt.RepresentativeQueryRan || receipt.RepresentativeQueryCitations != 2 ||
		receipt.CacheEntriesPopulated != 2 || receipt.SessionEntriesPopulated != 2 {
		t.Fatalf("representative query receipt = %+v", receipt)
	}
	if leaks := scenario.target.VerifyErasure("erased"); len(leaks.Leaks) != 0 {
		t.Fatalf("restored target leaks = %v", leaks.Leaks)
	}
	result, err := scenario.target.Query(principal("alice"), "payroll")
	if err != nil {
		t.Fatal(err)
	}
	got := ids(result.Citations)
	if got["erased"] || !got["kept"] || !got["late"] {
		t.Fatalf("restored citations = %v", got)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"erased payload", "retained payload", "queued payload"} {
		if strings.Contains(string(encoded), payload) {
			t.Fatalf("receipt disclosed payload: %s", encoded)
		}
	}
}

func TestRecoveryDrillKeepsStaleLifecycleGrantsDeniedAfterRestore(t *testing.T) {
	scenario := newRecoveryScenario(t)
	source := scenario.source
	company := orgscope.Scope{Kind: orgscope.ScopeCompany}
	team := orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}

	if _, err := source.auth.Grant(orgscope.Grant{Subject: "user:bob", Scope: company}); err != nil {
		t.Fatal(err)
	}
	source.provision("carol")
	if _, err := source.auth.Grant(orgscope.Grant{
		Subject: "user:carol", Scope: company, DelegatedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.dir.Deprovision("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.dir.Deprovision("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.dir.DeleteGroup("eng"); err != nil {
		t.Fatal(err)
	}
	source.provision("alice", "bob")
	if _, err := source.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}

	acl, err := source.auth.CreateACLSnapshot("generation-42", recoveryConfigDigest, source.now)
	if err != nil {
		t.Fatal(err)
	}
	userIncarnations := make(map[string]uint64, len(acl.Users))
	for _, user := range acl.Users {
		userIncarnations[user.ID] = user.Incarnation
	}
	if userIncarnations["alice"] != 3 || userIncarnations["bob"] != 3 || userIncarnations["carol"] != 1 {
		t.Fatalf("snapshot user incarnations = %v", userIncarnations)
	}
	if len(acl.Groups) != 1 || acl.Groups[0].ID != "eng" || acl.Groups[0].Incarnation != 3 {
		t.Fatalf("snapshot groups = %+v", acl.Groups)
	}
	grantBindings := make(map[string][2]uint64, len(acl.Grants))
	for _, grant := range acl.Grants {
		grantBindings[grant.Subject+"|"+grant.Scope.Key()] = [2]uint64{
			grant.SubjectIncarnation, grant.DelegatorIncarnation,
		}
	}
	if grantBindings["group:eng|team:eng"] != [2]uint64{1, 0} ||
		grantBindings["user:bob|company"] != [2]uint64{1, 0} ||
		grantBindings["user:carol|company"] != [2]uint64{1, 1} {
		t.Fatalf("snapshot grant bindings = %v", grantBindings)
	}
	encoded, err := json.Marshal(acl)
	if err != nil {
		t.Fatal(err)
	}
	var serializedACL orgscope.ACLSnapshot
	if err := json.Unmarshal(encoded, &serializedACL); err != nil {
		t.Fatal(err)
	}
	scenario.request.CurrentACL = serializedACL
	scenario.request.ACLProbes = []orgscope.RecoveryACLProbe{
		{Principal: principal("alice"), Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Allowed: true},
		{Principal: principal("bob"), Scope: company, Allowed: false},
		{Principal: principal("bob"), Scope: team, Allowed: false},
		{Principal: principal("carol"), Scope: company, Allowed: false},
	}

	receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Verified || !receipt.ACLRestored || receipt.ACLProbesPassed != len(scenario.request.ACLProbes) {
		t.Fatalf("lifecycle recovery receipt = %+v", receipt)
	}
}

func TestRecoveryDrillFailureInjectionNeverVerifies(t *testing.T) {
	points := []orgscope.RecoveryFailurePoint{
		orgscope.RecoveryFailureAfterBackup,
		orgscope.RecoveryFailureAfterQueue,
		orgscope.RecoveryFailureAfterACL,
		orgscope.RecoveryFailureAfterRebuild,
		orgscope.RecoveryFailureAfterVerify,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			scenario := newRecoveryScenario(t)
			scenario.request.FailAt = point
			receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
			if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || !errors.Is(err, orgscope.ErrInjectedRecoveryFailure) {
				t.Fatalf("injected failure = %v", err)
			}
			if receipt.Status != "failed" || receipt.Verified || receipt.ProductionCertified ||
				receipt.FailurePoint != point || receipt.FailureCode != "injected_failure" {
				t.Fatalf("receipt = %+v", receipt)
			}
		})
	}
}

func TestRecoveryDrillRejectsAdversarialPinsManifestsAndQueue(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*orgscope.RecoveryDrillRequest)
		wantCode string
	}{
		{
			name: "generation mismatch",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.GenerationID = "stale-generation"
			},
			wantCode: "invalid_request",
		},
		{
			name: "config mismatch",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.ConfigDigest = strings.Repeat("b", 64)
			},
			wantCode: "invalid_request",
		},
		{
			name: "tampered backup",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.Backup.Store.Items[0].Text = "tampered"
			},
			wantCode: "backup_rejected",
		},
		{
			name: "tampered acl",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.CurrentACL.Users[0].Active = !request.CurrentACL.Users[0].Active
			},
			wantCode: "acl_rejected",
		},
		{
			name: "queue gap",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.Queue[1].Sequence++
			},
			wantCode: "invalid_request",
		},
		{
			name: "queue generation rollback",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.Queue[1].GenerationID = "generation-41"
			},
			wantCode: "invalid_request",
		},
		{
			name: "incomplete tombstone manifest",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.ExpectedTombstones = nil
			},
			wantCode: "tombstone_incomplete",
		},
		{
			name: "extra tombstone manifest",
			mutate: func(request *orgscope.RecoveryDrillRequest) {
				request.ExpectedTombstones = append(request.ExpectedTombstones, orgscope.Tombstone{
					ItemID: "invented", Reason: "invented-overlay",
					At: time.Date(2026, 8, 4, 11, 57, 0, 0, time.UTC),
				})
			},
			wantCode: "tombstone_incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRecoveryScenario(t)
			test.mutate(&scenario.request)
			receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
			if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || receipt.Verified ||
				receipt.FailureCode != test.wantCode {
				t.Fatalf("receipt = %+v, err=%v", receipt, err)
			}
		})
	}
}

func TestRecoveryDrillRejectsQueueResurrectionAndObjectiveMisses(t *testing.T) {
	t.Run("queue resurrection", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		resurrect := *scenario.request.Queue[0].Tombstone
		item := orgscope.Item{
			ID: resurrect.ItemID, Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
			Owner: "alice", Text: "resurrection attempt",
		}
		scenario.request.Queue = append(scenario.request.Queue,
			orgscope.RecoveryQueueEntry{
				Sequence: 43, GenerationID: "generation-42",
				At:   time.Date(2026, 8, 4, 11, 59, 30, 0, time.UTC),
				Kind: orgscope.RecoveryQueuePut, Item: &item,
			},
		)
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || receipt.FailureCode != "queue_replay_failed" ||
			receipt.QueueEntriesReplayed != 2 || receipt.QueueReplayComplete || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
		if leaks := scenario.target.VerifyErasure("erased"); len(leaks.Leaks) != 0 {
			t.Fatalf("failed replay resurrected: %v", leaks.Leaks)
		}
	})

	t.Run("rpo miss before mutation", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.Objectives.MaxRPO = 30 * time.Second
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || receipt.FailureCode != "rpo_exceeded" ||
			receipt.RPOWithinObjective || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
		if backup, backupErr := scenario.target.CreateBackup(); backupErr != nil || len(backup.Items) != 0 {
			t.Fatalf("RPO failure mutated target: %+v, %v", backup, backupErr)
		}
	})

	t.Run("rto miss", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.Objectives.MaxRTO = 10 * time.Minute
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || receipt.FailureCode != "rto_exceeded" ||
			receipt.RTOWithinObjective || receipt.RTOMillis != (20*time.Minute).Milliseconds() || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
	})
}

func TestRecoveryDrillRequiresEmptyTargetAndKnownFailurePoint(t *testing.T) {
	t.Run("occupied target", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		if err := scenario.target.Put(orgscope.Item{
			ID: "occupied", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "occupied"},
			Owner: "occupied", Text: "occupied target",
		}); err != nil {
			t.Fatal(err)
		}
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || receipt.FailureCode != "target_not_empty" || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
	})

	t.Run("unknown failure point", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.FailAt = orgscope.RecoveryFailurePoint("after_writer_promotion")
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) || errors.Is(err, orgscope.ErrInjectedRecoveryFailure) ||
			receipt.FailureCode != "invalid_request" || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
	})
}
