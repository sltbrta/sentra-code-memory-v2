package marker

import "testing"

func TestMarkerLabel(t *testing.T) {
	if got := MarkerLabel(); got != "tracer-001-authorized-marker" {
		t.Fatalf("MarkerLabel() = %q", got)
	}
}
