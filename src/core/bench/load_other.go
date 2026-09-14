//go:build !darwin && !linux

package bench

// readLoadAverage has no reading on this platform, so load_at_start is unknown.
func readLoadAverage() *float64 { return nil }
