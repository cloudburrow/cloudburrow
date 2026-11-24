package tfconvert

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostmount"
)

// The TensorFlow runtime (#1775). BigQuery runs an imported SavedModel
// with TensorFlow itself, so a model TensorFlow Lite's builtins cannot take
// (it needs Flex, SELECT_TF_OPS, or TFLiteConverter cannot lower it) still
// predicts there, and ML.EXPLAIN_PREDICT's gradients need TensorFlow too.
// Here such a model runs in a long-lived predictor: the converter image's
// serve mode (deploy/tfconvert/convert.py) on the bundle's savedmodel/, on
// the --internal Network (no route out), spoken to over its stdin and
// stdout, one JSON line each way per batch of rows. A predictor loads the
// SavedModel once and stays up while it is used: at most maxRunners at a
// time, each stopped after runnerIdle without a request, and all of them
// when the server closes.
//
//   POST /v1/models?model=<sha256>: the bundle, whose SHA-256 it must be;
//        204. The server keeps its savedmodel/ for /v1/run.
//   POST /v1/run?model=<sha256>: one request line (convert.py's serve);
//        200 with its answer line, 404 {"kind": "missing"} when the server
//        does not hold the model (the client then sends it), 422 a Refusal
//        of kind "run" (TensorFlow's error), 503 one of kind "unavailable".

const (
	maxRunners = 2
	runnerIdle = 10 * time.Minute
	// runnerStart bounds a predictor's start: TensorFlow's import and the
	// SavedModel's load, under emulation on an arm64 host.
	runnerStart = 5 * time.Minute
)

// Runner is one model's predictor.
type Runner interface {
	// Do sends one request line and answers its answer line.
	Do(ctx context.Context, req []byte) ([]byte, error)
	Close() error
}

// Serving starts predictors: Docker does.
type Serving interface {
	Serve(ctx context.Context, dir string) (Runner, error)
}

type runnerEntry struct {
	r    Runner
	last time.Time
	busy int
}

// runners is a Server's models and predictors.
type runners struct {
	mu     sync.Mutex
	dir    string // models/<sha256>/, made on the first load
	live   map[string]*runnerEntry
	starts map[string]chan struct{} // a predictor being started
	reaper bool
	closed bool
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) modelDir(sum string) (string, error) {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	if s.run.closed {
		return "", errors.New("the TensorFlow converter is stopping")
	}
	if s.run.dir == "" {
		d, err := os.MkdirTemp(s.tmp, "cb-tfmodels-")
		if err != nil {
			return "", err
		}
		s.run.dir = d
	}
	return filepath.Join(s.run.dir, sum), nil
}

func (s *Server) serveLoad(w http.ResponseWriter, r *http.Request) {
	sum := r.URL.Query().Get("model")
	if r.Method != http.MethodPost || !shaPattern.MatchString(sum) {
		http.Error(w, "POST /v1/models?model=<sha256>", http.StatusBadRequest)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 2*MaxModelBytes+1<<20))
	if err != nil {
		refuse(w, http.StatusUnprocessableEntity, &Refusal{Kind: "format", Message: "the model did not arrive whole: " + err.Error()})
		return
	}
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != sum {
		refuse(w, http.StatusUnprocessableEntity, &Refusal{Kind: "format", Message: "the model's bytes are not those of SHA-256 " + sum})
		return
	}
	dir, err := s.modelDir(sum)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tmp := dir + ".part-" + randomName()
	if err := extractSavedModel(b, tmp); err != nil {
		os.RemoveAll(tmp)
		refuse(w, http.StatusUnprocessableEntity, &Refusal{Kind: "format", Message: err.Error()})
		return
	}
	_ = os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644)
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp) // another load of the same model won
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveRun(w http.ResponseWriter, r *http.Request) {
	sum := r.URL.Query().Get("model")
	if r.Method != http.MethodPost || !shaPattern.MatchString(sum) {
		http.Error(w, "POST /v1/run?model=<sha256>", http.StatusBadRequest)
		return
	}
	req, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir, err := s.modelDir(sum)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: err.Error()})
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err != nil {
		refuse(w, http.StatusNotFound, &Refusal{Kind: "missing", Message: "the converter does not hold model " + sum})
		return
	}
	run, err := s.runner(r.Context(), sum, dir)
	if err != nil {
		var rf *Refusal
		if !errors.As(err, &rf) {
			rf = &Refusal{Kind: "unavailable", Message: err.Error()}
		}
		refuse(w, http.StatusServiceUnavailable, rf)
		return
	}
	ans, err := run.Do(r.Context(), bytes.TrimSpace(req))
	s.release(sum, run, err != nil)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, &Refusal{Kind: "unavailable", Message: "the TensorFlow predictor failed: " + err.Error()})
		return
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(ans, &e) == nil && e.Error != "" {
		refuse(w, http.StatusUnprocessableEntity, &Refusal{Kind: "run", Message: e.Error})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(ans)
}

