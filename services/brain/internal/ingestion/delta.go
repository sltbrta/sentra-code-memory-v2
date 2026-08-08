package ingestion

import "context"

type renameKey struct {
	blobOID string
	mode    string
}

func deriveDelta(ctx context.Context, previous, next []FileRevision) ([]Change, error) {
	removed := make([]FileRevision, 0)
	added := make([]FileRevision, 0)
	modified := make([]Change, 0)
	oldIndex, newIndex := 0, 0
	for oldIndex < len(previous) || newIndex < len(next) {
		if err := checkDeltaContext(ctx, oldIndex+newIndex); err != nil {
			return nil, err
		}
		switch {
		case oldIndex == len(previous):
			added = append(added, next[newIndex])
			newIndex++
		case newIndex == len(next):
			removed = append(removed, previous[oldIndex])
			oldIndex++
		case previous[oldIndex].Path < next[newIndex].Path:
			removed = append(removed, previous[oldIndex])
			oldIndex++
		case previous[oldIndex].Path > next[newIndex].Path:
			added = append(added, next[newIndex])
			newIndex++
		default:
			oldFile, newFile := previous[oldIndex], next[newIndex]
			if oldFile.RevisionID != newFile.RevisionID {
				modified = append(modified, Change{
					Kind: ChangeModify, OldPath: oldFile.Path, NewPath: newFile.Path,
					OldID: oldFile.RevisionID, NewID: newFile.RevisionID,
				})
			}
			oldIndex++
			newIndex++
		}
	}

	addedByContent := make(map[renameKey][]int, len(added))
	for index, file := range added {
		if err := checkDeltaContext(ctx, index); err != nil {
			return nil, err
		}
		key := renameKey{blobOID: file.BlobOID, mode: file.Mode}
		addedByContent[key] = append(addedByContent[key], index)
	}
	usedAdds := make([]bool, len(added))
	nextByContent := make(map[renameKey]int, len(addedByContent))
	deleted := make([]Change, 0, len(removed))
	renamed := make([]Change, 0, len(removed))
	for index, oldFile := range removed {
		if err := checkDeltaContext(ctx, index); err != nil {
			return nil, err
		}
		key := renameKey{blobOID: oldFile.BlobOID, mode: oldFile.Mode}
		candidates := addedByContent[key]
		candidateIndex := nextByContent[key]
		if candidateIndex < len(candidates) {
			addedIndex := candidates[candidateIndex]
			nextByContent[key] = candidateIndex + 1
			usedAdds[addedIndex] = true
			newFile := added[addedIndex]
			renamed = append(renamed, Change{
				Kind: ChangeRename, OldPath: oldFile.Path, NewPath: newFile.Path,
				OldID: oldFile.RevisionID, NewID: newFile.RevisionID,
			})
			continue
		}
		deleted = append(deleted, Change{Kind: ChangeDelete, OldPath: oldFile.Path, OldID: oldFile.RevisionID})
	}

	changes := make([]Change, 0, len(added)+len(removed)+len(modified))
	for index, newFile := range added {
		if err := checkDeltaContext(ctx, index); err != nil {
			return nil, err
		}
		if !usedAdds[index] {
			changes = append(changes, Change{Kind: ChangeAdd, NewPath: newFile.Path, NewID: newFile.RevisionID})
		}
	}
	// The source slices are path-sorted. Appending lexicographically ordered
	// change kinds preserves the frozen deterministic delta order without a
	// second O(n log n) sort.
	changes = append(changes, deleted...)
	changes = append(changes, modified...)
	changes = append(changes, renamed...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return changes, nil
}

func checkDeltaContext(ctx context.Context, index int) error {
	if index&255 != 0 {
		return nil
	}
	return ctx.Err()
}

func equalFiles(left, right []FileRevision) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneGeneration(generation Generation) Generation {
	generation.Manifest.Files = append([]FileRevision(nil), generation.Manifest.Files...)
	generation.Delta = append([]Change(nil), generation.Delta...)
	return generation
}
