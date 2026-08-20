set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

# Preflight gate. Previously named 17 of 86 packages by hand, omitting every
# broker and gateway package -- the entire authorization surface. It now covers
# the whole module. -count=1 defeats the test cache: a cached PASS recorded
# before an edit is not evidence about the edit.
check:
    cd services && go build ./...
    cd services && go vet ./...
    cd services && go test -count=1 ./...
    gofmt -l services packages | (! grep .) || (echo "gofmt: files above are unformatted" && exit 1)

# The concurrency gate. The suite passed -race from the day it was written
# because nothing exercised concurrency; the hammer tests added in the 2026-08
# hardening pass are what make this meaningful.
check-race:
    cd services && go test -count=1 -race ./...

check-all: check check-race
    go test -count=1 ./packages/contracts/...
    cargo test --locked --offline --manifest-path workers/code-index/Cargo.toml
    cd packages/contracts && ruby tools/generated-manifest.rb check

# Reachable-vulnerability scan. Requires golang.org/x/vuln/cmd/govulncheck.
vuln:
    cd services && govulncheck ./...

ci: check-all vuln bench-code

cli-help:
    go run ./services/brain/cmd/sentra-code-memory --help

cli-smoke:
    go run ./services/brain/cmd/sentra-code-memory catalog
    printf '%s\n' '{"verb":"ping"}' | go run ./services/brain/cmd/sentra-code-memory serve
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ping","arguments":{}}}' | go run ./services/brain/cmd/sentra-code-memory mcp

# Deterministic offline retrieval benchmark gate (issue #48). Measures
# hit@1/5/10, latency, token savings, and failure classification on the
# checked-in qafixture, runs the CLI/HTTP/MCP smoke matrix, checks thresholds,
# and records an artifact digest. No credentials or network required.
bench-code:
    go run ./services/brain/cmd/bench-code

# Print the benchmark artifact JSON to stdout instead of the log path.
bench-code-json:
    go run ./services/brain/cmd/bench-code --out - --quiet

code-index root workers="4":
    go run ./services/brain/cmd/sentra-code-memory index --root {{root}} --workers {{workers}}

code-search root query workers="4" top_k="20":
    go run ./services/brain/cmd/sentra-code-memory search --root {{root}} --q {{query}} --workers {{workers}} --top-k {{top_k}}
