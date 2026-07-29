package supervisor

import (
	"runtime"
	"sync"
)

// HostInfo describes static, machine-level resources. They give context to the
// per-process metrics: CPUCount is the baseline for the single-core ps %CPU
// value (a process can use up to CPUCount*100%), and TotalMemory lets the UI
// show a memory占用 ratio against the machine total.
type HostInfo struct {
	TotalMemory int64 `json:"memory_total"` // physical memory in bytes
	CPUCount    int   `json:"cpu_count"`    // logical CPU cores
}

var (
	hostInfoOnce sync.Once
	hostInfo     *HostInfo
)

// CachedHostInfo returns machine-level resource info. The values are static for
// the lifetime of the process, so they are collected once and cached.
func CachedHostInfo() *HostInfo {
	hostInfoOnce.Do(func() {
		hostInfo = &HostInfo{
			TotalMemory: collectTotalMemory(),
			CPUCount:    runtime.NumCPU(),
		}
	})
	return hostInfo
}
