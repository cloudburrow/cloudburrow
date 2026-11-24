package logagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	ltype "google.golang.org/genproto/googleapis/logging/type"
)

// MaxLineBytes bounds one CRI record the agent reads; Cloud Logging's
// entry limit is 256 KiB.
const MaxLineBytes = 256 * 1024

// batchEntries and batchBytes bound one WriteLogEntries request, below
// Cloud Logging's 1,000 entries and 10 MB per call
// (https://docs.cloud.google.com/logging/quotas).
const (
	batchEntries = 500
	batchBytes   = 4 << 20
)

// Cluster is what the agent reads from the Kubernetes API.
type Cluster interface {
	// Config is the fluentbit-gke-config ConfigMap's configuration.
	Config(ctx context.Context) (Config, error)
	// Pods are the pods of the agent's node.
	Pods(ctx context.Context) ([]Pod, error)
}

// PermanentError is a write Cloud Logging refused for good (a 4xx other
// than 429): its entries are dropped rather than retried forever, as an
// agent drops what the API rejects.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Agent ships one node's container logs.
type Agent struct {
	// Node is the node's name, the entries' compute.googleapis.com/resource_name.
	Node string
	// Root is the node's pod log directory, /var/log/pods.
	Root string
	// Positions is the position file: how far each log file, by inode,
	// has been shipped.
	Positions string
	Cluster   Cluster
	// Write sends one WriteLogEntries request.
	Write func(context.Context, *loggingpb.WriteLogEntriesRequest) error
	Now   func() time.Time
	Every time.Duration
	// Logf reports a change of state (a failing write, then recovery).
	Logf func(format string, args ...any)

	pos     map[string]int64
	failing string
}

// logFile names the files of a container's log directory the agent reads:
// the current one, N.log, and those the kubelet rotated, N.log.<time>,
// before it compresses them.
var logFile = regexp.MustCompile(`^[0-9]+\.log(\.[0-9]{8}-[0-9]{6})?$`)

// Run ships until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Every <= 0 {
		a.Every = time.Second
	}
	a.load()
	t := time.NewTicker(a.Every)
	defer t.Stop()
	for {
		a.Round(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// load reads the position file; a missing or unreadable one is no
// position, so every file is read from its start.
func (a *Agent) load() {
	a.pos = map[string]int64{}
	b, err := os.ReadFile(a.Positions)
	if err != nil {
		return
	}
	var p struct {
		Files map[string]int64 `json:"files"`
	}
	if json.Unmarshal(b, &p) == nil && p.Files != nil {
		a.pos = p.Files
	}
}

// save writes the position file, through a rename so it is never half
// written.
func (a *Agent) save() error {
	b, _ := json.Marshal(map[string]any{"files": a.pos})
	tmp := a.Positions + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.Positions)
}

// Round ships what every log file holds past its position and was written
// before the configuration was read: a line written later waits for the
// next round, so one written after a category is turned off is never
// shipped on a configuration read before.
func (a *Agent) Round(ctx context.Context) {
	if a.pos == nil {
		a.load()
	}
	before := a.Now()
	cfg, err := a.Cluster.Config(ctx)
	if err != nil {
		a.state("read the configuration: %v", err)
		return
	}
	pods, err := a.Cluster.Pods(ctx)
	if err != nil {
		a.state("list the node's pods: %v", err)
		return
	}
	byDir := map[string]Pod{}
	for _, p := range pods {
		byDir[p.Namespace+"_"+p.Name] = p
	}
	seen := map[string]bool{}
	dirs, _ := os.ReadDir(a.Root)
	ok := true
	for _, d := range dirs {
		parts := strings.SplitN(d.Name(), "_", 3)
		if !d.IsDir() || len(parts) != 3 {
			continue
		}
		pod, found := byDir[parts[0]+"_"+parts[1]]
		if !found {
			pod = Pod{Namespace: parts[0], Name: parts[1], UID: parts[2]}
		}
		containers, _ := os.ReadDir(filepath.Join(a.Root, d.Name()))
		for _, c := range containers {
			if !c.IsDir() {
				continue
			}
			src, ship := cfg.Classify(pod, c.Name(), a.Node)
			dir := filepath.Join(a.Root, d.Name(), c.Name())
			for _, f := range containerFiles(dir) {
				id, size := fileID(f)
				if id == "" {
					continue
				}
				seen[id] = true
				if size < a.pos[id] {
					a.pos[id] = 0 // truncated: read again from its start
				}
				if !ok || size <= a.pos[id] {
					continue
				}
				if err := a.ship(ctx, f, id, cfg, src, ship, before); err != nil {
					a.state("write to Cloud Logging: %v", err)
					ok = false
				}
			}
		}
	}
	if ok {
		// The positions of files gone (a pod deleted, a rotated file
		// compressed) are dropped once every file has been read.
		for id := range a.pos {
			if !seen[id] {
				delete(a.pos, id)
			}
		}
		a.state("")
	}
	if err := a.save(); err != nil {
		a.logf("save the positions: %v", err)
	}
}

// state logs a failure once, and recovery from it.
func (a *Agent) state(format string, args ...any) {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	switch {
	case msg == a.failing:
	case msg == "":
		a.logf("shipping again")
	default:
		a.logf("%s; retrying", msg)
	}
	a.failing = msg
}

// containerFiles are a container directory's log files, oldest first: the
// rotated ones by their time, then the current one by restart count.
func containerFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && logFile.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	key := func(n string) (int, string, int) {
		base, rotated, _ := strings.Cut(n, ".log")
		i, _ := strconv.Atoi(base)
		if rotated == "" {
			return i, "~", 0
		}
		return i, rotated, 0
	}
	sort.Slice(out, func(i, j int) bool {
		ai, ar, _ := key(out[i])
		bi, br, _ := key(out[j])
		if ai != bi {
			return ai < bi
		}
		return ar < br
	})
	for i, n := range out {
		out[i] = filepath.Join(dir, n)
	}
	return out
}

