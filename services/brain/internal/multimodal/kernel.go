package multimodal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"
)

// Kernel serializes multimodal lifecycle operations over migration 007. It is
// safe for concurrent use: one mutex serializes every operation, matching the
// durability posture of the Stage 07 meeting store.
type Kernel struct {
	db       *sql.DB
	payloads PayloadStore
	clock    Clock
	mu       sync.Mutex
}

// Open attaches the multimodal kernel to an already-migrated authority database.
// Migration 007 must already be applied; Open takes neither migrations nor the
// process owner lock.
func Open(ctx context.Context, config Config) (*Kernel, error) {
	clean := filepath.Clean(config.DatabasePath)
	if !filepath.IsAbs(clean) || config.Payloads == nil || config.Clock == nil {
		return nil, ErrInvalidInput
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("multimodal: open database: %w", err)
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
			return fmt.Errorf("multimodal: configure database: %w", err)
		}
	}
	return nil
}

func (k *Kernel) requireSchema(ctx context.Context) error {
	var applied int
	if err := k.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version=7`).Scan(&applied); err != nil {
		return errors.Join(ErrSchemaUnsupported, fmt.Errorf("multimodal: inspect migrations: %w", err))
	}
	if applied != 1 {
		return ErrSchemaUnsupported
	}
	return nil
}

// Admit admits one bounded multimodal envelope, vaults original bytes, runs
// deterministic extractors, and returns the initial admitted/extracting state
// per the frozen contract (public readiness is READY or PARTIAL_READY after
// the synchronous residual extract pass completes).
func (k *Kernel) Admit(ctx context.Context, command AdmitCommand) (*contractsv1.AdmitMultimodalSourceSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := command.Request
	if request.Envelope == nil || request.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	digest, err := admitRequestDigest(request)
	if err != nil {
		return nil, ErrInvalidInput
	}
	payload, err := k.loadAdmitPayload(command)
	if err != nil {
		return nil, err
	}
	view := envelopeView(request.Envelope)
	if err := preDecode(view, len(payload)); err != nil {
		return nil, err
	}
	if digestBytes(payload) != request.Envelope.ContentDigest.Hex {
		return nil, ErrMalformed
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "admit", request.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest {
			return nil, ErrNotFoundOrDenied
		}
		return k.admitSuccessFromRow(ctx, command.Identity, existing.sourceID)
	}

	sourceID := identity(
		"ouroboros.stage11.source.v1",
		command.Identity.Tenant, command.Identity.Principal,
		request.IdempotencyKey, digest,
	)
	revisionID := identity("ouroboros.stage11.revision.v1", sourceID, request.Envelope.ContentDigest.Hex)
	extracted, err := extract(view.Kind, payload, command.ForcePartial, revisionID)
	if err != nil {
		return nil, mapExtractError(err)
	}
	payloadArtifact, err := k.payloads.Put(ctx, command.Identity.Tenant, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: stage original", ErrPayloadUnavailable)
	}
	evidenceJSON, err := json.Marshal(evidenceBody{
		Version: payloadVersion, Items: extracted.Items, Lanes: extracted.Lanes,
	})
	if err != nil {
		return nil, ErrInvalidInput
	}
	evidenceArtifact, err := k.payloads.Put(ctx, command.Identity.Tenant, evidenceJSON)
	if err != nil {
		return nil, fmt.Errorf("%w: stage evidence", ErrPayloadUnavailable)
	}
	boundsJSON, err := json.Marshal(boundsMap(request.Envelope))
	if err != nil {
		return nil, ErrInvalidInput
	}
	state := "READY"
	if extracted.Partial {
		state = "PARTIAL_READY"
	}
	nowMs := k.clock.NowUnixMilli()
	kindText := kindToText(view.Kind)
	brainID := ""
	if request.Envelope.BrainId != nil {
		brainID = request.Envelope.BrainId.Value
	}
	objectNS, objectVal := "", ""
	if request.Envelope.SourceObjectId != nil {
		objectNS = request.Envelope.SourceObjectId.Namespace
		objectVal = request.Envelope.SourceObjectId.Value
	}
	extractorHex := request.Envelope.ExtractorIdentity.Hex
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("multimodal: begin admit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO multimodal_sources (
		tenant_id,principal_id,source_id,session_id,source_revision_id,kind,media_type,
		byte_length,content_digest,payload_artifact_id,evidence_artifact_id,
		extractor_identity_hex,brain_id,source_object_ns,source_object_val,bounds_json,
		partial,admitted_at_ms
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, sourceID, command.Identity.Session,
		revisionID, kindText, request.Envelope.MediaType, int64(request.Envelope.ByteLength),
		request.Envelope.ContentDigest.Hex, payloadArtifact, evidenceArtifact, extractorHex,
		brainID, objectNS, objectVal, string(boundsJSON), boolToInt(extracted.Partial), nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert source: %w", err)
	}
	// Residual extract is synchronous: open in ADMITTED then advance to READY
	// or PARTIAL_READY so admit success reports admitted/extracting while status
	// discloses the completed readiness vocabulary.
	if _, err := tx.ExecContext(ctx, `INSERT INTO multimodal_source_states
		(tenant_id,principal_id,source_id,sequence,state,occurred_at_ms)
		VALUES (?,?,?,1,'ADMITTED',?)`,
		command.Identity.Tenant, command.Identity.Principal, sourceID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert admitted state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO multimodal_source_states
		(tenant_id,principal_id,source_id,sequence,state,occurred_at_ms)
		VALUES (?,?,?,2,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, sourceID, state, nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert ready state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO multimodal_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,source_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "admit", request.IdempotencyKey,
		digest, sourceID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert idempotency: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("multimodal: commit admit: %w", err)
	}
	// Admit success CEL requires admitted (1) or extracting (2). The residual
	// path completed extract synchronously; report ADMITTED as the initial
	// success vocabulary and let Status surface READY/PARTIAL_READY.
	return &contractsv1.AdmitMultimodalSourceSuccess{
		SourceId:         &contractsv1.Identifier{Namespace: "multimodal-source", Value: sourceID},
		SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: revisionID},
		State:            contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_ADMITTED,
	}, nil
}

// Status reads readiness for one non-revoked, non-purged multimodal source.
func (k *Kernel) Status(ctx context.Context, command StatusCommand) (*contractsv1.GetMultimodalStatusSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.SourceID) {
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
	row, state, err := k.loadQueryable(ctx, command.Identity, command.SourceID)
	if err != nil {
		return nil, err
	}
	body, err := k.loadEvidence(ctx, command.Identity.Tenant, row.evidenceArtifact)
	if err != nil {
		return nil, err
	}
	status := &contractsv1.MultimodalSourceStatus{
		SourceId:         &contractsv1.Identifier{Namespace: "multimodal-source", Value: command.SourceID},
		SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: row.revisionID},
		Kind:             kindFromText(row.kind),
		State:            stateFromText(state),
		DeletionState:    contractsv1.DeletionState_DELETION_STATE_ACTIVE,
		AclEpoch:         0,
		Lanes:            lanesFromBody(body.Lanes),
		ExtractorIdentity: &contractsv1.Digest{
			Algorithm: "sha256", Hex: row.extractorHex,
		},
		ObservedAt: timestamppb.New(time.UnixMilli(row.admittedAtMs).UTC()),
	}
	return &contractsv1.GetMultimodalStatusSuccess{Status: status}, nil
}

// Evidence pages modality-native anchors for one admitted source.
func (k *Kernel) Evidence(ctx context.Context, command EvidenceCommand) (*contractsv1.GetMultimodalEvidenceSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.SourceID) {
		return nil, ErrInvalidInput
	}
	if command.PageSize == 0 || command.PageSize > 100 {
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
	row, _, err := k.loadQueryable(ctx, command.Identity, command.SourceID)
	if err != nil {
		return nil, err
	}
	body, err := k.loadEvidence(ctx, command.Identity.Tenant, row.evidenceArtifact)
	if err != nil {
		return nil, err
	}
	start := 0
	if command.After != "" {
		for index, item := range body.Items {
			if item.EvidenceID == command.After {
				start = index + 1
				break
			}
		}
	}
	end := start + int(command.PageSize)
	if end > len(body.Items) {
		end = len(body.Items)
	}
	page := body.Items[start:end]
	items := make([]*contractsv1.MultimodalEvidenceItem, 0, len(page))
	for _, item := range page {
		items = append(items, evidenceItemToProto(item))
	}
	var next *contractsv1.Cursor
	if end < len(body.Items) {
		next = &contractsv1.Cursor{Token: body.Items[end-1].EvidenceID}
	}
	return &contractsv1.GetMultimodalEvidenceSuccess{Items: items, NextCursor: next}, nil
}

// Revoke denies hydration and evidence immediately, or returns the original
// outcome for an exact authenticated idempotent replay.
func (k *Kernel) Revoke(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeMultimodalSourceSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) ||
		!validBoundedID(command.SourceID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := digestText("revoke\x00" + command.SourceID)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "revoke", command.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest || existing.sourceID != command.SourceID {
			return nil, ErrNotFoundOrDenied
		}
		return &contractsv1.RevokeMultimodalSourceSuccess{
			SourceId:      &contractsv1.Identifier{Namespace: "multimodal-source", Value: command.SourceID},
			State:         contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_REVOKED,
			DeletionState: contractsv1.DeletionState_DELETION_STATE_TOMBSTONED,
		}, nil
	}
	state, err := k.currentState(ctx, command.Identity, command.SourceID)
	if err != nil {
		return nil, err
	}
	if state == "PURGED" {
		return nil, ErrNotFoundOrDenied
	}
	if state != "REVOKED" {
		if err := k.appendState(ctx, command.Identity, command.SourceID, "REVOKED"); err != nil {
			return nil, err
		}
	}
	nowMs := k.clock.NowUnixMilli()
	if _, err := k.db.ExecContext(ctx, `INSERT INTO multimodal_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,source_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "revoke", command.IdempotencyKey,
		digest, command.SourceID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert revoke idempotency: %w", err)
	}
	return &contractsv1.RevokeMultimodalSourceSuccess{
		SourceId:      &contractsv1.Identifier{Namespace: "multimodal-source", Value: command.SourceID},
		State:         contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_REVOKED,
		DeletionState: contractsv1.DeletionState_DELETION_STATE_TOMBSTONED,
	}, nil
}

