package eventkernel

import (
	"errors"
	"reflect"
	"testing"
)

func TestReactIsVersionedDeterministicAndBounded(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register("artifact.admitted", 1, func(event Event) ([]Command, error) {
		return []Command{
			{Type: "z", AggregateType: event.AggregateType, AggregateID: event.AggregateID, PayloadDigest: "b"},
			{Type: "a", AggregateType: event.AggregateType, AggregateID: event.AggregateID, PayloadDigest: "a"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "e1", Type: "artifact.admitted", AggregateType: "artifact.admitted", AggregateID: "a1", AggregateVersion: 3, SchemaVersion: 1, PayloadDigest: "d"}
	commands, err := registry.React([]Event{event}, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{
		{Type: "a", AggregateType: "artifact.admitted", AggregateID: "a1", PayloadDigest: "a"},
		{Type: "z", AggregateType: "artifact.admitted", AggregateID: "a1", PayloadDigest: "b"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	if _, err := registry.React([]Event{event}, 1); !errors.Is(err, ErrReactionLimit) {
		t.Fatalf("limit error = %v", err)
	}
	commands, err = registry.React([]Event{{ID: "e2", Type: "artifact.admitted", AggregateType: "artifact.admitted", AggregateID: "a1", AggregateVersion: 3, SchemaVersion: 2, PayloadDigest: "d"}}, 1)
	if !errors.Is(err, ErrInvalidEvent) || len(commands) != 0 {
		t.Fatalf("unsupported version emitted %#v with error %v", commands, err)
	}
}

func TestRegisterAndReactRejectMalformedInput(t *testing.T) {
	registry := NewRegistry()
	if !errors.Is(registry.Register("", 1, func(Event) ([]Command, error) { return nil, nil }), ErrInvalidEvent) {
		t.Fatal("empty event type registered")
	}
	if _, err := registry.React([]Event{{ID: "e"}}, 1); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("malformed event error = %v", err)
	}
}
