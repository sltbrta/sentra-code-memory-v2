// Package github implements the Stage 08 GitHub source-connector provider surface.
//
// It admits repository and issue evidence only. Action connectors (draft PR,
// merge, deploy) live in the separate Stage 06 effect broker and never share
// source grants. Unit tests inject FakeSourceAPI and never open sockets; live
// REST is optional when GITHUB_TOKEN or OUROBOROS_GITHUB_TOKEN is present.
package github
