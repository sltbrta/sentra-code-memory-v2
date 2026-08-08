package localauthority

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/conversation"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Config contains explicit local authority state and receipt configuration.
// DatabasePath and MigrationSQL are required; no current-directory lookup or
// implicit migration source is used. Brain fixes the evidence security domain.
type Config struct {
	DatabasePath        string
	MigrationSQL        string
	Brain               Identifier
	ConfigurationDigest Digest
	Clock               Clock
}

// Runtime serializes storage effects with the canonical SQLite command ledger.
// It owns the supplied Storage, database, and optional non-owning metadata view.
type Runtime struct {
	store          *localstate.Store
	storage        *Storage
	metadata       interface{ Close() error }
	brain          Identifier
	ingestTenant   Identifier
	ingestKeyEpoch uint64
	config         Digest
	clock          Clock
	mu             sync.Mutex
	ingestionWork  sync.Mutex
	ingestionMu    sync.RWMutex
	ingestion      *ingestionRuntime
	closed         bool
	closeOnce      sync.Once
	closeError     error
	// databasePath and conversationPayloads compose the Stage 04 query surface.
	// They are set only by the durable opener; a non-durable runtime leaves
	// them empty and OpenQuerySurface fails closed.
	databasePath         string
	conversationPayloads *conversation.VaultPayloads
	// productMemory is optional ontology+gardener state for product SOTA path
	// (ADR 0020). Nil until EnsureProductMemory or company-doc enrich.
	productMemory *ProductMemory
}

// New opens and migrates the real SQLite authority ledger and binds encrypted
// storage. It returns ErrInvalid for any missing path, migration, clock, brain,
// digest, or storage dependency and never creates key material.
func New(ctx context.Context, config Config, storage *Storage) (*Runtime, error) {
	if storage == nil || config.DatabasePath == "" || config.MigrationSQL == "" || config.Clock == nil ||
		!validID(config.Brain, "brain") || !validSHA256(config.ConfigurationDigest) {
		return nil, ErrInvalid
	}
	store, err := localstate.Open(ctx, config.DatabasePath, config.MigrationSQL, config.Clock)
	if err != nil {
		return nil, fmt.Errorf("open canonical local authority: %w", ErrInvalid)
	}
	runtime, err := newRuntime(
		store, storage, nil, config.Brain, Identifier{}, 0, config.ConfigurationDigest, config.Clock,
	)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return runtime, nil
}

func newRuntime(
	store *localstate.Store,
	storage *Storage,
	metadata interface{ Close() error },
	brain Identifier,
	ingestTenant Identifier,
	ingestKeyEpoch uint64,
	config Digest,
	clock Clock,
) (*Runtime, error) {
	if store == nil || storage == nil || !validID(brain, "brain") || !validSHA256(config) || clock == nil {
		return nil, ErrInvalid
	}
	return &Runtime{
		store: store, storage: storage, metadata: metadata,
		brain: brain, ingestTenant: ingestTenant, ingestKeyEpoch: ingestKeyEpoch,
		config: config, clock: clock,
	}, nil
}

// Close releases encrypted objects, the non-owning metadata bundle, and the
// SQLite owner in reverse construction order. It is nil-safe, concurrency-safe,
// and idempotent; callers must stop accepting new requests before invoking it.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ingestionWork.Lock()
		defer r.ingestionWork.Unlock()
		r.ingestionMu.Lock()
		defer r.ingestionMu.Unlock()
		r.closed = true
		var storageErr, metadataErr, storeErr error
		if r.storage != nil {
			storageErr = r.storage.Close()
		}
		if r.metadata != nil {
			metadataErr = r.metadata.Close()
		}
		if r.store != nil {
			storeErr = r.store.Close()
		}
		r.closeError = errors.Join(storageErr, metadataErr, storeErr)
	})
	return r.closeError
}

// PrepareArtifact stages encrypted artifact bytes for trusted ingestion
// composition before an artifact.admit command. It is not a client endpoint:
// it performs no authorization, publication, evidence admission, or command
// commit. A durable runtime accepts only its configured tenant and current key
// epoch. Malformed inputs return ErrInvalid; storage failures return the static
// ErrDenied, while context cancellation is preserved.
func (r *Runtime) PrepareArtifact(ctx context.Context, artifact Artifact, content io.Reader) error {
	if ctx == nil || r == nil || content == nil || !validArtifactForStage(artifact) {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.storage == nil {
		return ErrDenied
	}
	if !validID(r.ingestTenant, "tenant") || r.ingestKeyEpoch == 0 {
		return ErrDenied
	}
	if artifact.Tenant != r.ingestTenant || artifact.KeyEpoch != r.ingestKeyEpoch {
		return ErrInvalid
	}
	if err := r.storage.stage(ctx, artifact, content); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, artifactvault.ErrInvalid) {
			return ErrInvalid
		}
		return ErrDenied
	}
	return nil
}

