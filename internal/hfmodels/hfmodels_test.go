package hfmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rev = "0123456789abcdef0123456789abcdef01234567"

// fakeHub serves the two Hugging Face routes the manager reads.
func fakeHub(t *testing.T, gated string, weights []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(weights)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/org/tiny", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("blobs") != "true" {
			t.Error("model info asked for without blobs, so with no sizes")
		}
		fmt.Fprintf(w, `{"id":"org/tiny","sha":%q,"gated":%s,"pipeline_tag":"text-generation",
			"cardData":{"license":"apache-2.0"},
			"siblings":[{"rfilename":"config.json","size":2},
			{"rfilename":"model.safetensors","size":%d,"lfs":{"sha256":%q,"size":%d}}]}`,
			rev, gated, len(weights), hex.EncodeToString(sum[:]), len(weights))
	})
	mux.HandleFunc("GET /org/tiny/resolve/"+rev+"/config.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	})
	mux.HandleFunc("GET /org/tiny/resolve/"+rev+"/model.safetensors", func(w http.ResponseWriter, r *http.Request) {
		w.Write(weights)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func wait(t *testing.T, m *Manager, id, state string) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, s := range m.States() {
			if s.ID == id && s.State == state {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %s: %+v", id, state, m.States())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDownloadVerifiesAndDeletes (#1840): the licence and gated status come
// from the model info, every file is fetched at the pinned revision and
// checked against its SHA-256, and Delete frees the space.
func TestDownloadVerifiesAndDeletes(t *testing.T) {
	hub := fakeHub(t, "false", []byte("weights!"))
	m := &Manager{Dir: t.TempDir(), Base: hub.URL, FreeBytes: func(string) (int64, error) { return 60 << 30, nil },
		Token: func() string { return "" }}
	info, err := m.Lookup(context.Background(), "org/tiny")
	if err != nil {
		t.Fatal(err)
	}
	if info.License != "apache-2.0" || info.Gated != "" || info.Bytes != 10 || info.Revision != rev {
		t.Fatalf("info %+v", info)
	}
	if _, err := m.Start(context.Background(), "org/tiny"); err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, "org/tiny", "downloaded")
	if st.Downloaded != 10 {
		t.Errorf("downloaded %d bytes, want 10", st.Downloaded)
	}
	b, err := os.ReadFile(filepath.Join(m.Dir, "org--tiny", "model.safetensors"))
	if err != nil || string(b) != "weights!" {
		t.Fatalf("weights %q %v", b, err)
	}
	if err := m.Delete("org/tiny"); err != nil {
		t.Fatal(err)
	}
	if len(m.States()) != 0 {
		t.Errorf("after Delete: %+v", m.States())
	}
}

// TestDownloadRefusals (#1840): a corrupted file is not kept, a gated model
// with no HF_TOKEN in the environment is refused with what to do, and a
// download that would leave under 50 GB free is refused before a byte.
func TestDownloadRefusals(t *testing.T) {
	bad := fakeHub(t, "false", []byte("weights!"))
	m := &Manager{Dir: t.TempDir(), Base: bad.URL, FreeBytes: func(string) (int64, error) { return 60 << 30, nil },
		Token: func() string { return "" }}
	// Corrupt the listed digest by serving other bytes under the same info.
	m.Base = fakeHubMismatch(t).URL
	if _, err := m.Start(context.Background(), "org/tiny"); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, "org/tiny", "failed"); !strings.Contains(st.Error, "SHA-256") {
		t.Errorf("a corrupted file failed with %q", st.Error)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "org--tiny")); err == nil {
		t.Error("a corrupted download was kept")
	}

	gated := fakeHub(t, `"manual"`, []byte("weights!"))
	m2 := &Manager{Dir: t.TempDir(), Base: gated.URL, FreeBytes: func(string) (int64, error) { return 60 << 30, nil },
		Token: func() string { return "" }}
	if _, err := m2.Start(context.Background(), "org/tiny"); !errors.Is(err, ErrGated) || !strings.Contains(err.Error(), TokenEnv) {
		t.Errorf("a gated model with no token: %v", err)
	}

	m3 := &Manager{Dir: t.TempDir(), Base: bad.URL, FreeBytes: func(string) (int64, error) { return 50<<30 + 5, nil },
		Token: func() string { return "" }}
	if _, err := m3.Start(context.Background(), "org/tiny"); !errors.Is(err, ErrDiskSpace) {
		t.Errorf("a download leaving under 50 GB: %v", err)
	}
	for _, id := range []string{"tiny", "../etc/passwd", "a/b/c", "/abs"} {
		if err := CheckID(id); !errors.Is(err, ErrBadID) {
			t.Errorf("CheckID(%q) = %v", id, err)
		}
	}
}

func fakeHubMismatch(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/org/tiny", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"org/tiny","sha":%q,"gated":false,"siblings":[{"rfilename":"model.safetensors","lfs":{"sha256":"%064d","size":8}}]}`, rev, 0)
	})
	mux.HandleFunc("GET /org/tiny/resolve/"+rev+"/model.safetensors", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("weights!"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
