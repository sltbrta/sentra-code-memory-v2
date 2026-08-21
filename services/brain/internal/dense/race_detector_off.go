//go:build !race

package dense

// RaceDetectorEnabled reports whether this binary was built with -race.
//
// Wall-clock assertions are only meaningful when the process is not sharing
// its cores with the rest of the suite under the race detector, which
// multiplies both the work and the contention. The bounds that describe the
// algorithm -- recall, allocations per query, and the distance-calculation
// ceiling -- hold either way and are never skipped.
const RaceDetectorEnabled = false
