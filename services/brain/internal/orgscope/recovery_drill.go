package orgscope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	// ErrRecoveryDrillFailed reports that a disposable recovery target did
	// not satisfy every recovery check.
	ErrRecoveryDrillFailed = errors.New("orgscope: recovery drill failed")
	// ErrInjectedRecoveryFailure distinguishes a deliberate drill stop from
	// a substrate or validation failure.
	ErrInjectedRecoveryFailure = errors.New("orgscope: injected recovery drill failure")
)

const (
	defaultRecoveryRPO       = 5 * time.Minute
	defaultRecoveryRTO       = 60 * time.Minute
	defaultMaxRecoveryEvents = 10_000
)

// RecoveryFailurePoint names deterministic interruptions between recovery
// stages. Injection never produces a verified receipt.
type RecoveryFailurePoint string

const (
	RecoveryFailureNone         RecoveryFailurePoint = ""
	RecoveryFailureAfterBackup  RecoveryFailurePoint = "after_backup_restore"
	RecoveryFailureAfterQueue   RecoveryFailurePoint = "after_queue_replay"
	RecoveryFailureAfterACL     RecoveryFailurePoint = "after_acl_restore"
	RecoveryFailureAfterRebuild RecoveryFailurePoint = "after_projection_rebuild"
	RecoveryFailureAfterVerify  RecoveryFailurePoint = "after_tombstone_validation"
)

// RecoveryObjectives are hard drill bounds. Zero values use the Stage 10
// objectives (RPO <= 5 minutes and RTO <= 60 minutes).
type RecoveryObjectives struct {
	MaxRPO         time.Duration `json:"max_rpo"`
	MaxRTO         time.Duration `json:"max_rto"`
	MaxQueueEvents int           `json:"max_queue_events"`
}

// RecoveryBackupPin identifies one immutable backup point and the queue
// cursor immediately included in it.
type RecoveryBackupPin struct {
	GenerationID  string    `json:"generation_id"`
	ConfigDigest  string    `json:"config_digest"`
	CreatedAt     time.Time `json:"created_at"`
	QueueSequence uint64    `json:"queue_sequence"`
}

// RecoveryBackup wraps the existing store backup with generation,
// configuration, time, and queue-cursor pins. Digest covers every pin and the
// canonical store payload.
type RecoveryBackup struct {
	TenantID      string    `json:"tenant_id"`
	GenerationID  string    `json:"generation_id"`
	ConfigDigest  string    `json:"config_digest"`
	CreatedAt     time.Time `json:"created_at"`
	QueueSequence uint64    `json:"queue_sequence"`
	Store         Backup    `json:"store"`
	Digest        string    `json:"digest"`
}

// CreateRecoveryBackup creates a generation-pinned backup without changing
// the compatibility-oriented CreateBackup API.
func (s *Store) CreateRecoveryBackup(pin RecoveryBackupPin) (RecoveryBackup, error) {
	if s == nil || s.auth == nil || s.auth.dir == nil || !validID(pin.GenerationID) ||
		!validDigest(pin.ConfigDigest) || pin.CreatedAt.IsZero() {
		return RecoveryBackup{}, ErrRejected
	}
	backup, err := s.CreateBackup()
	if err != nil {
		return RecoveryBackup{}, err
	}
	out := RecoveryBackup{
		TenantID: s.auth.dir.TenantID(), GenerationID: pin.GenerationID,
		ConfigDigest: pin.ConfigDigest, CreatedAt: pin.CreatedAt.UTC(),
		QueueSequence: pin.QueueSequence, Store: backup,
	}
	out.Digest, err = recoveryBackupDigest(out)
	if err != nil {
		return RecoveryBackup{}, err
	}
	return out, nil
}

// ACLUser is one user state in a current authorization snapshot.
type ACLUser struct {
	ID          string `json:"id"`
	Active      bool   `json:"active"`
	Incarnation uint64 `json:"incarnation"`
}

// ACLGroup is one group and its current member set.
type ACLGroup struct {
	ID          string   `json:"id"`
	Members     []string `json:"members"`
	Incarnation uint64   `json:"incarnation"`
}

// ACLGrant is the recovery-local wire representation of a grant. Keeping the
// lifecycle bindings here preserves Grant's compatibility-oriented API while
// ensuring a restore cannot rebind an old grant to a new subject or delegator.
type ACLGrant struct {
	Grant
	SubjectIncarnation   uint64 `json:"subject_incarnation"`
	DelegatorIncarnation uint64 `json:"delegator_incarnation"`
}

// ACLDeny is one current deny overlay.
type ACLDeny struct {
	UserID string `json:"user_id"`
	Scope  Scope  `json:"scope"`
}

// ACLSnapshot is a canonical, generation/config-pinned copy of all
// authorization state represented by this package. It includes lifecycle
// receipts so restored sequence/epoch ordering continues monotonically.
type ACLSnapshot struct {
	TenantID     string     `json:"tenant_id"`
	GenerationID string     `json:"generation_id"`
	ConfigDigest string     `json:"config_digest"`
	CapturedAt   time.Time  `json:"captured_at"`
	Epoch        uint64     `json:"epoch"`
	Sequence     uint64     `json:"sequence"`
	Users        []ACLUser  `json:"users"`
	Groups       []ACLGroup `json:"groups"`
	Grants       []ACLGrant `json:"grants"`
	Denies       []ACLDeny  `json:"denies"`
	Receipts     []Receipt  `json:"receipts"`
	Digest       string     `json:"digest"`
}

