package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const tcpListenState = "0A"

func collectTCPPortMetrics(statuses []Status, children map[int][]int) {
	pidSet := make(map[int]struct{})
	processTrees := make([][]int, len(statuses))
	for i := range statuses {
		processTrees[i] = processTreePIDs(statuses[i].PID, children)
		for _, pid := range processTrees[i] {
			pidSet[pid] = struct{}{}
		}
	}
	if len(pidSet) == 0 {
		return
	}

	pids := make([]int, 0, len(pidSet))
	for pid := range pidSet {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	portsByPID, err := tcpListeningPorts(pids)
	if err != nil {
		return
	}
	applyTCPPortMetrics(statuses, processTrees, portsByPID)
}

func applyTCPPortMetrics(statuses []Status, processTrees [][]int, portsByPID map[int][]int) {
	for i := range statuses {
		portSet := make(map[int]struct{})
		for _, pid := range processTrees[i] {
			for _, port := range portsByPID[pid] {
				if port > 0 && port <= 65535 {
					portSet[port] = struct{}{}
				}
			}
		}
		statuses[i].TCPPorts = sortedPorts(portSet)
	}
}

func tcpListeningPorts(pids []int) (map[int][]int, error) {
	switch runtime.GOOS {
	case "linux":
		ports, err := linuxTCPListeningPorts("/proc", pids)
		if err == nil {
			return ports, nil
		}
		return lsofTCPListeningPorts(pids)
	case "darwin":
		return lsofTCPListeningPorts(pids)
	default:
		return nil, fmt.Errorf("TCP port collection is unsupported on %s", runtime.GOOS)
	}
}

func linuxTCPListeningPorts(procRoot string, pids []int) (map[int][]int, error) {
	inodePorts := make(map[string]int)
	var readErrors []error
	readTables := 0
	for _, name := range []string{"tcp", "tcp6"} {
		path := filepath.Join(procRoot, "net", name)
		file, err := os.Open(path)
		if err != nil {
			readErrors = append(readErrors, err)
			continue
		}
		readTables++
		if err := parseProcNetTCP(file, inodePorts); err != nil {
			readErrors = append(readErrors, err)
		}
		_ = file.Close()
	}
	if readTables == 0 {
		return nil, errors.Join(readErrors...)
	}

	portsByPID := make(map[int][]int)
	for _, pid := range pids {
		entries, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "fd"))
		if err != nil {
			continue
		}
		portSet := make(map[int]struct{})
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "fd", entry.Name()))
			if err != nil {
				continue
			}
			inode, ok := socketInode(target)
			if !ok {
				continue
			}
			if port, exists := inodePorts[inode]; exists {
				portSet[port] = struct{}{}
			}
		}
		if ports := sortedPorts(portSet); len(ports) > 0 {
			portsByPID[pid] = ports
		}
	}
	return portsByPID, nil
}

func parseProcNetTCP(reader io.Reader, inodePorts map[string]int) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != tcpListenState {
			continue
		}
		separator := strings.LastIndexByte(fields[1], ':')
		if separator < 0 || separator == len(fields[1])-1 {
			continue
		}
		port, err := strconv.ParseUint(fields[1][separator+1:], 16, 16)
		if err != nil || port == 0 || fields[9] == "0" {
			continue
		}
		inodePorts[fields[9]] = int(port)
	}
	return scanner.Err()
}

func socketInode(target string) (string, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return "", false
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(target, prefix), "]")
	if inode == "" {
		return "", false
	}
	return inode, true
}

func lsofTCPListeningPorts(pids []int) (map[int][]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	path, err := exec.LookPath("lsof")
	if err != nil && runtime.GOOS == "darwin" {
		if _, statErr := os.Stat("/usr/sbin/lsof"); statErr == nil {
			path, err = "/usr/sbin/lsof", nil
		}
	}
	if err != nil {
		return nil, err
	}

	pidValues := make([]string, len(pids))
	for i, pid := range pids {
		pidValues[i] = strconv.Itoa(pid)
	}
	output, commandErr := exec.Command(path,
		"-nP", "-a", "-p", strings.Join(pidValues, ","),
		"-iTCP", "-sTCP:LISTEN", "-Fpn",
	).Output()
	if len(output) == 0 && commandErr != nil {
		return nil, commandErr
	}
	return parseLsofTCP(output), nil
}

func parseLsofTCP(output []byte) map[int][]int {
	portSets := make(map[int]map[int]struct{})
	currentPID := 0
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, err := strconv.Atoi(line[1:])
			if err != nil || pid <= 0 {
				currentPID = 0
				continue
			}
			currentPID = pid
		case 'n':
			port, ok := portFromLsofName(line[1:])
			if currentPID <= 0 || !ok {
				continue
			}
			if portSets[currentPID] == nil {
				portSets[currentPID] = make(map[int]struct{})
			}
			portSets[currentPID][port] = struct{}{}
		}
	}

	portsByPID := make(map[int][]int, len(portSets))
	for pid, set := range portSets {
		portsByPID[pid] = sortedPorts(set)
	}
	return portsByPID
}

func portFromLsofName(name string) (int, bool) {
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), "(LISTEN)"))
	separator := strings.LastIndexByte(name, ':')
	if separator < 0 || separator == len(name)-1 {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(name[separator+1:]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func sortedPorts(set map[int]struct{}) []int {
	if len(set) == 0 {
		return nil
	}
	ports := make([]int, 0, len(set))
	for port := range set {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}
