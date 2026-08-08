package localauthority

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type searchCursor struct {
	GenerationID string     `json:"generation_id"`
	Query        string     `json:"query"`
	Kind         SearchKind `json:"kind"`
	Offset       int        `json:"offset"`
	Checksum     string     `json:"checksum"`
}

// SearchCode returns a stable bounded page of exact P5 occurrences from the
// requested current generation. Authorization happens before source state is read.
func (r *Runtime) SearchCode(ctx context.Context, request SearchCodeRequest) (SearchCodeResult, error) {
	if !r.validIngestionContext(ctx, request.IngestionContext) || !validHexDigest(request.GenerationID) ||
		strings.TrimSpace(request.Query) == "" || len(request.Query) > 512 || request.Limit == 0 ||
		request.Limit > 100 || !validSearchKind(request.Kind) || len(request.Cursor) > 2048 {
		return SearchCodeResult{}, ErrInvalid
	}
	if err := r.authorizeIngestion(ctx, request.IngestionContext, "source.search"); err != nil {
		return SearchCodeResult{}, err
	}
	r.ingestionMu.Lock()
	if r.closed || r.ingestion == nil {
		r.ingestionMu.Unlock()
		return SearchCodeResult{}, ErrDenied
	}
	if err := r.restoreIngestionLocked(ctx, request.Identity); err != nil {
		r.ingestionMu.Unlock()
		return SearchCodeResult{}, err
	}
	r.ingestionMu.Unlock()

	r.ingestionMu.RLock()
	defer r.ingestionMu.RUnlock()
	current := r.ingestion.current
	if r.closed || r.ingestion.revoked || current == nil || current.generation.ID != request.GenerationID {
		return SearchCodeResult{}, ErrDenied
	}
	offset, err := decodeSearchCursor(request)
	if err != nil {
		return SearchCodeResult{}, ErrInvalid
	}
	matches := collectMatches(current, request.Query, request.Kind)
	if offset > len(matches) {
		return SearchCodeResult{}, ErrInvalid
	}
	end := offset + int(request.Limit)
	if end > len(matches) {
		end = len(matches)
	}
	result := SearchCodeResult{GenerationID: request.GenerationID, Matches: matches[offset:end]}
	if end < len(matches) {
		result.NextCursor = encodeSearchCursor(request, end)
	}
	return result, nil
}

// RevokeSource immediately denies the in-memory source before atomically
// tombstoning the durable pointer and revisions. A failed durable write remains
// fail-closed in this process and can be retried with the exact command.
func (r *Runtime) RevokeSource(ctx context.Context, request RevokeSourceRequest) (IngestionResult, error) {
	if !r.validIngestionContext(ctx, request.IngestionContext) || !validOpaque(request.IdempotencyKey) ||
		!validHexDigest(request.ExpectedGenerationID) || request.RevocationEpoch == 0 {
		return IngestionResult{}, ErrInvalid
	}
	if err := r.authorizeIngestion(ctx, request.IngestionContext, "source.revoke"); err != nil {
		return IngestionResult{}, err
	}
	r.ingestionWork.Lock()
	defer r.ingestionWork.Unlock()
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.closed || r.ingestion == nil {
		return IngestionResult{}, ErrDenied
	}
	if err := r.restoreIngestionLocked(ctx, request.Identity); err != nil {
		return IngestionResult{}, err
	}
	current := r.ingestion.current
	if r.ingestion.revoked {
		if r.ingestion.revokedGeneration != request.ExpectedGenerationID {
			return IngestionResult{}, ErrDenied
		}
	} else {
		if current == nil || current.generation.ID != request.ExpectedGenerationID {
			return IngestionResult{}, ErrDenied
		}
		if err := r.ingestion.authority.Revoke(ctx, ingestion.RevokeRequest{
			ExpectedGenerationID: request.ExpectedGenerationID, IdempotencyKey: request.IdempotencyKey,
		}); err != nil {
			return IngestionResult{}, collapseIngestionError(ctx, err)
		}
		r.ingestion.revoked = true
		r.ingestion.revokedGeneration = request.ExpectedGenerationID
	}
	revocation := localstate.IngestionRevocation{
		Scope: r.ingestion.scope, ExpectedCurrentGenerationID: request.ExpectedGenerationID,
		RevocationEpoch: request.RevocationEpoch, ReasonCode: "source_revoked",
	}
	revocation.Command = shared.CommandRecord{
		Command: shared.Identifier{Namespace: "command", Value: boundedIdentity("ingestion-revoke", request.IdempotencyKey)},
		Tenant:  request.Identity.Tenant, Principal: request.Identity.Principal, Session: request.Identity.Session,
		CommandType: localstate.IngestionRevokeCommand, IdempotencyKey: request.IdempotencyKey, Fence: request.Fence,
	}
	revocation.Command.AuthenticatedDigest = localstate.IngestionRevocationDigest(revocation)
	execution, err := r.store.RevokeIngestionSource(ctx, revocation)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return IngestionResult{}, contextErr
		}
		return IngestionResult{}, ErrDenied
	}
	// The revoked flag denies every Stage 03 read and every query authorization
	// checkpoint, while the retained published source keeps an already admitted
	// query's pinned freshness and coverage truthful; immutable generation
	// facts are never rewritten by revocation.
	return IngestionResult{
		Receipt: execution.Receipt, Replayed: execution.Replayed,
		Status: SourceStatus{Revoked: true, ConfigurationDigest: r.config},
	}, nil
}

