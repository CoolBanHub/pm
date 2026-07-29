//go:build darwin

package supervisor

import "golang.org/x/sys/unix"

// collectTotalMemory returns the physical memory size via the hw.memsize sysctl
// (bytes). Returns 0 if unavailable.
func collectTotalMemory() int64 {
	if bytes, err := unix.SysctlUint64("hw.memsize"); err == nil {
		return int64(bytes)
	}
	return 0
}
