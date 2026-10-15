package localai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuntime records loads.
type fakeRuntime struct {
	mu     sync.Mutex
	loaded map[string]string
}

func (r *fakeRuntime) Loads(m Model) (bool, string) {
	if m.Modality != ModalityEmbedding {
		return false, "not an embedding model"
	}
	return true, ""
}
func (r *fakeRuntime) Load(m Model, dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded[m.ID] = dir
	return nil
}
func (r *fakeRuntime) Unload(m Model) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.loaded, m.ID)
	return nil
}
func (r *fakeRuntime) Loaded(m Model) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.loaded[m.ID]
	return ok
}

// testPinned adds a small pinned model served by a fake HTTPGet, which
// records every URL fetched.
func testPinned(t *testing.T, files map[string]string) (Model, *[]string) {
	t.Helper()
	m := Model{ID: "test-pinned", Repo: "example/test-pinned", Publisher: PublisherCommunity,
		Access: AccessOpen, License: "test", Modality: ModalityEmbedding, Runtime: RuntimeONNX,
		Pin: &Pin{Revision: strings.Repeat("a", 40)}}
	for _, p := range []string{"model.onnx", "tokenizer.json"} {
		sum := sha256.Sum256([]byte(files[p]))
		m.Pin.Files = append(m.Pin.Files, PinnedFile{Path: p, Bytes: int64(len(files[p])), SHA256: hex.EncodeToString(sum[:])})
	}
	catalog[m.ID] = m
	var fetched []string
	var mu sync.Mutex
	old := HTTPGet
	HTTPGet = func(_ context.Context, url string) (io.ReadCloser, error) {
		mu.Lock()
		fetched = append(fetched, url)
		mu.Unlock()
		for p, body := range files {
			if strings.HasSuffix(url, "/resolve/"+m.Pin.Revision+"/"+p) {
				return io.NopCloser(bytes.NewReader([]byte(body))), nil
			}
		}
		return nil, errors.New("not found: " + url)
	}
	t.Cleanup(func() { delete(catalog, m.ID); HTTPGet = old })
	return m, &fetched
}

func stateOf(t *testing.T, g *Manager, id string) ModelState {
	t.Helper()
	for _, s := range g.States() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%s is not listed", id)
	return ModelState{}
}

// TestManagerDownloadsOnlyPinnedModelsOnceTheSizeIsConfirmed (#1306): a model
// with no pin is never fetched; a pinned one only with its exact size
// confirmed, from the pinned revision, verified; then it loads, unloads and
// is deleted, and a loaded model cannot be deleted.
func TestManagerDownloadsOnlyPinnedModelsOnceTheSizeIsConfirmed(t *testing.T) {
	m, fetched := testPinned(t, map[string]string{"model.onnx": "weights", "tokenizer.json": "{}"})
	rt := &fakeRuntime{loaded: map[string]string{}}
	g := &Manager{Cache: Cache{Dir: t.TempDir()}, Runtime: rt}

	if err := g.Download("gemma-4-e2b-it-community", 1); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("unpinned model: %v, want ErrNotPinned", err)
	}
	if s := stateOf(t, g, "gemma-4-e2b-it-community"); s.State != "not-pinned" || s.DownloadNote == "" || s.Loadable {
		t.Fatalf("unpinned state %+v", s)
	}
	if s := stateOf(t, g, m.ID); s.State != "absent" || s.Bytes != 9 || s.Loaded {
		t.Fatalf("before: %+v", s)
	}
	if err := g.Download(m.ID, 8); !errors.Is(err, ErrSizeNotConfirmed) {
		t.Fatalf("wrong size: %v", err)
	}
	if len(*fetched) != 0 {
		t.Fatalf("fetched before confirmation: %v", *fetched)
	}
	if err := g.Load(m.ID); !errors.Is(err, ErrNotCached) {
		t.Fatalf("load before download: %v", err)
	}
	if err := g.Download(m.ID, 9); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.Wait(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	s := stateOf(t, g, m.ID)
	if s.State != "downloaded" || s.Downloaded != 9 || s.Error != "" {
		t.Fatalf("after: %+v", s)
	}
	for _, u := range *fetched {
		if !strings.HasPrefix(u, "https://huggingface.co/example/test-pinned/resolve/"+m.Pin.Revision+"/") {
			t.Errorf("fetched %s, not the pinned revision", u)
		}
	}

	if err := g.Load(m.ID); err != nil || !stateOf(t, g, m.ID).Loaded {
		t.Fatalf("load: %v", err)
	}
	if err := g.Delete(m.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete while loaded: %v", err)
	}
	if err := g.Unload(m.ID); err != nil || stateOf(t, g, m.ID).Loaded {
		t.Fatalf("unload: %v", err)
	}
	if err := g.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Cache.PinnedDir(m)); !os.IsNotExist(err) {
		t.Fatalf("still on disk: %v", err)
	}
	if s := stateOf(t, g, m.ID); s.State != "absent" {
		t.Fatalf("after delete: %+v", s)
	}
}

// TestManagerRefusesAFileThatFailsItsDigest: a corrupt upstream file is not
// kept, and the failure is reported.
func TestManagerRefusesAFileThatFailsItsDigest(t *testing.T) {
	m, _ := testPinned(t, map[string]string{"model.onnx": "weights", "tokenizer.json": "{}"})
	m.Pin.Files[0].SHA256 = strings.Repeat("0", 64)
	catalog[m.ID] = m
	g := &Manager{Cache: Cache{Dir: t.TempDir()}}
	if err := g.Download(m.ID, 9); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = g.Wait(ctx, m.ID)
	s := stateOf(t, g, m.ID)
	if s.State == "downloaded" || !strings.Contains(s.Error, "checksum mismatch") {
		t.Fatalf("corrupt download: %+v", s)
	}
	if _, err := os.Stat(g.Cache.PinnedPath(m, m.Pin.Files[0])); !os.IsNotExist(err) {
		t.Fatalf("the corrupt file was kept: %v", err)
	}
	if err := g.Load(m.ID); !errors.Is(err, ErrNotLoadable) {
		t.Fatalf("load with no runtime: %v", err)
	}
}

// TestManagerRefusesGatedDownloads: the console never downloads a gated model.
func TestManagerRefusesGatedDownloads(t *testing.T) {
	m, fetched := testPinned(t, map[string]string{"model.onnx": "weights", "tokenizer.json": "{}"})
	m.Access = AccessGated
	catalog[m.ID] = m
	g := &Manager{Cache: Cache{Dir: t.TempDir()}}
	if err := g.Download(m.ID, 9); !errors.Is(err, ErrGated) || len(*fetched) != 0 {
		t.Fatalf("gated: %v, fetched %v", err, *fetched)
	}
}