// runner is the model's live predictor, started when there is none.
func (s *Server) runner(ctx context.Context, sum, dir string) (Runner, error) {
	sv, ok := s.engine.(Serving)
	if !ok {
		return nil, &Refusal{Kind: "unavailable", Message: "this converter runs no TensorFlow predictor"}
	}
	for {
		s.run.mu.Lock()
		if s.run.closed {
			s.run.mu.Unlock()
			return nil, errors.New("the TensorFlow converter is stopping")
		}
		if s.run.live == nil {
			s.run.live, s.run.starts = map[string]*runnerEntry{}, map[string]chan struct{}{}
		}
		if e, ok := s.run.live[sum]; ok {
			e.busy++
			e.last = time.Now()
			s.run.mu.Unlock()
			return e.r, nil
		}
		if ch, ok := s.run.starts[sum]; ok {
			s.run.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		s.run.starts[sum] = ch
		evict := s.evictLocked()
		if !s.run.reaper {
			s.run.reaper = true
			go s.reap()
		}
		s.run.mu.Unlock()
		for _, r := range evict {
			_ = r.Close()
		}
		// Its own deadline, not the request's: one that gives up leaves
		// the predictor starting for the next.
		sctx, cancel := context.WithTimeout(context.Background(), runnerStart)
		r, err := sv.Serve(sctx, dir)
		cancel()
		s.run.mu.Lock()
		delete(s.run.starts, sum)
		close(ch)
		if err == nil {
			if s.run.closed {
				s.run.mu.Unlock()
				_ = r.Close()
				return nil, errors.New("the TensorFlow converter is stopping")
			}
			s.run.live[sum] = &runnerEntry{r: r, last: time.Now(), busy: 1}
		}
		s.run.mu.Unlock()
		return r, err
	}
}

// evictLocked removes the least recently used idle predictors so one more
// fits in maxRunners, answering them for the caller to close.
func (s *Server) evictLocked() []Runner {
	var out []Runner
	for len(s.run.live)+len(s.run.starts) > maxRunners {
		old, oldest := "", time.Time{}
		for k, e := range s.run.live {
			if e.busy == 0 && (old == "" || e.last.Before(oldest)) {
				old, oldest = k, e.last
			}
		}
		if old == "" {
			break // all busy: one more for now, the reaper trims it
		}
		out = append(out, s.run.live[old].r)
		delete(s.run.live, old)
	}
	return out
}

// release ends a request on r; a predictor that failed is stopped.
func (s *Server) release(sum string, r Runner, failed bool) {
	s.run.mu.Lock()
	e, ok := s.run.live[sum]
	if ok && e.r == r {
		e.busy--
		e.last = time.Now()
		if failed {
			delete(s.run.live, sum)
		}
	}
	s.run.mu.Unlock()
	if failed {
		_ = r.Close()
	}
}

// reap stops predictors idle for runnerIdle.
func (s *Server) reap() {
	for {
		time.Sleep(time.Minute)
		var idle []Runner
		s.run.mu.Lock()
		if s.run.closed {
			s.run.mu.Unlock()
			return
		}
		for k, e := range s.run.live {
			if e.busy == 0 && time.Since(e.last) > runnerIdle {
				idle = append(idle, e.r)
				delete(s.run.live, k)
			}
		}
		idle = append(idle, s.evictLocked()...)
		s.run.mu.Unlock()
		for _, r := range idle {
			_ = r.Close()
		}
	}
}

// Close stops every predictor and removes the models the server holds.
func (s *Server) Close() error {
	s.run.mu.Lock()
	s.run.closed = true
	var all []Runner
	for _, e := range s.run.live {
		all = append(all, e.r)
	}
	s.run.live = nil
	dir := s.run.dir
	s.run.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range all {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Close() }()
	}
	wg.Wait()
	if dir != "" {
		return os.RemoveAll(dir)
	}
	return nil
}

