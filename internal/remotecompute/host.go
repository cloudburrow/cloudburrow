package remotecompute

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// HostInfo is what the remote host is, as Prerequisites read it.
type HostInfo struct {
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	CPUs          int    `json:"cpus,omitempty"`
	MemoryBytes   uint64 `json:"memoryBytes,omitempty"`
	MemoryFree    uint64 `json:"memoryAvailableBytes,omitempty"`
	DiskFree      uint64 `json:"diskFreeBytes,omitempty"`
	DiskPath      string `json:"diskPath,omitempty"`
	DockerVersion string `json:"dockerVersion,omitempty"`
	CgroupV2      bool   `json:"cgroupV2"`
	// Kind is the kind release on the host's PATH, if any. kind runs on
	// this machine against the host's Docker, so none is needed there.
	Kind string `json:"kind,omitempty"`
}

// Size is the host's size in words: "8 CPUs, 31.3 GiB memory, 120.0 GiB
// disk free".
func (h HostInfo) Size() string {
	parts := []string{fmt.Sprintf("%d CPUs", h.CPUs), gib(h.MemoryBytes) + " memory"}
	if h.DiskFree > 0 {
		parts = append(parts, gib(h.DiskFree)+" disk free")
	}
	return strings.Join(parts, ", ")
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// The host's minimums: what one instance with the default services needs.
const (
	MinCPUs        = 2
	MinMemoryBytes = 4 << 30
	MinDiskBytes   = 20 << 30
)

// Check is one prerequisite, in plain words.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// probeScript prints key=value lines about the host. It needs only POSIX
// sh and /proc, so it runs on any Linux.
const probeScript = `echo "os=$(uname -s)"; echo "arch=$(uname -m)"; echo "kernel=$(uname -r)"
echo "cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"
awk '/^MemTotal:/{print "memkb="$2} /^MemAvailable:/{print "memavailkb="$2}' /proc/meminfo 2>/dev/null
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then echo cgroup2=yes; else echo cgroup2=no; fi
s=/var/run/docker.sock; [ -n "$1" ] && s="$1"
if [ -S "$s" ]; then echo sock=yes; else echo sock=no; fi
if [ -r "$s" ] && [ -w "$s" ]; then echo sockrw=yes; else echo sockrw=no; fi
if command -v kind >/dev/null 2>&1; then echo "kind=$(kind version 2>/dev/null | awk '{print $2}')"; fi
echo "user=$(id -un)"`

// Probe reads the host's facts over the connection.
func (s *Session) Probe(ctx context.Context) (HostInfo, map[string]string, error) {
	out, err := s.Run(ctx, "sh -c "+shellQuote(probeScript)+" probe "+shellQuote(s.opts.RemoteDockerSocket))
	if err != nil {
		return HostInfo{}, nil, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	h := HostInfo{OS: kv["os"], Arch: kv["arch"], Kernel: kv["kernel"], CgroupV2: kv["cgroup2"] == "yes", Kind: kv["kind"]}
	h.CPUs, _ = strconv.Atoi(kv["cpus"])
	if kb, err := strconv.ParseUint(kv["memkb"], 10, 64); err == nil {
		h.MemoryBytes = kb << 10
	}
	if kb, err := strconv.ParseUint(kv["memavailkb"], 10, 64); err == nil {
		h.MemoryFree = kb << 10
	}
	return h, kv, nil
}

// docker runs the docker CLI on this machine against the host's Docker.
func (s *Session) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(dockerFreeEnv(), "DOCKER_HOST="+s.DockerHost())
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// dockerFreeEnv is this process's environment without the variables that
// would point docker elsewhere.
func dockerFreeEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "DOCKER_HOST=") || strings.HasPrefix(e, "DOCKER_CONTEXT=") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// DiskFree is the free space on the host's filesystem holding path, and
// the path measured, as df reports it.
func (s *Session) DiskFree(ctx context.Context, path string) (uint64, string, error) {
	out, err := s.Run(ctx, "df -Pk -- "+shellQuote(path)+" | tail -n 1")
	if err != nil {
		return 0, "", err
	}
	f := strings.Fields(out)
	if len(f) < 6 {
		return 0, "", fmt.Errorf("df gave %q", strings.TrimSpace(out))
	}
	kb, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("df gave %q", strings.TrimSpace(out))
	}
	return kb << 10, f[5] + " on " + s.opts.Target.Host, nil
}