// fileID is a file's key in the position file, and its size: its
// container's directory and its inode, which a rotation keeps. The
// directory is part of it because a node reuses a deleted file's inode,
// and a new pod's log must not take the position of one deleted while the
// agent was not running.
func fileID(path string) (string, int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return filepath.Dir(path) + "#" + strconv.FormatUint(uint64(st.Ino), 10), fi.Size()
	}
	return path, fi.Size()
}

// ship writes the records of one file past its position, in batches, and
// moves the position past each batch written. A record not yet ended, or
// written at or after before, ends it for this round.
func (a *Agent) ship(ctx context.Context, path, id string, cfg Config, src Source, ship bool, before time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	start := a.pos[id]
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	r := bufio.NewReaderSize(f, 64*1024)
	offset, committed := start, start
	var batch []*loggingpb.LogEntry
	size := 0
	flush := func() error {
		if len(batch) > 0 {
			err := a.Write(ctx, &loggingpb.WriteLogEntriesRequest{Entries: batch, PartialSuccess: true})
			var perm *PermanentError
			if err != nil && !errors.As(err, &perm) {
				return err
			}
			if err != nil {
				a.logf("Cloud Logging refused %d entries of %s: %v", len(batch), path, err)
			}
		}
		batch, size = nil, 0
		a.pos[id] = committed
		return nil
	}
	partial := map[string]string{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break // the end, or a record still being written
		}
		next := offset + int64(len(line))
		ts, stream, text, complete, parsed := ParseCRILine(strings.TrimSuffix(line, "\n"))
		if parsed && !ts.Before(before) {
			break
		}
		offset = next
		if !parsed {
			if len(partial) == 0 {
				committed = offset
			}
			continue
		}
		if !complete {
			if len(partial[stream])+len(text) < MaxLineBytes {
				partial[stream] += text
			}
			continue
		}
		text, partial[stream] = partial[stream]+text, ""
		delete(partial, stream)
		if len(partial) == 0 {
			committed = offset
		}
		if !ship || !cfg.Ships(src.Category, ts) {
			continue
		}
		log := src.Log
		if log == "" {
			log = stream
		}
		e := Entry(cfg.Project, log, src.Resource, src.Labels, ts, text)
		if e.Severity == ltype.LogSeverity_DEFAULT {
			e.Severity = ltype.LogSeverity_INFO
			if stream == "stderr" {
				e.Severity = ltype.LogSeverity_ERROR
			}
		}
		if e.InsertId == "" {
			e.InsertId = insertID(a.Node, id, next-int64(len(line)))
		}
		batch = append(batch, e)
		size += len(text) + 512
		if len(batch) >= batchEntries || size >= batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// insertID is a line's insertId: its node, file and offset, the same each
// time it is shipped.
func insertID(node, id string, offset int64) string {
	h := fnv.New64a()
	h.Write([]byte(node + "\x00" + id))
	return strconv.FormatUint(h.Sum64(), 36) + "-" + strconv.FormatInt(offset, 36)
}
