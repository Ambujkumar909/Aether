//go:build race

package identity

// raceEnabled lets the scale test step aside under the race detector, where
// the pure-Go SQLite runs far slower and the numbers mean nothing.
const raceEnabled = true
