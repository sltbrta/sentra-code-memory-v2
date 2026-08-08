// Package tracer001 compiles Tracer 001 approved-intent handoffs into a typed
// one-layer factory DAG. It reuses Stage 05 kernel shapes (orchestrator root,
// N∈[1,3] prefix-disjoint leaves, optional review, four required gates) without
// opening the kernel database or issuing leases. Leaves never redispatch:
// edges originate only at the orchestrator, and leaf grants exclude
// factory.dispatch, factory.task.create, merge, deploy, release, and promote.
//
// Status: [partial] Stage 06 L2 workflow compiler — no gateway, TUI, or live
// runner surface.
package tracer001
