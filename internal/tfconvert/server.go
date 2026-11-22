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

// Engine converts the SavedModel in dir to a bundle (bundle.go).
type Engine interface {
	Convert(ctx context.Context, dir string) ([]byte, error)
}

// Server serves POST /v1/convert: a tar of a SavedModel's files in, the
// bundle out (200, application/x-tar), or a Refusal as JSON (422; 503
// when the converter cannot run at all); and the TensorFlow runtime of
// converted models, POST /v1/models and /v1/run (predict.go).
type Server struct {
	engine Engine
	mu     sync.Mutex // one conversion at a time: TensorFlow is large
	tmp    string

	run runners // predict.go
}

// NewServer serves engine, with its SavedModels unpacked under tmp ("":
// the system's temporary directory).
func NewServer(engine Engine, tmp string) *Server { return &Server{engine: engine, tmp: tmp} }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		s.serveLoad(w, r)
		return
	case "/v1/run":
		s.serveRun(w, r)
		return
	}
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
		w.Header().Set("Content-Type", "application/x-tar")
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
	// Logf, when set, is told when a build starts and how it ended.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	built bool
	build *imageBuild // the build under way, nil when none is
}

// imageBuild is one `docker build` of the converter image; done closes
// when it ends, with err its outcome.
type imageBuild struct {
	done chan struct{}
	err  error
}

// BuildTimeout bounds one build of the converter image: pulling Google's
// tensorflow/tensorflow base (about 2.9 GB). It is not a caller's
// deadline: see ensure.
const BuildTimeout = 30 * time.Minute

func (d *Docker) bin() string {
	if d.Docker != "" {
		return d.Docker
	}
	return "docker"
}

func (d *Docker) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Build builds the image when it is missing and waits for it, as long as
// ctx allows (`cloudburrow prefetch --tensorflow-converter`, for an
// instance that will run offline).
func (d *Docker) Build(ctx context.Context) error { return d.ensure(ctx) }

// start returns the build under way, starting one when the image is
// neither built nor being built, or nil once the image is there.
func (d *Docker) start() *imageBuild {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.built {
		return nil
	}
	if d.build == nil {
		b := &imageBuild{done: make(chan struct{})}
		d.build = b
		go func() {
			b.err = d.buildImage()
			d.mu.Lock()
			d.built, d.build = b.err == nil, nil
			d.mu.Unlock()
			close(b.done)
		}()
	}
	return d.build
}

// ensure waits, at most as long as ctx allows, for the image.
//
// The image is built on the first CREATE MODEL … TENSORFLOW, never by
// `up`: it is 2.9 GB (Google's tensorflow/tensorflow), which an instance that imports no TensorFlow model
// should not pay. The build itself runs under its own BuildTimeout, never
// ctx: a cold build takes a minute or more (57 s on a workstation with the
// base cached, longer on a 4-CPU CI runner), and killing it with the
// request that started it meant a client with a shorter deadline restarted
// it from scratch on every attempt and never got a model (main run
// 44357900271: both TENSORFLOW tests' CREATE MODEL hit the harness's 60 s
// deadline). A caller that gives up leaves the build running for the next.
func (d *Docker) ensure(ctx context.Context) error {
	b := d.start()
	if b == nil {
		return nil
	}
	select {
	case <-b.done:
		return b.err
	case <-ctx.Done():
		return &Refusal{Kind: "unavailable", Message: "the TensorFlow converter image " + d.Image +
			" is still being built (its first use pulls TensorFlow); it goes on building, try again shortly: " + ctx.Err().Error()}
	}
}

// buildImage builds the image when it is missing.
func (d *Docker) buildImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), BuildTimeout)
	defer cancel()
	if exec.CommandContext(ctx, d.bin(), "image", "inspect", d.Image).Run() == nil {
		return nil
	}
	if d.Context == nil {
		return &Refusal{Kind: "unavailable", Message: "the converter image " + d.Image + " is not built"}
	}
	tarball, err := d.Context()
	if err != nil {
		return err
	}
	d.logf("tensorflow converter: building %s (TensorFlow, once)", d.Image)
	began := time.Now()
	cmd := exec.CommandContext(ctx, d.bin(), "build", "-t", d.Image, "-")
	cmd.Stdin = tarball
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		tail := out.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		d.logf("tensorflow converter: building %s failed after %s: %v", d.Image, time.Since(began).Round(time.Second), err)
		return &Refusal{Kind: "unavailable", Message: fmt.Sprintf("could not build the TensorFlow converter image %s: %v: %s", d.Image, err, tail)}
	}
	d.logf("tensorflow converter: built %s in %s", d.Image, time.Since(began).Round(time.Second))
	return nil
}

// Network is the Docker network the converter runs on: created
// --internal, so it has no route out of the host (no egress, no DNS to
// the outside), and nothing but the converter joins it.
const Network = "cloudburrow-tfconvert"

// EnsureNetwork creates Network, --internal, when it is missing. Two
// instances may race to create it; the loser finds it made.
func (d *Docker) EnsureNetwork(ctx context.Context) error {
	internal := func() (bool, bool) {
		out, err := exec.CommandContext(ctx, d.bin(), "network", "inspect", "--format", "{{.Internal}}", Network).Output()
		return err == nil, strings.TrimSpace(string(out)) == "true"
	}
	if ok, in := internal(); ok {
		if !in {
			return &Refusal{Kind: "unavailable", Message: "the Docker network " + Network + " exists but is not --internal; remove it so the converter can make it with no route out"}
		}
		return nil
	}
	out, err := exec.CommandContext(ctx, d.bin(), "network", "create", "--internal", Network).CombinedOutput()
	if ok, in := internal(); ok && in {
		return nil
	}
	return &Refusal{Kind: "unavailable", Message: fmt.Sprintf("could not create the --internal Docker network %s: %v: %s", Network, err, strings.TrimSpace(string(out)))}
}

// Convert runs the image on dir, on the --internal Network.
func (d *Docker) Convert(ctx context.Context, dir string) ([]byte, error) {
	if err := d.ensure(ctx); err != nil {
		return nil, err
	}
	if err := d.EnsureNetwork(ctx); err != nil {
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
	cmd := exec.CommandContext(ctx, d.bin(), "run", "--rm", "--network", Network, "--read-only", "--tmpfs", "/tmp",
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
	if _, err := os.Stat(filepath.Join(out, "signature.json")); err != nil {
		return nil, &Refusal{Kind: "convert", Message: "the TensorFlow converter wrote no model: " + strings.TrimSpace(log.String())}
	}
	// The bundle (bundle.go): the SavedModel, its signature, and the
	// TensorFlow Lite flatbuffer or why there is none.
	return tarDir(out)
}
