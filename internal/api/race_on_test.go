//go:build race

package api

// raceEnabled is true when tests run under the race detector, which
// slows the pure-Go SQLite driver too much for wall-clock limits.
const raceEnabled = true
