# Deletion state

This package applies the local ledger's immediate-deny state machine. Within an
existing command transaction it moves one published generation to tombstoned,
records its canonical receipt link, and schedules a fenced purge. A later
ArtifactVault success may advance the same generation and job to purged.

It does not delete object bytes, descendants, backups, or encryption keys.
