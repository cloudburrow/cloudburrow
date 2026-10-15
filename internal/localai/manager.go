package localai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func waitTick() { time.Sleep(10 * time.Millisecond) }

// Errors the console maps to statuses (#1306).
var (
	// ErrNotPinned means the model has no pinned revision and digests, so it
	// is never downloaded: what would be fetched could not be verified.
	ErrNotPinned = errors.New("model is not pinned")
	// ErrSizeNotConfirmed means the caller did not confirm the exact size.
	ErrSizeNotConfirmed = errors.New("download size not confirmed")
	// ErrBusy means a download is running, or the model is loaded.
	ErrBusy = errors.New("model is busy")
	// ErrNotLoadable means the runtime cannot load or unload this model.
	ErrNotLoadable = errors.New("model cannot be loaded here")
)

// Runtime loads and unloads a model into the running endpoint. Only the
// embedding runtime does today: the generation runtime is a container
// started with -local-ai-model, and has no unload.
type Runtime interface {
	// Loads reports whether the runtime can load m, and why not.
	Loads(m Model) (bool, string)
	// Load opens m from its verified files in dir and serves it.
	Load(m Model, dir string) error
	// Unload stops serving m and releases it.
	Unload(m Model) error
	// Loaded reports whether m is being served.
	Loaded(m Model) bool
}

// FileState is one pinned file's cache state.
type FileState struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Present  bool   `json:"present"`
	Verified bool   `json:"verified"`
}

// ModelState is one catalogued model as the Local models page shows it.
type ModelState struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Publisher string `json:"publisher"`
	Official  bool   `json:"official"`
	Access    string `json:"access"`
	License   string `json:"license"`
	Modality  string `json:"modality"`
	Notes     string `json:"notes"`
	// Pinned models alone can be downloaded.
	Pinned   bool        `json:"pinned"`
	Revision string      `json:"revision,omitempty"`
	Bytes    int64       `json:"bytes,omitempty"`
	Files    []FileState `json:"files,omitempty"`
	// State is not-pinned, absent, partial, corrupt, downloading or
	// downloaded.
	State string `json:"state"`
	// Downloaded is the bytes verified on disk, or fetched so far while
	// downloading.
	Downloaded int64 `json:"downloaded"`
	// Error is the last download's failure.
	Error string `json:"error,omitempty"`
	// DownloadNote says why Download is not offered.
	DownloadNote string `json:"downloadNote,omitempty"`
	// Loadable is whether Load and Unload are offered; LoadNote why not.
	Loadable bool   `json:"loadable"`
	LoadNote string `json:"loadNote,omitempty"`
	Loaded   bool   `json:"loaded"`
}

// Manager downloads, verifies, loads, unloads and deletes pinned models for
// the console (#1306). It downloads only a pinned model, from the pinned
// revision, verifying each file's SHA-256 (FetchPinned, the code
// -local-ai-embeddings uses), and only once the caller has confirmed the
// exact size.
type Manager struct {
	Cache Cache
	// Runtime, nil when no local AI endpoint is running, loads models.
	Runtime Runtime

	mu   sync.Mutex
	jobs map[string]*job
	// verified memoises a file's digest check by path, size and mtime: a
	// 300 MB file is not hashed again on every listing.
	verified map[string]string
}

type job struct {
	running bool
	base    int64
	files   map[string]int64
	err     string
	cancel  context.CancelFunc
}

// States lists every catalogued model.
func (g *Manager) States() []ModelState {
	out := []ModelState{}
	for _, m := range Models() {
		out = append(out, g.State(m))
	}
	return out
}

// State reports one model.
func (g *Manager) State(m Model) ModelState {
	s := ModelState{ID: m.ID, Repo: m.Repo, Publisher: string(m.Publisher), Official: m.Official,
		Access: string(m.Access), License: m.License, Modality: string(m.Modality), Notes: m.Notes}
	if g.Runtime != nil {
		s.Loaded = g.Runtime.Loaded(m)
		s.Loadable, s.LoadNote = g.Runtime.Loads(m)
	} else {
		s.LoadNote = "no local AI endpoint is running; start with -local-ai-embeddings to serve a loaded model"
	}
	if m.Pin == nil {
		s.State = "not-pinned"
		s.DownloadNote = "no pinned revision and digests, so the console never downloads it"
		if m.RequiresCredentials() {
			s.DownloadNote += "; it is also gated behind the " + m.License + " licence"
		}
		if s.Loadable {
			s.Loadable, s.LoadNote = false, "not pinned, so it is never downloaded or loaded here"
		}
		return s
	}
	s.Pinned, s.Revision, s.Bytes = true, m.Pin.Revision, m.Pin.TotalBytes()
	if m.RequiresCredentials() {
		s.DownloadNote = "gated: the console downloads only ungated models"
	}
	present, verified := 0, 0
	for _, f := range m.Pin.Files {
		fs := FileState{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256}
		fs.Present, fs.Verified = g.check(g.Cache.PinnedPath(m, f), f.SHA256)
		if fs.Present {
			present++
		}
		if fs.Verified {
			verified++
			s.Downloaded += f.Bytes
		}
		s.Files = append(s.Files, fs)
	}
	switch {
	case verified == len(m.Pin.Files):
		s.State = "downloaded"
	case present > verified:
		s.State = "corrupt"
	case present > 0:
		s.State = "partial"
	default:
		s.State = "absent"
	}
	g.mu.Lock()
	if j := g.jobs[m.ID]; j != nil {
		s.Error = j.err
		if j.running {
			s.State = "downloading"
			s.Downloaded = j.base
			for _, n := range j.files {
				s.Downloaded += n
			}
		}
	}
	g.mu.Unlock()
	return s
}

