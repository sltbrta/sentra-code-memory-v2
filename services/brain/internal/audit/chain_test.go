package audit

import (
	"errors"
	"testing"
)

func TestVerifyDetectsPayloadAndLinkTampering(t *testing.T) {
	firstMetadata := metadata(1, "event-a", "payload-a")
	first, err := Next(firstMetadata, "")
	if err != nil {
		t.Fatal(err)
	}
	secondMetadata := metadata(2, "event-b", "payload-b")
	second, err := Next(secondMetadata, first)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Metadata: firstMetadata, Digest: first},
		{Metadata: secondMetadata, Previous: first, Digest: second},
	}
	if err := Verify(entries); err != nil {
		t.Fatal(err)
	}
	entries[1].Metadata.AggregateID = "tampered"
	if !errors.Is(Verify(entries), ErrCorrupt) {
		t.Fatal("tampered payload verified")
	}
}

func metadata(sequence uint64, eventID, payload string) EventMetadata {
	return EventMetadata{
		Sequence: sequence, EventID: eventID, Tenant: "tenant-a", AggregateType: "artifact",
		AggregateID: "a", AggregateVersion: sequence, CommandID: "command", PayloadDigest: payload, OccurredAtMs: 1000,
	}
}
