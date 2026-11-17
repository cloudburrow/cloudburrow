package bigqueryfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/onnxrunner"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeRunner is an ONNX runner that runs onnxgen's linear model in Go:
// y = 2a + 3b + v[0] - v[1] + 1, label = y > 5, scores = [y, -y].
type fakeRunner struct {
	loaded map[string]bool
	asked  int // times the model's bytes were asked for
	err    error
}

var linearIn = []onnxmodel.Port{{Name: "a", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 1}},
	{Name: "b", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 1}},
	{Name: "v", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 2}}}
var linearOut = []onnxmodel.Port{{Name: "y", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 1}},
	{Name: "label", Kind: onnxmodel.KindTensor, Elem: "INT64", Dims: []int64{-1}},
	{Name: "scores", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 2}}}

func (r *fakeRunner) Load(_ context.Context, sum string, model []byte) ([]onnxmodel.Port, []onnxmodel.Port, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	if sha(model) != sum {
		return nil, nil, errors.New("wrong sum")
	}
	if r.loaded == nil {
		r.loaded = map[string]bool{}
	}
	r.loaded[sum] = true
	return linearIn, linearOut, nil
}

func (r *fakeRunner) Run(ctx context.Context, sum string, model func() ([]byte, error), inputs []onnxmodel.Port,
	in []onnxmodel.Tensor, outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error) {
	if r.err != nil {
		return nil, r.err
	}
	if !r.loaded[sum] {
		r.asked++
		b, err := model()
		if err != nil {
			return nil, err
		}
		if _, _, err := r.Load(ctx, sum, b); err != nil {
			return nil, err
		}
	}
	n := len(in[0].F)
	y, label, scores := onnxmodel.Tensor{Shape: []int64{int64(n), 1}}, onnxmodel.Tensor{Shape: []int64{int64(n)}}, onnxmodel.Tensor{Shape: []int64{int64(n), 2}}
	for k := 0; k < n; k++ {
		v := 2*in[0].F[k] + 3*in[1].F[k] + in[2].F[2*k] - in[2].F[2*k+1] + 1
		y.F = append(y.F, v)
		l := int64(0)
		if v > 5 {
			l = 1
		}
		label.I = append(label.I, l)
		scores.F = append(scores.F, v, -v)
	}
	return []onnxmodel.Tensor{y, label, scores}, nil
}

// TestRunONNXWithAFakeRunner (#1711): ML.PREDICT's keys to the runner's
// tensors and back to literals, the model's bytes sent only when the
// runner does not hold them, and the runner's failures as the front
// answers them.
func TestRunONNXWithAFakeRunner(t *testing.T) {
	model := []byte("a linear model")
	imp := &onnxImport{SHA256: sha(model), Inputs: linearIn, Outputs: linearOut}
	ins := make([]onnxColumn, len(linearIn))
	for i, p := range linearIn {
		ins[i], _ = onnxColumnOf(p, "input")
	}
	r := &fakeRunner{}
	bytes := func() ([]byte, error) { return model, nil }
	ctx := context.Background()
	keys := []string{`{"c0":1,"c1":1,"c2":[0,0]}`, `{"c0":0,"c1":0.5,"c2":[2,1]}`}
	for range 2 {
		got, code, msg := runONNX(ctx, r, bytes, imp, ins, keys)
		if code != 0 {
			t.Fatal(code, msg)
		}
		want := [][]string{{"6.0", "1", "[6.0, (-6.0)]"}, {"3.5", "0", "[3.5, (-3.5)]"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if r.asked != 1 {
		t.Errorf("the model's bytes were asked for %d times, want once", r.asked)
	}
	for _, c := range []struct {
		err  error
		code int
		msg  string
	}{
		{&onnxrunner.Error{Status: 400, Message: "bad input"}, 400, "The ONNX model failed to run: bad input"},
		{&onnxrunner.Error{Status: 501, Message: "no runtime"}, 501, "cannot run ONNX Runtime: no runtime"},
		{errors.New("the ONNX runner at http://x could not be reached"), 500, "could not be reached"},
	} {
		_, code, msg := runONNX(ctx, &fakeRunner{err: c.err}, bytes, imp, ins, keys)
		if code != c.code || !strings.Contains(msg, c.msg) {
			t.Errorf("%v: %d %s", c.err, code, msg)
		}
	}
	if _, code, msg := runONNX(ctx, &fakeRunner{}, func() ([]byte, error) { return nil, errors.New("gone") }, imp, ins, keys); code != 500 ||
		!strings.Contains(msg, "gone") {
		t.Errorf("a missing model file: %d %s", code, msg)
	}
}

// TestONNXWithoutARunner (#1711): an instance with no ONNX runner, as the
// default build's is, refuses an imported model 501, naming the make
// target that builds the one that runs it, before reading anything.
func TestONNXWithoutARunner(t *testing.T) {
	_, _, code, msg := front{}.importONNX(context.Background(), "gs://b/m.onnx")
	if code != 501 || !strings.Contains(msg, "make build-onnx") || !strings.Contains(msg, "Not implemented here") {
		t.Errorf("CREATE MODEL: %d %s", code, msg)
	}
	_, code, msg = front{}.onnxSubquery(nil, queryOptions{}, bqModel{Type: "ONNX", ONNX: &onnxImport{}}, mlCall{fn: "PREDICT", input: "TABLE t"})
	if code != 501 || !strings.Contains(msg, "make build-onnx") {
		t.Errorf("ML.PREDICT: %d %s", code, msg)
	}
}
