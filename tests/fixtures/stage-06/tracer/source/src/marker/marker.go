// Package marker is the hermetic Tracer 001 supporting-span source.
// Synthetic only: not a Sentra/SFS live reconstruction.
package marker

// MarkerLabel returns the single authorized supporting-span marker for Tracer 001.
func MarkerLabel() string {
	return "tracer-001-authorized-marker"
}
