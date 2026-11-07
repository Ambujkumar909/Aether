//go:build race

package scheduling

// raceEnabled lets scale tests keep their plan checks but drop their
// timing bounds under the race detector, where the pure-Go SQLite runs an
// order of magnitude slower and the numbers measure the detector.
const raceEnabled = true
