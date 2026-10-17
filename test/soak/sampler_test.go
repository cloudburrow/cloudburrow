//go:build soak

package soak

import (
	"context"
	"encoding/json"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sample is one reading of the instance's resources.
type sample struct {
	At time.Duration `json:"at"`
	// The host `cloudburrow up` process, which serves Tasks, Secret Manager
	// and Logging in process and fronts every other service.
	HostRSSKB int64 `json:"host_rss_kb,omitempty"`
	HostFDs   int   `json:"host_fds,omitempty"`
	HostThr   int   `json:"host_threads,omitempty"`
	// Each pod container, keyed pod/container.
	Containers map[string]containerSample `json:"containers,omitempty"`
	// The state directory's size on disk.
	StateBytes int64 `json:"state_bytes,omitempty"`
}

type containerSample struct {
	AnonKB   int64 `json:"anon_kb"`
	Threads  int   `json:"threads"`
	FDs      int   `json:"fds"`
	Restarts int   `json:"restarts"`
}

// sampler reads, every interval, the host process's resident memory, file
// descriptors and threads (ps and lsof), each pod container's (through
// crictl and the container's cgroup on the kind node, so no metrics-server is needed), the
// containers' restart counts and the state directory's size.
//
// The Go runtime's goroutine count is not exposed by the instance: neither
// the front nor the pods serve pprof. Threads stand in for it: a goroutine
// leak that blocks in syscalls shows as threads, and one that holds memory
// shows as RSS.
type sampler struct {
	hostPID  string
	node     string // the kind node container, cloudburrow-<name>-control-plane
	stateDir string

	mu      sync.Mutex
	samples []sample
}

func (s *sampler) run(ctx context.Context, start time.Time, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.take(time.Since(start))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *sampler) take(at time.Duration) {
	// Its own context: the last reading is taken after the run's is done.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sm := sample{At: at.Round(time.Second)}
	if s.hostPID != "" {
		if out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", s.hostPID).Output(); err == nil {
			sm.HostRSSKB, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		}
		if out, err := exec.CommandContext(ctx, "ps", "-M", "-p", s.hostPID).Output(); err == nil {
			sm.HostThr = len(strings.Split(strings.TrimSpace(string(out)), "\n")) - 1
		}
		if out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-p", s.hostPID).Output(); err == nil {
			sm.HostFDs = len(strings.Split(strings.TrimSpace(string(out)), "\n")) - 1
		}
	}
	if s.node != "" {
		sm.Containers = s.containers(ctx)
	}
	if s.stateDir != "" {
		_ = filepath.WalkDir(s.stateDir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					sm.StateBytes += fi.Size()
				}
			}
			return nil
		})
	}
	s.mu.Lock()
	s.samples = append(s.samples, sm)
	s.mu.Unlock()
}

// containers lists the node's CloudBurrow containers with crictl and reads
// each one's /proc entry on the node. One docker exec reads them all.
func (s *sampler) containers(ctx context.Context) map[string]containerSample {
	out, err := exec.CommandContext(ctx, "docker", "exec", s.node, "crictl", "ps", "-o", "json").Output()
	if err != nil {
		return nil
	}
	var ps struct {
		Containers []struct {
			ID       string `json:"id"`
			Metadata struct {
				Name    string `json:"name"`
				Attempt int    `json:"attempt"`
			} `json:"metadata"`
			Labels map[string]string `json:"labels"`
		} `json:"containers"`
	}
	if json.Unmarshal(out, &ps) != nil {
		return nil
	}
	res := map[string]containerSample{}
	var script strings.Builder
	keys := map[string]string{}
	for _, c := range ps.Containers {
		if c.Labels["io.kubernetes.pod.namespace"] == "kube-system" || c.Labels["io.kubernetes.pod.namespace"] == "local-path-storage" {
			continue
		}
		pod := c.Labels["io.kubernetes.pod.name"]
		if i := strings.Index(pod, "-"); i > 0 {
			pod = pod[:i] // the deployment's name: stable across restarts
		}
		key := pod + "/" + c.Metadata.Name
		keys[c.ID] = key
		res[key] = containerSample{Restarts: c.Metadata.Attempt}
		// The container's cgroup, read on the node: its anonymous memory in
		// kB (page cache left out: Storage's would swamp it), its tasks, which
		// are threads, and the open fds of every process in it. A container
		// whose entrypoint is a shell that starts the JVM is measured whole.
		script.WriteString(`p=$(crictl inspect -o go-template --template '{{.info.pid}}' ` + c.ID + `); ` +
			`g=/sys/fs/cgroup$(cut -d: -f3 /proc/$p/cgroup); f=0; ` +
			`for q in $(cat $g/cgroup.procs); do f=$((f+$(ls /proc/$q/fd | wc -l))); done; ` +
			`echo ` + c.ID + ` $(($(awk '/^anon /{print $2}' $g/memory.stat)/1024)) $(cat $g/pids.current) $f; `)
	}
	out, err = exec.CommandContext(ctx, "docker", "exec", s.node, "sh", "-c", script.String()).Output()
	if err != nil && len(out) == 0 {
		return res
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		key, ok := keys[f[0]]
		if !ok {
			continue
		}
		cs := res[key]
		cs.AnonKB, _ = strconv.ParseInt(f[1], 10, 64)
		cs.Threads, _ = strconv.Atoi(f[2])
		cs.FDs, _ = strconv.Atoi(f[3])
		res[key] = cs
	}
	return res
}

func (s *sampler) all() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sample(nil), s.samples...)
}
