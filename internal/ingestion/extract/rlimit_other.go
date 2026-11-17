//go:build !linux

package extract

import "runtime/debug"

// applyLimits on non-Linux development machines only sets the Go soft
// memory limit; production workers always run on Linux (Cloud Run).
func applyLimits(memBytes, _ uint64) error {
	if memBytes > 0 {
		debug.SetMemoryLimit(int64(memBytes / 4 * 3)) //nolint:gosec // bounded config value
	}
	return nil
}
