package localauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var p5LanguageOrder = []codeindex.Language{
	codeindex.LanguageGo,
	codeindex.LanguageTypeScript,
	codeindex.LanguagePython,
	codeindex.LanguageRust,
	codeindex.LanguageJava,
}

type ingestionRuntime struct {
	config            ingestion.Config
	authority         *ingestion.Authority
	scope             localstate.IngestionScope
	limits            codeindex.Limits
	current           *publishedSource
	previous          *publishedSource
	revoked           bool
	revokedGeneration string
}

type publishedSource struct {
	generation        ingestion.Generation
	previousCommitOID string
	index             codeindex.Snapshot
	files             map[string]ingestion.HydratedFile
	readiness         []localstate.IngestionReadiness
}

func newIngestionRuntime(ctx context.Context, durable DurableConfig) (*ingestionRuntime, error) {
	if durable.Ingestion == nil {
		return nil, nil
	}
	selected := durable.Ingestion
	config := ingestion.Config{
		ApprovedRoot: selected.ApprovedRoot, GitExecutable: selected.GitExecutable,
		TenantID: durable.Tenant.Value, BrainID: durable.Brain.Value,
		RepositoryID: selected.RepositoryID, ConfigurationDigest: durable.ConfigurationDigest.Hex,
		Policy: ingestion.Policy{UseGitIgnore: true, UseOuroborosIgnore: true,
			Symlinks: ingestion.RecordWithoutFollow},
		CommandTimeout: selected.CommandTimeout, MaxFiles: selected.MaxFiles,
		MaxPathBytes: selected.MaxPathBytes, MaxFileBytes: selected.MaxFileBytes,
		MaxTotalBytes: selected.MaxTotalBytes, MaxIdempotencyRecords: selected.MaxIdempotencyRecords,
	}
	if !filepath.IsAbs(selected.ApprovedRoot) || !filepath.IsAbs(selected.GitExecutable) ||
		strings.TrimSpace(selected.RepositoryID) == "" {
		return nil, ErrInvalid
	}
	authority, err := ingestion.New(ctx, config)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, ErrInvalid
	}
	status := authority.Status()
	limits := codeindex.DefaultLimits()
	if config.MaxFiles < limits.MaxFiles {
		limits.MaxFiles = config.MaxFiles
	}
	if config.MaxFileBytes < int64(limits.MaxInputBytes) {
		limits.MaxInputBytes = int(config.MaxFileBytes)
	}
	return &ingestionRuntime{
		config: config, authority: authority, limits: limits,
		scope: localstate.IngestionScope{Tenant: durable.Tenant, Brain: durable.Brain, SourceID: status.SourceID},
	}, nil
}

