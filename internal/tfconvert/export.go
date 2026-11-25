package tfconvert

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Exporter writes the TensorFlow SavedModel of a BigQuery ML model to a
// directory, for EXPORT MODEL (#2210): spec is the model's preprocessing
// and weights as the BigQuery front writes them
// (internal/bigqueryfront/modelexport.go), which the converter image's
// export mode (deploy/tfconvert/convert.py) builds a SavedModel of with
// TensorFlow's own tf.saved_model.save.
type Exporter interface {
	Export(ctx context.Context, spec []byte, dir string) error
}

// serveExport is POST /v1/export: the spec in, a tar of the SavedModel's
// files out (200, application/x-tar), or a Refusal.
func (s *Server) serveExport(w http.ResponseWriter, r *http.Request) {
	ex, ok := s.engine.(Exporter)
	if !ok {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: "this converter does not export models"})
		return
	}
	spec, err := io.ReadAll(io.LimitReader(r.Body, MaxModelBytes+1))
	if err != nil || len(spec) > MaxModelBytes {
		refuse(w, http.StatusUnprocessableEntity, &Refusal{Kind: "limit", Message: "the model is too large to export"})
		return
	}
	dir, err := os.MkdirTemp(s.tmp, "cb-export-")
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
		return
	}
	defer os.RemoveAll(dir)
	s.mu.Lock()
	err = ex.Export(r.Context(), spec, dir)
	s.mu.Unlock()
	var rf *Refusal
	switch {
	case errors.As(err, &rf):
		code := http.StatusUnprocessableEntity
		if rf.Kind == "unavailable" {
			code = http.StatusServiceUnavailable
		}
		refuse(w, code, rf)
		return
	case err != nil:
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
		return
	}
	b, err := tarDir(dir) // bundle.go's
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	_, _ = w.Write(b)
}

// Export runs the image's export mode on spec, with no network, and
// leaves the SavedModel's files, and nothing else, in dir.
func (d *Docker) Export(ctx context.Context, spec []byte, dir string) error {
	if err := d.ensure(ctx); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(dir), "cb-export-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	in := filepath.Join(work, "spec")
	out := filepath.Join(work, "out")
	for _, p := range []string{in, out} {
		if err := os.MkdirAll(p, 0o777); err != nil {
			return err
		}
		// The image runs as nobody, which must write /out.
		if err := os.Chmod(p, 0o777); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(in, "spec.json"), spec, 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin(), "run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp",
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=2",
		"-v", in+":/spec:ro", "-v", out+":/out", d.Image, "export")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		tail := log.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return &Refusal{Kind: "convert", Message: fmt.Sprintf("the TensorFlow exporter failed (%v): %s", err, strings.TrimSpace(tail))}
	}
	if b, err := os.ReadFile(filepath.Join(out, "error.json")); err == nil {
		return decodeRefusal(b)
	}
	model := filepath.Join(out, "model")
	if _, err := os.Stat(filepath.Join(model, "saved_model.pb")); err != nil {
		return &Refusal{Kind: "convert", Message: "the TensorFlow exporter wrote no SavedModel: " + strings.TrimSpace(log.String())}
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	return os.Rename(model, dir)
}

// Export sends spec to the converter and returns the SavedModel's files,
// or a *Refusal.
func (c *Client) Export(ctx context.Context, spec []byte) ([]File, error) {
	if c.base == "" {
		return nil, &Refusal{Kind: "unavailable", Message: "no TensorFlow converter address"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/export", bytes.NewReader(spec))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter could not be reached: " + err.Error()}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxModelBytes*2))
	if err != nil {
		return nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter's answer was cut short: " + err.Error()}
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnprocessableEntity, http.StatusServiceUnavailable:
		return nil, decodeRefusal(body)
	default:
		return nil, &Refusal{Kind: "unavailable", Message: fmt.Sprintf("the TensorFlow converter answered HTTP %d", res.StatusCode)}
	}
	var files []File
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, &Refusal{Kind: "unavailable", Message: "the exported SavedModel did not arrive whole: " + err.Error()}
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Name: h.Name, Data: b})
	}
}