// CreateACLSnapshot captures the current directory and authority atomically.
func (a *Authority) CreateACLSnapshot(generationID, configDigest string, capturedAt time.Time) (ACLSnapshot, error) {
	if a == nil || a.dir == nil || !validID(generationID) || !validDigest(configDigest) || capturedAt.IsZero() {
		return ACLSnapshot{}, ErrRejected
	}
	a.mu.Lock()
	a.dir.mu.Lock()
	snapshot := ACLSnapshot{
		TenantID: a.dir.tenantID, GenerationID: generationID, ConfigDigest: configDigest,
		CapturedAt: capturedAt.UTC(), Epoch: a.dir.epoch, Sequence: a.dir.seq,
		Receipts: append([]Receipt(nil), a.dir.receipts...),
	}
	for id, active := range a.dir.users {
		snapshot.Users = append(snapshot.Users, ACLUser{
			ID: id, Active: active, Incarnation: a.dir.versions[id],
		})
	}
	for id, members := range a.dir.groups {
		group := ACLGroup{ID: id, Incarnation: a.dir.groupVer[id]}
		for member := range members {
			group.Members = append(group.Members, member)
		}
		snapshot.Groups = append(snapshot.Groups, group)
	}
	for _, byScope := range a.grants {
		for _, grant := range byScope {
			snapshot.Grants = append(snapshot.Grants, ACLGrant{
				Grant: grant.Grant, SubjectIncarnation: grant.subjectIncarnation,
				DelegatorIncarnation: grant.delegatorIncarnation,
			})
		}
	}
	for key := range a.denies {
		userID, scope, ok := splitDenyKey(key)
		if !ok {
			a.dir.mu.Unlock()
			a.mu.Unlock()
			return ACLSnapshot{}, ErrRejected
		}
		snapshot.Denies = append(snapshot.Denies, ACLDeny{UserID: userID, Scope: scope})
	}
	a.dir.mu.Unlock()
	a.mu.Unlock()

	normalizeACLSnapshot(&snapshot)
	digest, err := aclSnapshotDigest(snapshot)
	if err != nil {
		return ACLSnapshot{}, err
	}
	snapshot.Digest = digest
	return snapshot, nil
}

// RecoveryQueueKind is one canonical post-backup mutation.
type RecoveryQueueKind string

const (
	RecoveryQueuePut   RecoveryQueueKind = "item.put"
	RecoveryQueueErase RecoveryQueueKind = "item.erase"
)

// RecoveryQueueEntry is a bounded, contiguous mutation after the backup
// cursor. Every entry is pinned to the generation being restored.
type RecoveryQueueEntry struct {
	Sequence     uint64            `json:"sequence"`
	GenerationID string            `json:"generation_id"`
	At           time.Time         `json:"at"`
	Kind         RecoveryQueueKind `json:"kind"`
	Item         *Item             `json:"item,omitempty"`
	Tombstone    *Tombstone        `json:"tombstone,omitempty"`
}

// RecoveryACLProbe verifies behavior, not merely serialization equality,
// after the current ACL snapshot is restored.
type RecoveryACLProbe struct {
	Principal Principal `json:"principal"`
	Scope     Scope     `json:"scope"`
	Allowed   bool      `json:"allowed"`
}

// RecoveryDrillRequest is one deterministic exercise into an empty,
// disposable target. ExpectedTombstones is the complete current set, not a
// sample; an omitted or extra tombstone fails the drill.
type RecoveryDrillRequest struct {
	DrillID             string                  `json:"drill_id"`
	TenantID            string                  `json:"tenant_id"`
	GenerationID        string                  `json:"generation_id"`
	ConfigDigest        string                  `json:"config_digest"`
	IncidentAt          time.Time               `json:"incident_at"`
	StartedAt           time.Time               `json:"started_at"`
	Backup              RecoveryBackup          `json:"backup"`
	Queue               []RecoveryQueueEntry    `json:"queue"`
	CurrentACL          ACLSnapshot             `json:"current_acl"`
	ACLProbes           []RecoveryACLProbe      `json:"acl_probes,omitempty"`
	RepresentativeQuery RecoveryQueryProbe      `json:"representative_query"`
	ExpectedTombstones  []Tombstone             `json:"expected_tombstones"`
	Objectives          RecoveryObjectives      `json:"objectives"`
	FailAt              RecoveryFailurePoint    `json:"fail_at,omitempty"`
	Substrates          RecoverySubstrateMatrix `json:"-"`
}

// RecoveryQueryProbe populates the recovered target's cache and session
// projections before erasure verification. A query with no authorized
// citations is inconclusive and fails the drill.
type RecoveryQueryProbe struct {
	Principal Principal `json:"principal"`
	Query     string    `json:"query"`
}

// RecoveryCheck is one payload-free drill step result.
type RecoveryCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// RecoverySubstrateReceipt reports a conclusive identifier-only erasure probe
// for one required typed substrate. ProviderBoundary makes hermetic evidence
// distinguishable from an explicitly supplied provider adapter.
type RecoverySubstrateReceipt struct {
	Name                  string                   `json:"name"`
	Kind                  RecoverySubstrateKind    `json:"kind"`
	ProviderBoundary      RecoveryProviderBoundary `json:"provider_boundary"`
	RestoreCandidates     int                      `json:"restore_candidates"`
	RepresentativeRecords int                      `json:"representative_records"`
	TombstonesChecked     int                      `json:"tombstones_checked"`
	Passed                bool                     `json:"passed"`
}

