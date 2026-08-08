package meeting

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// Kernel serializes meeting lifecycle operations over migration 006. It is safe
// for concurrent use: one mutex serializes every operation, matching the
// durability posture of the Stage 04 conversation store.
type Kernel struct {
	db       *sql.DB
	payloads PayloadStore
	clock    Clock
	mu       sync.Mutex
}

// Open attaches the meeting kernel to an already-migrated authority database.
// Migration 006 must already be applied; Open takes neither migrations nor the
// process owner lock.
func Open(ctx context.Context, config Config) (*Kernel, error) {
	clean := filepath.Clean(config.DatabasePath)
	if !filepath.IsAbs(clean) || config.Payloads == nil || config.Clock == nil {
		return nil, ErrInvalidInput
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("meeting: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	kernel := &Kernel{db: db, payloads: config.Payloads, clock: config.Clock}
	if err := kernel.configure(ctx); err != nil {
		return nil, errors.Join(err, kernel.Close())
	}
	if err := kernel.requireSchema(ctx); err != nil {
		return nil, errors.Join(err, kernel.Close())
	}
	return kernel, nil
}

// Close releases the database handle. It is idempotent.
func (k *Kernel) Close() error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil
	}
	err := k.db.Close()
	k.db = nil
	return err
}

func (k *Kernel) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000",
	} {
		if _, err := k.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("meeting: configure database: %w", err)
		}
	}
	return nil
}

