package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
)

// portRange is an inclusive range of ports.
type portRange struct{ lo, hi int }

// ephemeralPortRange is the range the OS picks a socket's local port from
// when none is asked for: every outgoing connection, and every listener on
// port 0. ok is false when it cannot be read.
//
// A fixed host port inside it can be taken, between the preflight and the
// bind, by any outgoing connection on the machine. That is what failed the
// Offline workflow (#1526): at --port-base 40200, inside Linux's
// 32768–60999, the Bigtable tunnel's 40212 was the local port of a
// connection already open, most likely one of the instance's own
// long-lived connections to the kind API server (the Storage and Pub/Sub
// tunnels hold one each), and kubectl could not listen on it. On Linux a
// listener cannot share a local address with a connected socket; on macOS
// it can, so the same base passes there.
var ephemeralPortRange = func() (portRange, bool) {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
		if err != nil {
			return portRange{}, false
		}
		return parsePortRange(string(b))
	case "darwin", "freebsd":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sysctl", "-n", "net.inet.ip.portrange.first", "net.inet.ip.portrange.last").Output()
		if err != nil {
			return portRange{}, false
		}
		return parsePortRange(string(out))
	default:
		return portRange{}, false
	}
}

// parsePortRange reads "lo hi", separated by any white space.
func parsePortRange(s string) (portRange, bool) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return portRange{}, false
	}
	lo, err1 := strconv.Atoi(f[0])
	hi, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || lo <= 0 || hi < lo || hi > 65535 {
		return portRange{}, false
	}
	return portRange{lo, hi}, true
}

// ephemeralCollisions names, sorted by port, each fixed host port of the
// instance that lies inside r.
func ephemeralCollisions(ports map[string]int, r portRange) []string {
	names := make([]string, 0, len(ports))
	for name, p := range ports {
		if p >= r.lo && p <= r.hi {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return ports[a] - ports[b] })
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%s %d", n, ports[n])
	}
	return out
}

// ephemeralPortsResult is doctor's finding on whether the instance's fixed
// host ports lie in the OS's ephemeral range. It is a warning, not a
// failure: the ports are free now, and usually still free when bound.
func ephemeralPortsResult(cfg config.Config) doctor.Result {
	name := "ports outside the ephemeral range"
	r, ok := ephemeralPortRange()
	if !ok {
		return doctor.Result{Name: name, Level: doctor.LevelOK, Detail: "the OS's ephemeral port range could not be read; not checked"}
	}
	hit := ephemeralCollisions(hostPorts(cfg), r)
	if len(hit) == 0 {
		return doctor.Result{Name: name, Level: doctor.LevelOK, Detail: fmt.Sprintf("no fixed port is in %d–%d", r.lo, r.hi)}
	}
	return doctor.Result{
		Name:  name,
		Level: doctor.LevelWarn,
		Detail: fmt.Sprintf("%d fixed port(s) are in the OS's ephemeral range %d–%d (%s): any outgoing connection "+
			"can take one before `up` binds it", len(hit), r.lo, r.hi, strings.Join(hit, ", ")),
		Remedy: fmt.Sprintf("choose a --port-base whose block (base to base+%d) is below %d",
			65535-config.MaxPortBase(), r.lo),
	}
}
