package tfconvert

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// fakeEngine records the files it was given and answers out or err.
type fakeEngine struct {
	files []string
	out   []byte
	err   error
}

func (e *fakeEngine) Convert(_ context.Context, dir string) ([]byte, error) {
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			e.files = append(e.files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(e.files)
	return e.out, e.err
}

// The client's tar arrives as the SavedModel's folder, a file name that
// would leave it is refused, and a refusal comes back whole.
func TestServerRoundTrip(t *testing.T) {
	e := &fakeEngine{out: []byte("TFL3")}
	srv := httptest.NewServer(NewServer(e, t.TempDir()))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	got, err := c.Convert(context.Background(), []File{{"saved_model.pb", []byte("pb")}, {"variables/variables.index", []byte("i")}})
	if err != nil || string(got) != "TFL3" {
		t.Fatalf("Convert = %q, %v", got, err)
	}
	if strings.Join(e.files, ",") != "saved_model.pb,variables/variables.index" {
		t.Fatalf("engine saw %v", e.files)
	}
	e.err = &Refusal{Kind: "flex", Message: "needs Flex"}
	_, err = c.Convert(context.Background(), []File{{"saved_model.pb", nil}})
	var rf *Refusal
	if !errors.As(err, &rf) || rf.Kind != "flex" || rf.Message != "needs Flex" || !rf.Unsupported() {
		t.Fatalf("refusal = %#v", err)
	}
	e.err = nil
	e.files = nil
	_, err = c.Convert(context.Background(), []File{{"../../etc/x", []byte("x")}})
	if err != nil {
		t.Fatalf("a cleaned name is kept inside the folder: %v", err)
	}
	if strings.Join(e.files, ",") != "etc/x" {
		t.Fatalf("engine saw %v", e.files)
	}
	if _, err := NewClient("", nil).Convert(context.Background(), nil); !errors.As(err, &rf) || rf.Kind != "unavailable" {
		t.Fatalf("no address: %v", err)
	}
}

// TestDockerConverter runs the real converter image (deploy/tfconvert) on
// the testdata SavedModels when CLOUDBURROW_TFCONVERT_IMAGE names it: the
// converted mlp and loop (WHILE, IF, comparisons and SELECT_V2, #1775),
// run on internal/tflitemodel, give TensorFlow's own serving_default
// output (tensorflow_predictions.json), and the SVD model
// is refused as needing Flex.
func TestDockerConverter(t *testing.T) {
	image := os.Getenv("CLOUDBURROW_TFCONVERT_IMAGE")
	if image == "" {
		t.Skip("CLOUDBURROW_TFCONVERT_IMAGE is not set")
	}
	dir := filepath.Join("..", "tflitemodel", "testdata")
	d := &Docker{Image: image}
	conv := func(name string) ([]byte, error) {
		tmp := t.TempDir()
		src := filepath.Join(dir, "savedmodel", name)
		model := filepath.Join(tmp, "model")
		_ = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			b, _ := os.ReadFile(p)
			_ = os.MkdirAll(filepath.Join(model, filepath.Dir(rel)), 0o755)
			return os.WriteFile(filepath.Join(model, rel), b, 0o644)
		})
		return d.Convert(context.Background(), model)
	}
	type rows []struct {
		Inputs  map[string][]float64 `json:"inputs"`
		Outputs map[string][]float64 `json:"outputs"`
	}
	var ref struct {
		Rows rows `json:"rows"`
		Loop rows `json:"loop"`
		Ops2 rows `json:"ops2"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "tensorflow_predictions.json"))
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]rows{"mlp": ref.Rows, "loop": ref.Loop, "ops2": ref.Ops2} {
		data, err := conv(model)
		if err != nil {
			t.Fatalf("convert %s: %v", model, err)
		}
		m, err := tflitemodel.Load(data)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		for _, row := range want {
			var ins []tflitemodel.Tensor
			for _, p := range m.Inputs {
				vs := row.Inputs[p.Name]
				shape := []int{1, len(vs)}
				if len(p.Shape) == 1 {
					shape = []int{len(vs)}
				}
				tn := tflitemodel.Tensor{Type: p.Type, Shape: shape}
				for _, v := range vs {
					if p.Type.IsInt() {
						tn.I = append(tn.I, int64(v))
					} else {
						tn.F = append(tn.F, float32(v))
					}
				}
				ins = append(ins, tn)
			}
			outs, err := m.Run(ins)
			if err != nil {
				t.Fatalf("%s: %v", model, err)
			}
			for k, p := range m.Outputs {
				w := row.Outputs[p.Name]
				if len(w) != outs[k].Len() {
					t.Fatalf("%s %s: %d values, TensorFlow %d", model, p.Name, outs[k].Len(), len(w))
				}
				for i, wv := range w {
					var g float64
					if outs[k].I != nil {
						g = float64(outs[k].I[i])
					} else {
						g = float64(outs[k].F[i])
					}
					if math.Abs(g-wv) > 1e-5*max(1, math.Abs(wv)) {
						t.Errorf("%s %s[%d] = %v, TensorFlow %v", model, p.Name, i, g, wv)
					}
				}
			}
		}
		uses := strings.Join(m.Uses(), " ")
		if model == "loop" && (!strings.Contains(uses, "WHILE") || !strings.Contains(uses, "IF")) {
			t.Errorf("loop converted without WHILE and IF: %s", uses)
		}
		t.Logf("%s ran, as TensorFlow does: %s", model, uses)
	}
	_, err := conv("flex")
	var rf *Refusal
	if !errors.As(err, &rf) || rf.Kind != "flex" {
		t.Fatalf("flex: %#v", err)
	}
	t.Logf("flex refusal: %s", rf.Message)
}
