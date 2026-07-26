//go:build !darwin && !linux

package runner

// LoadAvg1 has no implementation here, so the load limit is simply not applied.
func LoadAvg1() (float64, bool) { return 0, false }