// check reports whether path exists and has the digest want.
func (g *Manager) check(path, want string) (present, verified bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false, false
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	g.mu.Lock()
	got, ok := g.verified[key]
	g.mu.Unlock()
	if !ok {
		if got, err = fileSHA256(path); err != nil {
			return true, false
		}
		g.mu.Lock()
		if g.verified == nil {
			g.verified = map[string]string{}
		}
		g.verified[key] = got
		g.mu.Unlock()
	}
	return true, got == want
}

// pinned looks up a model that may be downloaded.
func pinned(id string) (Model, error) {
	m, err := Lookup(id)
	if err != nil {
		return Model{}, err
	}
	if m.Pin == nil {
		return Model{}, fmt.Errorf("%w: %s has no pinned revision and digests, so it is never downloaded", ErrNotPinned, m.ID)
	}
	return m, nil
}

// Download starts fetching a pinned, ungated model in the background.
// confirmBytes must be the pinned total size: it is the size the user was
// shown and agreed to.
func (g *Manager) Download(id string, confirmBytes int64) error {
	m, err := pinned(id)
	if err != nil {
		return err
	}
	if m.RequiresCredentials() {
		return fmt.Errorf("%w: %s", ErrGated, m.ID)
	}
	if confirmBytes != m.Pin.TotalBytes() {
		return fmt.Errorf("%w: %s is %d bytes; confirm that size to download it", ErrSizeNotConfirmed, m.ID, m.Pin.TotalBytes())
	}
	var base int64
	for _, f := range m.Pin.Files {
		if _, ok := g.check(g.Cache.PinnedPath(m, f), f.SHA256); ok {
			base += f.Bytes
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jobs == nil {
		g.jobs = map[string]*job{}
	}
	if j := g.jobs[m.ID]; j != nil && j.running {
		return fmt.Errorf("%w: %s is already downloading", ErrBusy, m.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{running: true, base: base, files: map[string]int64{}, cancel: cancel}
	g.jobs[m.ID] = j
	go func() {
		err := g.Cache.FetchPinned(ctx, m, func(file string, done int64) {
			g.mu.Lock()
			j.files[file] = done
			g.mu.Unlock()
		})
		g.mu.Lock()
		defer g.mu.Unlock()
		j.running = false
		cancel()
		if errors.Is(err, context.Canceled) {
			j.err = "cancelled"
		} else if err != nil {
			j.err = err.Error()
		}
	}()
	return nil
}

// Wait blocks until id's download, if any, has finished. For tests.
func (g *Manager) Wait(ctx context.Context, id string) error {
	for {
		g.mu.Lock()
		j := g.jobs[id]
		running := j != nil && j.running
		g.mu.Unlock()
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		waitTick()
	}
}

// Cancel stops a running download; what was fetched and verified stays.
func (g *Manager) Cancel(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := g.jobs[strings.ToLower(strings.TrimSpace(id))]
	if j == nil || !j.running {
		return fmt.Errorf("%w: no download of %s is running", ErrNotCached, id)
	}
	j.cancel()
	return nil
}

func (g *Manager) loadable(id string) (Model, error) {
	m, err := pinned(id)
	if err != nil {
		return Model{}, err
	}
	if g.Runtime == nil {
		return Model{}, fmt.Errorf("%w: no local AI endpoint is running; start with -local-ai-embeddings", ErrNotLoadable)
	}
	if ok, why := g.Runtime.Loads(m); !ok {
		return Model{}, fmt.Errorf("%w: %s", ErrNotLoadable, why)
	}
	return m, nil
}

// Load serves a downloaded model, after verifying every file again.
func (g *Manager) Load(id string) error {
	m, err := g.loadable(id)
	if err != nil {
		return err
	}
	if err := g.Cache.VerifyPinned(m); err != nil {
		return err
	}
	return g.Runtime.Load(m, g.Cache.PinnedDir(m))
}

// Unload stops serving a model.
func (g *Manager) Unload(id string) error {
	m, err := g.loadable(id)
	if err != nil {
		return err
	}
	return g.Runtime.Unload(m)
}

// Delete removes a model's local copy: the directory of its pinned revision.
// A loaded model, or one downloading, is refused.
func (g *Manager) Delete(id string) error {
	m, err := pinned(id)
	if err != nil {
		return err
	}
	if g.Runtime != nil && g.Runtime.Loaded(m) {
		return fmt.Errorf("%w: %s is loaded; unload it first", ErrBusy, m.ID)
	}
	g.mu.Lock()
	j := g.jobs[m.ID]
	running := j != nil && j.running
	if j != nil && !running {
		delete(g.jobs, m.ID)
	}
	g.mu.Unlock()
	if running {
		return fmt.Errorf("%w: %s is downloading; cancel it first", ErrBusy, m.ID)
	}
	dir := g.Cache.PinnedDir(m)
	// The revision is the last element, so nothing outside this model's
	// own revision directory can be named.
	root, err := filepath.Abs(g.Cache.Dir)
	if err != nil || g.Cache.Dir == "" || m.Pin.Revision == "" {
		return fmt.Errorf("refusing to delete %s: no cache directory", m.ID)
	}
	if abs, err := filepath.Abs(dir); err != nil || !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return fmt.Errorf("refusing to delete %s outside %s", dir, root)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// The repository directory goes too when nothing else is in it.
	_ = os.Remove(filepath.Dir(dir))
	return nil
}
