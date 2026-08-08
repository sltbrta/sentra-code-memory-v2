package query

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// This file rebuilds the frozen Stage 03 mixed-P5 corpus in memory exactly the
// way tests/stage-04/contracts/fixture_test.go expands it, then projects both
// generations through the real Stage 03 codeindex so engine tests run against
// the same occurrence evidence the conformance suite validates.

const (
	testSourceID  = "source:fixture"
	testTenant    = "tenant:test"
	testPrincipal = "principal:test"
	testSession   = "session:test"
)

var testNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

type deltaOperation struct {
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	OldPath   string `json:"oldPath"`
	NewPath   string `json:"newPath"`
	Malformed bool   `json:"malformed"`
}

type deltaManifest struct {
	SchemaVersion string           `json:"schemaVersion"`
	PathCounting  string           `json:"pathCounting"`
	Operations    []deltaOperation `json:"operations"`
}

type groundingCase struct {
	CaseID            string             `json:"caseId"`
	Category          string             `json:"category"`
	Query             string             `json:"query"`
	PinnedGeneration  string             `json:"pinnedGeneration"`
	Freshness         string             `json:"freshness"`
	Interference      string             `json:"interference"`
	ExpectedStatus    string             `json:"expectedStatus"`
	ExpectedCitations []expectedCitation `json:"expectedCitations"`
	ExpectedReasons   []string           `json:"expectedReasons"`
	Note              string             `json:"note"`
}

