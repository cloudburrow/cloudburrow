package sparkproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Engine runs one job.
type Engine interface {
	Run(ctx context.Context, job Job) (Result, error)
}

// Server serves POST /v1/run: a Job in, its Result out (200), or
// {"message": …} (503) when Spark cannot run at all.
type Server struct {
	engine Engine
	mu     sync.Mutex // one Spark application at a time: each is a JVM of its own
}

// NewServer serves engine.
func NewServer(engine Engine) *Server { return &Server{engine: engine} }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/run" || r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var job Job
	if err := json.NewDecoder(io.LimitReader(r.Body, MaxJobBytes)).Decode(&job); err != nil {
		http.Error(w, "the job cannot be read: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	res, err := s.engine.Run(r.Context(), job)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// Client is the front's side of the runner.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient is a client of the runner at base (http://host:port).
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		// A Spark application may run long; BigQuery's own limit on a
		// script is six hours.
		hc = &http.Client{Timeout: 6 * time.Hour}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

// Run runs job and returns how it ended, or an *Unavailable.
func (c *Client) Run(ctx context.Context, job Job) (Result, error) {
	if c == nil || c.base == "" {
		return Result{}, &Unavailable{Msg: "no Spark runner address"}
	}
	b, err := json.Marshal(job)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/run", bytes.NewReader(b))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return Result{}, &Unavailable{Msg: "the Spark runner cannot be reached: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusServiceUnavailable {
		return Result{}, decodeUnavailable(body)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("the Spark runner answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var res Result
	if err := json.Unmarshal(body, &res); err != nil {
		return Result{}, errors.New("the Spark runner's answer cannot be read: " + err.Error())
	}
	return res, nil
}