func (k *Kernel) requireSchema(ctx context.Context) error {
	var applied int
	if err := k.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version=6`).Scan(&applied); err != nil {
		return errors.Join(ErrSchemaUnsupported, fmt.Errorf("meeting: inspect migrations: %w", err))
	}
	if applied != 1 {
		return ErrSchemaUnsupported
	}
	return nil
}

// ImportTranscript admits one timestamped fixture transcript, or returns the
// original outcome for an exact authenticated idempotent replay.
func (k *Kernel) ImportTranscript(ctx context.Context, command ImportCommand) (*contractsv1.ImportTranscriptSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := command.Request
	if request.Retention == nil || !request.ParticipantNotifyAcknowledged ||
		len(request.Segments) == 0 || request.IdempotencyKey == "" ||
		request.Title == "" || request.SourceScope == "" ||
		request.StartedAt == nil || request.EndedAt == nil {
		return nil, ErrInvalidInput
	}
	digest, err := requestDigest(request)
	if err != nil {
		return nil, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "import", request.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest {
			return nil, ErrNotFoundOrDenied
		}
		return k.importSuccessFromRow(ctx, command.Identity, existing.meetingID)
	}
	payload, timelineStart, timelineEnd, err := buildPayload(request)
	if err != nil {
		return nil, ErrInvalidInput
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidInput
	}
	artifactID, err := k.payloads.Put(ctx, command.Identity.Tenant, encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: stage transcript", ErrPayloadUnavailable)
	}
	meetingID := identity(
		"ouroboros.stage07.meeting.v1",
		command.Identity.Tenant, command.Identity.Principal,
		request.IdempotencyKey, digest,
	)
	nowMs := k.clock.NowUnixMilli()
	state := "READY"
	if request.Partial {
		state = "PARTIAL"
	}
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("meeting: begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_sessions (
		tenant_id,principal_id,meeting_session_id,session_id,title_digest,source_scope,
		started_at_ms,ended_at_ms,timeline_start_millis,timeline_end_millis,segment_count,
		raw_media_retention,screenshot_retention,derivative_retention,
		notify_reminder_recorded,partial,payload_artifact_id,payload_digest,admitted_at_ms
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, meetingID, command.Identity.Session,
		digestText(request.Title), request.SourceScope,
		request.StartedAt.AsTime().UTC().UnixMilli(), request.EndedAt.AsTime().UTC().UnixMilli(),
		timelineStart, timelineEnd, len(request.Segments),
		request.Retention.RawMediaRetention, request.Retention.ScreenshotRetention,
		request.Retention.DerivativeRetention,
		boolToInt(request.Partial), artifactID, digestBytes(encoded), nowMs,
	); err != nil {
		return nil, fmt.Errorf("meeting: insert session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_session_states
		(tenant_id,principal_id,meeting_session_id,sequence,state,occurred_at_ms)
		VALUES (?,?,?,1,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, meetingID, state, nowMs,
	); err != nil {
		return nil, fmt.Errorf("meeting: insert state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,meeting_session_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "import", request.IdempotencyKey,
		digest, meetingID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("meeting: insert idempotency: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("meeting: commit import: %w", err)
	}
	return &contractsv1.ImportTranscriptSuccess{
		MeetingSessionId:       &contractsv1.Identifier{Namespace: "meeting-session", Value: meetingID},
		State:                  stateFromText(state),
		SegmentCount:           uint32(len(request.Segments)),
		Retention:              cloneRetention(request.Retention),
		NotifyReminderRecorded: true,
		SourceScope:            request.SourceScope,
	}, nil
}

// GetMeetingStatus reads readiness for one non-revoked, non-purged meeting.
func (k *Kernel) GetMeetingStatus(ctx context.Context, command StatusCommand) (*contractsv1.GetMeetingStatusSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.MeetingID) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	row, state, err := k.loadQueryable(ctx, command.Identity, command.MeetingID)
	if err != nil {
		return nil, err
	}
	payload, err := k.loadPayload(ctx, command.Identity.Tenant, row.payloadArtifact)
	if err != nil {
		return nil, err
	}
	return &contractsv1.GetMeetingStatusSuccess{
		MeetingSessionId:       &contractsv1.Identifier{Namespace: "meeting-session", Value: command.MeetingID},
		State:                  stateFromText(state),
		SegmentCount:           uint32(row.segmentCount),
		TimelineStartMillis:    uint64(row.timelineStart),
		TimelineEndMillis:      uint64(row.timelineEnd),
		Retention:              row.retention(),
		NotifyReminderRecorded: true,
		SourceScope:            row.sourceScope,
		Title:                  payload.Title,
	}, nil
}

// QueryMeeting answers one question with exact time-range anchors.
func (k *Kernel) QueryMeeting(ctx context.Context, command QueryCommand) (*contractsv1.QueryMeetingSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := command.Request
	if request.MeetingSessionId == nil || !validBoundedID(request.MeetingSessionId.Value) ||
		request.Query == "" || request.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	row, state, err := k.loadQueryable(ctx, command.Identity, request.MeetingSessionId.Value)
	if err != nil {
		return nil, err
	}
	payload, err := k.loadPayload(ctx, command.Identity.Tenant, row.payloadArtifact)
	if err != nil {
		return nil, err
	}
	answer := synthesizeAnswer(request, payload, state)
	return &contractsv1.QueryMeetingSuccess{
		Answer:           answer,
		MeetingSessionId: &contractsv1.Identifier{Namespace: "meeting-session", Value: request.MeetingSessionId.Value},
		State:            stateFromText(state),
	}, nil
}

// RevokeMeeting denies hydration and query immediately, or returns the original
// outcome for an exact authenticated idempotent replay.
func (k *Kernel) RevokeMeeting(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeMeetingSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) ||
		!validBoundedID(command.MeetingID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := digestText("revoke\x00" + command.MeetingID)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "revoke", command.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest || existing.meetingID != command.MeetingID {
			return nil, ErrNotFoundOrDenied
		}
		return &contractsv1.RevokeMeetingSuccess{
			MeetingSessionId: &contractsv1.Identifier{Namespace: "meeting-session", Value: command.MeetingID},
			State:            contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_REVOKED,
		}, nil
	}
	state, err := k.currentState(ctx, command.Identity, command.MeetingID)
	if err != nil {
		return nil, err
	}
	if state == "PURGED" {
		return nil, ErrNotFoundOrDenied
	}
	if state != "REVOKED" {
		if err := k.appendState(ctx, command.Identity, command.MeetingID, "REVOKED"); err != nil {
			return nil, err
		}
	}
	nowMs := k.clock.NowUnixMilli()
	if _, err := k.db.ExecContext(ctx, `INSERT INTO meeting_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,meeting_session_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "revoke", command.IdempotencyKey,
		digest, command.MeetingID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("meeting: insert revoke idempotency: %w", err)
	}
	return &contractsv1.RevokeMeetingSuccess{
		MeetingSessionId: &contractsv1.Identifier{Namespace: "meeting-session", Value: command.MeetingID},
		State:            contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_REVOKED,
	}, nil
}

