package ingestion

import "context"

// HydrateCurrent returns exact committed content for the fenced current generation.
//
// Git objects are the only content source. The method never reads working-tree
// files, returns caller-owned byte slices, and returns ErrLimit when the current
// generation exceeds request bounds. Revocation and tombstoning deny hydration,
// including when either lifecycle transition wins during the Git scan.
func (a *Authority) HydrateCurrent(ctx context.Context, request HydrationRequest) ([]HydratedFile, error) {
	if err := a.validateHydrationRequest(ctx, request); err != nil {
		return nil, err
	}
	base, err := a.hydrationBase(request)
	if err != nil {
		return nil, err
	}
	scanned, blobs, scanErr := a.readSnapshotAndBlobs(ctx, base.CommitOID)
	var candidate []HydratedFile
	var candidateErr error
	if scanErr == nil {
		candidate, candidateErr = buildHydratedFiles(ctx, scanned, blobs, base)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.denialError(); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if a.current == nil || a.current.ID != base.ID || a.current.CommitOID != base.CommitOID {
		return nil, ErrStaleGeneration
	}
	if candidateErr != nil {
		return nil, candidateErr
	}
	return candidate, nil
}

func (a *Authority) hydrationBase(request HydrationRequest) (Generation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if a.current == nil {
		return Generation{}, ErrInvalidInput
	}
	if a.current.ID != request.ExpectedGenerationID {
		return Generation{}, ErrStaleGeneration
	}
	base := cloneGeneration(*a.current)
	if err := validateHydrationBounds(base.Manifest.Files, request); err != nil {
		return Generation{}, err
	}
	return base, nil
}

func buildHydratedFiles(
	ctx context.Context,
	scanned snapshot,
	blobs map[string][]byte,
	base Generation,
) ([]HydratedFile, error) {
	if !snapshotMatchesGeneration(scanned, base) {
		return nil, ErrGit
	}
	hydrated := make([]HydratedFile, 0, len(scanned.manifest.Files))
	for index, revision := range scanned.manifest.Files {
		if index&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		content, ok := blobs[revision.BlobOID]
		if !ok || int64(len(content)) != revision.SizeBytes || digestBytes(content) != revision.ContentDigest {
			return nil, ErrGit
		}
		hydrated = append(hydrated, HydratedFile{
			Revision: revision,
			Content:  append([]byte(nil), content...),
		})
	}
	return hydrated, nil
}

func (a *Authority) validateHydrationRequest(ctx context.Context, request HydrationRequest) error {
	if ctx == nil || !isDigest(request.ExpectedGenerationID) || request.MaxFiles <= 0 ||
		request.MaxTotalBytes <= 0 {
		return ErrInvalidInput
	}
	if request.MaxFiles > a.config.MaxFiles || request.MaxTotalBytes > a.config.MaxTotalBytes {
		return ErrLimit
	}
	return ctx.Err()
}

func validateHydrationBounds(files []FileRevision, request HydrationRequest) error {
	if len(files) > request.MaxFiles {
		return ErrLimit
	}
	var totalBytes int64
	for _, file := range files {
		if file.SizeBytes < 0 {
			return ErrGit
		}
		if file.SizeBytes > request.MaxTotalBytes-totalBytes {
			return ErrLimit
		}
		totalBytes += file.SizeBytes
	}
	return nil
}

func snapshotMatchesGeneration(scanned snapshot, generation Generation) bool {
	return scanned.id == generation.SnapshotID && scanned.commit == generation.CommitOID &&
		scanned.tree == generation.TreeOID && scanned.manifest.Digest == generation.Manifest.Digest &&
		scanned.manifest.PolicyDigest == generation.Manifest.PolicyDigest &&
		equalFiles(scanned.manifest.Files, generation.Manifest.Files)
}
