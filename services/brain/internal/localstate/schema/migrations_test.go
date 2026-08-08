package schema

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationsAreEmbeddedOrderedAndDefensivelyCopied(t *testing.T) {
	first := Migrations()
	if len(first) != 7 || first[0].Version != 1 || first[1].Version != 2 || first[2].Version != 3 ||
		first[3].Version != 4 || first[4].Version != 5 || first[5].Version != 6 || first[6].Version != 7 {
		t.Fatalf("migration order = %#v", first)
	}
	for index, name := range []string{
		"001_stage02_authority.sql",
		"002_durable_storage_adapters.sql",
		"003_stage03_ingestion.sql",
		"004_stage04_conversation.sql",
		"005_stage05_factory.sql",
		"006_stage07_meetings.sql",
		"007_stage11_multimodal.sql",
	} {
		want, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if first[index].SQL != string(want) {
			t.Fatalf("migration %d does not match checked-in SQL", index+1)
		}
	}

	first[0].Version = 99
	first[1].SQL = "mutated"
	first[2].SQL = "mutated"
	first[3].SQL = "mutated"
	first[4].SQL = "mutated"
	first[5].SQL = "mutated"
	first[6].SQL = "mutated"
	second := Migrations()
	if second[0].Version != 1 || second[1].Version != 2 || second[2].Version != 3 || second[3].Version != 4 ||
		second[4].Version != 5 || second[5].Version != 6 || second[6].Version != 7 ||
		second[1].SQL == "mutated" || second[2].SQL == "mutated" || second[3].SQL == "mutated" ||
		second[4].SQL == "mutated" || second[5].SQL == "mutated" || second[6].SQL == "mutated" {
		t.Fatalf("migration descriptors share mutable state: %#v", second)
	}
}
