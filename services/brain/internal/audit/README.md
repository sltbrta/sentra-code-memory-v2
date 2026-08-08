# Audit chain

This package defines the deterministic hash function used by the local
authority audit ledger and verifies the chain on read. The digest binds tenant,
canonical sequence, event identity, aggregate type/identity/version, command,
timestamp, payload digest, and the prior link. The local ledger also checks an
explicit persisted head checkpoint so missing audit rows fail verification. It
stores no event payloads and emits no logs.
