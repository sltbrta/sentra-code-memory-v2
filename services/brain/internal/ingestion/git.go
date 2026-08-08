package ingestion

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxConfiguredFiles              = 100_000
	maxConfiguredPathBytes          = 4_096
	maxConfiguredFileBytes    int64 = 16 << 20
	maxConfiguredTotalBytes   int64 = 64 << 20
	maxBufferedGitOutputBytes int64 = 128 << 20
)

type gitEntry struct {
	path string
	mode string
	oid  string
	size int64
}

type snapshot struct {
	id       string
	commit   string
	tree     string
	manifest Manifest
}

func (a *Authority) readSnapshot(ctx context.Context, commitOID string) (snapshot, error) {
	scanned, _, err := a.readSnapshotAndBlobs(ctx, commitOID)
	return scanned, err
}

func (a *Authority) readSnapshotAndBlobs(ctx context.Context, commitOID string) (snapshot, map[string][]byte, error) {
	if ctx == nil || !isGitOID(commitOID) {
		return snapshot{}, nil, ErrInvalidInput
	}
	resolved, err := a.runGit(ctx, 256, "rev-parse", "--verify", commitOID+"^{commit}")
	if err != nil {
		return snapshot{}, nil, err
	}
	if strings.TrimSpace(string(resolved)) != commitOID {
		return snapshot{}, nil, ErrGit
	}
	treeOutput, err := a.runGit(ctx, 256, "rev-parse", "--verify", commitOID+"^{tree}")
	if err != nil {
		return snapshot{}, nil, err
	}
	treeOID := strings.TrimSpace(string(treeOutput))
	if !isGitOID(treeOID) {
		return snapshot{}, nil, ErrGit
	}
	outputLimit := int64(a.config.MaxFiles)*(int64(a.config.MaxPathBytes)+160) + 1
	listing, err := a.runGit(ctx, outputLimit, "ls-tree", "-r", "-z", "-l", "--full-tree", treeOID)
	if err != nil {
		return snapshot{}, nil, err
	}
	entries, err := parseTree(listing, a.config)
	if err != nil {
		return snapshot{}, nil, err
	}
	ignoreBytes, err := a.readIgnoreFiles(ctx, entries)
	if err != nil {
		return snapshot{}, nil, err
	}
	policy, err := parseIgnorePolicy(a.config.Policy, ignoreBytes, a.config.MaxPathBytes)
	if err != nil {
		return snapshot{}, nil, err
	}
	included, totalBytes, err := filterEntries(entries, policy, a.config)
	if err != nil {
		return snapshot{}, nil, err
	}
	blobs, err := a.readBlobs(ctx, included, totalBytes)
	if err != nil {
		return snapshot{}, nil, err
	}
	snapshotID := identity("ouroboros.stage03.snapshot.v1", a.sourceID, commitOID, treeOID, policy.digest)
	files := make([]FileRevision, 0, len(included))
	for _, entry := range included {
		contents, ok := blobs[entry.oid]
		if !ok {
			return snapshot{}, nil, ErrGit
		}
		kind := EntryFile
		if entry.mode == "120000" {
			kind = EntrySymlink
		}
		pathDigest := digest(entry.path)
		files = append(files, FileRevision{
			Path:          entry.path,
			PathDigest:    pathDigest,
			Kind:          kind,
			Mode:          entry.mode,
			SizeBytes:     entry.size,
			BlobOID:       entry.oid,
			ContentDigest: digestBytes(contents),
			RevisionID:    identity("ouroboros.stage03.source-revision.v1", a.sourceID, pathDigest, entry.oid, entry.mode),
		})
	}
	manifestDigest := identity(
		"ouroboros.stage03.snapshot-manifest.v1",
		a.config.TenantID,
		a.config.BrainID,
		a.sourceID,
		snapshotID,
		treeOID,
		policy.digest,
	)
	return snapshot{
		id:     snapshotID,
		commit: commitOID,
		tree:   treeOID,
		manifest: Manifest{
			Digest:       manifestDigest,
			PolicyDigest: policy.digest,
			Files:        files,
		},
	}, blobs, nil
}

