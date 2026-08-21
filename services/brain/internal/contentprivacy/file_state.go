package contentprivacy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
)

// FileStateStore persists the guard's authority beneath a brain directory.
//
// Two append-only logs, fsynced per record. Append-only rather than a rewritten
// snapshot because the failure that matters is losing a tombstone: a rewrite
// interrupted part way can lose records that were already durable, and a lost
// tombstone lets erased content be re-ingested. An append that does not survive
// simply is not there, and the caller is told.
//
// The files are 0600. They hold no content -- a tombstone names an id and a
// reason, and receipts are content-free by construction -- but they do name
// what a tenant erased, which is not something to leave world-readable.
type FileStateStore struct {
	mu             sync.Mutex
	tombstonePath  string
	receiptPath    string
	tombstoneFile  *os.File
	receiptFile    *os.File
	tombstoneCount int
}

const (
	privacyTombstoneFile = "content-privacy-tombstones.jsonl"
	privacyReceiptFile   = "content-privacy-receipts.jsonl"
	privacyStatePerm     = 0o600
	// privacyMaxLineBytes matches the other JSONL readers in this repository,
	// which raise the scanner bound rather than taking the 64 KiB default that
	// silently ends a scan mid-file.
	privacyMaxLineBytes = 8 << 20
)

// OpenFileStateStore opens or creates the authority logs under dir.
func OpenFileStateStore(dir string) (*FileStateStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("contentprivacy: state directory required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("contentprivacy: create state directory: %w", err)
	}
	store := &FileStateStore{
		tombstonePath: filepath.Join(dir, privacyTombstoneFile),
		receiptPath:   filepath.Join(dir, privacyReceiptFile),
	}
	var err error
	if store.tombstoneFile, err = openAppend(store.tombstonePath); err != nil {
		return nil, err
	}
	if store.receiptFile, err = openAppend(store.receiptPath); err != nil {
		_ = store.tombstoneFile.Close()
		return nil, err
	}
	return store, nil
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, privacyStatePerm)
	if err != nil {
		return nil, fmt.Errorf("contentprivacy: open %s: %w", filepath.Base(path), err)
	}
	// An existing file created before this mode was set keeps its old one, so
	// it is narrowed explicitly rather than only on creation.
	if err := f.Chmod(privacyStatePerm); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = f.Close()
		return nil, fmt.Errorf("contentprivacy: restrict %s: %w", filepath.Base(path), err)
	}
	return f, nil
}

// Close releases both logs.
func (s *FileStateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, f := range []*os.File{s.tombstoneFile, s.receiptFile} {
		if f == nil {
			continue
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.tombstoneFile, s.receiptFile = nil, nil
	return firstErr
}

// LoadState reads both logs in append order.
//
// A truncated final line -- the signature of a crash between the write and the
// fsync -- is dropped, because a record that was never durable was never
// promised. A malformed line anywhere else is an error: it means the file has
// been corrupted or edited, and silently skipping it would drop tombstones
// while reporting success.
func (s *FileStateStore) LoadState() ([]Tombstone, []Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tombstones []Tombstone
	if err := readJSONL(s.tombstonePath, func(line []byte, last bool) error {
		var stone Tombstone
		if err := json.Unmarshal(line, &stone); err != nil {
			if last {
				return nil
			}
			return err
		}
		tombstones = append(tombstones, stone)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("contentprivacy: read tombstones: %w", err)
	}

	var receipts []Receipt
	if err := readJSONL(s.receiptPath, func(line []byte, last bool) error {
		var receipt Receipt
		if err := json.Unmarshal(line, &receipt); err != nil {
			if last {
				return nil
			}
			return err
		}
		receipts = append(receipts, receipt)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("contentprivacy: read receipts: %w", err)
	}
	return tombstones, receipts, nil
}

// AppendTombstone writes and fsyncs one tombstone.
func (s *FileStateStore) AppendTombstone(stone Tombstone) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendRecord(s.tombstoneFile, stone); err != nil {
		return fmt.Errorf("contentprivacy: append tombstone: %w", err)
	}
	s.tombstoneCount++
	return nil
}

// AppendReceipt writes and fsyncs one receipt.
func (s *FileStateStore) AppendReceipt(receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendRecord(s.receiptFile, receipt); err != nil {
		return fmt.Errorf("contentprivacy: append receipt: %w", err)
	}
	return nil
}

// appendRecord writes one JSON line and fsyncs it.
//
// The fsync is per record and is the point of the whole type. Buffering would
// make an append that returned success survivable only until the next crash,
// which is the guarantee being replaced rather than a cheaper version of it.
func appendRecord(f *os.File, value any) error {
	if f == nil {
		return errors.New("state store is closed")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readJSONL calls visit for each non-empty line, telling it which line is last.
func readJSONL(path string, visit func(line []byte, last bool) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	var lines [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), privacyMaxLineBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for i, line := range lines {
		if err := visit(line, i == len(lines)-1); err != nil {
			return err
		}
	}
	return nil
}

// compile-time assertions: both stores satisfy the port, and durablefile is
// referenced so the dependency is explicit for readers comparing this with the
// repository's other durable writers. Appends here are not atomic replaces --
// see appendRecord for why.
var (
	_ StateStore = (*FileStateStore)(nil)
	_ StateStore = (*MemoryStateStore)(nil)
	_            = durablefile.DefaultMode
)