type expectedCitation struct {
	Path        string `json:"path"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	EndLine     int    `json:"endLine"`
	EndColumn   int    `json:"endColumn"`
}

type groundingManifest struct {
	SchemaVersion string          `json:"schemaVersion"`
	Corpus        string          `json:"corpus"`
	Cases         []groundingCase `json:"cases"`
}

func loadGroundingCases(t *testing.T) groundingManifest {
	t.Helper()
	encoded := readTestFixture(t, "tests/fixtures/stage-04/grounding/query-cases.json")
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var manifest groundingManifest
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode grounding cases: %v", err)
	}
	if manifest.SchemaVersion != "ouroboros.stage04.grounding-cases.v1" || len(manifest.Cases) != 12 {
		t.Fatalf("unexpected grounding manifest: %#v", manifest.SchemaVersion)
	}
	return manifest
}

func loadDeltaManifest(t *testing.T) deltaManifest {
	t.Helper()
	encoded := readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/delta-manifest.json")
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var manifest deltaManifest
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode delta manifest: %v", err)
	}
	if manifest.SchemaVersion != "ouroboros.stage03.mixed-p5-delta.v1" || len(manifest.Operations) != 100 {
		t.Fatalf("unexpected delta manifest: %#v", manifest.SchemaVersion)
	}
	return manifest
}

func readTestFixture(t *testing.T, repositoryRelative string) []byte {
	t.Helper()
	candidates := []string{
		repositoryRelative,
		filepath.Join("..", "..", "..", "..", repositoryRelative),
	}
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		candidates = append([]string{filepath.Join(root, os.Getenv("TEST_WORKSPACE"), repositoryRelative)}, candidates...)
	}
	for _, candidate := range candidates {
		content, err := os.ReadFile(candidate)
		if err == nil {
			return content
		}
	}
	t.Fatalf("fixture %q unavailable through %v", repositoryRelative, candidates)
	return nil
}

// fixtureCorpus is an in-memory Corpus holding both frozen generations.
type fixtureCorpus struct {
	snapshots map[string]Snapshot
	currentID string
}

func (c *fixtureCorpus) Snapshot(_ context.Context, sourceID, generationID string) (Snapshot, error) {
	if sourceID != testSourceID {
		return Snapshot{}, ErrUnknownScope
	}
	snapshot, exists := c.snapshots[generationID]
	if !exists {
		return Snapshot{}, ErrUnknownScope
	}
	return snapshot, nil
}

func (c *fixtureCorpus) CurrentGeneration(_ context.Context, sourceID string) (GenerationPin, error) {
	if sourceID != testSourceID {
		return GenerationPin{}, ErrUnknownScope
	}
	return GenerationPin{SourceID: sourceID, GenerationID: c.currentID}, nil
}

// buildFixtureCorpus expands both frozen generations and projects each through
// the real Stage 03 codeindex, mirroring the localauthority published source.
func buildFixtureCorpus(t *testing.T) *fixtureCorpus {
	t.Helper()
	delta := loadDeltaManifest(t)
	seeds := map[string]string{
		"go":         string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/go/seed.go")),
		"typescript": string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/typescript/seed.ts")),
		"python":     string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/python/seed.py")),
		"rust":       string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/rust/seed.rs.fixture")),
		"java":       string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/java/Seed.java")),
	}
	malformed := string(readTestFixture(t, "tests/fixtures/stage-03/mixed-p5/malformed/unterminated.ts"))
	expand := func(generation string) map[string]fixtureFile {
		files := make(map[string]fixtureFile)
		for _, operation := range delta.Operations {
			var path, phase string
			switch generation {
			case "stale":
				switch operation.Kind {
				case "modify", "delete", "rename":
					path, phase = operation.OldPath, "base"
				default:
					continue
				}
			case "current":
				switch operation.Kind {
				case "add":
					path, phase = operation.NewPath, "added"
				case "modify":
					path, phase = operation.OldPath, "modified"
				case "rename":
					path, phase = operation.NewPath, "base"
				default:
					continue
				}
			default:
				t.Fatalf("unknown generation %q", generation)
			}
			seed := seeds[operation.Language]
			if operation.Malformed {
				seed = malformed
			}
			files[path] = fixtureFile{
				Path:     path,
				Language: operation.Language,
				Content:  expandFixtureContent(seed, operation.Language, path, phase),
			}
		}
		return files
	}
	stale := expand("stale")
	current := expand("current")
	corpus := &fixtureCorpus{snapshots: make(map[string]Snapshot)}
	for name, files := range map[string]map[string]fixtureFile{"stale": stale, "current": current} {
		snapshot := buildSnapshot(t, name, files)
		corpus.snapshots[snapshot.GenerationID] = snapshot
		if name == "current" {
			corpus.currentID = snapshot.GenerationID
		}
	}
	return corpus
}

type fixtureFile struct {
	Path     string
	Language string
	Content  string
}

// expandFixtureContent mirrors the generatedLines recipe frozen by the Stage 04
// contract conformance suite: seed lines, one empty line, then the fixture
// provenance comment.
func expandFixtureContent(seed, language, path, phase string) string {
	comment := "//"
	if language == "python" {
		comment = "#"
	}
	lines := strings.Split(strings.TrimSuffix(seed, "\n"), "\n")
	lines = append(lines, "", fmt.Sprintf("%s fixture=%s phase=%s", comment, path, phase))
	return strings.Join(lines, "\n")
}

func buildSnapshot(t *testing.T, name string, files map[string]fixtureFile) Snapshot {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	sources := make([]codeindex.SourceFile, 0, len(paths))
	for _, path := range paths {
		sources = append(sources, codeindex.SourceFile{
			Path:     path,
			Language: codeindex.Language(files[path].Language),
			Content:  []byte(files[path].Content),
		})
	}
	projection, err := codeindex.Build(context.Background(), sources, codeindex.DefaultLimits())
	if err != nil {
		t.Fatalf("project %s generation: %v", name, err)
	}
	commitOID := testGitOID("commit-" + name)
	generationID := testHexDigest("generation-" + name)
	revisions := make([]ingestion.FileRevision, 0, len(paths))
	hydrated := make(map[string]ingestion.HydratedFile, len(paths))
	for _, path := range paths {
		content := []byte(files[path].Content)
		revision := ingestion.FileRevision{
			Path:          path,
			PathDigest:    testHexDigest("path:" + path),
			Kind:          ingestion.EntryFile,
			Mode:          "100644",
			SizeBytes:     int64(len(content)),
			BlobOID:       gitBlobOID(content),
			ContentDigest: testContentDigest(content),
			RevisionID:    testHexDigest("revision:" + name + ":" + path),
		}
		revisions = append(revisions, revision)
		hydrated[path] = ingestion.HydratedFile{Revision: revision, Content: content}
	}
	readiness, state := snapshotReadiness(projection)
	sequence := uint64(1)
	if name == "current" {
		sequence = 2
	}
	return Snapshot{
		GenerationID: generationID,
		Sequence:     sequence,
		CommitOID:    commitOID,
		TreeOID:      testGitOID("tree-" + name),
		State:        state,
		Readiness:    readiness,
		Revisions:    revisions,
		Projection: ProjectionView{
			State: ProjectionReady,
			Index: &projection,
			Files: hydrated,
		},
	}
}

func snapshotReadiness(projection codeindex.Snapshot) ([]LaneReadiness, GenerationState) {
	degraded := make(map[codeindex.Language]bool)
	for _, file := range projection.Files {
		degraded[file.Language] = degraded[file.Language] || file.Coverage == codeindex.CoverageLexicalDegraded
	}
	readiness := make([]LaneReadiness, 0, 5)
	state := GenerationReady
	for _, language := range []codeindex.Language{
		codeindex.LanguageGo, codeindex.LanguageTypeScript, codeindex.LanguagePython,
		codeindex.LanguageRust, codeindex.LanguageJava,
	} {
		lane := LaneReadiness{Language: string(language), Coverage: "syntax_aware"}
		if degraded[language] {
			lane.Coverage = "lexical_degraded"
			lane.ReasonCode = "malformed_source"
			state = GenerationDegraded
		}
		readiness = append(readiness, lane)
	}
	return readiness, state
}

func testHexDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func testContentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func testGitOID(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

// stubAuthorizer replays scripted per-action decisions.
type stubAuthorizer struct {
	epoch uint64
	deny  map[Action]bool
	errOn map[Action]bool
	calls []Action
}

func (s *stubAuthorizer) Authorize(_ context.Context, _ Principal, action Action, _ string) (Decision, error) {
	s.calls = append(s.calls, action)
	if s.errOn[action] {
		return Decision{}, errors.New("authorizer unavailable")
	}
	if s.deny[action] {
		return Decision{Allowed: false, Epoch: s.epoch}, nil
	}
	return Decision{Allowed: true, Epoch: s.epoch}, nil
}

type stubClock struct{ now time.Time }

func (s stubClock) Now() time.Time { return s.now }

func newTestEngine(corpus Corpus, authorizer Authorizer, synthesizer Synthesizer) *Engine {
	engine, err := NewEngine(Config{
		Corpus:                        corpus,
		Authorizer:                    authorizer,
		Synthesizer:                   synthesizer,
		Clock:                         stubClock{now: testNow},
		Limits:                        DefaultLimits(),
		AllowLegacyUnadmittedEvidence: true,
	})
	if err != nil {
		panic(err)
	}
	return engine
}

func fixtureQuery(caseID, generationID, text, freshness string) Query {
	return Query{
		QueryID:        "query-" + caseID,
		Principal:      Principal{Tenant: testTenant, Principal: testPrincipal, Session: testSession},
		SourceID:       testSourceID,
		GenerationID:   generationID,
		Text:           text,
		Freshness:      FreshnessRequirement(freshness),
		IdempotencyKey: "idempotency-" + caseID,
	}
}
