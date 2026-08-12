set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

check:
    go test ./services/brain/cmd/sentra-code-memory ./services/brain/cmd/bench-code ./services/brain/internal/adapters ./services/brain/internal/codecrawl ./services/brain/internal/codeindex ./services/brain/internal/codeserve ./services/brain/internal/contextpack ./services/brain/internal/savings ./services/brain/internal/scmbench ./services/brain/internal/memory ./services/brain/internal/productsearch ./services/brain/internal/repoignore ./services/brain/internal/sessionlog ./services/brain/internal/workflow
    go vet ./services/brain/cmd/sentra-code-memory ./services/brain/cmd/bench-code ./services/brain/internal/adapters ./services/brain/internal/codecrawl ./services/brain/internal/codeindex ./services/brain/internal/codeserve ./services/brain/internal/contextpack ./services/brain/internal/savings ./services/brain/internal/scmbench ./services/brain/internal/memory ./services/brain/internal/productsearch ./services/brain/internal/repoignore ./services/brain/internal/sessionlog ./services/brain/internal/workflow

check-all:
    go test ./services/...
    go test ./packages/contracts/...
    cargo test --locked --offline --manifest-path workers/code-index/Cargo.toml

ci: check-all

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
