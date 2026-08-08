// Package codeindex builds deterministic, bounded P5 syntax projections.
//
// Project validates one repository-relative source file and emits exact lexical
// anchors for definitions, imports, and references. Build and Apply provide
// clean and incremental repository projections without filesystem, network,
// model, compiler, or language-server side effects.
package codeindex
