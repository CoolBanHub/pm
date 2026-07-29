//go:build linux

package supervisor

import (
	"os"
	"strconv"
	"strings"
)

// collectTotalMemory reads MemTotal from /proc/meminfo (kB -> bytes).
// Returns 0 if unavailable.
func collectTotalMemory() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		const prefix = "MemTotal:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
