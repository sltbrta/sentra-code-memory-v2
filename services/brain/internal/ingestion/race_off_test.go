//go:build !race

package ingestion_test

// raceEnabled reports whether this test binary was built with -race.
//
// The race detector multiplies both the work and the contention, and under
// `go test -race ./...` this package also shares its cores with every other
// package in the module. Wall-clock allowances that are comfortable in
// isolation are not, and this package's allowances bound git subprocesses --
// so the failure is a context deadline in production code rather than an
// obviously test-shaped timeout.
const raceEnabled = false
