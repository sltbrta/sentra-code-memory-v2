# Stage 12 workflow inspector kernel

Read-only Workflow IR inspection, deterministic replay, and effect-free
simulation. No authoring canvas (DEF-011).

```sh
bazel test //services/brain/internal/workflowinspect:workflowinspect_test
just stage-exit 12
```
