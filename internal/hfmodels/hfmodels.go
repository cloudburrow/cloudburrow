// Package hfmodels downloads Hugging Face models for Model Garden's
// Hugging Face deploy flow (#1840), from huggingface.co's public API.
//
// Google offers no local runtime for a Hugging Face deployment, so the
// files come from Hugging Face itself, as Google's deploy pulls them. A
// model's licence and gated status are read from its model info before
// anything is fetched. A gated model needs the HF_TOKEN environment variable
// of the CloudBurrow process: the token is never accepted from the console,
// never stored and never shown. A download is refused when it would leave
// less than MinFree bytes on the disk, every LFS file is checked against
// its SHA-256, and a downloaded model can be deleted to free the space.
package hfmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// DefaultBase is Hugging Face's public endpoint.
const DefaultBase = "https://huggingface.co"

// TokenEnv names the environment variable a gated model's token is read
// from, in the CloudBurrow process only.
const TokenEnv = "HF_TOKEN"

// DefaultMinFree is the space a download must leave free: 50 GB.
const DefaultMinFree = 50 << 30

var (
	// ErrBadID is an ID that is not {author}/{name}.
	ErrBadID = errors.New("not a Hugging Face model ID")
	// ErrGated is a gated model with no token in the environment.
	ErrGated = errors.New("gated model")
	// ErrDiskSpace is a download that would leave too little free space.
	ErrDiskSpace = errors.New("not enough free disk space")
	// ErrBusy is an action on a model that is downloading.
	ErrBusy = errors.New("download in progress")
	// ErrNotFound is a model that is neither on Hugging Face nor downloaded.
	ErrNotFound = errors.New("model not found")
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// File is one file of a model's repository.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// Info is what Hugging Face says about a model.
type Info struct {
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	License     string `json:"license"`
	Gated       string `json:"gated"` // "", "auto" or "manual"
	PipelineTag string `json:"pipelineTag,omitempty"`
	Files       []File `json:"files"`
	Bytes       int64  `json:"bytes"`
}

// State is a model's local state.
type State struct {
	Info
	// State is downloading, downloaded or failed.
	State      string `json:"state"`
	Downloaded int64  `json:"downloaded"`
	Error      string `json:"error,omitempty"`
}

// Manager downloads, lists and deletes Hugging Face models under Dir.
type Manager struct {
	Dir     string
	Base    string // DefaultBase when empty
	Client  *http.Client
	MinFree int64 // DefaultMinFree when zero
	// FreeBytes reports the free space at a path; statfs when nil.
	FreeBytes func(string) (int64, error)
	// Token returns the gated-model token; os.Getenv(TokenEnv) when nil.
	Token func() string

	mu     sync.Mutex
	active map[string]*State
	cancel map[string]context.CancelFunc
}

func (m *Manager) base() string {
	if m.Base != "" {
		return strings.TrimSuffix(m.Base, "/")
	}
	return DefaultBase
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 0}
}

func (m *Manager) token() string {
	if m.Token != nil {
		return m.Token()
	}
	return os.Getenv(TokenEnv)
}

func (m *Manager) minFree() int64 {
	if m.MinFree > 0 {
		return m.MinFree
	}
	return DefaultMinFree
}

