<!-- markdownlint-disable MD013 -->

# ADR 0025 — Explicit local trust for HTTP/MCP; crash-safe durable code index

**Status:** Accepted; **shipped** (issues #41, #42).
**Date:** 2026-08-12
**Context:** The parity audit found the local HTTP/MCP adapters were
unauthenticated and unbounded by any explicit trust policy, `code_read` could
read any regular file under a caller-selected root regardless of the
repository ignore policy or index membership, and durable index persistence
relied on atomic rename alone with no cross-process coordination, no fsync
durability chain, and no root-binding validation on read paths.

## Decision

### Trust boundary (issue #41)

1. **Loopback is the default and the gate.** `sentra-code-memory http` binds
   `127.0.0.1:8765` by default. `adapters.ValidateListenAddr` runs before the
   socket opens and **refuses any non-loopback bind** (`0.0.0.0`, wildcard,
   LAN addresses) unless a bearer token is configured or `--allow-insecure`
   is passed as an explicit opt-out. Loopback hosts are `127.0.0.0/8`,
   `::1`, and `localhost`; an empty host is a wildcard and is not loopback.
2. **Bearer token when configured.** `--token` / env
   `SENTRA_CODE_MEMORY_HTTP_TOKEN` makes every endpoint — including
   `/health` — require `Authorization: Bearer <token>` (constant-time
   compare). Failures return HTTP 401 with `WWW-Authenticate: Bearer` and a
   structured codeserve envelope carrying `error_code: "unauthorized"`, so
   clients branch on the same shape across CLI/JSONL/HTTP/MCP.
3. **MCP stdio needs no credential.** The MCP adapter's trust boundary is the
   spawning parent process, which owns the child's stdin/stdout; there is no
   network surface to authenticate. This is documented behavior, not a gap.
4. **`code_read` is constrained by default.** After the existing traversal /
   absolute-path / symlink-escape / non-regular-file rejections (which are
   never bypassable), a read must survive:
   - the repository ignore policy (`internal/repoignore`: built-in
     generated/secret exclusions plus root `.gitignore`, `.dockerignore`,
     `.git/info/exclude`) — denied with `error_code: "path_denied"`;
   - durable-index membership when an index exists at `index_cache` or the
     default `.sentra/code-index.gob` — non-members are denied with
     `path_denied`; a corrupt index fails closed as `index_unavailable`.
   Typed opt-in fields `allow_ignored` and `allow_unindexed` (on the wire and
   in `ReadRequest`, catalog metadata, and the MCP tool schema) restore the
   legacy behavior explicitly. Without a durable index the ignore gate is the
   only membership policy (compatibility fallback).

### Durability (issue #42)

1. **Lock coordination.** `Index.Save` holds a per-path in-process mutex plus
   an exclusive advisory flock on `<gob>.lock` (darwin/linux; no-op elsewhere,
   where tmp+rename remains the guarantee) and fails closed if the lock
   cannot be taken. `Load` takes the shared lock best-effort — a read-only
   directory must not make a valid gob unreadable.
2. **fsync chain.** Save fsyncs the temp file before rename and fsyncs the
   parent directory after rename; directory-sync `EINVAL`/`ENOTSUP` is
   tolerated (documented filesystem limitation), everything else fails.
3. **Old-or-complete-new.** A stale `.tmp` from an interrupted writer is
   discarded before each Save; the live gob is untouched until the atomic
   rename, so a crash always leaves old-or-complete-new, never partial. A
   corrupt gob fails `Load` and `OpenOrRefresh` recovers by reindexing.
4. **Root binding.** `DurableMeta.Root` is validated symlink-aware
   (`codecrawl.ValidateRoot`, `ErrRootMismatch`): `no_refresh` loads,
   `code_freshness`, and `code_ingest_paths` fail clearly with
   `index_unavailable` + a "root mismatch" message instead of serving another
   workspace's index; `OpenOrRefresh` reindexes and rebinds.

## Migration

- **HTTP:** nothing changes for loopback users without a token. Scripts
  binding a non-loopback address must set `--token` (and send the
  `Authorization` header) or pass `--allow-insecure`.
- **`code_read`:** reads of ignored files (e.g. `.env`) or — when a durable
  index exists — non-member files now fail with `path_denied`. Add
  `"allow_ignored": true` and/or `"allow_unindexed": true` to the request to
  restore the previous behavior; both fields are reachable on CLI, JSONL,
  HTTP, and MCP with identical semantics.
- **Index consumers:** `no_refresh` / freshness callers that previously
  pointed a mismatched root at a foreign gob now get a clear
  `index_unavailable` error instead of silently wrong results.

## Consequences

- Fail-closed everywhere the policy cannot be verified (unreadable ignore
  file, corrupt index, unbound index root, lock failure on write).
- New error codes `unauthorized` and `path_denied` are additive to the
  `codeserve.ErrorCode` enum; existing consumers keying on `ok`/`error` are
  unaffected.
- The `.lock` sidecar sits next to the gob under `.sentra/` and is already
  covered by the built-in `.sentra/` ignore default.
