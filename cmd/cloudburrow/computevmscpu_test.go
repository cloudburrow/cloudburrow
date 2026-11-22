package main

import "testing"

// TestCPUStatSeconds (#2139): a cgroup v2 cpu.stat's usage_usec.
func TestCPUStatSeconds(t *testing.T) {
	got, err := cpuStatSeconds("usage_usec 49123456\nuser_usec 30000000\nsystem_usec 19123456\nnr_periods 0\n")
	if err != nil || got != 49.123456 {
		t.Fatalf("= %v, %v", got, err)
	}
	if _, err := cpuStatSeconds("nr_periods 0\n"); err == nil {
		t.Error("no usage_usec was read")
	}
}
