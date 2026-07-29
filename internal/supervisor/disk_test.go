package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalkTotalSize(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), 10)
	writeFile(t, filepath.Join(dir, "b"), 20)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub", "c"), 7)
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	total, err := walkTotalSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total != 37 { // symlinks are not counted
		t.Errorf("total = %d, want 37", total)
	}
}

func TestCollectDirFingerprint(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), 5)

	fp1, err := collectDirFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	fp2, _ := collectDirFingerprint(dir)
	if fp1 != fp2 {
		t.Error("fingerprint changed without any filesystem change")
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fp3, _ := collectDirFingerprint(dir)
	if fp3 == fp1 {
		t.Error("fingerprint did not change after adding a subdirectory")
	}
}

// TestDiskMonitorMeasuresWorkingDir drives the monitor end to end: after the
// background scan runs, Apply reports the working directory's true size.
func TestDiskMonitorMeasuresWorkingDir(t *testing.T) {
	stateDir := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(work, "bin"), 1234)

	monitor, err := NewDiskMonitor(stateDir, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Stop()

	statuses := []Status{{Directory: work}}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		monitor.Apply(statuses)
		if statuses[0].DiskUsage == 1234 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if statuses[0].DiskUsage != 1234 {
		t.Fatalf("disk usage = %d, want 1234", statuses[0].DiskUsage)
	}
}

// TestDiskMonitorPrunesUnchanged verifies the directory-mtime pruning: when a
// working directory is unchanged between scans, the expensive full walk is
// skipped; when a new subdirectory appears, the walk runs again.
func TestDiskMonitorPrunesUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(work, "data"), 99)

	// Long interval so the background ticker never fires during the test; we
	// drive scans explicitly via scanAll.
	monitor, err := NewDiskMonitor(stateDir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Stop()

	statuses := []Status{{Directory: work}}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		monitor.Apply(statuses)
		if statuses[0].DiskUsage == 99 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if statuses[0].DiskUsage != 99 {
		t.Fatalf("initial scan: disk usage = %d, want 99", statuses[0].DiskUsage)
	}
	base := atomic.LoadInt64(&monitor.walkCount)
	if base == 0 {
		t.Fatal("expected the initial scan to walk at least once")
	}

	monitor.scanAll() // unchanged -> should prune
	if got := atomic.LoadInt64(&monitor.walkCount); got != base {
		t.Errorf("pruned scan walked %d time(s), expected skip (base=%d)", got-base, base)
	}

	if err := os.Mkdir(filepath.Join(work, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	monitor.scanAll() // fingerprint changed -> should walk
	if got := atomic.LoadInt64(&monitor.walkCount); got != base+1 {
		t.Errorf("changed scan walked %d time(s), expected 1 (base=%d)", got-base, base)
	}
}

func TestDiskMonitorDiskTotal(t *testing.T) {
	monitor, err := NewDiskMonitor(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Stop()

	// Register the temp dir as a tracked working directory, then ensure the
	// backing filesystem capacity is positive.
	monitor.Apply([]Status{{Directory: t.TempDir()}})
	if total := monitor.DiskTotal(); total <= 0 {
		t.Errorf("disk total = %d, want > 0", total)
	}
}
