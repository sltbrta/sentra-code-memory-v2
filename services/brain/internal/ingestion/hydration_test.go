package ingestion_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

func TestHydrateCurrentReadsExactCommittedContentAndReturnsDefensiveCopies(t *testing.T) {
	root, git := newRepository(t, map[string]string{
		"main.go":  "package committed\n",
		"note.txt": "committed note\n",
	})
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	writeFiles(t, root, map[string]string{"main.go": "dirty working tree bytes\n"})

	request := ingestion.HydrationRequest{
		ExpectedGenerationID: generation.ID,
		MaxFiles:             2,
		MaxTotalBytes:        1 << 10,
	}
	first, err := authority.HydrateCurrent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Revision.Path != "main.go" ||
		!bytes.Equal(first[0].Content, []byte("package committed\n")) {
		t.Fatalf("unexpected hydrated records: %#v", first)
	}
	first[0].Revision.Path = "mutated.go"
	first[0].Content[0] = 'X'

	second, err := authority.HydrateCurrent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Revision.Path != "main.go" ||
		!bytes.Equal(second[0].Content, []byte("package committed\n")) {
		t.Fatalf("caller mutation escaped defensive copy: %#v", second[0])
	}
}

func TestHydrateCurrentValidatesCancellationGenerationAndLimits(t *testing.T) {
	root, git := newRepository(t, map[string]string{
		"main.go":  "package main\n",
		"note.txt": "bounded\n",
	})
	config := testConfig(t, root, git)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	valid := ingestion.HydrationRequest{
		ExpectedGenerationID: generation.ID,
		MaxFiles:             2,
		MaxTotalBytes:        1 << 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.HydrateCurrent(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled hydration got %v", err)
	}
	stale := valid
	stale.ExpectedGenerationID = generation.Manifest.Digest
	if _, err := authority.HydrateCurrent(context.Background(), stale); !errors.Is(err, ingestion.ErrStaleGeneration) {
		t.Fatalf("stale hydration got %v", err)
	}
	fileLimited := valid
	fileLimited.MaxFiles = 1
	if _, err := authority.HydrateCurrent(context.Background(), fileLimited); !errors.Is(err, ingestion.ErrLimit) {
		t.Fatalf("file-limited hydration got %v", err)
	}
	byteLimited := valid
	byteLimited.MaxTotalBytes = 1
	if _, err := authority.HydrateCurrent(context.Background(), byteLimited); !errors.Is(err, ingestion.ErrLimit) {
		t.Fatalf("byte-limited hydration got %v", err)
	}
	invalid := valid
	invalid.MaxFiles = 0
	if _, err := authority.HydrateCurrent(context.Background(), invalid); !errors.Is(err, ingestion.ErrInvalidInput) {
		t.Fatalf("invalid hydration got %v", err)
	}
	aboveAuthority := valid
	aboveAuthority.MaxFiles = config.MaxFiles + 1
	if _, err := authority.HydrateCurrent(context.Background(), aboveAuthority); !errors.Is(err, ingestion.ErrLimit) {
		t.Fatalf("authority file limit got %v", err)
	}
	aboveAuthority = valid
	aboveAuthority.MaxTotalBytes = config.MaxTotalBytes + 1
	if _, err := authority.HydrateCurrent(context.Background(), aboveAuthority); !errors.Is(err, ingestion.ErrLimit) {
		t.Fatalf("authority byte limit got %v", err)
	}
}

func TestHydrateCurrentPostScanProcessingDoesNotBlockRevoke(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	ctx := &postScanContext{
		Context: context.Background(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	hydrationResult := make(chan error, 1)
	go func() {
		_, hydrateErr := authority.HydrateCurrent(ctx, ingestion.HydrationRequest{
			ExpectedGenerationID: generation.ID,
			MaxFiles:             1,
			MaxTotalBytes:        1 << 10,
		})
		hydrationResult <- hydrateErr
	}()
	select {
	case <-ctx.started:
	case <-time.After(2 * time.Second):
		t.Fatal("hydration did not reach post-scan processing")
	}

	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- authority.Revoke(context.Background(), ingestion.RevokeRequest{
			ExpectedGenerationID: generation.ID,
			IdempotencyKey:       "revoke-during-hydration-copy",
		})
	}()
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatalf("revoke during hydration processing: %v", err)
		}
		close(ctx.release)
	case <-time.After(time.Second):
		close(ctx.release)
		t.Fatal("post-scan hydration processing held the authority lock")
	}
	if err := <-hydrationResult; !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("hydration returned after revoke: %v", err)
	}
}

