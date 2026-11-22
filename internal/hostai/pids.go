package hostai

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Pids is the file that records the running servers of one instance, so a
// CLI that was killed before it could stop them (SIGKILL, a crash) has
// them ended by the next run: `up` reaps them before starting any.
type Pids struct {
	File string
	// Command, when set, replaces reading a process's command line with
	// ps (tests).
	Command func(pid int) (string, error)

	mu sync.Mutex
}

type pidEntry struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Path string `json:"path"`
}

func (p *Pids) read() []pidEntry {
	b, err := os.ReadFile(p.File)
	if err != nil {
		return nil
	}
	var out []pidEntry
	_ = json.Unmarshal(b, &out)
	return out
}

func (p *Pids) write(es []pidEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	if len(es) == 0 {
		_ = os.Remove(p.File)
		return
	}
	b, _ := json.MarshalIndent(es, "", "  ")
	_ = os.MkdirAll(filepath.Dir(p.File), 0o755)
	tmp := p.File + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.File)
	}
}

// Add records a started server.
func (p *Pids) Add(name string, pid int, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	es := p.read()
	out := es[:0]
	for _, e := range es {
		if e.Name != name {
			out = append(out, e)
		}
	}
	p.write(append(out, pidEntry{Name: name, PID: pid, Path: path}))
}

// Remove forgets a server that ended.
func (p *Pids) Remove(name string, pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	es := p.read()
	out := es[:0]
	for _, e := range es {
		if e.Name != name || (pid != 0 && e.PID != pid) {
			out = append(out, e)
		}
	}
	p.write(out)
}

func (p *Pids) command(pid int) (string, error) {
	if p.Command != nil {
		return p.Command(pid)
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}

// Reap ends every recorded server that is still running, and returns their
// names. A process is ended only when its command line still starts with
// the recorded executable: a pid the system has since given to another
// program is left alone.
func (p *Pids) Reap() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var reaped []string
	for _, e := range p.read() {
		if e.PID <= 1 || e.Path == "" {
			continue
		}
		cmd, err := p.command(e.PID)
		if err != nil || !strings.HasPrefix(cmd, e.Path) {
			continue
		}
		_ = syscall.Kill(-e.PID, syscall.SIGKILL)
		_ = syscall.Kill(e.PID, syscall.SIGKILL)
		reaped = append(reaped, e.Name)
	}
	p.write(nil)
	return reaped
}