// PurgeMeeting tombstones lineage and purges encrypted transcript artifacts.
func (k *Kernel) PurgeMeeting(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeMeetingSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) ||
		!validBoundedID(command.MeetingID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := digestText("purge\x00" + command.MeetingID)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "purge", command.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest || existing.meetingID != command.MeetingID {
			return nil, ErrNotFoundOrDenied
		}
		return &contractsv1.PurgeMeetingSuccess{
			MeetingSessionId:    &contractsv1.Identifier{Namespace: "meeting-session", Value: command.MeetingID},
			State:               contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PURGED,
			PurgedArtifactCount: 1,
		}, nil
	}
	row, state, err := k.loadAny(ctx, command.Identity, command.MeetingID)
	if err != nil {
		return nil, err
	}
	purgedCount := uint32(0)
	if state != "PURGED" {
		if state != "REVOKED" {
			if err := k.appendState(ctx, command.Identity, command.MeetingID, "REVOKED"); err != nil {
				return nil, err
			}
		}
		if err := k.payloads.Purge(ctx, command.Identity.Tenant, row.payloadArtifact); err != nil {
			return nil, fmt.Errorf("%w: purge transcript", ErrPayloadUnavailable)
		}
		purgedCount = 1
		if err := k.appendState(ctx, command.Identity, command.MeetingID, "PURGED"); err != nil {
			return nil, err
		}
	} else {
		purgedCount = 1
	}
	nowMs := k.clock.NowUnixMilli()
	if _, err := k.db.ExecContext(ctx, `INSERT INTO meeting_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,meeting_session_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "purge", command.IdempotencyKey,
		digest, command.MeetingID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("meeting: insert purge idempotency: %w", err)
	}
	return &contractsv1.PurgeMeetingSuccess{
		MeetingSessionId:    &contractsv1.Identifier{Namespace: "meeting-session", Value: command.MeetingID},
		State:               contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PURGED,
		PurgedArtifactCount: purgedCount,
	}, nil
}

type meetingRow struct {
	meetingID        string
	sourceScope      string
	segmentCount     int
	timelineStart    int64
	timelineEnd      int64
	rawRetention     string
	screenshotRetain string
	derivativeRetain string
	payloadArtifact  string
	payloadDigest    string
	partial          bool
}

func (row meetingRow) retention() *contractsv1.ImportRetentionPolicy {
	return &contractsv1.ImportRetentionPolicy{
		RawMediaRetention:   row.rawRetention,
		ScreenshotRetention: row.screenshotRetain,
		DerivativeRetention: row.derivativeRetain,
	}
}

type idempotencyRow struct {
	requestDigest string
	meetingID     string
}

func (k *Kernel) lookupIdempotency(
	ctx context.Context, identity Identity, operation, key string,
) (idempotencyRow, bool, error) {
	var row idempotencyRow
	err := k.db.QueryRowContext(ctx, `SELECT request_digest,meeting_session_id FROM meeting_idempotency
		WHERE tenant_id=? AND principal_id=? AND operation=? AND idempotency_key=?`,
		identity.Tenant, identity.Principal, operation, key,
	).Scan(&row.requestDigest, &row.meetingID)
	if errors.Is(err, sql.ErrNoRows) {
		return idempotencyRow{}, false, nil
	}
	if err != nil {
		return idempotencyRow{}, false, fmt.Errorf("meeting: lookup idempotency: %w", err)
	}
	return row, true, nil
}

func (k *Kernel) loadQueryable(ctx context.Context, identity Identity, meetingID string) (meetingRow, string, error) {
	row, state, err := k.loadAny(ctx, identity, meetingID)
	if err != nil {
		return meetingRow{}, "", err
	}
	if state == "REVOKED" || state == "PURGED" {
		return meetingRow{}, "", ErrNotFoundOrDenied
	}
	return row, state, nil
}

func (k *Kernel) loadAny(ctx context.Context, identity Identity, meetingID string) (meetingRow, string, error) {
	var row meetingRow
	var partial int
	err := k.db.QueryRowContext(ctx, `SELECT meeting_session_id,source_scope,segment_count,
		timeline_start_millis,timeline_end_millis,raw_media_retention,screenshot_retention,
		derivative_retention,payload_artifact_id,payload_digest,partial
		FROM meeting_sessions
		WHERE tenant_id=? AND principal_id=? AND meeting_session_id=?`,
		identity.Tenant, identity.Principal, meetingID,
	).Scan(
		&row.meetingID, &row.sourceScope, &row.segmentCount, &row.timelineStart, &row.timelineEnd,
		&row.rawRetention, &row.screenshotRetain, &row.derivativeRetain,
		&row.payloadArtifact, &row.payloadDigest, &partial,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return meetingRow{}, "", ErrNotFoundOrDenied
	}
	if err != nil {
		return meetingRow{}, "", fmt.Errorf("meeting: load session: %w", err)
	}
	row.partial = partial == 1
	state, err := k.currentState(ctx, identity, meetingID)
	if err != nil {
		return meetingRow{}, "", err
	}
	return row, state, nil
}

func (k *Kernel) currentState(ctx context.Context, identity Identity, meetingID string) (string, error) {
	var state string
	err := k.db.QueryRowContext(ctx, `SELECT state FROM meeting_session_states
		WHERE tenant_id=? AND principal_id=? AND meeting_session_id=?
		ORDER BY sequence DESC LIMIT 1`,
		identity.Tenant, identity.Principal, meetingID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFoundOrDenied
	}
	if err != nil {
		return "", fmt.Errorf("meeting: read state: %w", err)
	}
	return state, nil
}

func (k *Kernel) appendState(ctx context.Context, identity Identity, meetingID, state string) error {
	nowMs := k.clock.NowUnixMilli()
	var sequence int
	if err := k.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM meeting_session_states
		WHERE tenant_id=? AND principal_id=? AND meeting_session_id=?`,
		identity.Tenant, identity.Principal, meetingID,
	).Scan(&sequence); err != nil {
		return fmt.Errorf("meeting: read state sequence: %w", err)
	}
	if _, err := k.db.ExecContext(ctx, `INSERT INTO meeting_session_states
		(tenant_id,principal_id,meeting_session_id,sequence,state,occurred_at_ms)
		VALUES (?,?,?,?,?,?)`,
		identity.Tenant, identity.Principal, meetingID, sequence, state, nowMs,
	); err != nil {
		return fmt.Errorf("meeting: append state: %w", err)
	}
	return nil
}

