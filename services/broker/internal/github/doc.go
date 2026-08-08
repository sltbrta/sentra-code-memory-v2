// Package github implements the Stage 06 two-phase GitHub draft-PR effect
// broker. Phase 1 publishes or reconciles a deterministic head ref; phase 2
// creates or reconciles exactly one draft pull request. Grants never express
// merge, deploy, release, force-push, or branch-delete. Live calls use a
// fine-grained PAT from GITHUB_TOKEN or OUROBOROS_GITHUB_TOKEN; unit tests
// inject a deterministic fake HTTP transport and never contact the network.
//
// Status: [partial] Stage 06 L2 broker leaf — no TUI, gateway, or live dogfood.
package github