// RecoveryDrillReceipt is evidence for this hermetic contract only. A passed
// receipt intentionally never sets ProductionCertified.
type RecoveryDrillReceipt struct {
	DrillID                      string                     `json:"drill_id"`
	TenantID                     string                     `json:"tenant_id"`
	GenerationID                 string                     `json:"generation_id"`
	ConfigDigest                 string                     `json:"config_digest"`
	BackupDigest                 string                     `json:"backup_digest,omitempty"`
	ACLDigest                    string                     `json:"acl_digest,omitempty"`
	QueueDigest                  string                     `json:"queue_digest,omitempty"`
	StartedAt                    string                     `json:"started_at,omitempty"`
	RecoveryPointAt              string                     `json:"recovery_point_at,omitempty"`
	VerifiedAt                   string                     `json:"verified_at,omitempty"`
	RPOMillis                    int64                      `json:"rpo_ms"`
	RTOMillis                    int64                      `json:"rto_ms"`
	MaxRPOMillis                 int64                      `json:"max_rpo_ms"`
	MaxRTOMillis                 int64                      `json:"max_rto_ms"`
	RPOWithinObjective           bool                       `json:"rpo_within_objective"`
	RTOWithinObjective           bool                       `json:"rto_within_objective"`
	QueueFromSequence            uint64                     `json:"queue_from_sequence"`
	QueueThroughSequence         uint64                     `json:"queue_through_sequence"`
	QueueEntriesReplayed         int                        `json:"queue_entries_replayed"`
	QueueReplayComplete          bool                       `json:"queue_replay_complete"`
	ProjectionRebuilt            bool                       `json:"projection_rebuilt"`
	ACLRestored                  bool                       `json:"acl_restored"`
	ACLProbesPassed              int                        `json:"acl_probes_passed"`
	TombstonesExpected           int                        `json:"tombstones_expected"`
	TombstonesVerified           int                        `json:"tombstones_verified"`
	TombstonesComplete           bool                       `json:"tombstones_complete"`
	TombstoneDigest              string                     `json:"tombstone_digest,omitempty"`
	Substrates                   []RecoverySubstrateReceipt `json:"substrates"`
	HermeticStoreChecks          []RecoverySubstrateReceipt `json:"hermetic_store_checks"`
	RepresentativeQueryRan       bool                       `json:"representative_query_ran"`
	RepresentativeQueryCitations int                        `json:"representative_query_citations"`
	CacheEntriesPopulated        int                        `json:"cache_entries_populated"`
	SessionEntriesPopulated      int                        `json:"session_entries_populated"`
	Status                       string                     `json:"status"`
	Verified                     bool                       `json:"verified"`
	ProductionCertified          bool                       `json:"production_certified"`
	FailurePoint                 RecoveryFailurePoint       `json:"failure_point,omitempty"`
	FailureCode                  string                     `json:"failure_code,omitempty"`
	Checks                       []RecoveryCheck            `json:"checks"`
}

// RunRecoveryDrill verifies pins before mutating an empty disposable target,
// restores the backup, replays the contiguous queue, restores current ACLs,
// rebuilds projections, and proves tombstone completeness/non-resurrection.
func RunRecoveryDrill(request RecoveryDrillRequest, target *Store) (RecoveryDrillReceipt, error) {
	return RunRecoveryDrillContext(context.Background(), request, target)
}

