package util

// RetryPolicy bounds how many times an operation may be retried and how long
// to wait between attempts.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelayMS int
}

// DefaultRetryPolicy is the fixture-standard retry budget.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelayMS: 50}
}

// Retry runs the operation until it succeeds or the policy budget is spent.
// The last error is returned when every attempt fails.
func Retry(policy RetryPolicy, op func() error) error {
	var err error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err = op(); err == nil {
			return nil
		}
	}
	return err
}

// BackoffMS computes the delay before the given zero-based attempt using
// exponential backoff from the policy base delay.
func BackoffMS(policy RetryPolicy, attempt int) int {
	delay := policy.BaseDelayMS
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	return delay
}