type postScanContext struct {
	context.Context
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (c *postScanContext) Err() error {
	callers := make([]uintptr, 8)
	count := runtime.Callers(2, callers)
	frames := runtime.CallersFrames(callers[:count])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".buildHydratedFiles") {
			c.once.Do(func() { close(c.started) })
			<-c.release
			break
		}
		if !more {
			break
		}
	}
	return nil
}

func TestHydrateCurrentRefusesRevokedAndTombstonedSources(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	authority, err := ingestion.New(context.Background(), testConfig(t, root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	request := ingestion.HydrationRequest{
		ExpectedGenerationID: generation.ID,
		MaxFiles:             1,
		MaxTotalBytes:        1 << 10,
	}
	if err := authority.Revoke(context.Background(), ingestion.RevokeRequest{
		ExpectedGenerationID: generation.ID,
		IdempotencyKey:       "revoke-before-hydrate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.HydrateCurrent(context.Background(), request); !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("revoked hydration got %v", err)
	}
	if err := authority.Tombstone(context.Background(), ingestion.TombstoneRequest{
		ExpectedGenerationID: generation.ID,
		IdempotencyKey:       "tombstone-before-hydrate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.HydrateCurrent(context.Background(), request); !errors.Is(err, ingestion.ErrTombstoned) {
		t.Fatalf("tombstoned hydration got %v", err)
	}
}

func TestReconciledCheckpointRestoresAndRejectsTampering(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	config := testConfig(t, root, git)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{
		"main.go":  "package two\n",
		"note.txt": "new\n",
	})
	second, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "checkpoint-reconcile",
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(checkpoint, []byte(root)) || bytes.Contains(checkpoint, []byte("main.go")) {
		t.Fatalf("checkpoint retained ambient path authority: %s", checkpoint)
	}
	restored, err := ingestion.Restore(context.Background(), config, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	current, err := restored.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, second) {
		t.Fatalf("reconciled generation changed across restart: %#v", current)
	}
	rebuilt, err := restored.Rebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rebuilt, second) {
		t.Fatalf("restored rebuild differs from publication: %#v", rebuilt)
	}

	for _, test := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "generation identity", old: second.ID, new: second.Manifest.Digest},
		{name: "tree identity", old: second.TreeOID, new: first.TreeOID},
		{name: "manifest identity", old: second.Manifest.Digest, new: first.Manifest.Digest},
		{name: "previous generation", old: first.ID, new: second.Manifest.Digest},
		{name: "previous commit", old: first.CommitOID, new: second.CommitOID},
	} {
		t.Run(test.name, func(t *testing.T) {
			tampered := bytes.Replace(checkpoint, []byte(test.old), []byte(test.new), 1)
			if bytes.Equal(tampered, checkpoint) {
				t.Fatal("test did not alter checkpoint")
			}
			if _, err := ingestion.Restore(context.Background(), config, tampered); err == nil {
				t.Fatal("tampered checkpoint restored")
			}
		})
	}
	sequenceTamper := bytes.Replace(checkpoint, []byte(`"sequence":2`), []byte(`"sequence":3`), 1)
	if bytes.Equal(sequenceTamper, checkpoint) {
		t.Fatal("sequence test did not alter checkpoint")
	}
	if _, err := ingestion.Restore(context.Background(), config, sequenceTamper); err == nil {
		t.Fatal("sequence-tampered checkpoint restored")
	}
}