// AddSource authorizes and atomically publishes one exact committed source generation.
func (r *Runtime) AddSource(ctx context.Context, request AddSourceRequest) (IngestionResult, error) {
	if !r.validIngestionContext(ctx, request.IngestionContext) || !validOpaque(request.IdempotencyKey) ||
		!validGitOID(request.ExpectedCommitOID) {
		return IngestionResult{}, ErrInvalid
	}
	if err := r.authorizeIngestion(ctx, request.IngestionContext, "source.add"); err != nil {
		return IngestionResult{}, err
	}
	r.ingestionWork.Lock()
	defer r.ingestionWork.Unlock()
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.closed || r.ingestion == nil || r.ingestion.revoked {
		return IngestionResult{}, ErrDenied
	}
	if err := r.restoreIngestionLocked(ctx, request.Identity); err != nil {
		return IngestionResult{}, err
	}
	generation, err := r.ingestion.authority.Admit(ctx, ingestion.Admission{
		ExpectedCommitOID: request.ExpectedCommitOID, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return IngestionResult{}, collapseIngestionError(ctx, err)
	}
	candidate, err := r.buildPublishedSource(ctx, r.ingestion.authority, generation, nil)
	if err != nil {
		return IngestionResult{}, err
	}
	execution, err := r.publishSource(ctx, request.IngestionContext, "source.add", request.IdempotencyKey, candidate)
	if err != nil {
		return IngestionResult{}, err
	}
	r.ingestion.current = candidate
	r.maybeEnrichPublished(ctx, candidate)
	return ingestionResult(execution, r.config, candidate), nil
}

// GetSourceStatus returns only the authorized configured source's path-free status.
func (r *Runtime) GetSourceStatus(ctx context.Context, request SourceStatusRequest) (SourceStatus, error) {
	if !r.validIngestionContext(ctx, request.IngestionContext) {
		return SourceStatus{}, ErrInvalid
	}
	if err := r.authorizeIngestion(ctx, request.IngestionContext, "source.status"); err != nil {
		return SourceStatus{}, err
	}
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.closed || r.ingestion == nil {
		return SourceStatus{}, ErrDenied
	}
	if err := r.restoreIngestionLocked(ctx, request.Identity); err != nil {
		return SourceStatus{}, err
	}
	if r.ingestion.revoked || r.ingestion.current == nil {
		return SourceStatus{}, ErrDenied
	}
	return sourceStatus(r.config, r.ingestion.current), nil
}

// ReconcileSource publishes an exact fenced commit transition while retaining
// the previous searchable generation until the SQLite publication commits.
func (r *Runtime) ReconcileSource(ctx context.Context, request ReconcileSourceRequest) (IngestionResult, error) {
	if !r.validIngestionContext(ctx, request.IngestionContext) || !validOpaque(request.IdempotencyKey) ||
		!validHexDigest(request.ExpectedGenerationID) || !validGitOID(request.ExpectedCommitOID) ||
		!validGitOID(request.TargetCommitOID) {
		return IngestionResult{}, ErrInvalid
	}
	if err := r.authorizeIngestion(ctx, request.IngestionContext, "source.reconcile"); err != nil {
		return IngestionResult{}, err
	}
	r.ingestionWork.Lock()
	defer r.ingestionWork.Unlock()
	r.ingestionMu.Lock()
	if r.closed || r.ingestion == nil || r.ingestion.revoked {
		r.ingestionMu.Unlock()
		return IngestionResult{}, ErrDenied
	}
	if err := r.restoreIngestionLocked(ctx, request.Identity); err != nil {
		r.ingestionMu.Unlock()
		return IngestionResult{}, err
	}
	base := r.ingestion.current
	if base == nil || (base.generation.Sequence == 1 &&
		(base.generation.ID != request.ExpectedGenerationID || base.generation.CommitOID != request.ExpectedCommitOID)) ||
		(base.generation.Sequence != 1 && base.generation.Sequence != 2) {
		r.ingestionMu.Unlock()
		return IngestionResult{}, ErrDenied
	}
	if base.generation.Sequence == 2 {
		if base.generation.ExpectedPreviousID != request.ExpectedGenerationID ||
			base.previousCommitOID != request.ExpectedCommitOID ||
			base.generation.CommitOID != request.TargetCommitOID {
			r.ingestionMu.Unlock()
			return IngestionResult{}, ErrDenied
		}
		execution, err := r.publishSource(
			ctx, request.IngestionContext, "source.reconcile", request.IdempotencyKey, base,
		)
		r.ingestionMu.Unlock()
		if err != nil {
			return IngestionResult{}, err
		}
		return ingestionResult(execution, r.config, base), nil
	}
	r.ingestionMu.Unlock()
	encoded, err := r.ingestion.authority.MarshalBinary()
	if err != nil {
		return IngestionResult{}, ErrDenied
	}
	candidateAuthority, err := ingestion.Restore(ctx, r.ingestion.config, encoded)
	if err != nil {
		return IngestionResult{}, collapseIngestionError(ctx, err)
	}
	generation, err := candidateAuthority.Reconcile(ctx, ingestion.ReconcileRequest{
		ExpectedGenerationID: request.ExpectedGenerationID, ExpectedCommitOID: request.ExpectedCommitOID,
		TargetCommitOID: request.TargetCommitOID, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return IngestionResult{}, collapseIngestionError(ctx, err)
	}
	candidate, err := r.buildPublishedSource(ctx, candidateAuthority, generation, base)
	if err != nil {
		return IngestionResult{}, err
	}
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.closed || r.ingestion.revoked || r.ingestion.current != base {
		return IngestionResult{}, ErrDenied
	}
	execution, err := r.publishSource(
		ctx, request.IngestionContext, "source.reconcile", request.IdempotencyKey, candidate,
	)
	if err != nil {
		return IngestionResult{}, err
	}
	r.ingestion.authority = candidateAuthority
	r.ingestion.previous = base
	r.ingestion.current = candidate
	r.maybeEnrichPublished(ctx, candidate)
	return ingestionResult(execution, r.config, candidate), nil
}

// maybeEnrichPublished best-effort builds ontology + gardener jobs from
// published file texts (product SOTA path). Failures are ignored so Stage 03
// ingestion never fails closed on enrichment.
func (r *Runtime) maybeEnrichPublished(ctx context.Context, candidate *publishedSource) {
	if r == nil || candidate == nil || len(candidate.files) == 0 {
		return
	}
	docs := make(map[string]string, len(candidate.files))
	for path, file := range candidate.files {
		if len(file.Content) == 0 {
			continue
		}
		// Cap body size for gardener budgets.
		text := string(file.Content)
		text = textbound.Bytes(text, 8_000)
		docs[path] = text
	}
	if len(docs) == 0 {
		return
	}
	_, _ = r.EnrichGeneration(ctx, candidate.generation.ID, docs)
}

// ConfiguredIngestionSourceID returns the opaque identity of the sole
// bootstrap-approved ingestion source. It remains stable after revocation so
// the gateway can authenticate exact retries without accepting caller-chosen
// source scope. An empty result means ingestion is not configured.
func (r *Runtime) ConfiguredIngestionSourceID() string {
	if r == nil {
		return ""
	}
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.ingestion == nil {
		return ""
	}
	return r.ingestion.scope.SourceID
}

func (r *Runtime) validIngestionContext(ctx context.Context, request IngestionContext) bool {
	return ctx != nil && r != nil && validIdentity(request.Identity) && request.Authorize != nil &&
		request.ConfigurationDigest == r.config && request.Policy == IngestionPolicyBothIgnoreNoFollow &&
		request.Fence > 0
}

func (r *Runtime) authorizeIngestion(ctx context.Context, request IngestionContext, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	decision, err := request.Authorize(ctx, request.Identity, action, r.brain)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if err != nil || !decision.Allowed {
		return ErrDenied
	}
	return nil
}

func (r *Runtime) restoreIngestionLocked(ctx context.Context, identity Identity) error {
	if r.ingestion.current != nil || r.ingestion.revoked {
		return nil
	}
	checkpoint, err := r.store.LoadIngestionCheckpoint(ctx, localstate.IngestionCheckpointQuery{
		Identity: identity, Scope: r.ingestion.scope,
	})
	if errors.Is(err, localstate.ErrInvalidInput) {
		return nil
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrDenied
	}
	if checkpoint.ConfigurationDigest != r.ingestion.config.ConfigurationDigest ||
		checkpoint.RepositoryID != r.ingestion.config.RepositoryID ||
		checkpoint.ApprovedRootID != r.ingestion.authority.Status().ApprovedRootID {
		return ErrDenied
	}
	if checkpoint.Revoked || checkpoint.Tombstoned {
		r.ingestion.revoked = true
		r.ingestion.revokedGeneration = checkpoint.GenerationID
		// A revoked source still rebuilds its immutable generations and their
		// projections: every authorization checkpoint denies eligibility, while
		// an already admitted query keeps its pinned freshness and coverage
		// truthful, so the public outcome shape never leaks the revocation.
	}
	if checkpoint.GenerationSequence == 1 {
		generation, err := r.admitCheckpointGeneration(ctx, checkpoint.CommitOID, checkpoint.GenerationID)
		if err != nil || !matchesCheckpoint(generation, checkpoint) {
			return restartError(ctx, err)
		}
		candidate, err := r.buildPublishedSource(ctx, r.ingestion.authority, generation, nil)
		if err != nil {
			return err
		}
		r.ingestion.current = candidate
		return nil
	}
	if checkpoint.GenerationSequence != 2 {
		return ErrDenied
	}
	previous, err := r.admitCheckpointGeneration(ctx, checkpoint.PreviousCommitOID, checkpoint.PreviousGenerationID)
	if err != nil || previous.ID != checkpoint.PreviousGenerationID ||
		previous.CommitOID != checkpoint.PreviousCommitOID || previous.Sequence != 1 ||
		previous.SourceID != checkpoint.Scope.SourceID {
		return restartError(ctx, err)
	}
	base, err := r.buildPublishedSource(ctx, r.ingestion.authority, previous, nil)
	if err != nil {
		return err
	}
	generation, err := r.ingestion.authority.Reconcile(ctx, ingestion.ReconcileRequest{
		ExpectedGenerationID: previous.ID, ExpectedCommitOID: previous.CommitOID,
		TargetCommitOID: checkpoint.CommitOID, IdempotencyKey: "restart:" + checkpoint.GenerationID,
	})
	if err != nil || !matchesCheckpoint(generation, checkpoint) || generation.ExpectedPreviousID != previous.ID {
		return restartError(ctx, err)
	}
	candidate, err := r.buildPublishedSource(ctx, r.ingestion.authority, generation, base)
	if err != nil {
		return err
	}
	// The superseded generation stays projected alongside the current one so a
	// pinned stale query resolves canonical evidence exactly as published.
	r.ingestion.previous = base
	r.ingestion.current = candidate
	return nil
}

func (r *Runtime) admitCheckpointGeneration(ctx context.Context, commitOID, generationID string) (ingestion.Generation, error) {
	return r.ingestion.authority.Admit(ctx, ingestion.Admission{
		ExpectedCommitOID: commitOID, IdempotencyKey: "restart:" + generationID,
	})
}

func matchesCheckpoint(generation ingestion.Generation, checkpoint localstate.IngestionCheckpoint) bool {
	return generation.ID == checkpoint.GenerationID && generation.Sequence == checkpoint.GenerationSequence &&
		generation.SourceID == checkpoint.Scope.SourceID && generation.SnapshotID == checkpoint.SnapshotID &&
		generation.CommitOID == checkpoint.CommitOID && generation.TreeOID == checkpoint.TreeOID &&
		generation.Manifest.PolicyDigest == checkpoint.PolicyDigest && generation.Manifest.Digest == checkpoint.SnapshotDigest
}

func restartError(ctx context.Context, err error) error {
	if err != nil {
		return collapseIngestionError(ctx, err)
	}
	return ErrDenied
}

func (r *Runtime) buildPublishedSource(
	ctx context.Context,
	authority *ingestion.Authority,
	generation ingestion.Generation,
	base *publishedSource,
) (*publishedSource, error) {
	hydrated, err := authority.HydrateCurrent(ctx, ingestion.HydrationRequest{
		ExpectedGenerationID: generation.ID, MaxFiles: r.ingestion.config.MaxFiles,
		MaxTotalBytes: r.ingestion.config.MaxTotalBytes,
	})
	if err != nil {
		return nil, collapseIngestionError(ctx, err)
	}
	files := make(map[string]ingestion.HydratedFile, len(hydrated))
	for _, file := range hydrated {
		files[file.Revision.Path] = file
	}
	var snapshot codeindex.Snapshot
	if base == nil {
		snapshot, err = codeindex.Build(ctx, p5Sources(files), r.ingestion.limits)
	} else {
		snapshot, err = codeindex.Apply(ctx, base.index, indexChanges(generation.Delta, base.files, files), r.ingestion.limits)
	}
	if err != nil {
		return nil, collapseIngestionError(ctx, err)
	}
	candidate := &publishedSource{
		generation: generation, index: snapshot, files: files, readiness: readiness(snapshot),
	}
	if base != nil {
		candidate.previousCommitOID = base.generation.CommitOID
	}
	return candidate, nil
}

func (r *Runtime) publishSource(
	ctx context.Context,
	request IngestionContext,
	action string,
	idempotencyKey string,
	candidate *publishedSource,
) (localstate.IngestionExecution, error) {
	if err := r.authorizeIngestion(ctx, request, action); err != nil {
		return localstate.IngestionExecution{}, err
	}
	publication := generationPublication(r.ingestion, request, idempotencyKey, candidate)
	publication.Command.AuthenticatedDigest = localstate.GenerationPublicationDigest(publication)
	execution, err := r.store.PublishGeneration(ctx, publication)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return localstate.IngestionExecution{}, contextErr
		}
		return localstate.IngestionExecution{}, ErrDenied
	}
	return execution, nil
}

