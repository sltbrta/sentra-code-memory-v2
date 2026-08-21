//go:build race

package dense

// RaceDetectorEnabled reports whether this binary was built with -race.
// See race_detector_off.go for why the distinction is drawn.
const RaceDetectorEnabled = true
