package contentprivacy

import "context"

// ProductionProjectionAdapter is the explicit validated-publish-before-commit
// composition boundary. Its fields are private so ordinary callers cannot
// replace Guard output with caller-provided cache or index text.
//
// This adapter is deliberately persistence-neutral: deployments retain
// ownership of their projection sink while this type owns the only call from
// raw Input to that sink. A zero, partially configured, or nil adapter fails
// closed.
type ProductionProjectionAdapter struct {
	guard     *Guard
	publisher ProjectionPublisher
}

// NewProductionProjectionAdapter requires both enforcement and publication
// dependencies. Absence is a construction error, not a privacy-disabled mode.
func NewProductionProjectionAdapter(guard *Guard, publisher ProjectionPublisher) (*ProductionProjectionAdapter, error) {
	if guard == nil || publisher == nil {
		return nil, ErrComposition
	}
	return &ProductionProjectionAdapter{guard: guard, publisher: publisher}, nil
}

// AdmitAndPublish validates raw input, publishes only Guard's sanitized
// projection, and commits the admission only after publication succeeds.
// Quarantined and tombstoned decisions never reach the publisher. Publisher
// errors are collapsed and roll back the transient admission so an exact retry
// can use the same scoped content identity.
func (a *ProductionProjectionAdapter) AdmitAndPublish(ctx context.Context, input Input) (Decision, error) {
	if a == nil || a.guard == nil || a.publisher == nil {
		return Decision{}, ErrComposition
	}
	if ctx == nil {
		return Decision{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	decision, admission, err := a.guard.preparePublish(input)
	if err != nil {
		return decision, err
	}
	if decision.Projection == nil {
		return decision, nil
	}
	projection := cloneProjection(*decision.Projection)
	if err := publishSafely(a.publisher, ctx, projection); err != nil {
		a.guard.rollbackPublish(admission)
		return decision, ErrPublish
	}
	receipt, err := a.guard.commitPublish(admission)
	if err != nil {
		return Decision{}, err
	}
	decision.Receipt = receipt
	return decision, nil
}

func publishSafely(publisher ProjectionPublisher, ctx context.Context, projection Projection) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrPublish
		}
	}()
	return publisher.PublishProjection(ctx, projection)
}
