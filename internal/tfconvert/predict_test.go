package tfconvert

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// servingEngine is a fakeEngine whose predictors echo their model's
// saved_model.pb before each request.
type servingEngine struct {
	fakeEngine
	mu     sync.Mutex
	starts int
	closed int
}

type echoRunner struct {
	e  *servingEngine
	pb string
}

func (r *echoRunner) Do(_ context.Context, req []byte) ([]byte, error) {
	if string(req) == "fail" {
		return []byte(`{"error": "TensorFlow said no"}`), nil
	}
	return []byte(r.pb + ":" + string(req)), nil
}

func (r *echoRunner) Close() error {
	r.e.mu.Lock()
	r.e.closed++
	r.e.mu.Unlock()
	return nil
}

func (e *servingEngine) Serve(_ context.Context, dir string) (Runner, error) {
	b, err := os.ReadFile(filepath.Join(dir, "saved_model.pb"))
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.starts++
	e.mu.Unlock()
	return &echoRunner{e: e, pb: string(b)}, nil
}

func testBundle(t *testing.T, pb string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range [][2]string{{"savedmodel/saved_model.pb", pb}, {"signature.json", `{"inputs":[],"outputs":[]}`},
		{"fallback.json", `{"kind":"flex","message":"m"}`}} {
		name, body := f[0], f[1]
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	s := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(s[:])
}

// The TensorFlow runtime (#1775): a model the server does not hold is
// sent once, its predictor starts once and is kept, TensorFlow's error is
// a "run" refusal, at most maxRunners stay up, and Close stops them all.
func TestServerRun(t *testing.T) {
	e := &servingEngine{}
	srv := NewServer(e, t.TempDir())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := NewClient(ts.URL, nil)
	ctx := context.Background()
	sends := 0
	run := func(pb, req string) (string, error) {
		b, sum := testBundle(t, pb)
		out, err := c.Run(ctx, sum, func() ([]byte, error) { sends++; return b, nil }, []byte(req))
		return string(out), err
	}
	for range 3 {
		got, err := run("A", "x")
		if err != nil || got != "A:x" {
			t.Fatalf("Run = %q, %v", got, err)
		}
	}
	if sends != 1 || e.starts != 1 {
		t.Errorf("sent the model %d times and started %d predictors, want 1 and 1", sends, e.starts)
	}
	_, err := run("A", "fail")
	var rf *Refusal
	if !errors.As(err, &rf) || rf.Kind != "run" || !strings.Contains(rf.Message, "TensorFlow said no") {
		t.Errorf("a TensorFlow error: %#v", err)
	}
	for _, pb := range []string{"B", "C"} {
		if got, err := run(pb, "y"); err != nil || got != pb+":y" {
			t.Fatalf("Run %s = %q, %v", pb, got, err)
		}
	}
	if e.closed != 1 {
		t.Errorf("%d predictors closed with three models, want 1 (at most %d live)", e.closed, maxRunners)
	}
	// A bundle that is not the SHA-256 it claims is refused.
	b, _ := testBundle(t, "D")
	_, bad := testBundle(t, "E")
	if _, err := c.Run(ctx, bad, func() ([]byte, error) { return b, nil }, []byte("z")); !errors.As(err, &rf) || rf.Kind != "format" {
		t.Errorf("a bundle of another SHA-256: %#v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if e.closed != 3 {
		t.Errorf("%d predictors closed after Close, want 3", e.closed)
	}
}

// ReadBundle reads a fallback bundle's signature and reason.
func TestReadBundle(t *testing.T) {
	b, _ := testBundle(t, "A")
	got, err := ReadBundle(b)
	if err != nil || got.TFLite != nil || got.Fallback == nil || got.Fallback.Kind != "flex" {
		t.Fatalf("ReadBundle = %#v, %v", got, err)
	}
	if _, err := ReadBundle([]byte("not a tar")); err == nil {
		t.Error("a bundle that is not a tar was read")
	}
}
