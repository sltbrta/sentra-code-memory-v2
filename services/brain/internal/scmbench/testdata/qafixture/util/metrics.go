package util

// Counter is a monotonically increasing metric counter.
type Counter struct {
	name  string
	value int64
}

// NewCounter creates a named counter starting at zero.
func NewCounter(name string) *Counter {
	return &Counter{name: name}
}

// Inc advances the counter by one and returns the new value.
func (c *Counter) Inc() int64 {
	c.value++
	return c.value
}

// Add advances the counter by a non-negative delta.
func (c *Counter) Add(delta int64) int64 {
	if delta < 0 {
		delta = 0
	}
	c.value += delta
	return c.value
}

// Value returns the current counter reading.
func (c *Counter) Value() int64 { return c.value }

// Histogram buckets request latencies into coarse bands for cheap reporting.
type Histogram struct {
	name    string
	buckets [4]int64
}

// NewHistogram creates a named latency histogram.
func NewHistogram(name string) *Histogram {
	return &Histogram{name: name}
}

// Observe records one latency sample in milliseconds.
func (h *Histogram) Observe(ms int64) {
	switch {
	case ms < 10:
		h.buckets[0]++
	case ms < 50:
		h.buckets[1]++
	case ms < 250:
		h.buckets[2]++
	default:
		h.buckets[3]++
	}
}

// Counts returns a copy of the bucket readings.
func (h *Histogram) Counts() [4]int64 { return h.buckets }
