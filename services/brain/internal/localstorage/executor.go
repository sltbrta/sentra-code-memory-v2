package localstorage

import (
	"context"
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
)

func readResult[T any](ctx context.Context, authority *localstate.Store, read func(queryer) (T, error)) (T, error) {
	var result T
	err := authority.ReadMetadata(ctx, func(reader localstate.MetadataReader) error {
		var readErr error
		result, readErr = read(reader)
		return readErr
	})
	if errors.Is(err, localstate.ErrInvalidInput) {
		return result, ErrUnavailable
	}
	return result, err
}

func writeResult[T any](ctx context.Context, authority *localstate.Store, write func(localstate.MetadataWriter) (T, error)) (T, error) {
	var result T
	err := authority.WriteMetadata(ctx, func(writer localstate.MetadataWriter) error {
		var writeErr error
		result, writeErr = write(writer)
		return writeErr
	})
	if errors.Is(err, localstate.ErrInvalidInput) {
		return result, ErrUnavailable
	}
	return result, err
}

func writeOnly(ctx context.Context, authority *localstate.Store, write func(localstate.MetadataWriter) error) error {
	_, err := writeResult(ctx, authority, func(writer localstate.MetadataWriter) (struct{}, error) {
		return struct{}{}, write(writer)
	})
	return err
}