func generationPublication(
	runtime *ingestionRuntime,
	request IngestionContext,
	idempotencyKey string,
	candidate *publishedSource,
) localstate.GenerationPublication {
	generation := candidate.generation
	status := runtime.authority.Status()
	publication := localstate.GenerationPublication{
		Scope: runtime.scope,
		Source: localstate.IngestionSourceMetadata{
			RepositoryID: runtime.config.RepositoryID, ConfigurationDigest: runtime.config.ConfigurationDigest,
			IgnorePolicyDigest: generation.Manifest.PolicyDigest, ApprovedRootID: status.ApprovedRootID,
			ACLEpoch: request.Fence,
		},
		Snapshot: localstate.IngestionSnapshotMetadata{
			SnapshotID: generation.SnapshotID, CommitOID: generation.CommitOID, TreeOID: generation.TreeOID,
			PolicyDigest: generation.Manifest.PolicyDigest, SnapshotDigest: generation.Manifest.Digest,
		},
		GenerationID: generation.ID, Sequence: generation.Sequence,
		ExpectedCurrentGenerationID: generation.ExpectedPreviousID,
		State:                       "ready", SourceWatermark: generation.Sequence,
		Revisions: revisionMetadata(generation, candidate.files), Readiness: candidate.readiness,
	}
	for _, lane := range candidate.readiness {
		if lane.Coverage == "lexical_degraded" {
			publication.State = "degraded"
			break
		}
	}
	publication.Command = shared.CommandRecord{
		Command: shared.Identifier{Namespace: "command", Value: boundedIdentity("ingestion-publish", idempotencyKey)},
		Tenant:  request.Identity.Tenant, Principal: request.Identity.Principal, Session: request.Identity.Session,
		CommandType: localstate.IngestionPublishCommand, IdempotencyKey: idempotencyKey, Fence: request.Fence,
	}
	return publication
}

