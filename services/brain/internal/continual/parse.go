package continual

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func parseDocLine(line string) (hosted.LocalDocument, error) {
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return hosted.LocalDocument{}, fmt.Errorf("continual: jsonl: %w", err)
	}
	id := strField(row, "id", "document_id", "doc_id")
	text := strField(row, "text", "body", "content")
	title := strField(row, "title", "name")
	uri := strField(row, "source_uri", "uri", "path")
	if id == "" {
		return hosted.LocalDocument{}, fmt.Errorf("continual: jsonl row missing id")
	}
	if text == "" && title == "" {
		return hosted.LocalDocument{}, fmt.Errorf("continual: jsonl row %s empty text", id)
	}
	return hosted.LocalDocument{ID: id, Title: title, Text: text, SourceURI: uri}, nil
}

func strField(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := row[k]; ok && v != nil {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}
