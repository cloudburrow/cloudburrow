//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"

	tfconvertimage "github.com/cloudburrow/cloudburrow/deploy/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// zeros reads as zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestBigQueryMLTensorFlowKeras (#1775): Keras 3 models exported as
// SavedModels (model.export, as Keras documents for TensorFlow Serving),
// made by the test in CloudBurrow's TensorFlow converter image
// (deploy/tfconvert: Google's tensorflow/tensorflow:2.20.0, pinned by
// digest, run on an --internal Docker network;
// testdata/tensorflow/gen_keras.py), uploaded to the instance's Cloud
// Storage and imported with CREATE MODEL … OPTIONS(MODEL_TYPE='TENSORFLOW',
// MODEL_PATH='gs://…/*') through the official bigquery and storage Go
// clients, as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow
// documents (read 2026-11-21: its type table maps tf.float32 to FLOAT64
// and tf.string to STRING, a tensor to an ARRAY):
//   - a Keras Sequential of Dense layers predicts TensorFlow's own
//     serving_default output (output_0, ARRAY<FLOAT64>) for an
//     ARRAY<FLOAT64> features column;
//   - a text classifier over STRING tokens (StringLookup, Embedding, an
//     LSTM that TensorFlow writes as a tf.while_loop over a free batch, and
//     Dense) converts to TFLite's HASHTABLE operators and a WHILE subgraph
//     and predicts TensorFlow's output for an ARRAY<STRING> column, words
//     out of the vocabulary and empty ones among them;
//   - a STRING column (StringLookup one-hot, Dense) does too;
//   - a model that splits a sentence with tf.strings.split needs Flex
//     (StringSplitV2, which TensorFlow Lite has no builtin for): 501 with
//     the converter's list of operators, and no model is made;
//   - "Models are limited to 450 MB in size": a folder of 451 MB is 400.
//
// It runs in the localai shard, beside the other locally run models.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLTensorFlowKeras(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH to run the TensorFlow converter image in")
	}

	bctx, cancel := context.WithTimeout(context.Background(), tfconvert.BuildTimeout)
	defer cancel()
	d := &tfconvert.Docker{Image: tfconvertimage.Tag(), Context: tfconvertimage.Tar}
	if err := d.Build(bctx); err != nil {
		t.Fatalf("build %s: %v", d.Image, err)
	}
	if err := d.EnsureNetwork(bctx); err != nil {
		t.Fatal(err)
	}
	gen, err := filepath.Abs(filepath.Join("testdata", "tensorflow"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatal(err)
	}
	// As this process's user, as TestBigQueryMLTensorFlowImportGenerated
	// does, so t.TempDir's cleanup can remove what TensorFlow writes.
	cmd := exec.CommandContext(bctx, "docker", "run", "--rm", "--network", tfconvert.Network, "--read-only",
		"--tmpfs", "/tmp", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=3", "-v", gen+":/w:ro", "-v", out+":/out",
		"--entrypoint", "python3", d.Image, "-I", "/w/gen_keras.py", "/out")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		t.Fatalf("gen_keras.py: %v\n%s", err, log.String())
	}

	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	for _, m := range []string{"dense", "text", "word", "split"} {
		src := filepath.Join(out, m)
		err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			w := bh.Object("tfkeras/" + m + "/" + filepath.ToSlash(rel)).NewWriter(ctx)
			if _, err := w.Write(data); err != nil {
				return err
			}
			return w.Close()
		})
		if err != nil {
			t.Fatalf("upload %s: %v", m, err)
		}
	}
	type rowRef struct {
		Inputs  map[string]any       `json:"inputs"`
		Outputs map[string][]float64 `json:"outputs"`
	}
	var ref struct {
		Dense []rowRef `json:"dense"`
		Text  []rowRef `json:"text"`
		Word  []rowRef `json:"word"`
	}
	raw, err := os.ReadFile(filepath.Join(out, "predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	array := func(typ string, v any) string {
		var s []string
		for _, x := range v.([]any) {
			if str, ok := x.(string); ok {
				s = append(s, strconv.Quote(str))
			} else {
				s = append(s, fmt.Sprint(x))
			}
		}
		return "ARRAY<" + typ + ">[" + strings.Join(s, ", ") + "]"
	}
	var dRows, tRows, wRows []string
	for i, r := range ref.Dense {
		dRows = append(dRows, fmt.Sprintf("SELECT %d AS id, %s AS features", i, array("FLOAT64", r.Inputs["features"])))
	}
	for i, r := range ref.Text {
		tRows = append(tRows, fmt.Sprintf("SELECT %d AS id, %s AS tokens", i, array("STRING", r.Inputs["tokens"])))
	}
	for i, r := range ref.Word {
		wRows = append(wRows, fmt.Sprintf("SELECT %d AS id, %q AS word", i, r.Inputs["word"]))
	}
	name := mlDataset(t, h, c, map[string]string{"dense": strings.Join(dRows, " UNION ALL "), "text": strings.Join(tRows, " UNION ALL "),
		"word": strings.Join(wRows, " UNION ALL ")})

	// The first TENSORFLOW import may still build the instance's converter
	// image: tensorflowImportDeadline.
	create := func(m string) error {
		return mlRunWithin(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfkeras/%[2]s/*')", name, m, b),
			tensorflowImportDeadline)
	}
	check := func(m string, wantFeat []string, rows []rowRef) {
		t.Helper()
		if err := create(m); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
		ctx := h.Context()
		md, err := c.Dataset(name).Model(m).Metadata(ctx)
		if err != nil {
			t.Fatalf("Model.Metadata %s: %v", m, err)
		}
		if md.Type != "TENSORFLOW" {
			t.Errorf("%s: type %s, want TENSORFLOW", m, md.Type)
		}
		fs, _ := md.RawFeatureColumns()
		ls, _ := md.RawLabelColumns()
		var feats, labels []string
		for _, f := range fs {
			k := string(f.Type.TypeKind)
			if f.Type.ArrayElementType != nil {
				k += "<" + string(f.Type.ArrayElementType.TypeKind) + ">"
			}
			feats = append(feats, f.Name+" "+k)
		}
		for _, f := range ls {
			labels = append(labels, f.Name+" "+string(f.Type.TypeKind))
		}
		sort.Strings(feats)
		if !reflect.DeepEqual(feats, wantFeat) || !reflect.DeepEqual(labels, []string{"output_0 ARRAY"}) {
			t.Errorf("%s columns %v %v, want %v [output_0 ARRAY]", m, feats, labels, wantFeat)
		}
		got := mlQuery(t, h, c, fmt.Sprintf("SELECT id, output_0 FROM ML.PREDICT(MODEL `%s.%s`, TABLE `%[1]s.%[2]s`) ORDER BY id", name, m))
		if len(got) != len(rows) {
			t.Fatalf("%s: %d rows, want %d", m, len(got), len(rows))
		}
		for i, r := range got {
			want := rows[i].Outputs["output_0"]
			ys, _ := r["output_0"].([]bigquery.Value)
			if len(ys) != len(want) {
				t.Fatalf("%s row %d: %v, TensorFlow %v", m, i, r["output_0"], want)
			}
			for j, y := range ys {
				g, ok := y.(float64)
				if !ok || math.Abs(g-want[j]) > 1e-5*math.Max(1, math.Abs(want[j])) {
					t.Errorf("%s row %d [%d]: %v, TensorFlow %v", m, i, j, y, want[j])
				}
			}
		}
	}
	check("dense", []string{"features ARRAY<FLOAT64>"}, ref.Dense)
	check("text", []string{"tokens ARRAY<STRING>"}, ref.Text)
	check("word", []string{"word STRING"}, ref.Word)

	// tf.strings.split has no TensorFlow Lite builtin: 501, never a model.
	err = create("split")
	wantAPIError(t, "Flex", err, http.StatusNotImplemented, "StringSplitV2")
	if _, err := c.Dataset(name).Model("split").Metadata(h.Context()); err == nil {
		t.Error("a model that did not convert was made")
	}

	// "Models are limited to 450 MB in size": the dense model's files and
	// a 451 MB variables shard beside them.
	for _, o := range []string{"saved_model.pb", "fingerprint.pb", "variables/variables.index"} {
		src := bh.Object("tfkeras/dense/" + o)
		if _, err := src.Attrs(h.Context()); err != nil {
			continue
		}
		if _, err := bh.Object("tfkeras/big/" + o).CopierFrom(src).Run(h.Context()); err != nil {
			t.Fatalf("copy %s: %v", o, err)
		}
	}
	bigObj := bh.Object("tfkeras/big/variables/variables.data-00000-of-00001")
	w := bigObj.NewWriter(h.Context())
	w.ChunkSize = 16 << 20
	if _, err := io.Copy(w, io.LimitReader(zeros{}, 451<<20)); err != nil {
		t.Fatalf("upload the large shard: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload the large shard: %v", err)
	}
	defer bigObj.Delete(context.Background())
	err = create("big")
	wantAPIError(t, "450 MB", err, http.StatusBadRequest, "450 MB")
}