func revisionMetadata(
	generation ingestion.Generation,
	files map[string]ingestion.HydratedFile,
) []localstate.IngestionRevisionMetadata {
	revisions := make([]localstate.IngestionRevisionMetadata, 0, len(generation.Manifest.Files))
	for _, revision := range generation.Manifest.Files {
		language, _ := languageForPath(revision.Path)
		mediaType := "text/plain"
		if revision.Kind == ingestion.EntrySymlink {
			mediaType = "inode/symlink"
		}
		if _, exists := files[revision.Path]; !exists {
			continue
		}
		revisions = append(revisions, localstate.IngestionRevisionMetadata{
			RevisionID:     revision.RevisionID,
			SourceObjectID: boundedIdentity("source-object", revision.PathDigest),
			PathDigest:     revision.PathDigest, GitBlobOID: revision.BlobOID,
			ContentDigest: revision.ContentDigest, ByteLength: revision.SizeBytes,
			EntryKind: string(revision.Kind), MediaType: mediaType, Language: string(language),
		})
	}
	return revisions
}

func p5Sources(files map[string]ingestion.HydratedFile) []codeindex.SourceFile {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	sources := make([]codeindex.SourceFile, 0, len(paths))
	for _, path := range paths {
		language, ok := languageForPath(path)
		if !ok || files[path].Revision.Kind != ingestion.EntryFile {
			continue
		}
		sources = append(sources, codeindex.SourceFile{Path: path, Language: language, Content: files[path].Content})
	}
	return sources
}

