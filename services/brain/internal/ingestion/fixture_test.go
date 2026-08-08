package ingestion_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

type deltaFixture struct {
	SchemaVersion string             `json:"schemaVersion"`
	PathCounting  string             `json:"pathCounting"`
	Operations    []fixtureOperation `json:"operations"`
}

type fixtureOperation struct {
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	OldPath   string `json:"oldPath"`
	NewPath   string `json:"newPath"`
	Malformed bool   `json:"malformed"`
}

func TestFrozenExactly100ChangeFixture(t *testing.T) {
	fixture, encoded := readDeltaFixture(t)
	if fixture.SchemaVersion != "ouroboros.stage03.mixed-p5-delta.v1" || len(fixture.Operations) != 100 {
		t.Fatalf("fixture contract drift: %q, %d records", fixture.SchemaVersion, len(fixture.Operations))
	}
	if fixture.PathCounting != "Each operation is one repository-relative delta record; a rename is one record with oldPath and newPath endpoints. The manifest has 100 records and 125 distinct path endpoints." {
		t.Fatalf("path-counting contract drift: %q", fixture.PathCounting)
	}
	sum := sha256.Sum256(encoded)
	if got := hex.EncodeToString(sum[:]); got != "60e856cce507503be221e757d04346c32a554d28b43726839b7d50546ae16bd2" {
		t.Fatalf("exact fixture records drifted: %s", got)
	}
	distribution := make(map[string]int)
	endpoints := make(map[string]bool)
	malformed := make([]fixtureOperation, 0, 1)
	for _, operation := range fixture.Operations {
		distribution[operation.Language+"/"+operation.Kind]++
		if operation.OldPath != "" {
			endpoints[operation.OldPath] = true
		}
		if operation.NewPath != "" {
			endpoints[operation.NewPath] = true
		}
		if operation.Malformed {
			malformed = append(malformed, operation)
		}
	}
	for _, language := range []string{"go", "typescript", "python", "rust", "java"} {
		for _, kind := range []string{"add", "modify", "rename", "delete"} {
			if distribution[language+"/"+kind] != 5 {
				t.Fatalf("P5 distribution drifted: %s/%s=%d", language, kind, distribution[language+"/"+kind])
			}
		}
	}
	if len(endpoints) != 125 {
		t.Fatalf("endpoint contract drifted: %d", len(endpoints))
	}
	if len(malformed) != 1 || malformed[0].Kind != "modify" || malformed[0].Language != "typescript" ||
		malformed[0].OldPath != "src/typescript/modify-00.ts" || malformed[0].NewPath != "" {
		t.Fatalf("malformed TypeScript fixture drifted: %#v", malformed)
	}
	baseFiles := make(map[string]string)
	for index, operation := range fixture.Operations {
		switch operation.Kind {
		case "modify", "delete":
			baseFiles[operation.OldPath] = fixtureContents(operation, index, "before")
		case "rename":
			baseFiles[operation.OldPath] = fixtureContents(operation, index, "exact")
		case "add":
		default:
			t.Fatalf("unknown fixture operation: %#v", operation)
		}
	}
	root, git := newRepository(t, baseFiles)
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	base := admitHead(t, authority, git, root)
	for index, operation := range fixture.Operations {
		switch operation.Kind {
		case "add":
			writeFiles(t, root, map[string]string{operation.NewPath: fixtureContents(operation, index, "added")})
		case "modify":
			writeFiles(t, root, map[string]string{operation.OldPath: fixtureContents(operation, index, "after")})
		case "rename":
			oldPath := filepath.Join(root, filepath.FromSlash(operation.OldPath))
			newPath := filepath.Join(root, filepath.FromSlash(operation.NewPath))
			if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatal(err)
			}
		case "delete":
			writeFiles(t, root, map[string]string{operation.OldPath: ""})
		}
	}
	target := commitFiles(t, git, root, map[string]string{})
	generation, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: base.ID,
		ExpectedCommitOID:    base.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "frozen-100",
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[ingestion.ChangeKind]int{}
	for _, change := range generation.Delta {
		counts[change.Kind]++
	}
	if len(generation.Delta) != 100 || counts[ingestion.ChangeAdd] != 25 ||
		counts[ingestion.ChangeModify] != 25 || counts[ingestion.ChangeRename] != 25 ||
		counts[ingestion.ChangeDelete] != 25 {
		t.Fatalf("unexpected frozen delta: total=%d counts=%v", len(generation.Delta), counts)
	}
}

func readDeltaFixture(t *testing.T) (deltaFixture, []byte) {
	t.Helper()
	relative := filepath.Join("tests", "fixtures", "stage-03", "mixed-p5", "delta-manifest.json")
	candidates := []string{
		filepath.Join("..", "..", "..", "..", relative),
		relative,
	}
	if testSource := os.Getenv("TEST_SRCDIR"); testSource != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		candidates = append(candidates, filepath.Join(testSource, workspace, relative))
	}
	var encoded []byte
	var err error
	for _, candidate := range candidates {
		encoded, err = os.ReadFile(candidate)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture deltaFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture, encoded
}

func fixtureContents(operation fixtureOperation, index int, version string) string {
	if operation.Malformed {
		return fmt.Sprintf("export function broken%d( { // %s\n", index, version)
	}
	comment := "//"
	if operation.Language == "python" {
		comment = "#"
	}
	return fmt.Sprintf("%s %s %s %03d %s\n", comment, operation.Language, operation.Kind, index, version)
}