// RunRecoveryDrillContext is RunRecoveryDrill with cooperative cancellation
// at every stage and through each substrate adapter. Adapters must honor the
// supplied context for the runner's wall-clock bound to be effective.
func RunRecoveryDrillContext(ctx context.Context, request RecoveryDrillRequest, target *Store) (RecoveryDrillReceipt, error) {
	receipt := RecoveryDrillReceipt{
		DrillID: request.DrillID, TenantID: request.TenantID,
		GenerationID: request.GenerationID, ConfigDigest: request.ConfigDigest,
		StartedAt: request.StartedAt.UTC().Format(time.RFC3339Nano), Status: "failed",
		ProductionCertified: false,
	}
	if ctx == nil {
		return failRecoveryDrill(receipt, "request", "invalid_request", ErrRejected)
	}
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}
	objectives, err := normalizeRecoveryObjectives(request.Objectives)
	if err != nil {
		return failRecoveryDrill(receipt, "request", "invalid_request", err)
	}
	receipt.MaxRPOMillis = objectives.MaxRPO.Milliseconds()
	receipt.MaxRTOMillis = objectives.MaxRTO.Milliseconds()
	if err := validateRecoveryRequest(request, target, objectives); err != nil {
		code := "invalid_request"
		if errors.Is(err, ErrMissingRecoverySubstrate) {
			code = "missing_substrate"
		}
		return failRecoveryDrill(receipt, "request", code, err)
	}
	receipt.TombstonesExpected = len(request.ExpectedTombstones)
	receipt.TombstoneDigest, err = canonicalTombstoneDigest(request.ExpectedTombstones)
	if err != nil {
		return failRecoveryDrill(receipt, "request", "invalid_request", err)
	}
	receipt.QueueDigest, err = recoveryQueueDigest(request.Queue)
	if err != nil {
		return failRecoveryDrill(receipt, "request", "invalid_request", err)
	}
	if err := verifyRecoveryBackup(request.Backup); err != nil {
		return failRecoveryDrill(receipt, "backup_manifest", "backup_rejected", err)
	}
	if err := validateACLSnapshot(request.CurrentACL); err != nil {
		return failRecoveryDrill(receipt, "acl_manifest", "acl_rejected", err)
	}
	receipt.BackupDigest = request.Backup.Digest
	receipt.ACLDigest = request.CurrentACL.Digest
	receipt.Checks = append(receipt.Checks,
		RecoveryCheck{Name: "generation_config_pins", Status: "passed"},
		RecoveryCheck{Name: "backup_manifest", Status: "passed"},
		RecoveryCheck{Name: "acl_manifest", Status: "passed"},
	)
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}

	recoveryPoint := request.Backup.CreatedAt
	if len(request.Queue) > 0 {
		recoveryPoint = request.Queue[len(request.Queue)-1].At
	}
	receipt.RecoveryPointAt = recoveryPoint.UTC().Format(time.RFC3339Nano)
	receipt.RPOMillis = request.IncidentAt.Sub(recoveryPoint).Milliseconds()
	receipt.RPOWithinObjective = request.IncidentAt.Sub(recoveryPoint) <= objectives.MaxRPO
	if !receipt.RPOWithinObjective {
		return failRecoveryDrill(receipt, "rpo", "rpo_exceeded", ErrRejected)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "rpo", Status: "passed"})

	if err := requireEmptyRecoveryTarget(target); err != nil {
		return failRecoveryDrill(receipt, "target_isolation", "target_not_empty", err)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "target_isolation", Status: "passed"})
	if err := target.Restore(request.Backup.Store); err != nil {
		return failRecoveryDrill(receipt, "backup_restore", "restore_failed", err)
	}
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "backup_restore", Status: "passed"})
	if request.FailAt == RecoveryFailureAfterBackup {
		return injectedRecoveryFailure(receipt, request.FailAt)
	}

	receipt.QueueFromSequence = request.Backup.QueueSequence + 1
	receipt.QueueThroughSequence = request.Backup.QueueSequence
	if err := replayRecoveryQueue(target, request.Queue, &receipt); err != nil {
		return failRecoveryDrill(receipt, "queue_replay", "queue_replay_failed", err)
	}
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}
	receipt.QueueReplayComplete = true
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "queue_replay", Status: "passed"})
	if request.FailAt == RecoveryFailureAfterQueue {
		return injectedRecoveryFailure(receipt, request.FailAt)
	}

	if err := target.auth.restoreACLSnapshot(request.CurrentACL); err != nil {
		return failRecoveryDrill(receipt, "acl_restore", "acl_restore_failed", err)
	}
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}
	if err := verifyRestoredACL(target.auth, request.CurrentACL, request.ACLProbes, &receipt); err != nil {
		return failRecoveryDrill(receipt, "acl_restore", "acl_verification_failed", err)
	}
	receipt.ACLRestored = true
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "acl_restore", Status: "passed"})
	if request.FailAt == RecoveryFailureAfterACL {
		return injectedRecoveryFailure(receipt, request.FailAt)
	}

	target.RebuildProjections()
	receipt.ProjectionRebuilt = true
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "projection_rebuild", Status: "passed"})
	if request.FailAt == RecoveryFailureAfterRebuild {
		return injectedRecoveryFailure(receipt, request.FailAt)
	}
	if err := populateRecoveryQuery(target, request.RepresentativeQuery, &receipt); err != nil {
		return failRecoveryDrill(receipt, "representative_query", "query_population_failed", err)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "representative_query", Status: "passed"})
	if err := ctx.Err(); err != nil {
		return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", err)
	}

	hermeticChecks, err := verifyRecoveryTombstones(target, request.Backup.Store, request.ExpectedTombstones)
	receipt.HermeticStoreChecks = hermeticChecks
	if err != nil {
		return failRecoveryDrill(receipt, "tombstone_validation", "tombstone_incomplete", err)
	}
	fixture := recoverySubstrateFixture(request, target)
	receipt.Substrates, err = restoreAndVerifyRecoverySubstrates(ctx, request.Substrates, fixture)
	if err != nil {
		if ctx.Err() != nil {
			return failRecoveryDrill(receipt, "run_bound", "deadline_exceeded", ctx.Err())
		}
		return failRecoveryDrill(receipt, "substrate_validation", "substrate_verification_failed", err)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "substrate_validation", Status: "passed"})
	receipt.TombstonesVerified = len(request.ExpectedTombstones)
	receipt.TombstonesComplete = true
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "tombstone_validation", Status: "passed"})
	if request.FailAt == RecoveryFailureAfterVerify {
		return injectedRecoveryFailure(receipt, request.FailAt)
	}

	verifiedAt := target.auth.dir.clock().UTC()
	receipt.VerifiedAt = verifiedAt.Format(time.RFC3339Nano)
	if verifiedAt.Before(request.StartedAt) {
		return failRecoveryDrill(receipt, "rto", "invalid_clock", ErrRejected)
	}
	receipt.RTOMillis = verifiedAt.Sub(request.StartedAt).Milliseconds()
	receipt.RTOWithinObjective = verifiedAt.Sub(request.StartedAt) <= objectives.MaxRTO
	if !receipt.RTOWithinObjective {
		return failRecoveryDrill(receipt, "rto", "rto_exceeded", ErrRejected)
	}
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: "rto", Status: "passed"})
	receipt.Status = "passed"
	receipt.Verified = true
	return receipt, nil
}

func normalizeRecoveryObjectives(objectives RecoveryObjectives) (RecoveryObjectives, error) {
	if objectives.MaxRPO == 0 {
		objectives.MaxRPO = defaultRecoveryRPO
	}
	if objectives.MaxRTO == 0 {
		objectives.MaxRTO = defaultRecoveryRTO
	}
	if objectives.MaxQueueEvents == 0 {
		objectives.MaxQueueEvents = defaultMaxRecoveryEvents
	}
	if objectives.MaxRPO < 0 || objectives.MaxRTO < 0 || objectives.MaxQueueEvents < 0 {
		return RecoveryObjectives{}, ErrRejected
	}
	return objectives, nil
}