// OpenSession persists or exactly resumes one peer-authenticated session.
// Identity conflicts and audit corruption both return the same ErrDenied.
func (r *Runtime) OpenSession(ctx context.Context, mapped Identity) (Result, error) {
	if ctx == nil || r == nil || !validIdentity(mapped) {
		return Result{}, ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.store == nil || r.clock == nil {
		return Result{}, ErrDenied
	}
	if r.store.VerifyAudit(ctx, mapped.Tenant) != nil || r.store.OpenSession(ctx, mapped) != nil {
		return Result{}, ErrDenied
	}
	execution, err := r.store.Execute(ctx, sessionOpenMutation(mapped))
	if err != nil || r.store.VerifyAudit(ctx, mapped.Tenant) != nil {
		return Result{}, ErrDenied
	}
	now := r.clock.NowUnixMilli()
	return Result{
		Receipt: execution.Receipt, Replayed: execution.Replayed,
		RecordedAtMilli: now, ConfigurationDigest: r.config,
	}, nil
}

// Execute reauthorizes, durably reserves the canonical command, performs one
// bounded idempotent artifact operation, and finalizes its event, receipt,
// watermark, and audit link. Accepted crash reservations resume; completed
// mutating retries return without rerunning effects. A valid current-policy
// denial and absent or non-canonical artifact metadata return the same
// canonical non-disclosing rejected Result. Malformed, idempotency, audit, and
// post-canonicalization integrity failures collapse to ErrDenied.
func (r *Runtime) Execute(ctx context.Context, request ExecuteRequest) (Result, error) {
	if ctx == nil || r == nil || !validExecute(request) {
		return Result{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.store == nil || r.storage == nil || r.clock == nil {
		return Result{}, ErrDenied
	}
	if r.store.VerifyAudit(ctx, request.Identity.Tenant) != nil {
		return Result{}, ErrDenied
	}
	authorization, err := authorize(ctx, request)
	if err != nil || !authorization.Allowed {
		execution, recordErr := r.store.Execute(ctx, authorizationDeniedMutation(request))
		if recordErr != nil || r.store.VerifyAudit(ctx, request.Identity.Tenant) != nil {
			return Result{}, ErrDenied
		}
		return Result{
			Receipt: execution.Receipt, Authorization: authorization,
			Artifact: request.Artifact, Replayed: execution.Replayed,
			RecordedAtMilli: r.clock.NowUnixMilli(), ConfigurationDigest: r.config,
		}, nil
	}
	reservation, err := r.store.Reserve(ctx, commandRecord(request))
	if err != nil {
		return Result{}, ErrDenied
	}
	if reservation.Status == "completed" && reservation.Receipt.Status == "rejected" {
		return Result{
			Receipt: reservation.Receipt,
			Authorization: Authorization{
				Allowed: false, ReasonCode: reservation.Receipt.ReasonCode,
				RevocationEpoch: authorization.RevocationEpoch,
			},
			Artifact: request.Artifact, Replayed: true,
			RecordedAtMilli: r.clock.NowUnixMilli(), ConfigurationDigest: r.config,
		}, nil
	}
	canonical := request
	canonical.Command = domainCommand(reservation.Command)
	if canonical.Command.Type != "artifact.admit" {
		allowDeletedReplay := reservation.Status == "completed" && canonical.Command.Type == "artifact.delete"
		canonical.Artifact, err = r.storage.canonicalArtifact(ctx, canonical.Artifact, allowDeletedReplay)
		if err != nil {
			if !errors.Is(err, ErrDenied) || reservation.Status != "accepted" {
				return Result{}, ErrDenied
			}
			mutation := authorizationDeniedMutation(request)
			mutation.Command = reservation.Command
			execution, recordErr := r.store.Finalize(ctx, mutation)
			if recordErr != nil || r.store.VerifyAudit(ctx, request.Identity.Tenant) != nil {
				return Result{}, ErrDenied
			}
			return Result{
				Receipt: execution.Receipt,
				Authorization: Authorization{
					Allowed: false, ReasonCode: "not_found_or_denied",
					RevocationEpoch: authorization.RevocationEpoch,
				},
				Artifact: request.Artifact, Replayed: execution.Replayed,
				RecordedAtMilli: r.clock.NowUnixMilli(), ConfigurationDigest: r.config,
			}, nil
		}
	}
	authorization, err = authorize(ctx, canonical)
	if err != nil || !authorization.Allowed {
		return Result{}, ErrDenied
	}
	if reservation.Status == "completed" {
		result := Result{}
		if canonical.Command.Type == "artifact.read" {
			result, _, err = r.apply(ctx, canonical)
			if err != nil {
				return Result{}, ErrDenied
			}
		}
		result.Receipt = reservation.Receipt
		result.Authorization = authorization
		result.Artifact = canonical.Artifact
		result.Replayed = true
		result.RecordedAtMilli = r.clock.NowUnixMilli()
		result.ConfigurationDigest = r.config
		return result, nil
	}
	if reservation.Status != "accepted" {
		return Result{}, ErrDenied
	}
	result, reason, err := r.apply(ctx, canonical)
	if err != nil {
		return Result{}, ErrDenied
	}
	version, err := r.nextVersion(ctx, request.Identity.Tenant, request.Artifact.ID)
	if err != nil {
		clear(result.Bytes)
		return Result{}, ErrDenied
	}
	execution, err := r.store.Finalize(ctx, localstate.Mutation{
		Command: reservation.Command,
		Events: []localstate.MutationEvent{{
			Type: "artifact", SchemaVersion: 1,
			Record: shared.EventRecord{
				Event:     Identifier{Namespace: "event", Value: reservation.Command.Command.Value},
				Aggregate: request.Artifact.ID, Version: version,
				PayloadDigest: reservation.Command.AuthenticatedDigest,
			},
		}},
		Receipt:    shared.Receipt{Status: "completed", ReasonCode: reason},
		Projection: "local-authority",
	})
	if err != nil || r.store.VerifyAudit(ctx, request.Identity.Tenant) != nil {
		clear(result.Bytes)
		return Result{}, ErrDenied
	}
	result.Receipt = execution.Receipt
	result.Authorization = authorization
	result.Artifact = canonical.Artifact
	result.Replayed = reservation.Replayed || execution.Replayed
	result.RecordedAtMilli = r.clock.NowUnixMilli()
	result.ConfigurationDigest = r.config
	return result, nil
}

func authorize(ctx context.Context, request ExecuteRequest) (Authorization, error) {
	action, resource := actionResource(request.Command.Type, request.Artifact.ID)
	return request.Authorize(ctx, request.Identity, action, resource)
}

func sessionOpenMutation(identity Identity) localstate.Mutation {
	digest := trustedFingerprint("ouroboros.session.open.v1", identity.Tenant, identity.Principal, identity.Session)
	value := "session.open:" + digest.Hex
	command := shared.CommandRecord{
		Command: Identifier{Namespace: "command", Value: value}, Tenant: identity.Tenant,
		Principal: identity.Principal, Session: identity.Session, CommandType: "session.open",
		IdempotencyKey: value, AuthenticatedDigest: digest, Fence: 1,
	}
	return localstate.Mutation{
		Command: command,
		Events: []localstate.MutationEvent{{
			Type: "session", SchemaVersion: 1,
			Record: shared.EventRecord{
				Event: Identifier{Namespace: "event", Value: value}, Aggregate: identity.Session,
				Version: 1, PayloadDigest: digest,
			},
		}},
		Receipt:    shared.Receipt{Status: "completed", ReasonCode: "session_opened"},
		Projection: "local-authority",
	}
}

func authorizationDeniedMutation(request ExecuteRequest) localstate.Mutation {
	recordDigest := trustedFingerprint(
		"ouroboros.authorization.denied.record.v1",
		request.Identity.Tenant,
		request.Identity.Principal,
		request.Command.ID,
		Identifier{Namespace: "command-type", Value: request.Command.Type},
		Identifier{Namespace: "payload-digest", Value: request.Command.PayloadDigest.Hex},
	)
	value := "authorization.denied:" + recordDigest.Hex
	return localstate.Mutation{
		Command: commandRecord(request),
		Events: []localstate.MutationEvent{{
			Type: "authorization", SchemaVersion: 1,
			Record: shared.EventRecord{
				Event:     Identifier{Namespace: "event", Value: value},
				Aggregate: Identifier{Namespace: "authorization", Value: request.Command.ID.Value},
				Version:   1, PayloadDigest: request.Command.PayloadDigest,
			},
		}},
		Receipt:    shared.Receipt{Status: "rejected", ReasonCode: "not_found_or_denied"},
		Projection: "local-authority",
	}
}

func trustedFingerprint(purpose string, identifiers ...Identifier) Digest {
	hash := sha256.New()
	write := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write(purpose)
	for _, identifier := range identifiers {
		write(identifier.Namespace)
		write(identifier.Value)
	}
	return Digest{Algorithm: "sha256", Hex: hex.EncodeToString(hash.Sum(nil))}
}

func commandRecord(request ExecuteRequest) shared.CommandRecord {
	return shared.CommandRecord{
		Command: request.Command.ID, Tenant: request.Identity.Tenant,
		Principal: request.Identity.Principal, Session: request.Identity.Session,
		CommandType: request.Command.Type, IdempotencyKey: request.Command.IdempotencyKey,
		AuthenticatedDigest: request.Command.PayloadDigest, Fence: request.Command.Fence,
	}
}

func domainCommand(record shared.CommandRecord) Command {
	return Command{
		ID: record.Command, Type: record.CommandType,
		IdempotencyKey: record.IdempotencyKey,
		PayloadDigest:  record.AuthenticatedDigest, Fence: record.Fence,
	}
}

// ReadStatus verifies the complete tenant audit chain before returning bounded
// session state. It does not disclose whether a denied resource exists.
func (r *Runtime) ReadStatus(ctx context.Context, mapped Identity) (Status, error) {
	if ctx == nil || r == nil || !validIdentity(mapped) {
		return Status{}, ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.store == nil || r.clock == nil || r.store.VerifyAudit(ctx, mapped.Tenant) != nil {
		return Status{}, ErrDenied
	}
	watermark, err := r.store.SessionWatermark(ctx, mapped)
	if err != nil {
		return Status{}, ErrDenied
	}
	now := r.clock.NowUnixMilli()
	return Status{
		Identity: mapped, Watermark: watermark, ObservedAtMilli: now,
		ConfigurationDigest: r.config,
		Receipt: shared.Receipt{
			OperationID: Identifier{Namespace: "status", Value: mapped.Session.Value},
			Status:      "completed", ReasonCode: "status_read",
		},
	}, nil
}

func (r *Runtime) apply(ctx context.Context, request ExecuteRequest) (Result, string, error) {
	switch request.Command.Type {
	case "artifact.admit":
		if err := r.storage.publish(ctx, request.Artifact, r.brain); err != nil {
			return Result{}, "", err
		}
		return Result{}, "artifact_admitted", nil
	case "artifact.read":
		hydrated, err := r.storage.read(ctx, request.Artifact, request.Offset, request.Length)
		if err != nil {
			return Result{}, "", err
		}
		digest := sha256.Sum256(hydrated.Bytes)
		return Result{
			Bytes:       hydrated.Bytes,
			RangeDigest: Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digest[:])},
			NextOffset:  hydrated.Metadata.NextOffset,
		}, "artifact_read", nil
	case "artifact.delete":
		if err := r.storage.delete(ctx, request.Artifact, r.brain, request.PurgeNow); err != nil {
			return Result{}, "", err
		}
		if request.PurgeNow {
			return Result{}, "artifact_purged", nil
		}
		return Result{}, "artifact_tombstoned", nil
	default:
		return Result{}, "", ErrInvalid
	}
}

func (r *Runtime) nextVersion(ctx context.Context, tenant, artifact Identifier) (uint64, error) {
	version, err := r.store.AggregateVersion(ctx, tenant, artifact)
	if err != nil {
		return 0, err
	}
	return version + 1, nil
}

func actionResource(commandType string, artifact Identifier) (string, Identifier) {
	return commandType, evidenceID(artifact)
}

func validExecute(request ExecuteRequest) bool {
	if !validIdentity(request.Identity) || request.Authorize == nil ||
		!validID(request.Command.ID, "command") || request.Command.IdempotencyKey == "" ||
		request.Command.Fence == 0 || !validSHA256(request.Command.PayloadDigest) ||
		!validID(request.Artifact.ID, "artifact") || request.Artifact.Tenant != request.Identity.Tenant ||
		request.Artifact.Generation == 0 || request.Artifact.KeyEpoch == 0 || !validSHA256(request.Artifact.Digest) {
		return false
	}
	switch request.Command.Type {
	case "artifact.admit":
		return request.Artifact.Length > 0 && request.Artifact.FrameCount > 0 && request.Offset == 0 && request.Length == 0
	case "artifact.read":
		return request.Length > 0 && request.Offset <= ^uint64(0)-request.Length
	case "artifact.delete":
		return request.Artifact.ExpectedGeneration > 0 && request.Offset == 0 && request.Length == 0
	default:
		return false
	}
}

func validArtifactForStage(artifact Artifact) bool {
	return validID(artifact.ID, "artifact") && validID(artifact.Tenant, "tenant") &&
		validSHA256(artifact.Digest) && artifact.Generation > 0 &&
		artifact.ExpectedGeneration < ^uint64(0) && artifact.Generation == artifact.ExpectedGeneration+1 &&
		artifact.KeyEpoch > 0 && artifact.Length > 0 && artifact.FrameCount > 0
}

func validIdentity(identity Identity) bool {
	return validID(identity.Principal, "principal") && validID(identity.Tenant, "tenant") &&
		validID(identity.Session, "session") && identity.Credentials.PID != 0
}

func validID(identifier Identifier, namespace string) bool {
	return identifier.Namespace == namespace && identifier.Value != "" && len(identifier.Value) <= 512
}

func validSHA256(digest Digest) bool {
	if digest.Algorithm != "sha256" || len(digest.Hex) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest.Hex)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest.Hex
}
