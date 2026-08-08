# Internal service contracts

This package freezes the narrow Go ports shared by Stage 02 leaves without
allowing one service leaf to import another leaf's `internal` implementation.
It contains types and interfaces only.

Implementations must validate identifiers and bounds at their authenticated
boundary, return static typed errors, default deny on missing policy facts, and
never place key bytes or artifact payloads in these values. `LedgerTx` is valid
only during its owning `AtomicLedgerTransaction.Within` callback.

Run `bazel test //services/internal/contracts:contracts_test`.