// Checks reads the host and says, check by check, whether it can run an
// instance: Linux, Docker reachable to this account, cgroup v2 for kind's
// nodes, the reverse forwards workloads reach this machine through, and
// the CPU, memory and disk minimums.
func (s *Session) Checks(ctx context.Context) (HostInfo, []Check) {
	h, kv, err := s.Probe(ctx)
	if err != nil {
		return h, []Check{{Name: "remote host", Detail: "could not read the host: " + err.Error(),
			Remedy: "check that the account can run a shell on " + s.opts.Target.Host}}
	}
	var cs []Check
	add := func(c Check) { cs = append(cs, c) }
	add(Check{Name: "remote host", OK: h.OS == "Linux",
		Detail: fmt.Sprintf("%s %s (%s), signed in as %s", h.OS, h.Kernel, h.Arch, kv["user"]),
		Remedy: "the compute host must run Linux"})
	switch {
	case kv["sock"] != "yes":
		add(Check{Name: "remote Docker", Detail: "Docker is not installed or not running on the host (no " + s.opts.RemoteDockerSocket + ")",
			Remedy: "install Docker Engine on the host (https://docs.docker.com/engine/install/) and start it"})
	case kv["sockrw"] != "yes":
		add(Check{Name: "remote Docker", Detail: "Docker runs on the host, but the account " + kv["user"] + " may not use it",
			Remedy: "add the account to the docker group on the host: sudo usermod -aG docker " + kv["user"] + ", then sign in again"})
	default:
		v, err := s.docker(ctx, "version", "--format", "{{.Server.Version}}")
		if err != nil {
			add(Check{Name: "remote Docker", Detail: "Docker on the host did not answer through the SSH connection: " + v,
				Remedy: "check that sshd allows stream-local forwarding (AllowStreamLocalForwarding yes, the default)"})
		} else {
			h.DockerVersion = v
			add(Check{Name: "remote Docker", OK: true, Detail: "Docker " + v + ", reached through the SSH connection"})
			add(s.GatewayCheck(ctx))
		}
	}
	if h.Kind != "" {
		add(Check{Name: "remote kind", OK: true, Detail: "kind " + h.Kind + " is on the host; CloudBurrow runs its own kind from this machine against the host's Docker"})
	} else {
		add(Check{Name: "remote kind", OK: true, Detail: "not needed on the host: kind runs on this machine against the host's Docker"})
	}
	add(Check{Name: "remote cgroups", OK: h.CgroupV2, Warn: !h.CgroupV2,
		Detail: map[bool]string{true: "cgroup v2, as kind's nodes need", false: "cgroup v1: kind's nodes may not start"}[h.CgroupV2],
		Remedy: "boot the host with cgroup v2 (systemd.unified_cgroup_hierarchy=1)"})
	add(Check{Name: "remote CPU", OK: h.CPUs >= MinCPUs, Warn: h.CPUs < MinCPUs,
		Detail: fmt.Sprintf("%d CPUs", h.CPUs), Remedy: fmt.Sprintf("use a host with at least %d CPUs", MinCPUs)})
	add(Check{Name: "remote memory", OK: h.MemoryBytes >= MinMemoryBytes, Warn: h.MemoryBytes < MinMemoryBytes,
		Detail: fmt.Sprintf("%s, %s available", gib(h.MemoryBytes), gib(h.MemoryFree)),
		Remedy: "use a host with at least " + gib(MinMemoryBytes)})
	root := "/var/lib/docker"
	if out, err := s.docker(ctx, "info", "--format", "{{.DockerRootDir}}"); err == nil && strings.HasPrefix(out, "/") {
		root = out
	}
	if free, where, err := s.DiskFree(ctx, root); err != nil {
		add(Check{Name: "remote disk", Warn: true, OK: true, Detail: "could not measure " + root + ": " + err.Error()})
	} else {
		h.DiskFree, h.DiskPath = free, where
		add(Check{Name: "remote disk", OK: free >= MinDiskBytes, Warn: free < MinDiskBytes,
			Detail: gib(free) + " free on " + where, Remedy: "free space on the host, or give it a larger disk (at least " + gib(MinDiskBytes) + ")"})
	}
	return h, cs
}

// Prerequisites is Checks with the blocking failures as plain sentences:
// a host that is not Linux or whose Docker this account cannot use.
func (s *Session) Prerequisites(ctx context.Context) (HostInfo, []string) {
	h, cs := s.Checks(ctx)
	var problems []string
	for _, c := range cs {
		if !c.OK && !c.Warn {
			p := c.Detail
			if c.Remedy != "" {
				p += ": " + c.Remedy
			}
			problems = append(problems, p)
		}
	}
	return h, problems
}

// GatewayCheck asks the host's sshd to listen on the default Docker
// bridge's gateway, as every reverse forward does, and says whether it
// may: sshd allows it only with GatewayPorts clientspecified.
func (s *Session) GatewayCheck(ctx context.Context) Check {
	gw, err := s.docker(ctx, "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return Check{Name: "remote forwarding", Warn: true, OK: true, Detail: "could not read the host's Docker bridge: " + gw}
	}
	ip := strings.Fields(gw)
	if len(ip) == 0 {
		return Check{Name: "remote forwarding", Warn: true, OK: true, Detail: "the host's Docker bridge has no gateway"}
	}
	bound, cancel, err := s.Reverse(ctx, ip[0]+":0", "127.0.0.1:9")
	if err != nil {
		return Check{Name: "remote forwarding", Detail: "the host's SSH server does not let CloudBurrow listen on its Docker network, which is how workloads reach this machine",
			Remedy: "set GatewayPorts clientspecified in the host's /etc/ssh/sshd_config and reload sshd"}
	}
	_ = cancel()
	return Check{Name: "remote forwarding", OK: true, Detail: "workloads on the host reach this machine through the SSH connection (tested on " + bound + ")"}
}
