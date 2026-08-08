# Event kernel

This package owns deterministic, version-pinned reaction evaluation. Reactions
receive immutable event facts and can only emit typed command proposals; the
effect broker and authority ledger remain responsible for authorization and
execution. `Registry.React` applies a caller-supplied hard command bound and
stable ordering so replay cannot grow an unbounded or nondeterministic loop.

Stage 02 persists only `aggregate_type`; that exact value is both the event type
and reaction key, with implicit schema version `1`. Aggregate version remains a
separate ordering fact. Distinct event type/schema persistence requires a later
contract-and-migration anchor delta rather than an encoded identifier.

The package does not persist events, execute commands, or call tools.
