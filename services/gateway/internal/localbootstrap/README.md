# Local bootstrap loader

This internal package loads the bounded, non-secret `BootstrapV1` manifest used
to compose the Stage 02 local authority process. `Load` requires an explicit
absolute manifest path, an exact lowercase SHA-256 pin, and an injected clock.
It never consults the environment, current working directory, or default files.

Before parsing, the loader requires a current-user-owned, non-symlink `0700`
parent and a current-user-owned, non-symlink regular `0600` file. It reads at
most 256 KiB, compares the SHA-256 in constant time, and accepts one complete
v1 JSON object. Its schema-aware token pass rejects duplicate keys at every
object level, case aliases, omitted fields, nulls, unknown fields, wrong
containers, and trailing documents before Go decoding. Configuration receipts
bind the exact accepted bytes; policy receipts bind purpose-prefixed,
length-delimited, canonically sorted relationship facts plus their tenant,
principal, brain, and revocation epoch.

`state_root` is an existing, real, current-user-owned `0700` directory whose
ancestors are non-symlink and not group/other writable. V1 fixes all state to
three direct children: `authority.sqlite3`, `authority.sock`, and `objects`.
Existing children must have their expected owner, type, and owner-only mode;
absent children are not created. The state tree cannot overlap or alias the
pinned manifest. `Config` returns canonical absolute paths, but validation is
not a race-safe open: downstream composition must retain a state-root
descriptor, open children relative to it with no-follow semantics, and
immediately verify each opened leaf's identity.

`approved_source_root` is the sole Stage 03 v1 Git root. It must already exist
as a current-user-owned, non-symlink directory whose ancestry is not writable
by group or others, and it cannot overlap the state tree or manifest. The RPC
surface carries only its opaque approved-root ID and never echoes this absolute
path. Loading validates the path but is not a race-safe traversal; ingestion
must retain a descriptor and use no-follow relative opens.

The manifest carries only the fixed state paths, one approved source root,
identifiers, Keychain references, numeric
bounds, relationships, and issued grant facts. It cannot contain encryption
keys, credentials, source payloads, wildcard authority, delegation, expired
grants, or arbitrary actions. The package does not provision Keychain items,
open databases, create directories, sign configuration, or claim protection
from root or same-user malware racing the process.

Run `go test -race ./gateway/internal/localbootstrap` from `services`, or
`bazel test //services/gateway/internal/localbootstrap:localbootstrap_test`.
