package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// procStartTime is when pid started, as the kernel records it (kern.proc.pid's
// p_starttime). It changes when the pid is reused, which is what identifies
// an `up` beside its pid (#820).
func procStartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(kp.Proc.P_pid) != pid {
		return "", fmt.Errorf("no process %d", pid)
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("darwin:%d.%06d", tv.Sec, tv.Usec), nil
}