func (m *Manager) free(p string) (int64, error) {
	if m.FreeBytes != nil {
		return m.FreeBytes(p)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// CheckID validates a model ID.
func CheckID(id string) error {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q; want {author}/{model}, as huggingface.co/{author}/{model}", ErrBadID, id)
	}
	return nil
}

func (m *Manager) get(ctx context.Context, u string, auth bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if auth {
		if t := m.token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	return m.client().Do(req)
}

// Lookup reads a model's licence, gated status, revision and files.
func (m *Manager) Lookup(ctx context.Context, id string) (Info, error) {
	if err := CheckID(id); err != nil {
		return Info{}, err
	}
	resp, err := m.get(ctx, m.base()+"/api/models/"+id+"?blobs=true", true)
	if err != nil {
		return Info{}, fmt.Errorf("read %s from Hugging Face: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return Info{}, fmt.Errorf("%w: %s is not a public model on huggingface.co", ErrNotFound, id)
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("read %s from Hugging Face: HTTP %d", id, resp.StatusCode)
	}
	var raw struct {
		ID          string          `json:"id"`
		SHA         string          `json:"sha"`
		Gated       json.RawMessage `json:"gated"`
		PipelineTag string          `json:"pipeline_tag"`
		Tags        []string        `json:"tags"`
		CardData    struct {
			License any `json:"license"`
		} `json:"cardData"`
		Siblings []struct {
			RFilename string `json:"rfilename"`
			Size      int64  `json:"size"`
			LFS       *struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"lfs"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&raw); err != nil {
		return Info{}, fmt.Errorf("decode %s's model info: %w", id, err)
	}
	info := Info{ID: id, Revision: raw.SHA, PipelineTag: raw.PipelineTag}
	if g := strings.Trim(string(raw.Gated), `"`); g != "" && g != "false" && g != "null" {
		info.Gated = g
	}
	switch l := raw.CardData.License.(type) {
	case string:
		info.License = l
	case []any:
		parts := []string{}
		for _, v := range l {
			parts = append(parts, fmt.Sprint(v))
		}
		info.License = strings.Join(parts, ", ")
	}
	if info.License == "" {
		for _, t := range raw.Tags {
			if l, ok := strings.CutPrefix(t, "license:"); ok {
				info.License = l
			}
		}
	}
	if info.License == "" {
		info.License = "not stated"
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(info.Revision) {
		return Info{}, fmt.Errorf("%s's model info has no commit revision", id)
	}
	for _, s := range raw.Siblings {
		if !safePath(s.RFilename) {
			return Info{}, fmt.Errorf("%s lists an unsafe file name %q", id, s.RFilename)
		}
		f := File{Path: s.RFilename, Size: s.Size}
		if s.LFS != nil {
			f.SHA256, f.Size = s.LFS.SHA256, s.LFS.Size
		}
		info.Files = append(info.Files, f)
		info.Bytes += f.Size
	}
	return info, nil
}

func safePath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "\\") &&
		path.Clean(p) == p && !strings.HasPrefix(p, "../") && p != ".." && !strings.Contains(p, "/../")
}

// dirOf is where a model's files are kept.
func (m *Manager) dirOf(id string) string {
	return filepath.Join(m.Dir, strings.ReplaceAll(id, "/", "--"))
}

// Start checks the model and the disk, then downloads in the background.
func (m *Manager) Start(ctx context.Context, id string) (State, error) {
	info, err := m.Lookup(ctx, id)
	if err != nil {
		return State{}, err
	}
	if info.Gated != "" && m.token() == "" {
		return State{}, fmt.Errorf("%w: %s is gated on Hugging Face (%s approval). Accept its licence on "+
			"huggingface.co/%s, then set %s in the environment CloudBurrow runs in and restart it; "+
			"the console never asks for the token", ErrGated, id, info.Gated, id, TokenEnv)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return State{}, err
	}
	free, err := m.free(m.Dir)
	if err != nil {
		return State{}, fmt.Errorf("check free disk space: %w", err)
	}
	if free-info.Bytes < m.minFree() {
		return State{}, fmt.Errorf("%w: %s needs %s, and %s is free; a download must leave %s free. "+
			"Delete a downloaded model or free disk space first", ErrDiskSpace, id,
			gb(info.Bytes), gb(free), gb(m.minFree()))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active, m.cancel = map[string]*State{}, map[string]context.CancelFunc{}
	}
	if st, ok := m.active[id]; ok && st.State == "downloading" {
		return *st, fmt.Errorf("%w: %s", ErrBusy, id)
	}
	st := &State{Info: info, State: "downloading"}
	m.active[id] = st
	dctx, cancel := context.WithCancel(context.Background())
	m.cancel[id] = cancel
	go m.download(dctx, st)
	return *st, nil
}

func gb(n int64) string { return fmt.Sprintf("%.2f GB", float64(n)/1e9) }

func (m *Manager) download(ctx context.Context, st *State) {
	dir := m.dirOf(st.ID)
	tmp := dir + ".partial"
	err := func() error {
		if err := os.RemoveAll(tmp); err != nil {
			return err
		}
		for _, f := range st.Files {
			if err := m.fetch(ctx, st, tmp, f); err != nil {
				return err
			}
		}
		b, _ := json.MarshalIndent(st.Info, "", "  ")
		if err := os.WriteFile(filepath.Join(tmp, ".cloudburrow-model.json"), b, 0o644); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return os.Rename(tmp, dir)
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cancel, st.ID)
	if err != nil {
		_ = os.RemoveAll(tmp)
		st.State, st.Error = "failed", err.Error()
		if ctx.Err() != nil {
			st.Error = "cancelled"
		}
		return
	}
	st.State = "downloaded"
	delete(m.active, st.ID)
}

func (m *Manager) fetch(ctx context.Context, st *State, dir string, f File) error {
	u := m.base() + "/" + st.ID + "/resolve/" + st.Revision + "/" + (&url.URL{Path: f.Path}).EscapedPath()
	resp, err := m.get(ctx, u, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", f.Path, resp.StatusCode)
	}
	dst := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	h := sha256.New()
	var n int64
	buf := make([]byte, 1<<20)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if _, werr := out.Write(buf[:k]); werr != nil {
				return werr
			}
			h.Write(buf[:k])
			n += int64(k)
			m.mu.Lock()
			st.Downloaded += int64(k)
			m.mu.Unlock()
			if n > f.Size && f.Size > 0 {
				return fmt.Errorf("%s is larger than its listed %d bytes", f.Path, f.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if f.Size > 0 && n != f.Size {
		return fmt.Errorf("%s: %d bytes, listed as %d", f.Path, n, f.Size)
	}
	if f.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s does not match its SHA-256 on Hugging Face", f.Path)
	}
	return out.Close()
}

// Cancel stops a download.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cancel[id]
	if !ok {
		return fmt.Errorf("%w: %s is not downloading", ErrNotFound, id)
	}
	c()
	return nil
}

// Delete removes a downloaded model's files.
func (m *Manager) Delete(id string) error {
	if err := CheckID(id); err != nil {
		return err
	}
	m.mu.Lock()
	if st, ok := m.active[id]; ok {
		if st.State == "downloading" {
			m.mu.Unlock()
			return fmt.Errorf("%w: cancel %s first", ErrBusy, id)
		}
		delete(m.active, id)
	}
	m.mu.Unlock()
	dir := m.dirOf(id)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%w: %s is not downloaded", ErrNotFound, id)
	}
	return os.RemoveAll(dir)
}

// States lists every downloaded, downloading or failed model.
func (m *Manager) States() []State {
	out := map[string]State{}
	entries, _ := os.ReadDir(m.Dir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.Dir, e.Name(), ".cloudburrow-model.json"))
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(b, &info) == nil && info.ID != "" {
			out[info.ID] = State{Info: info, State: "downloaded", Downloaded: info.Bytes}
		}
	}
	m.mu.Lock()
	for id, st := range m.active {
		out[id] = *st
	}
	m.mu.Unlock()
	list := make([]State, 0, len(out))
	for _, s := range out {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Free reports the free space where models are kept, and the floor a
// download must leave.
func (m *Manager) Free() (free, floor int64) {
	_ = os.MkdirAll(m.Dir, 0o755)
	f, err := m.free(m.Dir)
	if err != nil {
		return -1, m.minFree()
	}
	return f, m.minFree()
}
