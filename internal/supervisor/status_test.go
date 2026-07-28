package supervisor

import "testing"

func TestStatusExposesDescription(t *testing.T) {
	program := testProgram("sleeper", "sleep 30")
	program.Description = "中文备注"
	status := NewProcess(program).Status()
	if status.Description != "中文备注" {
		t.Fatalf("status.Description = %q, want %q", status.Description, "中文备注")
	}
}
