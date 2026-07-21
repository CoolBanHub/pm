package supervisor

import (
	"net/http"
	"net/http/httptest"
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
