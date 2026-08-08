// Package factorytracer re-exports the Tracer-001 L2 compiler for product
// composition outside services/brain/internal (Go internal-import rules).
package factorytracer

import (
	internal "github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/tracer001"
)

type (
	CompiledWorkflow = internal.CompiledWorkflow
	IntentHandoff    = internal.IntentHandoff
	IntentLeaf       = internal.IntentLeaf
)

var (
	CompileFromHandoff    = internal.CompileFromHandoff
	ValidateNoRedispatch  = internal.ValidateNoRedispatch
	ValidateSealedActions = internal.ValidateSealedActions
)
