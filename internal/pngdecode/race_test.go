//go:build race

package pngdecode

// raceEnabled reports a race-detector build, where the instrumentation makes
// extra allocations and strict zero-allocation assertions must skip.
const raceEnabled = true
