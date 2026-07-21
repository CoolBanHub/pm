package supervisor

import (
	"os/exec"
	"strconv"
	"strings"
)

func CollectMetrics(statuses []Status) {
	pids := make([]string, 0, len(statuses))
	index := make(map[int]int, len(statuses))
	for i := range statuses {
		if statuses[i].PID > 0 {
			pids = append(pids, strconv.Itoa(statuses[i].PID))
			index[statuses[i].PID] = i
		}
	}
	if len(pids) == 0 {
		return
	}
	output, err := exec.Command("ps", "-p", strings.Join(pids, ","), "-o", "pid=,%cpu=,rss=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		cpu, cpuErr := strconv.ParseFloat(fields[1], 64)
		memoryKB, memoryErr := strconv.ParseInt(fields[2], 10, 64)
		position, exists := index[pid]
		if pidErr == nil && cpuErr == nil && memoryErr == nil && exists {
			statuses[position].CPU = cpu
			statuses[position].Memory = memoryKB * 1024
		}
	}
}