func parseTree(output []byte, config Config) ([]gitEntry, error) {
	records := bytes.Split(output, []byte{0})
	entries := make([]gitEntry, 0, len(records)-1)
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, ErrGit
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 4 || fields[1] != "blob" || !isGitOID(fields[2]) {
			return nil, fmt.Errorf("tree entry kind: %w", ErrUnsupportedPolicy)
		}
		if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
			return nil, fmt.Errorf("tree entry mode: %w", ErrUnsupportedPolicy)
		}
		entryPath := string(name)
		if err := validateRepositoryPath(entryPath, config.MaxPathBytes); err != nil {
			return nil, err
		}
		base := path.Base(entryPath)
		if strings.Contains(entryPath, "/") && (base == ".gitignore" || base == ".ouroborosignore") {
			return nil, fmt.Errorf("nested ignore file: %w", ErrUnsupportedPolicy)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, ErrGit
		}
		entries = append(entries, gitEntry{path: entryPath, mode: fields[0], oid: fields[2], size: size})
		if len(entries) > config.MaxFiles {
			return nil, ErrLimit
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func (a *Authority) readIgnoreFiles(ctx context.Context, entries []gitEntry) (map[string][]byte, error) {
	wanted := make([]gitEntry, 0, 2)
	var total int64
	for _, entry := range entries {
		if entry.path == ".gitignore" && a.config.Policy.UseGitIgnore ||
			entry.path == ".ouroborosignore" && a.config.Policy.UseOuroborosIgnore {
			if entry.mode == "120000" {
				return nil, fmt.Errorf("symlink ignore file: %w", ErrUnsupportedPolicy)
			}
			if entry.size > a.config.MaxFileBytes || total > a.config.MaxTotalBytes-entry.size {
				return nil, ErrLimit
			}
			total += entry.size
			wanted = append(wanted, entry)
		}
	}
	bytesByOID, err := a.readBlobs(ctx, wanted, total)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte, len(wanted))
	for _, entry := range wanted {
		result[entry.path] = bytesByOID[entry.oid]
	}
	return result, nil
}

func filterEntries(entries []gitEntry, policy ignorePolicy, config Config) ([]gitEntry, int64, error) {
	included := make([]gitEntry, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if policy.ignores(entry.path) {
			continue
		}
		if entry.size > config.MaxFileBytes || total > config.MaxTotalBytes-entry.size {
			return nil, 0, ErrLimit
		}
		total += entry.size
		included = append(included, entry)
	}
	return included, total, nil
}

