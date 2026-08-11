// Package scmfixture is the static conformance fixture for the codeserve
// contract tests. It is indexed, never built.
package scmfixture

// Alpha is the fixture seed symbol.
func Alpha() int { return 1 }

// AlphaHelper gives Alpha a same-file neighbor.
func AlphaHelper() int { return Alpha() }
