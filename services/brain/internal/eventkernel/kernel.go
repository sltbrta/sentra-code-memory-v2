// Package eventkernel runs deterministic, versioned reactions over immutable events.
// Reactions are pure: they may emit typed commands, but they cannot perform effects.
package eventkernel

import (
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrInvalidEvent reports a missing event identity, type, aggregate, or version.
	ErrInvalidEvent = errors.New("eventkernel: invalid event")
	// ErrReactionLimit reports that a reaction graph exceeded its explicit work bound.
	ErrReactionLimit = errors.New("eventkernel: reaction limit exceeded")
)

// Event is the immutable input visible to a pure reaction.
type Event struct {
	ID               string
	Type             string
	AggregateType    string
	AggregateID      string
	AggregateVersion uint64
	SchemaVersion    uint64
	PayloadDigest    string
}

// Command is a typed proposal emitted by a reaction; executing it remains external.
type Command struct {
	Type          string
	AggregateType string
	AggregateID   string
	PayloadDigest string
}

// Reaction deterministically maps one event to zero or more command proposals.
type Reaction func(Event) ([]Command, error)

// Registry binds event types to versioned reactions.
type Registry struct {
	reactions map[string]map[uint64]Reaction
}

// NewRegistry returns an empty deterministic reaction registry.
func NewRegistry() *Registry {
	return &Registry{reactions: make(map[string]map[uint64]Reaction)}
}

// Register binds one event type and version exactly once.
// It returns an error for empty keys, version zero, nil reactions, or duplicates.
func (r *Registry) Register(eventType string, version uint64, reaction Reaction) error {
	if eventType == "" || version != 1 || reaction == nil {
		return ErrInvalidEvent
	}
	versions := r.reactions[eventType]
	if versions == nil {
		versions = make(map[uint64]Reaction)
		r.reactions[eventType] = versions
	}
	if _, exists := versions[version]; exists {
		return fmt.Errorf("%w: duplicate reaction", ErrInvalidEvent)
	}
	versions[version] = reaction
	return nil
}

// React evaluates events in caller order and commands in stable lexical order.
// maxCommands is a hard safety bound across the entire batch; zero is invalid.
func (r *Registry) React(events []Event, maxCommands uint64) ([]Command, error) {
	if maxCommands == 0 {
		return nil, ErrReactionLimit
	}
	commands := make([]Command, 0)
	for _, event := range events {
		if err := validateEvent(event); err != nil {
			return nil, err
		}
		reaction := r.reactions[event.Type][event.SchemaVersion]
		if reaction == nil {
			continue
		}
		emitted, err := reaction(event)
		if err != nil {
			return nil, fmt.Errorf("eventkernel: react %s v%d: %w", event.Type, event.SchemaVersion, err)
		}
		if uint64(len(commands))+uint64(len(emitted)) > maxCommands {
			return nil, ErrReactionLimit
		}
		for _, command := range emitted {
			if command.Type == "" || command.AggregateType == "" || command.AggregateID == "" || command.PayloadDigest == "" {
				return nil, fmt.Errorf("%w: malformed emitted command", ErrInvalidEvent)
			}
		}
		sort.SliceStable(emitted, func(i, j int) bool {
			left := emitted[i]
			right := emitted[j]
			return left.Type+"\x00"+left.AggregateType+"\x00"+left.AggregateID+"\x00"+left.PayloadDigest <
				right.Type+"\x00"+right.AggregateType+"\x00"+right.AggregateID+"\x00"+right.PayloadDigest
		})
		commands = append(commands, emitted...)
	}
	return commands, nil
}

func validateEvent(event Event) error {
	if event.ID == "" || event.Type == "" || event.Type != event.AggregateType || event.AggregateID == "" ||
		event.AggregateVersion == 0 || event.SchemaVersion != 1 || event.PayloadDigest == "" {
		return ErrInvalidEvent
	}
	return nil
}
