set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

check:
    go test ./services/brain/cmd/sentra-code-memory ./services/brain/internal/codecrawl ./services/brain/internal/codeindex ./services/brain/internal/codeserve ./services/brain/internal/memory ./services/brain/internal/productsearch
    go vet ./services/brain/cmd/sentra-code-memory ./services/brain/internal/codecrawl ./services/brain/internal/codeindex ./services/brain/internal/codeserve ./services/brain/internal/memory ./services/brain/internal/productsearch

cli-help:
    go run ./services/brain/cmd/sentra-code-memory --help

cli-smoke:
    go run ./services/brain/cmd/sentra-code-memory catalog
    printf '%s\n' '{"verb":"ping"}' | go run ./services/brain/cmd/sentra-code-memory serve

code-index root workers="4":
    go run ./services/brain/cmd/sentra-code-memory index --root {{root}} --workers {{workers}}

code-search root query workers="4" top_k="20":
    go run ./services/brain/cmd/sentra-code-memory search --root {{root}} --q {{query}} --workers {{workers}} --top-k {{top_k}}
