//go:build linux

package extract

import (
	"runtime/debug"
	"syscall"
)

// applyLimits restricts the helper process itself before it parses
// anything: address space, CPU seconds (SIGXCPU then SIGKILL), open files,
// and no file writes at all (SIGXFSZ).
func applyLimits(memBytes, cpuSeconds uint64) error {
	if memBytes > 0 {
		// Soft limit first so the GC works hard before the hard wall.
		debug.SetMemoryLimit(int64(memBytes / 4 * 3)) //nolint:gosec // bounded config value
		if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: memBytes, Max: memBytes}); err != nil {
			return err
		}
	}
	if cpuSeconds > 0 {
		if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpuSeconds, Max: cpuSeconds + 1}); err != nil {
			return err
		}
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: 32, Max: 32}); err != nil {
		return err
	}
	return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: 0})
}
