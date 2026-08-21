package hosted

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-006. countJSONLLines used bufio.Scanner's 64 KiB default bound while every
// reader in the same file raises its bound to 8 MiB, and it swallowed the
// scanner's error. One oversized record therefore returned a short count --
// often zero -- which is the input to the compaction threshold. Compaction
// never fired, so the delta grew without bound and the base was never
// rewritten: the file that could not be read was the reason it was never
// cleaned up.
//
// Nothing wrote a line over 64 KiB, so reverting the fix left the suite green.

func writeJSONL(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chunks.delta.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonLine(id, body string) string {
	return `{"chunk_id":"` + id + `","op":"upsert","text":"` + body + `"}`
}

// TestCountJSONLLinesCountsRecordsLargerThanTheScannerDefault covers the
// common case: a document well past 64 KiB but within the 8 MiB bound the
// readers use. Under the old default this line ended the scan and the records
// after it were never counted.
func TestCountJSONLLinesCountsRecordsLargerThanTheScannerDefault(t *testing.T) {
	big := strings.Repeat("x", 512*1024)
	path := writeJSONL(t, []string{
		jsonLine("a", "small"),
		jsonLine("b", big),
		jsonLine("c", "small"),
		jsonLine("d", "small"),
	})
	if got := countJSONLLines(path); got != 4 {
		t.Fatalf("count = %d, want 4: a record past the scanner's default bound "+
			"truncated the count, and the count is what decides compaction", got)
	}
}

// TestCountJSONLLinesForcesCompactionOnAnUnreadableLine covers the other half:
// a record past even the raised bound. Returning the short count would keep
// compaction from ever firing on the file that needs it most; returning the
// threshold guarantees the next flush rewrites the base and drops the delta.
func TestCountJSONLLinesForcesCompactionOnAnUnreadableLine(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~8 MiB")
	}
	huge := strings.Repeat("y", maxJSONLLineBytes+1024)
	path := writeJSONL(t, []string{jsonLine("a", "small"), jsonLine("b", huge)})

	got := countJSONLLines(path)
	if got < compactDeltaLines {
		t.Fatalf("count = %d, want at least the compaction threshold %d: an "+
			"unreadable delta must be compacted away, not left to grow",
			got, compactDeltaLines)
	}
}

// TestCountJSONLLinesIgnoresBlankLinesAndMissingFiles pins the ordinary
// behaviour the two cases above sit on top of.
func TestCountJSONLLinesIgnoresBlankLinesAndMissingFiles(t *testing.T) {
	path := writeJSONL(t, []string{jsonLine("a", "one"), "", "   ", jsonLine("b", "two")})
	if got := countJSONLLines(path); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	if got := countJSONLLines(filepath.Join(t.TempDir(), "absent.jsonl")); got != 0 {
		t.Fatalf("a missing file counted %d, want 0", got)
	}
}