func randomName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// dockerRunner is a predictor container.
type dockerRunner struct {
	d    *Docker
	name string
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	log  *tailBuffer
	mu   sync.Mutex // one request at a time on its pipes
	once sync.Once
	dead bool
	// model is the SavedModel's mount, a volume on a remote compute host
	// removed when the predictor stops.
	model *hostmount.Mount
}

// tailBuffer keeps the last of what it is written: the predictor's stderr.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 4000 {
		t.b = t.b[len(t.b)-4000:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.b))
}

// Serve starts the image's predictor on the SavedModel in dir, on the
// --internal Network, and waits for it to load.
func (d *Docker) Serve(ctx context.Context, dir string) (Runner, error) {
	if err := d.ensure(ctx); err != nil {
		return nil, err
	}
	if err := d.EnsureNetwork(ctx); err != nil {
		return nil, err
	}
	r := &dockerRunner{d: d, name: "cloudburrow-tfpredict-" + randomName(), log: &tailBuffer{}}
	model, err := hostmount.Scratch(ctx, dir, "/model", hostmount.ReadOnly)
	if err != nil {
		return nil, &Refusal{Kind: "unavailable", Message: err.Error()}
	}
	r.model = model
	args := []string{"run", "-i", "--rm", "--name", r.name, "--network", Network, "--read-only", "--tmpfs", "/tmp",
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=2"}
	r.cmd = exec.Command(d.bin(), append(append(args, model.Args()...), d.Image, "serve")...)
	in, err := r.cmd.StdinPipe()
	if err != nil {
		model.Release()
		return nil, err
	}
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r.in, r.out = in, bufio.NewReaderSize(out, 1<<20)
	r.cmd.Stderr = r.log
	if err := r.cmd.Start(); err != nil {
		model.Release()
		return nil, &Refusal{Kind: "unavailable", Message: "could not start the TensorFlow predictor: " + err.Error()}
	}
	d.logf("tensorflow predictor: starting %s", r.name)
	ready, err := r.line(ctx)
	if err == nil && !bytes.Contains(ready, []byte(`"ready"`)) {
		err = fmt.Errorf("it answered %q", ready)
	}
	if err != nil {
		_ = r.Close()
		return nil, &Refusal{Kind: "unavailable", Message: fmt.Sprintf("the TensorFlow predictor did not start (%v): %s", err, r.log.String())}
	}
	return r, nil
}

// line reads one line of the predictor's stdout, as long as ctx allows;
// a predictor that does not answer in time is stopped.
func (r *dockerRunner) line(ctx context.Context) ([]byte, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := r.out.ReadBytes('\n')
		ch <- res{b, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			r.dead = true
			return nil, fmt.Errorf("the predictor exited: %v: %s", x.err, r.log.String())
		}
		return bytes.TrimSpace(x.b), nil
	case <-ctx.Done():
		r.dead = true
		go r.Close()
		return nil, ctx.Err()
	}
}

func (r *dockerRunner) Do(ctx context.Context, req []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead {
		return nil, errors.New("the predictor has stopped")
	}
	if _, err := r.in.Write(append(append([]byte{}, req...), '\n')); err != nil {
		r.dead = true
		return nil, fmt.Errorf("the predictor exited: %v: %s", err, r.log.String())
	}
	return r.line(ctx)
}

// Close ends the predictor: its stdin closed, which ends serve, and the
// container removed if it is still there.
func (r *dockerRunner) Close() error {
	r.once.Do(func() {
		_ = r.in.Close()
		done := make(chan struct{})
		go func() { _ = r.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = exec.CommandContext(ctx, r.d.bin(), "rm", "-f", r.name).Run()
			cancel()
			_ = r.cmd.Process.Kill()
			<-done
		}
		r.model.Release()
		r.d.logf("tensorflow predictor: stopped %s", r.name)
	})
	return nil
}
