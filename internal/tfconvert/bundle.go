package tfconvert

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A converted model is a bundle: the tar of what the converter image
// wrote (deploy/tfconvert/convert.py):
//
//   - savedmodel/...: the SavedModel BigQuery's own runtime would run,
//     with the inputs its columns fill (a tf.train.Example parse or a
//     SparseTensor input rewritten to the columns' dense inputs), which the
//     TensorFlow runtime (Docker.Serve) runs;
//   - signature.json: its serving_default's ports (Signature);
//   - model.tflite: the TensorFlow Lite flatbuffer of it, when
//     TensorFlow Lite's builtin operators take it; or
//   - fallback.json: a Refusal saying why they do not (needs Flex, or
//     TFLiteConverter cannot lower it), in which case the model runs on
//     TensorFlow itself, as BigQuery runs it.

// Port is one of serving_default's inputs or outputs.
type Port struct {
	Name string `json:"name"`
	// DType is TensorFlow's name of the dtype: float32, int64, string, ...
	DType string `json:"dtype"`
	// Shape is -1 where free; nil for an unknown rank.
	Shape []int `json:"shape"`
}

// Signature is a bundle's signature.json.
type Signature struct {
	Inputs  []Port `json:"inputs"`
	Outputs []Port `json:"outputs"`
	// Sparse names the SparseTensor inputs the converter made dense
	// [batch, n] inputs, with their dense shape after the batch.
	Sparse map[string][]int `json:"sparse"`
}

// Bundle is a converted model, read.
type Bundle struct {
	// TFLite is model.tflite, nil when the model runs on TensorFlow.
	TFLite []byte
	// Fallback is why TensorFlow Lite does not take it, when it does not.
	Fallback *Refusal
	// Signature is its SavedModel's serving_default.
	Signature Signature
}

// ReadBundle reads a converter's bundle.
func ReadBundle(b []byte) (*Bundle, error) {
	var out Bundle
	var sig []byte
	err := walkBundle(b, func(name string, r io.Reader) error {
		var err error
		switch name {
		case "model.tflite":
			out.TFLite, err = io.ReadAll(r)
		case "signature.json":
			sig, err = io.ReadAll(r)
		case "fallback.json":
			var fb []byte
			if fb, err = io.ReadAll(r); err == nil {
				out.Fallback = decodeRefusal(fb)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, errors.New("the converted model has no signature.json")
	}
	if err := json.Unmarshal(sig, &out.Signature); err != nil {
		return nil, errors.New("the converted model's signature.json is unreadable: " + err.Error())
	}
	if out.TFLite == nil && out.Fallback == nil {
		return nil, errors.New("the converted model has neither model.tflite nor fallback.json")
	}
	return &out, nil
}

// walkBundle calls fn on each regular file of the tar b, its name clean
// and inside the bundle.
func walkBundle(b []byte, fn func(name string, r io.Reader) error) error {
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("the converted model is not a whole tar: " + err.Error())
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean("/" + h.Name)[1:]
		if name == "" || strings.HasPrefix(name, "../") {
			return errors.New("the converted model holds a bad file name " + h.Name)
		}
		if err := fn(name, tr); err != nil {
			return err
		}
	}
}

// extractSavedModel writes the bundle's savedmodel/ under dir.
func extractSavedModel(b []byte, dir string) error {
	found := false
	err := walkBundle(b, func(name string, r io.Reader) error {
		rel, ok := strings.CutPrefix(name, "savedmodel/")
		if !ok {
			return nil
		}
		found = true
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
	if err == nil && !found {
		err = errors.New("the converted model holds no savedmodel/")
	}
	return err
}

// tarDir is the tar of the regular files under dir, named relative to it.
func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
