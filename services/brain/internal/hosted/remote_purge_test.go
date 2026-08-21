package hosted

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Both remote dense backends returned ErrPurgeUnsupported, so a deployment on
// either could never complete an erasure: the receipt named `vectors` as
// skipped and refused VerifiedComplete, permanently.
//
// The reason given was that shipping an erasure path this repository cannot
// exercise is worse than a named gap. That missed the shape of the fan-out: it
// verifies by re-querying after the delete, so a wrong implementation surfaces
// as a leak -- the ids come back and the receipt says so -- rather than as a
// silent success. Refusing to try leaves a deployment unable to erase; trying,
// against a self-checking fan-out, can only report honestly.
//
// These exercise both against fakes speaking their documented APIs. What is
// not claimed, here or in the ledger, is that either has run against a live
// server -- and the last test is the reason that is tolerable.

// fakeVectorStore records what it was asked to delete and answers counts from
// what it still holds.
type fakeVectorStore struct {
	documents map[string]int
	deletes   int
}

func newFakeVectorStore(docs ...string) *fakeVectorStore {
	store := &fakeVectorStore{documents: map[string]int{}}
	for _, doc := range docs {
		store.documents[doc] = 3 // three chunks each
	}
	return store
}

// qdrantFake speaks the two endpoints the purge uses.
func (s *fakeVectorStore) qdrantFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Filter struct {
				Must []struct {
					Key   string `json:"key"`
					Match struct {
						Any []string `json:"any"`
					} `json:"match"`
				} `json:"must"`
			} `json:"filter"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body.Filter.Must) != 1 || body.Filter.Must[0].Key != "dsid" {
			// A filter on the wrong field would delete nothing in a real
			// collection, so the fake refuses it rather than pretending.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ids := body.Filter.Must[0].Match.Any

		switch {
		case strings.Contains(r.URL.Path, "/points/delete"):
			s.deletes++
			for _, id := range ids {
				delete(s.documents, id)
			}
			_, _ = io.WriteString(w, `{"result":{"status":"completed"}}`)
		case strings.Contains(r.URL.Path, "/points/count"):
			count := 0
			for _, id := range ids {
				count += s.documents[id]
			}
			_, _ = io.WriteString(w, `{"result":{"count":`+strconv.Itoa(count)+`}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// faissFake speaks the sidecar's delete and documents endpoints.
func (s *fakeVectorStore) faissFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			DocumentIDs []string `json:"document_ids"`
		}
		_ = json.Unmarshal(raw, &body)
		switch r.URL.Path {
		case "/delete":
			s.deletes++
			removed := 0
			for _, id := range body.DocumentIDs {
				removed += s.documents[id]
				delete(s.documents, id)
			}
			_, _ = io.WriteString(w, `{"deleted":`+strconv.Itoa(removed)+`}`)
		case "/documents":
			var still []string
			for _, id := range body.DocumentIDs {
				if s.documents[id] > 0 {
					still = append(still, id)
				}
			}
			out, _ := json.Marshal(map[string]any{"document_ids": still})
			_, _ = w.Write(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestQdrantPurgeRemovesAndVerifies(t *testing.T) {
	store := newFakeVectorStore("secret-doc", "public-doc")
	server := store.qdrantFake(t)
	defer server.Close()

	backend := &residualQdrantDense{cfg: Config{
		QdrantURL: server.URL, QdrantAPIKey: "test", ChunkCollection: "chunks",
	}}

	before, err := backend.HasDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("the fixture holds no vectors for the document: %v", before)
	}

	if _, err := backend.DeleteDocuments([]string{"secret-doc"}); err != nil {
		t.Fatalf("DeleteDocuments: %v", err)
	}
	after, err := backend.HasDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("the document still has vectors after the purge: %v", after)
	}
	if surviving, _ := backend.HasDocuments([]string{"public-doc"}); len(surviving) != 1 {
		t.Fatal("purging one document removed another")
	}
}

func TestFaissPurgeRemovesAndVerifies(t *testing.T) {
	store := newFakeVectorStore("secret-doc", "public-doc")
	server := store.faissFake(t)
	defer server.Close()

	backend := openFAISSDense(server.URL)

	removed, err := backend.DeleteDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatalf("DeleteDocuments: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d vectors, want the document's 3 chunks", removed)
	}
	after, err := backend.HasDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("the document still has vectors after the purge: %v", after)
	}
	if surviving, _ := backend.HasDocuments([]string{"public-doc"}); len(surviving) != 1 {
		t.Fatal("purging one document removed another")
	}
}

// TestARemoteThatDoesNotImplementDeleteIsReportedAsAFailure is why
// implementing these is safe despite never having run against a live server.
//
// A sidecar or collection that does not speak the endpoint returns non-2xx.
// That becomes an error, the purge does not claim to have removed anything,
// and the verification pass reports the ids as residual -- so an endpoint this
// repository guessed wrong shows up as an incomplete erasure rather than as a
// successful one.
func TestARemoteThatDoesNotImplementDeleteIsReportedAsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	for name, backend := range map[string]denseBackend{
		"faiss": openFAISSDense(server.URL),
		"qdrant": &residualQdrantDense{cfg: Config{
			QdrantURL: server.URL, QdrantAPIKey: "test", ChunkCollection: "chunks",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.DeleteDocuments([]string{"doc"}); err == nil {
				t.Fatal("a backend that does not implement delete reported success")
			}
			// And the fan-out reads an unanswerable verification as residual,
			// never as verified empty.
			purger := denseVectorPurger{backend: backend}
			if residual := purger.HasDocumentVectors([]string{"doc"}); len(residual) != 1 {
				t.Fatalf("an unanswerable verification was read as empty: %v", residual)
			}
		})
	}
}
