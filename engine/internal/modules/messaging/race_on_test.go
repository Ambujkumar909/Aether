//go:build race

package messaging

// raceEnabled lets the scale test step aside under the race detector, where
// the pure-Go SQLite runs forty times slower and the numbers mean nothing.
// Every other test runs under -race; only the timing one does not.
const raceEnabled = true
