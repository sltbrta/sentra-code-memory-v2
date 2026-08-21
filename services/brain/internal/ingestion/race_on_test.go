//go:build race

package ingestion_test

// raceEnabled reports whether this test binary was built with -race.
// See race_off_test.go for why the distinction is drawn.
const raceEnabled = true
