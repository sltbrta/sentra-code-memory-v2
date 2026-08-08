// Package outcomes re-exports brain/internal/outcomes for product composition
// outside services/brain/internal (Go internal-import rules).
package outcomes

import (
	internal "github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/outcomes"
)

type (
	Admissions   = internal.Admissions
	AdmitRequest = internal.AdmitRequest
)

const AuthorityMachineObservation = internal.AuthorityMachineObservation

var New = internal.New