func (k *Kernel) importSuccessFromRow(
	ctx context.Context, identity Identity, meetingID string,
) (*contractsv1.ImportTranscriptSuccess, error) {
	// Exact import replay returns the original admission shape even after a
	// later revoke or purge: the idempotency record, not the live state, is
	// the authority for the completed import outcome.
	row, _, err := k.loadAny(ctx, identity, meetingID)
	if err != nil {
		return nil, err
	}
	var admitted string
	if err := k.db.QueryRowContext(ctx, `SELECT state FROM meeting_session_states
		WHERE tenant_id=? AND principal_id=? AND meeting_session_id=? AND sequence=1`,
		identity.Tenant, identity.Principal, meetingID,
	).Scan(&admitted); err != nil {
		return nil, fmt.Errorf("meeting: read admitted state: %w", err)
	}
	return &contractsv1.ImportTranscriptSuccess{
		MeetingSessionId:       &contractsv1.Identifier{Namespace: "meeting-session", Value: meetingID},
		State:                  stateFromText(admitted),
		SegmentCount:           uint32(row.segmentCount),
		Retention:              row.retention(),
		NotifyReminderRecorded: true,
		SourceScope:            row.sourceScope,
	}, nil
}

func (k *Kernel) loadPayload(ctx context.Context, tenant, artifactID string) (transcriptPayload, error) {
	encoded, err := k.payloads.Get(ctx, tenant, artifactID)
	if err != nil {
		return transcriptPayload{}, fmt.Errorf("%w: hydrate transcript", ErrPayloadUnavailable)
	}
	var payload transcriptPayload
	if err := json.Unmarshal(encoded, &payload); err != nil || payload.Version != payloadVersion {
		return transcriptPayload{}, ErrPayloadUnavailable
	}
	return payload, nil
}

func buildPayload(request *contractsv1.ImportTranscriptRequest) (transcriptPayload, int64, int64, error) {
	if len(request.Segments) == 0 {
		return transcriptPayload{}, 0, 0, ErrInvalidInput
	}
	payload := transcriptPayload{
		Version:  payloadVersion,
		Title:    request.Title,
		Segments: make([]transcriptPayloadSegment, 0, len(request.Segments)),
	}
	var start, end int64
	for index, segment := range request.Segments {
		if segment == nil || segment.EndMillis <= segment.StartMillis || segment.Text == "" {
			return transcriptPayload{}, 0, 0, ErrInvalidInput
		}
		payload.Segments = append(payload.Segments, transcriptPayloadSegment{
			StartMillis: segment.StartMillis, EndMillis: segment.EndMillis,
			Text: segment.Text, SpeakerLabel: segment.SpeakerLabel,
		})
		if index == 0 || int64(segment.StartMillis) < start {
			start = int64(segment.StartMillis)
		}
		if int64(segment.EndMillis) > end {
			end = int64(segment.EndMillis)
		}
	}
	if end <= start {
		return transcriptPayload{}, 0, 0, ErrInvalidInput
	}
	return payload, start, end, nil
}