func (a *Authority) readBlobs(ctx context.Context, entries []gitEntry, totalBytes int64) (map[string][]byte, error) {
	unique := make(map[string]int64, len(entries))
	for _, entry := range entries {
		unique[entry.oid] = entry.size
	}
	oids := make([]string, 0, len(unique))
	for oid := range unique {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	if len(oids) == 0 {
		return map[string][]byte{}, nil
	}
	input := strings.Join(oids, "\n") + "\n"
	output, err := a.runGitInput(ctx, totalBytes+int64(len(oids))*160+1, input, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(output))
	result := make(map[string][]byte, len(oids))
	for _, expectedOID := range oids {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, ErrGit
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != expectedOID || fields[1] != "blob" {
			return nil, ErrGit
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size != unique[expectedOID] || size > a.config.MaxFileBytes {
			return nil, ErrGit
		}
		contents := make([]byte, size)
		if _, err := io.ReadFull(reader, contents); err != nil {
			return nil, ErrGit
		}
		terminator, err := reader.ReadByte()
		if err != nil || terminator != '\n' {
			return nil, ErrGit
		}
		result[expectedOID] = contents
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return nil, ErrGit
	}
	return result, nil
}

func (a *Authority) runGit(ctx context.Context, maxOutput int64, args ...string) ([]byte, error) {
	return a.runGitInput(ctx, maxOutput, "", args...)
}

func (a *Authority) runGitInput(ctx context.Context, maxOutput int64, input string, args ...string) ([]byte, error) {
	if !a.approvedRootUnchanged() {
		return nil, ErrOutOfRoot
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.config.CommandTimeout)
	defer cancel()
	baseArgs := []string{
		"--no-optional-locks",
		"--no-replace-objects",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-C", a.config.ApprovedRoot,
	}
	cmd := exec.CommandContext(commandCtx, a.config.GitExecutable, append(baseArgs, args...)...)
	cmd.Env = []string{
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
	cmd.Stdin = strings.NewReader(input)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, ErrGit
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, ErrGit
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if int64(len(output)) > maxOutput {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, ErrLimit
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrGit
	}
	if err := cmd.Wait(); err != nil {
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, ErrGit
	}
	if !a.approvedRootUnchanged() {
		return nil, ErrOutOfRoot
	}
	return output, nil
}

func (a *Authority) approvedRootUnchanged() bool {
	if a == nil || a.approvedRootInfo == nil {
		return false
	}
	current, err := os.Lstat(a.config.ApprovedRoot)
	return err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 &&
		os.SameFile(a.approvedRootInfo, current)
}

func validateRepositoryPath(value string, maxBytes int) error {
	if len(value) > maxBytes {
		return ErrLimit
	}
	if value == "" || !utf8.ValidString(value) || path.IsAbs(value) ||
		strings.Contains(value, "\\") || path.Clean(value) != value {
		return ErrOutOfRoot
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrOutOfRoot
		}
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return ErrOutOfRoot
		}
	}
	return nil
}

func validateConfig(config Config) (Config, error) {
	treeOutputLimit := int64(config.MaxFiles)*(int64(config.MaxPathBytes)+160) + 1
	if !filepath.IsAbs(config.ApprovedRoot) || !filepath.IsAbs(config.GitExecutable) ||
		!validIdentifier(config.TenantID) || !validIdentifier(config.BrainID) ||
		!validIdentifier(config.RepositoryID) ||
		!isDigest(config.ConfigurationDigest) || config.Policy.Symlinks != RecordWithoutFollow ||
		config.CommandTimeout <= 0 || config.CommandTimeout > 10*time.Minute ||
		config.MaxFiles <= 0 || config.MaxFiles > maxConfiguredFiles ||
		config.MaxPathBytes <= 0 || config.MaxPathBytes > maxConfiguredPathBytes ||
		config.MaxFileBytes <= 0 || config.MaxFileBytes > maxConfiguredFileBytes ||
		config.MaxTotalBytes <= 0 || config.MaxTotalBytes > maxConfiguredTotalBytes ||
		config.MaxFileBytes > config.MaxTotalBytes || treeOutputLimit > maxBufferedGitOutputBytes ||
		config.MaxIdempotencyRecords <= 0 || config.MaxIdempotencyRecords > 1_000_000 {
		return Config{}, ErrInvalidInput
	}
	rootInfo, err := os.Lstat(config.ApprovedRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return Config{}, ErrInvalidInput
	}
	resolvedRoot, err := filepath.EvalSymlinks(config.ApprovedRoot)
	if err != nil {
		return Config{}, ErrInvalidInput
	}
	resolvedGit, err := filepath.EvalSymlinks(config.GitExecutable)
	if err != nil {
		return Config{}, ErrInvalidInput
	}
	gitInfo, err := os.Stat(resolvedGit)
	if err != nil || !gitInfo.Mode().IsRegular() || gitInfo.Mode()&0o111 == 0 {
		return Config{}, ErrInvalidInput
	}
	config.ApprovedRoot = filepath.Clean(resolvedRoot)
	config.GitExecutable = filepath.Clean(resolvedGit)
	return config, nil
}

func isGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value) && value == strings.ToLower(value)
}

func isDigest(value string) bool {
	return len(value) == sha256.Size*2 && isGitOID(value)
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func newIdentityHasher(domain string) hash.Hash {
	hasher := sha256.New()
	writeIdentityField(hasher, domain)
	return hasher
}

func finishIdentity(hasher hash.Hash) string {
	return hex.EncodeToString(hasher.Sum(nil))
}

func identity(domain string, fields ...string) string {
	hasher := newIdentityHasher(domain)
	for _, field := range fields {
		writeIdentityField(hasher, field)
	}
	return finishIdentity(hasher)
}

func digest(value string) string {
	return digestBytes([]byte(value))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
