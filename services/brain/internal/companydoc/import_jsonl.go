package companydoc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// JSONLRow is one line of an import feed (ERB-compatible subset).
type JSONLRow struct {
	DocumentID  string   `json:"document_id"`
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Title       string   `json:"title"`
	SourceTypes []string `json:"source_types"`
	Source      string   `json:"source"`
}

// ImportJSONL reads newline-delimited JSON documents into a Batch.
func ImportJSONL(r io.Reader, sourceID, generationID string) (Batch, error) {
	sc := bufio.NewScanner(r)
	// Large enterprise docs
	buf := make([]byte, 0, 1024*64)
	sc.Buffer(buf, 1024*1024*8)
	var docs []Document
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row JSONLRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return Batch{}, fmt.Errorf("companydoc: line %d: %w", lineNo, err)
		}
		id := row.DocumentID
		if id == "" {
			id = row.ID
		}
		if id == "" {
			return Batch{}, fmt.Errorf("companydoc: line %d missing document_id", lineNo)
		}
		docs = append(docs, Document{
			ID:          id,
			Title:       row.Title,
			Text:        row.Text,
			Source:      row.Source,
			SourceTypes: row.SourceTypes,
			MimeType:    "text/plain",
			ObservedAt:  time.Now().UTC(),
		})
	}
	if err := sc.Err(); err != nil {
		return Batch{}, err
	}
	b := Batch{
		SourceID:     sourceID,
		GenerationID: generationID,
		Kind:         SourceJSONL,
		Documents:    docs,
		CreatedAt:    time.Now().UTC(),
	}
	if err := ValidateBatch(b); err != nil {
		return Batch{}, err
	}
	return b, nil
}