func indexChanges(
	delta []ingestion.Change,
	base map[string]ingestion.HydratedFile,
	target map[string]ingestion.HydratedFile,
) []codeindex.Change {
	changes := make([]codeindex.Change, 0, len(delta)*2)
	for _, change := range delta {
		_, oldP5 := languageForPath(change.OldPath)
		newLanguage, newP5 := languageForPath(change.NewPath)
		_, oldExists := base[change.OldPath]
		newFile, newExists := target[change.NewPath]
		oldP5 = oldP5 && oldExists && base[change.OldPath].Revision.Kind == ingestion.EntryFile
		newP5 = newP5 && newExists && newFile.Revision.Kind == ingestion.EntryFile
		source := codeindex.SourceFile{Path: change.NewPath, Language: newLanguage, Content: newFile.Content}
		switch change.Kind {
		case ingestion.ChangeAdd, ingestion.ChangeModify:
			if newP5 {
				changes = append(changes, codeindex.Change{Kind: codeindex.ChangeUpsert, File: source})
			}
		case ingestion.ChangeDelete:
			if oldP5 {
				changes = append(changes, codeindex.Change{Kind: codeindex.ChangeDelete, OldPath: change.OldPath})
			}
		case ingestion.ChangeRename:
			switch {
			case oldP5 && newP5:
				changes = append(changes, codeindex.Change{Kind: codeindex.ChangeRename, OldPath: change.OldPath, File: source})
			case oldP5:
				changes = append(changes, codeindex.Change{Kind: codeindex.ChangeDelete, OldPath: change.OldPath})
			case newP5:
				changes = append(changes, codeindex.Change{Kind: codeindex.ChangeUpsert, File: source})
			}
		}
	}
	return changes
}

