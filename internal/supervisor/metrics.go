package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var metricsHTTPClient = &http.Client{Timeout: 750 * time.Millisecond}

func CollectMetrics(statuses []Status) {
	if len(statuses) == 0 {
		return
	}
	if output, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=,%cpu=,rss=").Output(); err == nil {
		children := applyProcessMetrics(statuses, output)
		collectTCPPortMetrics(statuses, children)
	}
	collectGoroutineMetrics(statuses)
}

type processMetric struct {
	cpu    float64
	memory int64
}

func applyProcessMetrics(statuses []Status, output []byte) map[int][]int {
	metrics := make(map[int]processMetric)
	children := make(map[int][]int)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		cpu, cpuErr := strconv.ParseFloat(fields[2], 64)
		memoryKB, memoryErr := strconv.ParseInt(fields[3], 10, 64)
		if pidErr != nil || ppidErr != nil || cpuErr != nil || memoryErr != nil {
			continue
		}
		metrics[pid] = processMetric{cpu: cpu, memory: memoryKB * 1024}
		children[ppid] = append(children[ppid], pid)
	}
	for i := range statuses {
		root := statuses[i].PID
		metric, exists := metrics[root]
		if root <= 0 || !exists {
			continue
		}
		statuses[i].CPU = metric.cpu
		statuses[i].Memory = metric.memory
		statuses[i].Children = len(children[root])
		statuses[i].Descendants = descendantCount(root, children)
	}
	return children
}

func descendantCount(root int, children map[int][]int) int {
	pids := processTreePIDs(root, children)
	if len(pids) == 0 {
		return 0
	}
	return len(pids) - 1
}

func processTreePIDs(root int, children map[int][]int) []int {
	if root <= 0 {
		return nil
	}
	seen := map[int]bool{root: true}
	pids := []int{root}
	stack := append([]int(nil), children[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
		stack = append(stack, children[pid]...)
	}
	return pids
}

func collectGoroutineMetrics(statuses []Status) {
	var wait sync.WaitGroup
	for i := range statuses {
		if statuses[i].PID <= 0 || statuses[i].PprofURL == "" {
			continue
		}
		wait.Add(1)
		go func(position int) {
			defer wait.Done()
			count, err := fetchGoroutineCount(statuses[position].PprofURL)
			if err == nil {
				statuses[position].Goroutines = &count
			}
		}(i)
	}
	wait.Wait()
}

func fetchGoroutineCount(base string) (int, error) {
	endpoint, err := url.Parse(base)
	if err != nil {
		return 0, err
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/goroutine"
	query := endpoint.Query()
	query.Set("debug", "1")
	endpoint.RawQuery = query.Encode()
	response, err := metricsHTTPClient.Get(endpoint.String())
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pprof returned %s", response.Status)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 4096))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, fmt.Errorf("read goroutine profile: %w", err)
		}
		return 0, errors.New("empty goroutine profile")
	}
	const prefix = "goroutine profile: total "
	line := strings.TrimSpace(scanner.Text())
	if !strings.HasPrefix(line, prefix) {
		return 0, fmt.Errorf("unexpected goroutine profile header")
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid goroutine count %q", line)
	}
	return count, nil
}