// Purge tombstones lineage and purges encrypted original + evidence artifacts.
func (k *Kernel) Purge(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeMultimodalSourceSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) ||
		!validBoundedID(command.SourceID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := digestText("purge\x00" + command.SourceID)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if existing, found, err := k.lookupIdempotency(ctx, command.Identity, "purge", command.IdempotencyKey); err != nil {
		return nil, err
	} else if found {
		if existing.requestDigest != digest || existing.sourceID != command.SourceID {
			return nil, ErrNotFoundOrDenied
		}
		return purgeSuccess(command.SourceID), nil
	}
	row, state, err := k.loadAny(ctx, command.Identity, command.SourceID)
	if err != nil {
		return nil, err
	}
	if state != "PURGED" {
		if state != "REVOKED" {
			if err := k.appendState(ctx, command.Identity, command.SourceID, "REVOKED"); err != nil {
				return nil, err
			}
		}
		if err := k.payloads.Purge(ctx, command.Identity.Tenant, row.payloadArtifact); err != nil {
			return nil, fmt.Errorf("%w: purge original", ErrPayloadUnavailable)
		}
		if err := k.payloads.Purge(ctx, command.Identity.Tenant, row.evidenceArtifact); err != nil {
			return nil, fmt.Errorf("%w: purge evidence", ErrPayloadUnavailable)
		}
		if err := k.appendState(ctx, command.Identity, command.SourceID, "PURGED"); err != nil {
			return nil, err
		}
	}
	nowMs := k.clock.NowUnixMilli()
	if _, err := k.db.ExecContext(ctx, `INSERT INTO multimodal_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,source_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		command.Identity.Tenant, command.Identity.Principal, "purge", command.IdempotencyKey,
		digest, command.SourceID, nowMs,
	); err != nil {
		return nil, fmt.Errorf("multimodal: insert purge idempotency: %w", err)
	}
	return purgeSuccess(command.SourceID), nil
}

func purgeSuccess(sourceID string) *contractsv1.PurgeMultimodalSourceSuccess {
	return &contractsv1.PurgeMultimodalSourceSuccess{
		SourceId:      &contractsv1.Identifier{Namespace: "multimodal-source", Value: sourceID},
		State:         contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_PURGED,
		DeletionState: contractsv1.DeletionState_DELETION_STATE_PURGED,
		PurgeReceipt: &contractsv1.Receipt{
			ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: "purge-" + sourceID},
			OperationId: &contractsv1.Identifier{Namespace: "operation", Value: "multimodal-purge"},
			Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
			Causal: &contractsv1.CausalContext{
				CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: sourceID},
				CausationId:   &contractsv1.Identifier{Namespace: "causation", Value: sourceID},
				TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: sourceID},
			},
			RecordedAt: timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
			ConfigurationDigest: &contractsv1.Digest{
				Algorithm: "sha256",
				Hex:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		},
	}
}

type sourceRow struct {
	sourceID         string
	revisionID       string
	kind             string
	mediaType        string
	payloadArtifact  string
	evidenceArtifact string
	extractorHex     string
	admittedAtMs     int64
	partial          bool
}

type idempotencyRow struct {
	requestDigest string
	sourceID      string
}

func (k *Kernel) loadAdmitPayload(command AdmitCommand) ([]byte, error) {
	if len(command.Payload) > 0 {
		return command.Payload, nil
	}
	envelope := command.Request.Envelope
	if envelope == nil || envelope.SourceObjectId == nil {
		return nil, ErrInvalidInput
	}
	if envelope.SourceObjectId.Namespace != localPathNamespace {
		return nil, ErrPartialPayload
	}
	path := envelope.SourceObjectId.Value
	if !filepath.IsAbs(path) || strings.Contains(path, "..") || strings.Contains(path, "\x00") {
		return nil, ErrInvalidInput
	}
	// Cap residual local-path reads at the largest Stage 11 bound.
	const maxRead = maxWAVBytes
	info, err := os.Stat(path)
	if err != nil {
		return nil, ErrPartialPayload
	}
	if info.Size() <= 0 || info.Size() > maxRead {
		return nil, ErrOversized
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrPartialPayload
	}
	return payload, nil
}

func (k *Kernel) lookupIdempotency(
	ctx context.Context, identity Identity, operation, key string,
) (idempotencyRow, bool, error) {
	var row idempotencyRow
	err := k.db.QueryRowContext(ctx, `SELECT request_digest,source_id FROM multimodal_idempotency
		WHERE tenant_id=? AND principal_id=? AND operation=? AND idempotency_key=?`,
		identity.Tenant, identity.Principal, operation, key,
	).Scan(&row.requestDigest, &row.sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return idempotencyRow{}, false, nil
	}
	if err != nil {
		return idempotencyRow{}, false, fmt.Errorf("multimodal: lookup idempotency: %w", err)
	}
	return row, true, nil
}

func (k *Kernel) loadQueryable(ctx context.Context, identity Identity, sourceID string) (sourceRow, string, error) {
	row, state, err := k.loadAny(ctx, identity, sourceID)
	if err != nil {
		return sourceRow{}, "", err
	}
	if state == "REVOKED" || state == "PURGED" {
		return sourceRow{}, "", ErrNotFoundOrDenied
	}
	return row, state, nil
}

func (k *Kernel) loadAny(ctx context.Context, identity Identity, sourceID string) (sourceRow, string, error) {
	var row sourceRow
	var partial int
	err := k.db.QueryRowContext(ctx, `SELECT source_id,source_revision_id,kind,media_type,
		payload_artifact_id,evidence_artifact_id,extractor_identity_hex,admitted_at_ms,partial
		FROM multimodal_sources
		WHERE tenant_id=? AND principal_id=? AND source_id=?`,
		identity.Tenant, identity.Principal, sourceID,
	).Scan(
		&row.sourceID, &row.revisionID, &row.kind, &row.mediaType,
		&row.payloadArtifact, &row.evidenceArtifact, &row.extractorHex, &row.admittedAtMs, &partial,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sourceRow{}, "", ErrNotFoundOrDenied
	}
	if err != nil {
		return sourceRow{}, "", fmt.Errorf("multimodal: load source: %w", err)
	}
	row.partial = partial == 1
	state, err := k.currentState(ctx, identity, sourceID)
	if err != nil {
		return sourceRow{}, "", err
	}
	return row, state, nil
}

func (k *Kernel) currentState(ctx context.Context, identity Identity, sourceID string) (string, error) {
	var state string
	err := k.db.QueryRowContext(ctx, `SELECT state FROM multimodal_source_states
		WHERE tenant_id=? AND principal_id=? AND source_id=?
		ORDER BY sequence DESC LIMIT 1`,
		identity.Tenant, identity.Principal, sourceID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFoundOrDenied
	}
	if err != nil {
		return "", fmt.Errorf("multimodal: read state: %w", err)
	}
	return state, nil
}

func (k *Kernel) appendState(ctx context.Context, identity Identity, sourceID, state string) error {
	nowMs := k.clock.NowUnixMilli()
	var sequence int
	if err := k.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM multimodal_source_states
		WHERE tenant_id=? AND principal_id=? AND source_id=?`,
		identity.Tenant, identity.Principal, sourceID,
	).Scan(&sequence); err != nil {
		return fmt.Errorf("multimodal: read state sequence: %w", err)
	}
	if _, err := k.db.ExecContext(ctx, `INSERT INTO multimodal_source_states
		(tenant_id,principal_id,source_id,sequence,state,occurred_at_ms)
		VALUES (?,?,?,?,?,?)`,
		identity.Tenant, identity.Principal, sourceID, sequence, state, nowMs,
	); err != nil {
		return fmt.Errorf("multimodal: append state: %w", err)
	}
	return nil
}

