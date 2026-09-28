package main

import (
	"fmt"
	"os"
	"strings"
)

// procStartTime is when pid started, as the kernel records it: field 22 of
// /proc/<pid>/stat, in clock ticks since boot. It changes when the pid is
// reused, which is what identifies an `up` beside its pid (#820).
func procStartTime(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The command name, field 2, is parenthesised and may hold spaces or
	// parentheses of its own, so the fields are counted after its last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state), so field 22 (starttime) is f[19].
	if len(f) < 20 {
		return "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	return "linux:" + f[19], nil
}