func validateRecoveryRequest(request RecoveryDrillRequest, target *Store, objectives RecoveryObjectives) error {
	if !validID(request.DrillID) || !validID(request.TenantID) || !validID(request.GenerationID) ||
		!validDigest(request.ConfigDigest) || request.IncidentAt.IsZero() || request.StartedAt.IsZero() ||
		request.StartedAt.Before(request.IncidentAt) ||
		target == nil || target.auth == nil || target.auth.dir == nil || target.auth.dir.TenantID() != request.TenantID ||
		len(request.Queue) > objectives.MaxQueueEvents {
		return ErrRejected
	}
	if err := validateRecoverySubstrateMatrix(request.Substrates); err != nil {
		return err
	}
	if !validID(request.RepresentativeQuery.Principal.UserID) ||
		request.RepresentativeQuery.Principal.TenantID != request.TenantID ||
		len(tokenize(request.RepresentativeQuery.Query)) == 0 {
		return ErrRejected
	}
	if request.Backup.TenantID != request.TenantID || request.Backup.GenerationID != request.GenerationID ||
		request.Backup.ConfigDigest != request.ConfigDigest || request.CurrentACL.TenantID != request.TenantID ||
		request.CurrentACL.GenerationID != request.GenerationID || request.CurrentACL.ConfigDigest != request.ConfigDigest {
		return ErrRejected
	}
	switch request.FailAt {
	case RecoveryFailureNone, RecoveryFailureAfterBackup, RecoveryFailureAfterQueue,
		RecoveryFailureAfterACL, RecoveryFailureAfterRebuild, RecoveryFailureAfterVerify:
	default:
		return ErrRejected
	}
	wantSequence := request.Backup.QueueSequence + 1
	lastAt := request.Backup.CreatedAt
	for i := range request.Queue {
		entry := request.Queue[i]
		if entry.Sequence != wantSequence || entry.GenerationID != request.GenerationID || entry.At.IsZero() || entry.At.Before(lastAt) {
			return ErrRejected
		}
		switch entry.Kind {
		case RecoveryQueuePut:
			if entry.Item == nil || entry.Tombstone != nil || !validItem(*entry.Item) {
				return ErrRejected
			}
		case RecoveryQueueErase:
			if entry.Item != nil || entry.Tombstone == nil || !validTombstone(*entry.Tombstone) || !entry.Tombstone.At.Equal(entry.At) {
				return ErrRejected
			}
		default:
			return ErrRejected
		}
		lastAt = entry.At
		wantSequence++
	}
	if request.IncidentAt.Before(lastAt) {
		return ErrRejected
	}
	seenTombstones := make(map[string]struct{}, len(request.ExpectedTombstones))
	for _, stone := range request.ExpectedTombstones {
		if !validTombstone(stone) || stone.At.After(request.IncidentAt) {
			return ErrRejected
		}
		if _, duplicate := seenTombstones[stone.ItemID]; duplicate {
			return ErrRejected
		}
		seenTombstones[stone.ItemID] = struct{}{}
	}
	for _, probe := range request.ACLProbes {
		if !validID(probe.Principal.UserID) || !validID(probe.Principal.TenantID) || !probe.Scope.valid() {
			return ErrRejected
		}
	}
	return nil
}

func verifyRecoveryBackup(backup RecoveryBackup) error {
	if !validID(backup.TenantID) || !validID(backup.GenerationID) || !validDigest(backup.ConfigDigest) ||
		backup.CreatedAt.IsZero() || backup.Store.TenantID != backup.TenantID || backup.Digest == "" {
		return ErrRejected
	}
	want, err := recoveryBackupDigest(backup)
	if err != nil || want != backup.Digest {
		return ErrRejected
	}
	if backup.Store.Digest == "" {
		return ErrRejected
	}
	storeDigest, err := canonicalBackupDigest(backup.Store.TenantID, backup.Store.Items, backup.Store.Tombstones)
	if err != nil || storeDigest != backup.Store.Digest {
		return ErrRejected
	}
	itemIDs := make(map[string]struct{}, len(backup.Store.Items))
	for _, item := range backup.Store.Items {
		if !validItem(item) {
			return ErrRejected
		}
		if _, duplicate := itemIDs[item.ID]; duplicate {
			return ErrRejected
		}
		itemIDs[item.ID] = struct{}{}
	}
	stoneIDs := make(map[string]struct{}, len(backup.Store.Tombstones))
	for _, stone := range backup.Store.Tombstones {
		if !validTombstone(stone) || stone.At.After(backup.CreatedAt) {
			return ErrRejected
		}
		if _, duplicate := stoneIDs[stone.ItemID]; duplicate {
			return ErrRejected
		}
		if _, live := itemIDs[stone.ItemID]; live {
			return ErrRejected
		}
		stoneIDs[stone.ItemID] = struct{}{}
	}
	return nil
}