func synthesizeAnswer(
	request *contractsv1.QueryMeetingRequest, payload transcriptPayload, state string,
) *contractsv1.MeetingAnswer {
	terms := tokenize(request.Query)
	var matches []transcriptPayloadSegment
	for _, segment := range payload.Segments {
		if request.TimeRange != nil {
			if segment.EndMillis <= request.TimeRange.StartMillis ||
				segment.StartMillis >= request.TimeRange.EndMillis {
				continue
			}
		}
		if matchesTerms(segment.Text, terms) {
			matches = append(matches, segment)
		}
	}
	if len(matches) == 0 {
		return &contractsv1.MeetingAnswer{
			Status:             contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_ABSTAINED,
			DegradedReasons:    []string{"absent_support"},
			FactualConsistency: meetingFactualConsistencyAbstained(),
		}
	}
	match := matches[0]
	supportDigest := digestText(match.Text)
	evidenceID := identity("ouroboros.stage07.evidence.v1", request.MeetingSessionId.Value,
		fmt.Sprintf("%d", match.StartMillis), fmt.Sprintf("%d", match.EndMillis))
	revisionID := identity("ouroboros.stage07.revision.v1", request.MeetingSessionId.Value)
	claimID := identity("ouroboros.stage07.claim.v1", request.MeetingSessionId.Value, supportDigest)
	citation := &contractsv1.MeetingTemporalCitation{
		Evidence: &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: "evidence", Value: evidenceID},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: revisionID},
		},
		StartMillis:          match.StartMillis,
		EndMillis:            match.EndMillis,
		SupportingTextDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: supportDigest},
	}
	claim := &contractsv1.MeetingAnswerClaim{
		ClaimId:            &contractsv1.Identifier{Namespace: "claim", Value: claimID},
		Statement:          truncate(match.Text, 4096),
		Citations:          []*contractsv1.MeetingTemporalCitation{citation},
		AuthorityClass:     contractsv1.AuthorityClass_AUTHORITY_CLASS_MODEL_PROPOSAL,
		ConfidencePerMille: 900,
	}
	prose := fmt.Sprintf("At %d–%dms: %s", match.StartMillis, match.EndMillis, truncate(match.Text, 512))
	if state == "PARTIAL" || request.TimeRange != nil && len(matches) < len(payload.Segments) {
		return &contractsv1.MeetingAnswer{
			Status:             contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_PARTIAL,
			Prose:              prose,
			Claims:             []*contractsv1.MeetingAnswerClaim{claim},
			DegradedReasons:    []string{"partial_coverage"},
			FactualConsistency: meetingFactualConsistencyUnknown(1),
		}
	}
	return &contractsv1.MeetingAnswer{
		Status:             contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_ANSWERED,
		Prose:              prose,
		Claims:             []*contractsv1.MeetingAnswerClaim{claim},
		FactualConsistency: meetingFactualConsistencyUnknown(1),
	}
}

func meetingFactualConsistencyAbstained() *contractsv1.FactualConsistencyScore {
	return &contractsv1.FactualConsistencyScore{
		Status: contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_ABSTAINED,
		Reason: contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_ANSWER_ABSTAINED,
	}
}

func meetingFactualConsistencyUnknown(totalClaims uint32) *contractsv1.FactualConsistencyScore {
	return &contractsv1.FactualConsistencyScore{
		Status:          contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN,
		Reason:          contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_SCORER_UNAVAILABLE,
		TotalClaimCount: totalClaims,
	}
}

func tokenize(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) >= 2 {
			out = append(out, field)
		}
	}
	return out
}

func matchesTerms(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func requestDigest(request *contractsv1.ImportTranscriptRequest) (string, error) {
	clone := proto.Clone(request).(*contractsv1.ImportTranscriptRequest)
	// Idempotency binds the request body excluding the key itself.
	clone.IdempotencyKey = ""
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func cloneRetention(policy *contractsv1.ImportRetentionPolicy) *contractsv1.ImportRetentionPolicy {
	if policy == nil {
		return nil
	}
	return &contractsv1.ImportRetentionPolicy{
		RawMediaRetention:   policy.RawMediaRetention,
		ScreenshotRetention: policy.ScreenshotRetention,
		DerivativeRetention: policy.DerivativeRetention,
	}
}

func stateFromText(state string) contractsv1.MeetingLifecycleState {
	switch state {
	case "READY":
		return contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_READY
	case "PARTIAL":
		return contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PARTIAL
	case "REVOKED":
		return contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_REVOKED
	case "PURGED":
		return contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PURGED
	default:
		return contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_UNSPECIFIED
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
