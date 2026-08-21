package hosted

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

// contentprivacy ran nowhere. It is the only PII and secret redaction in the
// product, and no ingest path called it, so sensitive content was written
// verbatim to chunks.jsonl, the HotLex index, the dense store and the memory
// cortex while guard.go documented guarantees nothing provided.

func redactPolicy() contentprivacy.Policy {
	return contentprivacy.Policy{
		ID: "local-ingest", Version: "v1",
		MaxContentBytes: 1 << 20, MaxFindings: 128,
		Scopes: map[contentprivacy.ScopeKind]contentprivacy.ScopePolicy{
			contentprivacy.ScopeCompany: {
				Actions: map[contentprivacy.Class]contentprivacy.Action{
					contentprivacy.ClassEmail:       contentprivacy.ActionRedact,
					contentprivacy.ClassAPIKey:      contentprivacy.ActionRedact,
					contentprivacy.ClassBearerToken: contentprivacy.ActionRedact,
					contentprivacy.ClassSSN:         contentprivacy.ActionRedact,
					contentprivacy.ClassCreditCard:  contentprivacy.ActionRedact,
					contentprivacy.ClassPrivateKey:  contentprivacy.ActionTombstone,
				},
				DetectorFailure: contentprivacy.ActionQuarantine,
				Retention:       24 * time.Hour,
			},
		},
	}
}

