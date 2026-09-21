package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// RealEnv returns probes that talk to the actual machine.
//
// It is separate from Env so that every check can be driven from a test
// without Docker, a listening socket or a filesystem.
func RealEnv() Env {
	return Env{
		LookPath: exec.LookPath,
		Run:      runCommand,
		PortFree: portFree,
		DiskFree: diskFree,
		HomeDir:  os.UserHomeDir,
		GOOS:     runtime.GOOS,
	}
}

// runCommand executes a probe with a bound of its own.
//
// `docker info` against a daemon that is starting can hang for minutes. A
// preflight that hangs is worse than one that reports "not reachable", so the
// probe is capped regardless of the caller's context.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// portFree reports whether a TCP port can be bound.
//
// It binds and immediately closes rather than dialling: a successful dial
// proves something is listening, but a failed dial proves nothing about
// whether *we* could bind, which is the question.
func portFree(host string, port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return err
	}
	return ln.Close()
}

// diskFree returns free bytes on the filesystem holding path.
//
// It walks up to the first existing ancestor, because the path of interest —
// a state directory, say — may not have been created yet, and failing on that
// would report "unknown" for a question that has a perfectly good answer.
func diskFree(path string) (uint64, string, error) {
	dir := path
	for {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err == nil {
			//nolint:unconvert // Bavail is int64 on some platforms, uint64 on others.
			return uint64(st.Bavail) * uint64(st.Bsize), dir, nil
		}
		parent := parentDir(dir)
		if parent == dir {
			return 0, dir, fmt.Errorf("no existing filesystem found above %s", path)
		}
		dir = parent
	}
}

func parentDir(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == os.PathSeparator {
			return p[:i]
		}
	}
	return string(os.PathSeparator)
}
