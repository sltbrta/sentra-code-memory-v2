// Package connector implements the bounded Stage 08 GitHub source-connector kernel.
//
// It owns connection lifecycle, cursor advancement after complete pages only,
// lexical evidence query with GitHub-native anchors, revoke, and purge.
// Provider messages are evidence, never trusted instructions. Source grants
// never imply action grants.
package connector
