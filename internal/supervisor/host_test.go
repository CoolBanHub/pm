package supervisor

import "testing"

func TestCachedHostInfo(t *testing.T) {
	info := CachedHostInfo()
	if info == nil {
		t.Fatal("CachedHostInfo returned nil")
	}
	if info.CPUCount <= 0 {
		t.Errorf("cpu count = %d, want > 0", info.CPUCount)
	}
	if info.TotalMemory <= 0 {
		t.Errorf("total memory = %d, want > 0", info.TotalMemory)
	}
	// Cached: second call returns the same pointer.
	if CachedHostInfo() != info {
		t.Error("CachedHostInfo did not return the cached pointer")
	}
}