func guardedBrain(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	client, err := OpenLocal(dir, "guarded")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	store, err := contentprivacy.OpenFileStateStore(filepath.Join(dir, "privacy"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	guard, err := contentprivacy.NewWithState(redactPolicy(), nil, nil, time.Now, store)
	if err != nil {
		t.Fatal(err)
	}
	client.WithContentPrivacy(guard, contentprivacy.Scope{Kind: contentprivacy.ScopeCompany})
	return client, dir
}

// brainBytes returns everything written under the brain directory, so the
// assertion is "this string is nowhere on disk" rather than "this one file
// does not contain it".
func brainBytes(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		all.Write(raw)
		all.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all.String()
}

func TestIngestedSecretsAreRedactedBeforeReachingDisk(t *testing.T) {
	client, dir := guardedBrain(t)
	const secret = "sk" + "-abcdefghijklmnopqrstuvwxyz012345"
	const email = "alice@example.invalid"

	result, err := client.BurstIngestLocal(context.Background(), []LocalDocument{{
		ID:   "doc-1",
		Text: "the deploy key is " + secret + " and questions go to " + email,
	}}, 1)
	if err != nil {
		t.Fatalf("BurstIngestLocal: %v", err)
	}
	if result.Redacted == 0 {
		t.Fatal("nothing was redacted, so this guard checked nothing")
	}

	onDisk := brainBytes(t, dir)
	if strings.Contains(onDisk, secret) {
		t.Fatal("an API key was written to the brain directory verbatim")
	}
	if strings.Contains(onDisk, email) {
		t.Fatal("an email address was written to the brain directory verbatim")
	}
	// The surrounding text must survive, or this is deletion rather than
	// redaction and retrieval is destroyed with it.
	if !strings.Contains(onDisk, "the deploy key is") {
		t.Fatal("the non-sensitive text did not survive redaction")
	}
}

// TestRedactionReachesTheCortexAndVectorsToo is the half a chunk-level
// redaction would have missed: BurstIngestLocal fans the *documents* out to
// the cortex and the dense store, not the chunks.
func TestRedactionReachesTheCortexAndVectorsToo(t *testing.T) {
	client, _ := guardedBrain(t)
	const secret = "sk" + "-abcdefghijklmnopqrstuvwxyz012345"

	if _, err := client.BurstIngestLocal(context.Background(), []LocalDocument{{
		ID: "doc-1", Text: "rotate " + secret + " before the release",
	}}, 1); err != nil {
		t.Fatal(err)
	}

	for id, text := range client.Mem.DocTexts() {
		if strings.Contains(text, secret) {
			t.Fatalf("the memory cortex holds the raw secret for %s", id)
		}
	}
}

// TestATombstonedDocumentIsWithheldAndNamed keeps a refusal visible. A caller
// that believes it ingested something and did not is the failure this path
// exists to avoid.
func TestATombstonedDocumentIsWithheldAndNamed(t *testing.T) {
	client, dir := guardedBrain(t)
	key := "-----BEGIN " + "PRIVATE KEY-----\nMIIEvQIBADAN\n-----END " + "PRIVATE KEY-----"

	result, err := client.BurstIngestLocal(context.Background(), []LocalDocument{
		{ID: "safe-doc", Text: "ordinary release notes for the quarter"},
		{ID: "key-doc", Text: "deploy with " + key},
	}, 1)
	if err != nil {
		t.Fatalf("BurstIngestLocal: %v", err)
	}
	if _, withheld := result.Withheld["key-doc"]; !withheld {
		t.Fatalf("a tombstoned document was not reported as withheld: %+v", result.Withheld)
	}
	if _, withheld := result.Withheld["safe-doc"]; withheld {
		t.Fatalf("a clean document was withheld: %+v", result.Withheld)
	}

	onDisk := brainBytes(t, dir)
	if strings.Contains(onDisk, "MIIEvQIBADAN") {
		t.Fatal("a tombstoned document's body reached disk")
	}
	if !strings.Contains(onDisk, "ordinary release notes") {
		t.Fatal("the clean document was not indexed")
	}
}

// TestAnUnguardedClientIsUnchanged keeps redaction from being something a
// deployment gets by accident: without a guard the path behaves exactly as it
// did, which is what every existing test depends on.
func TestAnUnguardedClientIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	client, err := OpenLocal(dir, "unguarded")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const email = "alice@example.invalid"
	result, err := client.BurstIngestLocal(context.Background(), []LocalDocument{
		{ID: "doc-1", Text: "questions to " + email},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Redacted != 0 || len(result.Withheld) != 0 {
		t.Fatalf("an unguarded client redacted something: %+v", result)
	}
	if !strings.Contains(brainBytes(t, dir), email) {
		t.Fatal("an unguarded client redacted the text anyway")
	}
}

// TestATombstonedDocumentStaysWithheldAfterARestart joins the two halves: the
// tombstone is durable, so a re-ingest of erased content is refused by a new
// process rather than being admitted because the map was empty.
func TestATombstonedDocumentStaysWithheldAfterARestart(t *testing.T) {
	dir := t.TempDir()
	privacyDir := filepath.Join(dir, "privacy")
	key := "-----BEGIN " + "PRIVATE KEY-----\nMIIEvQIBADAN\n-----END " + "PRIVATE KEY-----"

	ingest := func() map[string]string {
		t.Helper()
		client, err := OpenLocal(dir, "guarded")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		store, err := contentprivacy.OpenFileStateStore(privacyDir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		guard, err := contentprivacy.NewWithState(redactPolicy(), nil, nil, time.Now, store)
		if err != nil {
			t.Fatal(err)
		}
		client.WithContentPrivacy(guard, contentprivacy.Scope{Kind: contentprivacy.ScopeCompany})

		result, _ := client.BurstIngestLocal(context.Background(), []LocalDocument{
			{ID: "key-doc", Text: "deploy with " + key},
			{ID: "safe-doc", Text: "ordinary notes"},
		}, 1)
		return result.Withheld
	}

	if _, withheld := ingest()["key-doc"]; !withheld {
		t.Fatal("the first ingest did not withhold the tombstoned document")
	}
	// Second process, benign content under the same id: the tombstone must
	// still refuse it.
	if _, withheld := ingest()["key-doc"]; !withheld {
		t.Fatal("after a restart the tombstoned id was admitted again: the " +
			"tombstone did not survive, so erased content can be re-ingested")
	}
}
