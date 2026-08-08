# Stage 10 bounded company mode

Hermetic two-principal company profile over in-process PostgreSQL-shaped and
S3-compatible ports. Authorization reuses the OpenFGA-compatible evaluator with
company fixtures. The broker package now also ships an HTTP OpenFGA client and
hermetic dual-run harness; default path remains in-process. Live durable-store
conformance is still DEF-015 / #72 partial.

```sh
bazel test //services/brain/internal/companymode:companymode_test
just stage-exit 10
```
