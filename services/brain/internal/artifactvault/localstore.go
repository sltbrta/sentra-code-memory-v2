package artifactvault

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type objectStore interface {
	putIfAbsent(context.Context, string, []byte) (bool, error)
	read(context.Context, string) ([]byte, error)
	delete(context.Context, string) error
}

// LocalStore provides immutable create-if-absent, exact read, and durable delete
// semantics beneath ArtifactVault. All object traversal is descriptor-relative.
type LocalStore struct {
	root          *os.Root
	syncDirectory func(*os.Root) error
}

// NewLocalStore creates and opens an owner-only object root. The retained root
// descriptor remains authoritative if path ancestors are subsequently replaced.
func NewLocalStore(path string) (*LocalStore, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("artifactvault: create object root: %w", err)
	}
	requested, err := os.Lstat(path)
	if err != nil || !requested.IsDir() || requested.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalid
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: resolve object root: %w", err)
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: inspect object root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalid
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		return nil, fmt.Errorf("artifactvault: secure object root: %w", err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: open object root: %w", err)
	}
	return &LocalStore{root: root, syncDirectory: syncRoot}, nil
}

// Close releases the retained object-root descriptor.
func (s *LocalStore) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

func (s *LocalStore) putIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(data) == 0 || len(data) > maxObjectBytes {
		return false, ErrInvalid
	}
	shard, name, err := objectName(key)
	if err != nil {
		return false, err
	}
	shardRoot, err := s.openShard(shard, true)
	if err != nil {
		return false, err
	}
	defer shardRoot.Close()
	tempName, temp, err := createPartial(shardRoot)
	if err != nil {
		return false, err
	}
	cleanupPartial := func() error {
		closeErr := temp.Close()
		removeErr := shardRoot.Remove(tempName)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(closeErr, removeErr, s.syncDirectory(shardRoot))
	}
	if _, err := temp.Write(data); err != nil {
		return false, errors.Join(fmt.Errorf("artifactvault: write partial object: %w", err), cleanupPartial())
	}
	if err := temp.Sync(); err != nil {
		return false, errors.Join(fmt.Errorf("artifactvault: sync partial object: %w", err), cleanupPartial())
	}
	if err := temp.Close(); err != nil {
		cleanupErr := errors.Join(removeIfPresent(shardRoot, tempName), s.syncDirectory(shardRoot))
		return false, errors.Join(fmt.Errorf("artifactvault: close partial object: %w", err), cleanupErr)
	}
	if err := shardRoot.Link(tempName, name); err != nil {
		cleanupErr := errors.Join(removeIfPresent(shardRoot, tempName), s.syncDirectory(shardRoot))
		if !errors.Is(err, os.ErrExist) {
			return false, errors.Join(fmt.Errorf("artifactvault: publish immutable object: %w", err), cleanupErr)
		}
		existing, readErr := readBoundedObject(shardRoot, name)
		if readErr != nil {
			return false, errors.Join(fmt.Errorf("artifactvault: compare immutable object: %w", readErr), cleanupErr)
		}
		if !bytes.Equal(existing, data) {
			return false, errors.Join(ErrConflict, cleanupErr)
		}
		return false, cleanupErr
	}
	if err := shardRoot.Remove(tempName); err != nil {
		cleanupErr := errors.Join(removeIfPresent(shardRoot, name), s.syncDirectory(shardRoot))
		return false, errors.Join(fmt.Errorf("artifactvault: remove partial object: %w", err), cleanupErr)
	}
	if err := s.syncDirectory(shardRoot); err != nil {
		cleanupErr := errors.Join(removeIfPresent(shardRoot, name), s.syncDirectory(shardRoot))
		return false, errors.Join(err, cleanupErr)
	}
	return true, nil
}

func (s *LocalStore) read(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	shard, name, err := objectName(key)
	if err != nil {
		return nil, err
	}
	shardRoot, err := s.openShard(shard, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrIncomplete
		}
		return nil, err
	}
	defer shardRoot.Close()
	data, err := readBoundedObject(shardRoot, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrIncomplete
	}
	if err != nil {
		return nil, fmt.Errorf("artifactvault: read immutable object: %w", err)
	}
	return data, nil
}

func (s *LocalStore) delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	shard, name, err := objectName(key)
	if err != nil {
		return err
	}
	shardRoot, err := s.openShard(shard, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer shardRoot.Close()
	if err := shardRoot.Remove(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.syncDirectory(shardRoot)
		}
		return fmt.Errorf("artifactvault: delete immutable object: %w", err)
	}
	return s.syncDirectory(shardRoot)
}

func (s *LocalStore) openShard(name string, create bool) (*os.Root, error) {
	before, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := s.root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("artifactvault: create object shard: %w", err)
		}
		if err := s.syncDirectory(s.root); err != nil {
			return nil, err
		}
		before, err = s.root.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalid
	}
	shard, err := s.root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: open object shard: %w", err)
	}
	opened, openErr := shard.Stat(".")
	after, afterErr := s.root.Lstat(name)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = shard.Close()
		return nil, ErrInvalid
	}
	return shard, nil
}

func objectName(key string) (string, string, error) {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != key {
		return "", "", ErrInvalid
	}
	return key[:2], key[2:], nil
}

func objectKey(record GenerationRecord, frame uint32) string {
	digest := sha256.Sum256([]byte(encodeComposite(
		record.Manifest.Tenant.Value,
		record.Manifest.Artifact.Value,
		uintString(record.Manifest.Generation),
		record.Locator,
		uintString(uint64(frame)),
	)))
	return hex.EncodeToString(digest[:])
}

func createPartial(root *os.Root) (string, *os.File, error) {
	for range 4 {
		randomName := make([]byte, 16)
		if _, err := rand.Read(randomName); err != nil {
			return "", nil, fmt.Errorf("artifactvault: name partial object: %w", err)
		}
		name := ".object-" + hex.EncodeToString(randomName) + ".part"
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("artifactvault: create partial object: %w", err)
		}
		return name, file, nil
	}
	return "", nil, ErrConflict
}

func readBoundedObject(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrCorrupt
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, openErr := file.Stat()
	after, afterErr := root.Lstat(name)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return nil, ErrCorrupt
	}
	if opened.Size() <= 0 || opened.Size() > int64(maxObjectBytes) {
		return nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxObjectBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxObjectBytes {
		clear(data)
		return nil, ErrCorrupt
	}
	return data, nil
}

func removeIfPresent(root *os.Root, name string) error {
	err := root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("artifactvault: open object directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("artifactvault: sync object directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("artifactvault: close object directory: %w", err)
	}
	return nil
}
