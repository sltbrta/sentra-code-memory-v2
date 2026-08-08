// product-brain JSONL loaders for docs and chunks.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func loadDocs(path string) ([]hosted.LocalDocument, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var docs []hosted.LocalDocument
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		id, _ := row["document_id"].(string)
		if id == "" {
			id, _ = row["id"].(string)
		}
		title, _ := row["title"].(string)
		text, _ := row["text"].(string)
		if id == "" || text == "" {
			continue
		}
		docs = append(docs, hosted.LocalDocument{ID: id, Title: title, Text: text})
	}
	return docs, nil
}

func loadChunks(path string) ([]hosted.ChunkWrite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var chunks []hosted.ChunkWrite
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		dsid, _ := row["document_id"].(string)
		if dsid == "" {
			dsid, _ = row["dsid"].(string)
		}
		if dsid == "" {
			dsid, _ = row["id"].(string)
		}
		chunkID, _ := row["chunk_id"].(string)
		if chunkID == "" {
			chunkID = fmt.Sprintf("%s#0", dsid)
		}
		text, _ := row["text"].(string)
		if text == "" {
			text, _ = row["text_content"].(string)
		}
		uri, _ := row["source_uri"].(string)
		if dsid == "" || text == "" {
			continue
		}
		chunks = append(chunks, hosted.ChunkWrite{
			DocumentID: dsid,
			ChunkID:    chunkID,
			Text:       text,
			SourceURI:  uri,
		})
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty chunk jsonl")
	}
	return chunks, nil
}
