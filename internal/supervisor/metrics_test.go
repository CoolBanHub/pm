package supervisor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestApplyProcessMetricsCountsChildrenAndDescendants(t *testing.T) {
	statuses := []Status{{PID: 10}, {PID: 20}}
	output := []byte(`
10 1 1.5 100
11 10 2.0 200
12 10 3.0 300
13 11 4.0 400
20 1 5.5 500
21 20 6.0 600
`)

	applyProcessMetrics(statuses, output)

	if statuses[0].CPU != 1.5 || statuses[0].Memory != 100*1024 || statuses[0].Children != 2 || statuses[0].Descendants != 3 {
		t.Fatalf("first metrics = %+v", statuses[0])
	}
	if statuses[1].CPU != 5.5 || statuses[1].Memory != 500*1024 || statuses[1].Children != 1 || statuses[1].Descendants != 1 {
		t.Fatalf("second metrics = %+v", statuses[1])
	}
}

func TestDescendantCountHandlesCycles(t *testing.T) {
	children := map[int][]int{10: {11}, 11: {10, 12}}
	if got := descendantCount(10, children); got != 2 {
		t.Fatalf("descendantCount = %d, want 2", got)
	}
	if got := descendantCount(0, children); got != 0 {
		t.Fatalf("descendantCount for missing PID = %d, want 0", got)
	}
}

func TestApplyTCPPortMetricsIncludesDescendants(t *testing.T) {
	statuses := []Status{{PID: 10}, {PID: 20}}
	processTrees := [][]int{{10, 11, 12}, {20, 21}}
	portsByPID := map[int][]int{
		10: {9000},
		11: {8080, 9000},
		12: {443},
		21: {5432},
	}

	applyTCPPortMetrics(statuses, processTrees, portsByPID)

	if want := []int{443, 8080, 9000}; !reflect.DeepEqual(statuses[0].TCPPorts, want) {
		t.Fatalf("first TCP ports = %v, want %v", statuses[0].TCPPorts, want)
	}
	if want := []int{5432}; !reflect.DeepEqual(statuses[1].TCPPorts, want) {
		t.Fatalf("second TCP ports = %v, want %v", statuses[1].TCPPorts, want)
	}
}

func TestLinuxTCPListeningPortsReadsSocketInodes(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(procRoot, "123", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	tcpTable := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  501        0 12345 1
   1: 0100007F:C350 0100007F:01BB 01 00000000:00000000 00:00000000 00000000  501        0 54321 1
`
	if err := os.WriteFile(filepath.Join(procRoot, "net", "tcp"), []byte(tcpTable), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[12345]", filepath.Join(procRoot, "123", "fd", "7")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[54321]", filepath.Join(procRoot, "123", "fd", "8")); err != nil {
		t.Fatal(err)
	}

	got, err := linuxTCPListeningPorts(procRoot, []int{123})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int][]int{123: {8080}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
}

func TestParseLsofTCP(t *testing.T) {
	output := []byte("p100\nf4\nn*:9000\nf5\nn[::1]:8080\np200\nf3\nn127.0.0.1:5432\nf4\nn*:5432\n")
	want := map[int][]int{100: {8080, 9000}, 200: {5432}}
	if got := parseLsofTCP(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
}

func TestFetchGoroutineCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/pprof/goroutine" || r.URL.Query().Get("debug") != "1" {
			t.Fatalf("unexpected request URI: %s", r.URL.RequestURI())
		}
		_, _ = w.Write([]byte("goroutine profile: total 37\n"))
	}))
	defer server.Close()

	got, err := fetchGoroutineCount(server.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	if got != 37 {
		t.Fatalf("goroutine count = %d, want 37", got)
	}
}

func TestCollectGoroutineMetricsLeavesUnavailableCountUnset(t *testing.T) {
	statuses := []Status{{PID: 123, PprofURL: "http://127.0.0.1:1/debug/pprof"}}
	collectGoroutineMetrics(statuses)
	if statuses[0].Goroutines != nil {
		t.Fatalf("goroutines = %v, want unavailable", *statuses[0].Goroutines)
	}
}