func recoveryBackupDigest(backup RecoveryBackup) (string, error) {
	copyBackup := backup
	copyBackup.Digest = ""
	copyBackup.Store.Items = append([]Item(nil), backup.Store.Items...)
	copyBackup.Store.Tombstones = append([]Tombstone(nil), backup.Store.Tombstones...)
	sort.Slice(copyBackup.Store.Items, func(i, j int) bool { return copyBackup.Store.Items[i].ID < copyBackup.Store.Items[j].ID })
	sort.Slice(copyBackup.Store.Tombstones, func(i, j int) bool {
		return copyBackup.Store.Tombstones[i].ItemID < copyBackup.Store.Tombstones[j].ItemID
	})
	raw, err := json.Marshal(copyBackup)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validItem(item Item) bool {
	return validID(item.ID) && item.Scope.valid() && validID(item.Owner) && item.Text != ""
}

func validTombstone(t Tombstone) bool {
	return validID(t.ItemID) && t.Reason != "" && !t.At.IsZero()
}

func canonicalTombstoneDigest(stones []Tombstone) (string, error) {
	canonical := append([]Tombstone(nil), stones...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].ItemID < canonical[j].ItemID })
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func recoveryQueueDigest(queue []RecoveryQueueEntry) (string, error) {
	canonical := append([]RecoveryQueueEntry{}, queue...)
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeACLSnapshot(snapshot *ACLSnapshot) {
	for i := range snapshot.Groups {
		sort.Strings(snapshot.Groups[i].Members)
	}
	sort.Slice(snapshot.Users, func(i, j int) bool { return snapshot.Users[i].ID < snapshot.Users[j].ID })
	sort.Slice(snapshot.Groups, func(i, j int) bool { return snapshot.Groups[i].ID < snapshot.Groups[j].ID })
	sort.Slice(snapshot.Grants, func(i, j int) bool {
		if snapshot.Grants[i].Subject != snapshot.Grants[j].Subject {
			return snapshot.Grants[i].Subject < snapshot.Grants[j].Subject
		}
		return snapshot.Grants[i].Scope.Key() < snapshot.Grants[j].Scope.Key()
	})
	sort.Slice(snapshot.Denies, func(i, j int) bool {
		if snapshot.Denies[i].UserID != snapshot.Denies[j].UserID {
			return snapshot.Denies[i].UserID < snapshot.Denies[j].UserID
		}
		return snapshot.Denies[i].Scope.Key() < snapshot.Denies[j].Scope.Key()
	})
	sort.Slice(snapshot.Receipts, func(i, j int) bool { return snapshot.Receipts[i].Seq < snapshot.Receipts[j].Seq })
}

func aclSnapshotDigest(snapshot ACLSnapshot) (string, error) {
	copySnapshot := snapshot
	copySnapshot.Digest = ""
	copySnapshot.Users = append([]ACLUser(nil), snapshot.Users...)
	copySnapshot.Groups = make([]ACLGroup, len(snapshot.Groups))
	for i, group := range snapshot.Groups {
		copySnapshot.Groups[i] = group
		copySnapshot.Groups[i].Members = append([]string(nil), group.Members...)
	}
	copySnapshot.Grants = append([]ACLGrant(nil), snapshot.Grants...)
	copySnapshot.Denies = append([]ACLDeny(nil), snapshot.Denies...)
	copySnapshot.Receipts = append([]Receipt(nil), snapshot.Receipts...)
	normalizeACLSnapshot(&copySnapshot)
	raw, err := json.Marshal(copySnapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateACLSnapshot(snapshot ACLSnapshot) error {
	if !validID(snapshot.TenantID) || !validID(snapshot.GenerationID) || !validDigest(snapshot.ConfigDigest) ||
		snapshot.CapturedAt.IsZero() || snapshot.Sequence != snapshot.Epoch || snapshot.Digest == "" {
		return ErrRejected
	}
	want, err := aclSnapshotDigest(snapshot)
	if err != nil || want != snapshot.Digest {
		return ErrRejected
	}
	users := make(map[string]bool, len(snapshot.Users))
	userIncarnations := make(map[string]uint64, len(snapshot.Users))
	for _, user := range snapshot.Users {
		if !validID(user.ID) || user.Incarnation == 0 || user.Active != (user.Incarnation%2 == 1) {
			return ErrRejected
		}
		if _, duplicate := users[user.ID]; duplicate {
			return ErrRejected
		}
		users[user.ID] = user.Active
		userIncarnations[user.ID] = user.Incarnation
	}
	groups := make(map[string]map[string]bool, len(snapshot.Groups))
	groupIncarnations := make(map[string]uint64, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		if !validID(group.ID) || group.Incarnation == 0 || group.Incarnation%2 == 0 {
			return ErrRejected
		}
		if _, duplicate := groups[group.ID]; duplicate {
			return ErrRejected
		}
		members := make(map[string]bool, len(group.Members))
		for _, member := range group.Members {
			if _, exists := users[member]; !exists || !users[member] || members[member] {
				return ErrRejected
			}
			members[member] = true
		}
		groups[group.ID] = members
		groupIncarnations[group.ID] = group.Incarnation
	}
	grantKeys := make(map[string]struct{}, len(snapshot.Grants))
	for _, grant := range snapshot.Grants {
		kind, id, ok := splitSubject(grant.Subject)
		_, userExists := users[id]
		if !ok || !grant.Scope.valid() || grant.SubjectIncarnation == 0 || grant.SubjectIncarnation%2 == 0 ||
			(kind == "user" && (!userExists || grant.SubjectIncarnation > userIncarnations[id])) ||
			(kind == "group" && (groups[id] == nil || grant.SubjectIncarnation > groupIncarnations[id])) {
			return ErrRejected
		}
		if grant.DelegatedBy != "" {
			if _, exists := users[grant.DelegatedBy]; !exists || grant.DelegatorIncarnation == 0 ||
				grant.DelegatorIncarnation%2 == 0 || grant.DelegatorIncarnation > userIncarnations[grant.DelegatedBy] {
				return ErrRejected
			}
		} else if grant.DelegatorIncarnation != 0 {
			return ErrRejected
		}
		key := grant.Subject + "|" + grant.Scope.Key()
		if _, duplicate := grantKeys[key]; duplicate {
			return ErrRejected
		}
		grantKeys[key] = struct{}{}
	}
	denyKeys := make(map[string]struct{}, len(snapshot.Denies))
	for _, deny := range snapshot.Denies {
		if _, exists := users[deny.UserID]; !exists || !deny.Scope.valid() {
			return ErrRejected
		}
		key := deny.UserID + "|" + deny.Scope.Key()
		if _, duplicate := denyKeys[key]; duplicate {
			return ErrRejected
		}
		denyKeys[key] = struct{}{}
	}
	if uint64(len(snapshot.Receipts)) != snapshot.Sequence {
		return ErrRejected
	}
	for i, receipt := range snapshot.Receipts {
		wantSeq := uint64(i + 1)
		if receipt.Seq != wantSeq || receipt.Epoch != wantSeq || receipt.Kind == "" || receipt.Subject == "" || receipt.At.IsZero() {
			return ErrRejected
		}
	}
	return nil
}

func (a *Authority) restoreACLSnapshot(snapshot ACLSnapshot) error {
	if err := validateACLSnapshot(snapshot); err != nil || snapshot.TenantID != a.dir.TenantID() {
		return ErrRejected
	}
	a.mu.Lock()
	a.dir.mu.Lock()
	a.dir.users = make(map[string]bool, len(snapshot.Users))
	a.dir.versions = make(map[string]uint64, len(snapshot.Users))
	for _, user := range snapshot.Users {
		a.dir.users[user.ID] = user.Active
		a.dir.versions[user.ID] = user.Incarnation
	}
	a.dir.groups = make(map[string]map[string]bool, len(snapshot.Groups))
	a.dir.groupVer = make(map[string]uint64, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		members := make(map[string]bool, len(group.Members))
		for _, member := range group.Members {
			members[member] = true
		}
		a.dir.groups[group.ID] = members
		a.dir.groupVer[group.ID] = group.Incarnation
	}
	a.dir.epoch = snapshot.Epoch
	a.dir.seq = snapshot.Sequence
	a.dir.receipts = append([]Receipt(nil), snapshot.Receipts...)
	a.grants = make(map[string]map[string]grantRecord)
	for _, grant := range snapshot.Grants {
		if a.grants[grant.Subject] == nil {
			a.grants[grant.Subject] = make(map[string]grantRecord)
		}
		record := grantRecord{
			Grant: grant.Grant, subjectIncarnation: grant.SubjectIncarnation,
			delegatorIncarnation: grant.DelegatorIncarnation,
		}
		a.grants[grant.Subject][grant.Scope.Key()] = record
	}
	a.denies = make(map[string]bool, len(snapshot.Denies))
	for _, deny := range snapshot.Denies {
		a.denies[deny.UserID+"|"+deny.Scope.Key()] = true
	}
	a.dir.mu.Unlock()
	a.mu.Unlock()
	return nil
}

func splitDenyKey(key string) (string, Scope, bool) {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) != 2 || !validID(parts[0]) {
		return "", Scope{}, false
	}
	if parts[1] == string(ScopeCompany) {
		return parts[0], Scope{Kind: ScopeCompany}, true
	}
	for _, kind := range []ScopeKind{ScopeIndividual, ScopeTeam} {
		prefix := string(kind) + ":"
		if strings.HasPrefix(parts[1], prefix) {
			scope := Scope{Kind: kind, ID: strings.TrimPrefix(parts[1], prefix)}
			return parts[0], scope, scope.valid()
		}
	}
	return "", Scope{}, false
}

func requireEmptyRecoveryTarget(target *Store) error {
	target.mu.Lock()
	emptyStore := len(target.items) == 0 && len(target.index) == 0 && len(target.cache) == 0 &&
		len(target.sessions) == 0 && len(target.tombstones) == 0 && len(target.audit) == 0 && target.auditSeq == 0
	target.mu.Unlock()
	target.auth.mu.Lock()
	target.auth.dir.mu.Lock()
	emptyACL := len(target.auth.grants) == 0 && len(target.auth.denies) == 0 &&
		len(target.auth.dir.users) == 0 && len(target.auth.dir.versions) == 0 &&
		len(target.auth.dir.groups) == 0 && len(target.auth.dir.groupVer) == 0 &&
		len(target.auth.dir.receipts) == 0 && target.auth.dir.epoch == 0 && target.auth.dir.seq == 0
	target.auth.dir.mu.Unlock()
	target.auth.mu.Unlock()
	if !emptyStore || !emptyACL {
		return ErrRejected
	}
	return nil
}

func replayRecoveryQueue(target *Store, queue []RecoveryQueueEntry, receipt *RecoveryDrillReceipt) error {
	for _, entry := range queue {
		switch entry.Kind {
		case RecoveryQueuePut:
			if err := target.Put(*entry.Item); err != nil {
				return err
			}
		case RecoveryQueueErase:
			if _, err := target.Erase(entry.Tombstone.Reason, entry.Tombstone.ItemID); err != nil {
				return err
			}
			target.mu.Lock()
			target.tombstones[entry.Tombstone.ItemID] = *entry.Tombstone
			target.mu.Unlock()
		default:
			return ErrRejected
		}
		receipt.QueueEntriesReplayed++
		receipt.QueueThroughSequence = entry.Sequence
	}
	return nil
}

func verifyRestoredACL(auth *Authority, want ACLSnapshot, probes []RecoveryACLProbe, receipt *RecoveryDrillReceipt) error {
	got, err := auth.CreateACLSnapshot(want.GenerationID, want.ConfigDigest, want.CapturedAt)
	if err != nil || got.Digest != want.Digest {
		return ErrRejected
	}
	for _, probe := range probes {
		allowed := auth.Resolve(probe.Principal, probe.Scope) == nil
		if allowed != probe.Allowed {
			return ErrRejected
		}
		receipt.ACLProbesPassed++
	}
	return nil
}

func populateRecoveryQuery(target *Store, probe RecoveryQueryProbe, receipt *RecoveryDrillReceipt) error {
	result, err := target.Query(probe.Principal, probe.Query)
	if err != nil || len(result.Citations) == 0 || result.FromCache {
		return ErrRejected
	}
	principalKey, err := target.activePrincipalKey(probe.Principal)
	if err != nil {
		return ErrDenied
	}
	cacheKey := queryCacheKey{principal: principalKey, query: probe.Query}
	target.mu.Lock()
	cache := target.cache[cacheKey]
	sessions := target.sessions[principalKey]
	target.mu.Unlock()
	if len(cache.itemIDs) == 0 || len(sessions) == 0 || len(sessions[len(sessions)-1].itemIDs) == 0 {
		return ErrRejected
	}
	receipt.RepresentativeQueryRan = true
	receipt.RepresentativeQueryCitations = len(result.Citations)
	receipt.CacheEntriesPopulated = len(cache.itemIDs)
	receipt.SessionEntriesPopulated = len(sessions[len(sessions)-1].itemIDs)
	return nil
}

func verifyRecoveryTombstones(target *Store, oldBackup Backup, expected []Tombstone) ([]RecoverySubstrateReceipt, error) {
	wantStones := append([]Tombstone(nil), expected...)
	sort.Slice(wantStones, func(i, j int) bool { return wantStones[i].ItemID < wantStones[j].ItemID })
	want := make([]string, len(wantStones))
	for i, stone := range wantStones {
		want[i] = stone.ItemID
	}
	stones := target.Tombstones()
	if len(stones) != len(wantStones) {
		return nil, ErrRejected
	}
	for i := range wantStones {
		if stones[i].ItemID != wantStones[i].ItemID || stones[i].Reason != wantStones[i].Reason ||
			!stones[i].At.Equal(wantStones[i].At) {
			return nil, ErrRejected
		}
	}

	receipts := make([]RecoverySubstrateReceipt, 0, 7)
	target.mu.Lock()
	for _, id := range want {
		if _, exists := target.items[id]; exists {
			target.mu.Unlock()
			return receipts, ErrRejected
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "primary", ProviderBoundary: RecoveryBoundaryHermeticFake, TombstonesChecked: len(want), Passed: true})
	for _, id := range want {
		for _, ids := range target.index {
			if _, exists := ids[id]; exists {
				target.mu.Unlock()
				return receipts, ErrRejected
			}
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "search_index", ProviderBoundary: RecoveryBoundaryHermeticFake, TombstonesChecked: len(want), Passed: true})
	if len(target.cache) == 0 {
		target.mu.Unlock()
		return receipts, ErrRejected
	}
	for _, id := range want {
		for _, entry := range target.cache {
			if containsID(entry.itemIDs, id) {
				target.mu.Unlock()
				return receipts, ErrRejected
			}
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "query_cache", ProviderBoundary: RecoveryBoundaryHermeticFake, RepresentativeRecords: len(target.cache), TombstonesChecked: len(want), Passed: true})
	if len(target.sessions) == 0 {
		target.mu.Unlock()
		return receipts, ErrRejected
	}
	for _, id := range want {
		for _, entries := range target.sessions {
			for _, entry := range entries {
				if containsID(entry.itemIDs, id) {
					target.mu.Unlock()
					return receipts, ErrRejected
				}
			}
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "session_history", ProviderBoundary: RecoveryBoundaryHermeticFake, RepresentativeRecords: len(target.sessions), TombstonesChecked: len(want), Passed: true})
	target.mu.Unlock()

	postBackup, err := target.CreateBackup()
	if err != nil {
		return receipts, err
	}
	if len(postBackup.Tombstones) != len(wantStones) {
		return receipts, ErrRejected
	}
	for i := range wantStones {
		if postBackup.Tombstones[i].ItemID != wantStones[i].ItemID ||
			postBackup.Tombstones[i].Reason != wantStones[i].Reason ||
			!postBackup.Tombstones[i].At.Equal(wantStones[i].At) {
			return receipts, ErrRejected
		}
	}
	for _, item := range postBackup.Items {
		if containsID(want, item.ID) {
			return receipts, ErrRejected
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "backup_manifest", ProviderBoundary: RecoveryBoundaryHermeticFake, TombstonesChecked: len(want), Passed: true})

	probe := NewStore(target.auth)
	probe.mu.Lock()
	for _, stone := range stones {
		probe.tombstones[stone.ItemID] = stone
	}
	probe.mu.Unlock()
	if err := probe.Restore(oldBackup); err != nil {
		return receipts, err
	}
	if leaks := probe.VerifyErasure(want...); len(leaks.Leaks) != 0 {
		return receipts, ErrRejected
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "pre_erasure_restore", ProviderBoundary: RecoveryBoundaryHermeticFake, TombstonesChecked: len(want), Passed: true})

	oldItems := make(map[string]Item, len(oldBackup.Items))
	for _, item := range oldBackup.Items {
		oldItems[item.ID] = item
	}
	for _, id := range want {
		item, existed := oldItems[id]
		if !existed {
			continue
		}
		if err := probe.Put(item); !errors.Is(err, ErrRejected) {
			return receipts, ErrRejected
		}
	}
	receipts = append(receipts, RecoverySubstrateReceipt{Name: "reingest_guard", ProviderBoundary: RecoveryBoundaryHermeticFake, TombstonesChecked: len(want), Passed: true})
	return receipts, nil
}

func injectedRecoveryFailure(receipt RecoveryDrillReceipt, point RecoveryFailurePoint) (RecoveryDrillReceipt, error) {
	receipt.FailurePoint = point
	receipt.FailureCode = "injected_failure"
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: string(point), Status: "failed", Code: "injected_failure"})
	return receipt, fmt.Errorf("%w: %w at %s", ErrRecoveryDrillFailed, ErrInjectedRecoveryFailure, point)
}

func failRecoveryDrill(receipt RecoveryDrillReceipt, check, code string, cause error) (RecoveryDrillReceipt, error) {
	receipt.FailureCode = code
	receipt.Checks = append(receipt.Checks, RecoveryCheck{Name: check, Status: "failed", Code: code})
	return receipt, fmt.Errorf("%w: %s: %w", ErrRecoveryDrillFailed, check, cause)
}