func (k *Kernel) admitSuccessFromRow(
	ctx context.Context, identity Identity, sourceID string,
) (*contractsv1.AdmitMultimodalSourceSuccess, error) {
	row, _, err := k.loadAny(ctx, identity, sourceID)
	if err != nil {
		return nil, err
	}
	return &contractsv1.AdmitMultimodalSourceSuccess{
		SourceId:         &contractsv1.Identifier{Namespace: "multimodal-source", Value: sourceID},
		SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: row.revisionID},
		State:            contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_ADMITTED,
	}, nil
}

func (k *Kernel) loadEvidence(ctx context.Context, tenant, artifactID string) (evidenceBody, error) {
	encoded, err := k.payloads.Get(ctx, tenant, artifactID)
	if err != nil {
		return evidenceBody{}, fmt.Errorf("%w: hydrate evidence", ErrPayloadUnavailable)
	}
	var body evidenceBody
	if err := json.Unmarshal(encoded, &body); err != nil || body.Version != payloadVersion {
		return evidenceBody{}, ErrPayloadUnavailable
	}
	return body, nil
}

func admitRequestDigest(request *contractsv1.AdmitMultimodalSourceRequest) (string, error) {
	clone := proto.Clone(request).(*contractsv1.AdmitMultimodalSourceRequest)
	clone.IdempotencyKey = ""
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func envelopeView(envelope *contractsv1.MultimodalSourceEnvelope) *contractsv1Envelope {
	if envelope == nil {
		return nil
	}
	view := &contractsv1Envelope{
		MediaType:  envelope.MediaType,
		ByteLength: envelope.ByteLength,
	}
	if envelope.ContentDigest != nil {
		view.ContentDigest = &digestView{
			Algorithm: envelope.ContentDigest.Algorithm, Hex: envelope.ContentDigest.Hex,
		}
	}
	if envelope.ExtractorIdentity != nil {
		view.ExtractorIdentity = &digestView{
			Algorithm: envelope.ExtractorIdentity.Algorithm, Hex: envelope.ExtractorIdentity.Hex,
		}
	}
	switch envelope.Kind {
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_TEXT_MARKDOWN:
		view.Kind = kindText
		if text := envelope.GetText(); text != nil {
			view.Text = &textBounds{Utf8ByteLength: text.Utf8ByteLength}
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PDF:
		view.Kind = kindPDF
		if pdf := envelope.GetPdf(); pdf != nil {
			view.PDF = &pdfBounds{ByteLength: pdf.ByteLength, PageCount: pdf.PageCount}
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PNG:
		view.Kind = kindPNG
		if png := envelope.GetPng(); png != nil {
			view.PNG = &pngBounds{ByteLength: png.ByteLength, WidthPx: png.WidthPx, HeightPx: png.HeightPx}
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PCM_WAV:
		view.Kind = kindWAV
		if wav := envelope.GetPcmWav(); wav != nil {
			view.WAV = &wavBounds{
				ByteLength: wav.ByteLength, DurationMillis: wav.DurationMillis,
				SampleRateHz: wav.SampleRateHz, Channels: wav.Channels,
			}
		}
	default:
		view.Kind = kindUnspecified
	}
	return view
}

func boundsMap(envelope *contractsv1.MultimodalSourceEnvelope) map[string]any {
	out := map[string]any{"media_type": envelope.MediaType, "byte_length": envelope.ByteLength}
	switch envelope.Kind {
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_TEXT_MARKDOWN:
		if text := envelope.GetText(); text != nil {
			out["utf8_byte_length"] = text.Utf8ByteLength
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PDF:
		if pdf := envelope.GetPdf(); pdf != nil {
			out["page_count"] = pdf.PageCount
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PNG:
		if png := envelope.GetPng(); png != nil {
			out["width_px"] = png.WidthPx
			out["height_px"] = png.HeightPx
		}
	case contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PCM_WAV:
		if wav := envelope.GetPcmWav(); wav != nil {
			out["duration_millis"] = wav.DurationMillis
			out["sample_rate_hz"] = wav.SampleRateHz
			out["channels"] = wav.Channels
		}
	}
	return out
}

func kindToText(kind kindCode) string {
	switch kind {
	case kindText:
		return "TEXT_MARKDOWN"
	case kindPDF:
		return "PDF"
	case kindPNG:
		return "PNG"
	case kindWAV:
		return "PCM_WAV"
	default:
		return "TEXT_MARKDOWN"
	}
}

func kindFromText(kind string) contractsv1.MultimodalSourceKind {
	switch kind {
	case "TEXT_MARKDOWN":
		return contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_TEXT_MARKDOWN
	case "PDF":
		return contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PDF
	case "PNG":
		return contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PNG
	case "PCM_WAV":
		return contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PCM_WAV
	default:
		return contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_UNSPECIFIED
	}
}

func stateFromText(state string) contractsv1.MultimodalSourceState {
	switch state {
	case "ADMITTED":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_ADMITTED
	case "EXTRACTING":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_EXTRACTING
	case "PARTIAL_READY":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_PARTIAL_READY
	case "READY":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_READY
	case "FAILED":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_FAILED
	case "QUARANTINED":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_QUARANTINED
	case "REVOKED":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_REVOKED
	case "PURGED":
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_PURGED
	default:
		return contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_UNSPECIFIED
	}
}

func lanesFromBody(lanes []laneBody) []*contractsv1.MultimodalLaneCoverage {
	out := make([]*contractsv1.MultimodalLaneCoverage, 0, len(lanes))
	for _, lane := range lanes {
		out = append(out, &contractsv1.MultimodalLaneCoverage{
			Lane:             laneEnum(lane.Lane),
			State:            laneStateEnum(lane.State),
			Required:         lane.Required,
			CoveragePerMille: lane.CoveragePerMille,
		})
	}
	return out
}

func laneEnum(lane string) contractsv1.MultimodalReadinessLane {
	switch lane {
	case "ORIGINAL":
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_ORIGINAL
	case "TEXT":
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_TEXT
	case "PAGE":
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_PAGE
	case "REGION":
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_REGION
	case "TRANSCRIPT":
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_TRANSCRIPT
	default:
		return contractsv1.MultimodalReadinessLane_MULTIMODAL_READINESS_LANE_UNSPECIFIED
	}
}

func laneStateEnum(state string) contractsv1.MultimodalLaneState {
	switch state {
	case "PENDING":
		return contractsv1.MultimodalLaneState_MULTIMODAL_LANE_STATE_PENDING
	case "READY":
		return contractsv1.MultimodalLaneState_MULTIMODAL_LANE_STATE_READY
	case "FAILED":
		return contractsv1.MultimodalLaneState_MULTIMODAL_LANE_STATE_FAILED
	case "ABSENT":
		return contractsv1.MultimodalLaneState_MULTIMODAL_LANE_STATE_ABSENT
	default:
		return contractsv1.MultimodalLaneState_MULTIMODAL_LANE_STATE_UNSPECIFIED
	}
}

func evidenceItemToProto(item evidenceBodyItem) *contractsv1.MultimodalEvidenceItem {
	authority := contractsv1.AuthorityClass_AUTHORITY_CLASS_DIRECT_SOURCE
	if item.Authority == "MACHINE_OBSERVATION" {
		authority = contractsv1.AuthorityClass_AUTHORITY_CLASS_MACHINE_OBSERVATION
	}
	anchor := &contractsv1.EvidenceAnchor{}
	switch item.AnchorKind {
	case "bytes":
		anchor.Location = &contractsv1.EvidenceAnchor_Bytes{
			Bytes: &contractsv1.EvidenceAnchor_BytesAnchor{
				StartByte: item.StartByte, EndByte: item.EndByte,
			},
		}
	case "text":
		anchor.Location = &contractsv1.EvidenceAnchor_Text{
			Text: &contractsv1.EvidenceAnchor_TextAnchor{
				StartByte: item.StartByte, EndByte: item.EndByte,
			},
		}
	case "page":
		page := &contractsv1.EvidenceAnchor_PageAnchor{PageNumber: item.PageNumber}
		if item.RightPerMille > 0 || item.BottomPerMille > 0 {
			page.Bounds = &contractsv1.NormalizedBox{
				LeftPerMille: item.LeftPerMille, RightPerMille: item.RightPerMille,
				TopPerMille: item.TopPerMille, BottomPerMille: item.BottomPerMille,
			}
		}
		anchor.Location = &contractsv1.EvidenceAnchor_Page{Page: page}
	case "audio":
		anchor.Location = &contractsv1.EvidenceAnchor_Audio{
			Audio: &contractsv1.EvidenceAnchor_TimedAnchor{
				StartMillis: item.StartMillis, EndMillis: item.EndMillis,
			},
		}
	}
	out := &contractsv1.MultimodalEvidenceItem{
		Evidence: &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: "evidence", Value: item.EvidenceID},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: item.SourceRevisionID},
		},
		Anchor:         anchor,
		AuthorityClass: authority,
	}
	if item.SupportDigest != "" {
		out.SupportingTextDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: item.SupportDigest}
	}
	return out
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
