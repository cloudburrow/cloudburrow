package tfconvert

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Engine converts the SavedModel in dir to TensorFlow Lite.
type Engine interface {
	Convert(ctx context.Context, dir string) ([]byte, error)
}

// Server serves POST /v1/convert: a tar of a SavedModel's files in, the
// TensorFlow Lite flatbuffer out (200, application/octet-stream), or a
// Refusal as JSON (422; 503 when the converter cannot run at all).
type Server struct {
	engine Engine
	mu     sync.Mutex // one conversion at a time: TensorFlow is large
	tmp    string
}

// NewServer serves engine, with its SavedModels unpacked under tmp ("":
// the system's temporary directory).
func NewServer(engine Engine, tmp string) *Server { return &Server{engine: engine, tmp: tmp} }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/convert" || r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	dir, err := os.MkdirTemp(s.tmp, "cb-savedmodel-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	model := filepath.Join(dir, "model")
	if rf := untar(io.LimitReader(r.Body, MaxModelBytes+1<<20), model); rf != nil {
		refuse(w, http.StatusUnprocessableEntity, rf)
		return
	}
	s.mu.Lock()
	data, err := s.engine.Convert(r.Context(), model)
	s.mu.Unlock()
	var rf *Refusal
	switch {
	case errors.As(err, &rf):
		code := http.StatusUnprocessableEntity
		if rf.Kind == "unavailable" {
			code = http.StatusServiceUnavailable
		}
		refuse(w, code, rf)
	case err != nil:
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

func refuse(w http.ResponseWriter, code int, rf *Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(rf)
}

// untar writes the tar's regular files under dir, refusing any path that
// would leave it.
func untar(r io.Reader, dir string) *Refusal {
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &Refusal{Kind: "format", Message: "the SavedModel's files did not arrive whole: " + err.Error()}
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean("/" + h.Name)[1:]
		if name == "" || strings.HasPrefix(name, "../") {
			return &Refusal{Kind: "format", Message: "bad file name " + h.Name}
		}
		if total += h.Size; total > MaxModelBytes {
			return &Refusal{Kind: "limit", Message: "Models are limited to 450 MB in size"}
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return &Refusal{Kind: "unavailable", Message: err.Error()}
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return &Refusal{Kind: "unavailable", Message: err.Error()}
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return &Refusal{Kind: "format", Message: err.Error()}
		}
	}
}

// Docker runs the converter image with the docker CLI.
type Docker struct {
	// Image is the converter image's tag; Context its build context,
	// built when the tag is missing.
	Image   string
	Context func() (io.Reader, error)
	// Docker is the docker binary ("": docker on PATH).
	Docker string

	once  sync.Mutex
	built bool
}

func (d *Docker) bin() string {
	if d.Docker != "" {
		return d.Docker
	}
	return "docker"
}

// ensure builds the image when it is missing.
func (d *Docker) ensure(ctx context.Context) error {
	d.once.Lock()
	defer d.once.Unlock()
	if d.built {
		return nil
	}
	if exec.CommandContext(ctx, d.bin(), "image", "inspect", d.Image).Run() == nil {
		d.built = true
		return nil
	}
	if d.Context == nil {
		return &Refusal{Kind: "unavailable", Message: "the converter image " + d.Image + " is not built"}
	}
	tarball, err := d.Context()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, d.bin(), "build", "-t", d.Image, "-")
	cmd.Stdin = tarball
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		tail := out.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return &Refusal{Kind: "unavailable", Message: fmt.Sprintf("could not build the TensorFlow converter image %s: %v: %s", d.Image, err, tail)}
	}
	d.built = true
	return nil
}

// Convert runs the image on dir, with no network.
func (d *Docker) Convert(ctx context.Context, dir string) ([]byte, error) {
	if err := d.ensure(ctx); err != nil {
		return nil, err
	}
	out := filepath.Join(filepath.Dir(dir), "out")
	if err := os.MkdirAll(out, 0o777); err != nil {
		return nil, err
	}
	// The image runs as nobody, which must write /out.
	if err := os.Chmod(out, 0o777); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin(), "run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp",
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=2",
		"-v", dir+":/model:ro", "-v", out+":/out", d.Image, "convert")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		tail := log.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return nil, &Refusal{Kind: "convert", Message: fmt.Sprintf("the TensorFlow converter failed (%v): %s", err, strings.TrimSpace(tail))}
	}
	if b, err := os.ReadFile(filepath.Join(out, "error.json")); err == nil {
		return nil, decodeRefusal(b)
	}
	b, err := os.ReadFile(filepath.Join(out, "model.tflite"))
	if err != nil {
		return nil, &Refusal{Kind: "convert", Message: "the TensorFlow converter wrote no model: " + strings.TrimSpace(log.String())}
	}
	return b, nil
}