func collectMatches(current *publishedSource, query string, kind SearchKind) []CodeMatch {
	result := make([]CodeMatch, 0)
	for _, file := range current.index.Files {
		hydrated := current.files[file.Path]
		for _, occurrence := range file.Occurrences {
			if occurrence.Text != query || !matchesKind(occurrence.Kind, kind) {
				continue
			}
			result = append(result, CodeMatch{
				Path: file.Path, BlobOID: hydrated.Revision.BlobOID,
				ContentDigest: occurrence.ContentDigest, RevisionID: hydrated.Revision.RevisionID,
				SourceObjectID: boundedIdentity("source-object", hydrated.Revision.PathDigest),
				ByteLength:     uint64(hydrated.Revision.SizeBytes), MediaType: sourceMediaType(file.Language),
				Language: string(file.Language), Coverage: string(occurrence.Coverage), Kind: kind,
				StartLine: occurrence.Range.Start.Line, StartColumn: occurrence.Range.Start.Column,
				EndLine: occurrence.Range.End.Line, EndColumn: occurrence.Range.End.Column,
				Content: occurrence.Text,
			})
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Path != result[right].Path {
			return result[left].Path < result[right].Path
		}
		if result[left].StartLine != result[right].StartLine {
			return result[left].StartLine < result[right].StartLine
		}
		return result[left].StartColumn < result[right].StartColumn
	})
	return result
}

func sourceMediaType(language codeindex.Language) string {
	switch language {
	case codeindex.LanguageGo:
		return "text/x-go"
	case codeindex.LanguageTypeScript:
		return "text/typescript"
	case codeindex.LanguagePython:
		return "text/x-python"
	case codeindex.LanguageRust:
		return "text/x-rust"
	case codeindex.LanguageJava:
		return "text/x-java"
	default:
		return "text/plain"
	}
}

func validSearchKind(kind SearchKind) bool {
	return kind == SearchExact || kind == SearchSymbol || kind == SearchReference
}

func matchesKind(kind codeindex.Kind, requested SearchKind) bool {
	switch requested {
	case SearchExact:
		return true
	case SearchSymbol:
		return kind == codeindex.KindDefinition
	case SearchReference:
		return kind == codeindex.KindReference
	default:
		return false
	}
}

func encodeSearchCursor(request SearchCodeRequest, offset int) string {
	cursor := searchCursor{
		GenerationID: request.GenerationID, Query: request.Query, Kind: request.Kind, Offset: offset,
	}
	cursor.Checksum = cursorChecksum(cursor)
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeSearchCursor(request SearchCodeRequest) (int, error) {
	if request.Cursor == "" {
		return 0, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil {
		return 0, ErrInvalid
	}
	var cursor searchCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil || cursor.Offset < 0 ||
		cursor.GenerationID != request.GenerationID || cursor.Query != request.Query || cursor.Kind != request.Kind ||
		cursor.Checksum != cursorChecksum(cursor) {
		return 0, ErrInvalid
	}
	return cursor.Offset, nil
}

func cursorChecksum(cursor searchCursor) string {
	digest := sha256.Sum256([]byte(cursor.GenerationID + "\x00" + cursor.Query + "\x00" +
		string(cursor.Kind) + "\x00" + decimal(cursor.Offset)))
	return hex.EncodeToString(digest[:])
}

func decimal(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
