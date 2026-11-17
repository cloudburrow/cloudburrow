package onnxrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeEngine "runs" a model by doubling its one input, and refuses a model
// whose bytes start with "bad".
type fakeEngine struct {
	mu    sync.Mutex
	init  error
	runs  int
	ports []onnxmodel.Port
}

func (e *fakeEngine) Init() error { return e.init }

func (e *fakeEngine) Inspect(model []byte) ([]onnxmodel.Port, []onnxmodel.Port, error) {
	if strings.HasPrefix(string(model), "bad") {
		return nil, nil, errors.New("not an ONNX model")
	}
	return e.ports, e.ports, nil
}

func (e *fakeEngine) Run(model []byte, inputs []onnxmodel.Port, in []onnxmodel.Tensor, outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	if len(in) != 1 {
		return nil, errors.New("one input")
	}
	out := onnxmodel.Tensor{Shape: in[0].Shape}
	for _, f := range in[0].F {
		out.F = append(out.F, 2*f)
	}
	return []onnxmodel.Tensor{out}, nil
}

var xPort = []onnxmodel.Port{{Name: "x", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1}}}

func newRunner(t *testing.T, e Engine, maxBytes int64) (*Server, *Client) {
	t.Helper()
	s := NewServer(e, maxBytes)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, NewClient(ts.URL, nil)
}

// TestRunnerLoadsAndRuns (#1711): a model sent once runs by its SHA-256;
// one the runner does not hold is sent again when the client is asked to
// run it; floats that are not numbers cross as they are.
func TestRunnerLoadsAndRuns(t *testing.T) {
	e := &fakeEngine{ports: xPort}
	s, c := newRunner(t, e, 0)
	ctx := context.Background()
	m := []byte("model one")
	in, out, err := c.Load(ctx, sum(m), m)
	if err != nil || !reflect.DeepEqual(in, xPort) || !reflect.DeepEqual(out, xPort) {
		t.Fatalf("Load: %v %v %v", in, out, err)
	}
	asked := 0
	bytes := func() ([]byte, error) { asked++; return m, nil }
	x := []onnxmodel.Tensor{{Shape: []int64{4}, F: []float64{1, math.NaN(), math.Inf(1), math.Inf(-1)}}}
	res, err := c.Run(ctx, sum(m), bytes, xPort, x, xPort)
	if err != nil {
		t.Fatal(err)
	}
	if f := res[0].F; len(f) != 4 || f[0] != 2 || !math.IsNaN(f[1]) || !math.IsInf(f[2], 1) || !math.IsInf(f[3], -1) {
		t.Errorf("Run: %v", res)
	}
	if asked != 0 {
		t.Errorf("a loaded model's bytes were asked for")
	}
	// A runner that lost the model (a restart) is sent it again.
	_, c2 := newRunner(t, &fakeEngine{ports: xPort}, 0)
	if _, err := c2.Run(ctx, sum(m), bytes, xPort, x, xPort); err != nil || asked != 1 {
		t.Errorf("Run of a model the runner does not hold: %v, bytes asked for %d times", err, asked)
	}
	if got := s.Cached(); !reflect.DeepEqual(got, []string{sum(m)}) {
		t.Errorf("Cached = %v", got)
	}
}

// TestRunnerRefusals: what the runner refuses, and the status each is.
func TestRunnerRefusals(t *testing.T) {
	ctx := context.Background()
	_, c := newRunner(t, &fakeEngine{ports: xPort}, 0)
	bad := []byte("bad model")
	_, _, err := c.Load(ctx, sum(bad), bad)
	if !IsModelError(err) || !strings.Contains(err.Error(), "not an ONNX model") {
		t.Errorf("a model that does not load: %v", err)
	}
	m := []byte("model")
	if _, _, err := c.Load(ctx, sum([]byte("other")), m); !IsModelError(err) || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("bytes of another sum: %v", err)
	}
	if _, _, err := c.Load(ctx, "nothex", m); !IsModelError(err) {
		t.Errorf("a path that is not a SHA-256: %v", err)
	}
	if _, err := c.Run(ctx, sum(m), func() ([]byte, error) { return nil, errors.New("gone") }, xPort, nil, xPort); err == nil ||
		err.Error() != "gone" {
		t.Errorf("a model not held whose bytes are gone: %v", err)
	}
	_, none := newRunner(t, &fakeEngine{init: onnxmodel.ErrNotCompiled}, 0)
	if _, _, err := none.Load(ctx, sum(m), m); !IsUnavailable(err) || !strings.Contains(err.Error(), "make build-onnx") {
		t.Errorf("a runner with no runtime: %v", err)
	}
	if _, _, err := NewClient("http://127.0.0.1:1", nil).Load(ctx, sum(m), m); err == nil || IsModelError(err) || IsUnavailable(err) ||
		!strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("no runner: %v", err)
	}
	if _, _, err := NewClient("", nil).Load(ctx, sum(m), m); err == nil {
		t.Error("an empty address was dialled")
	}
	resp, err := http.Get(c.Base() + "/v1/models/" + sum(m))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET of a model: %d", resp.StatusCode)
	}
}

// TestRunnerCacheIsBounded: the models held fit the cache's bytes, the
// least recently used evicted first, and an evicted model is sent again.
func TestRunnerCacheIsBounded(t *testing.T) {
	ctx := context.Background()
	s, c := newRunner(t, &fakeEngine{ports: xPort}, 25)
	a, b, d := []byte("model a: ten"), []byte("model b: ten"), []byte("model d: ten")
	for _, m := range [][]byte{a, b} {
		if _, _, err := c.Load(ctx, sum(m), m); err != nil {
			t.Fatal(err)
		}
	}
	// a is used, so b is the least recently used when d comes.
	x := []onnxmodel.Tensor{{Shape: []int64{1}, F: []float64{1}}}
	if _, err := c.Run(ctx, sum(a), func() ([]byte, error) { return a, nil }, xPort, x, xPort); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Load(ctx, sum(d), d); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Cached(), []string{sum(d), sum(a)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Cached = %v, want d then a", got)
	}
	sent := 0
	if _, err := c.Run(ctx, sum(b), func() ([]byte, error) { sent++; return b, nil }, xPort, x, xPort); err != nil || sent != 1 {
		t.Errorf("an evicted model: %v, sent %d times", err, sent)
	}
	// A model larger than the whole cache is held alone.
	big := []byte(strings.Repeat("x", 40))
	if _, _, err := c.Load(ctx, sum(big), big); err != nil {
		t.Fatal(err)
	}
	if got := s.Cached(); !reflect.DeepEqual(got, []string{sum(big)}) {
		t.Errorf("Cached = %v, want the large model alone", got)
	}
}