func readiness(snapshot codeindex.Snapshot) []localstate.IngestionReadiness {
	degraded := make(map[codeindex.Language]bool, len(p5LanguageOrder))
	for _, file := range snapshot.Files {
		degraded[file.Language] = degraded[file.Language] || file.Coverage == codeindex.CoverageLexicalDegraded
	}
	result := make([]localstate.IngestionReadiness, 0, len(p5LanguageOrder))
	for _, language := range p5LanguageOrder {
		lane := localstate.IngestionReadiness{Language: string(language), Coverage: "syntax_aware"}
		if degraded[language] {
			lane.Coverage, lane.ReasonCode = "lexical_degraded", "malformed_source"
		}
		result = append(result, lane)
	}
	return result
}

func languageForPath(path string) (codeindex.Language, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return codeindex.LanguageGo, true
	case ".ts", ".tsx":
		return codeindex.LanguageTypeScript, true
	case ".py":
		return codeindex.LanguagePython, true
	case ".rs":
		return codeindex.LanguageRust, true
	case ".java":
		return codeindex.LanguageJava, true
	default:
		return "", false
	}
}

func sourceStatus(config Digest, current *publishedSource) SourceStatus {
	readiness := make([]LanguageReadiness, len(current.readiness))
	for index, lane := range current.readiness {
		readiness[index] = LanguageReadiness(lane)
	}
	return SourceStatus{
		SourceID: current.generation.SourceID, GenerationID: current.generation.ID, SnapshotID: current.generation.SnapshotID,
		CommitOID: current.generation.CommitOID, TreeOID: current.generation.TreeOID,
		PolicyDigest: Digest{Algorithm: "sha256", Hex: current.generation.Manifest.PolicyDigest}, Sequence: current.generation.Sequence,
		State: generationState(current.readiness), Readiness: readiness, ConfigurationDigest: config,
	}
}

func generationState(readiness []localstate.IngestionReadiness) string {
	for _, lane := range readiness {
		if lane.Coverage == "lexical_degraded" {
			return "degraded"
		}
	}
	return "ready"
}

func ingestionResult(execution localstate.IngestionExecution, config Digest, current *publishedSource) IngestionResult {
	return IngestionResult{Receipt: execution.Receipt, Status: sourceStatus(config, current), Replayed: execution.Replayed}
}

func boundedIdentity(prefix, value string) string {
	digest := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + ":" + hex.EncodeToString(digest[:])
}

func validOpaque(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value
}

func validHexDigest(value string) bool { return len(value) == 64 && lowerHex(value) }

func validGitOID(value string) bool { return (len(value) == 40 || len(value) == 64) && lowerHex(value) }

func lowerHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func collapseIngestionError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrDenied
}
