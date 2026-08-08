package tracer001

import "errors"

var (
	// ErrInvalidInput reports malformed compiler inputs (programming error).
	ErrInvalidInput = errors.New("tracer001: invalid input")
	// ErrPlanInvalid reports a DAG violating frozen one-layer Tracer 001 shape.
	ErrPlanInvalid = errors.New("tracer001: plan invalid")
	// ErrRedispatch reports any leaf redispatch or leaf-to-leaf edge.
	ErrRedispatch = errors.New("tracer001: leaf redispatch forbidden")
)
