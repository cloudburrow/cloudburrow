package onnxrunner

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// Server is the runner's HTTP API:
//
//	PUT  /v1/models/{sha256}      the model's bytes; answers its ports
//	POST /v1/models/{sha256}:run  a runRequest; answers a runResponse
//	GET  /v1/status               whether ONNX Runtime loads
//
// A refusal is {"error": "..."} with the status Error documents.
type Server struct {
	engine   Engine
	maxBytes int64

	mu     sync.Mutex
	models map[string]*list.Element // sha256 -> *cached in lru
	lru    *list.List               // front: most recently used
	bytes  int64
	// loads counts the models' loads, for tests.
	loads int
}

type cached struct {
	sum  string
	data []byte
}

// NewServer runs models on engine, keeping at most maxBytes of them
// (DefaultCacheBytes when 0 or less).
func NewServer(engine Engine, maxBytes int64) *Server {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &Server{engine: engine, maxBytes: maxBytes, models: map[string]*list.Element{}, lru: list.New()}
}

// Cached is the SHA-256 of every model held, most recently used first.
func (s *Server) Cached() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for e := s.lru.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*cached).sum)
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/status" && r.Method == http.MethodGet {
		if err := s.engine.Init(); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": len(s.Cached())})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/models/")
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{"onnxrunner: no such path " + r.URL.Path})
		return
	}
	sum, verb, _ := strings.Cut(rest, ":")
	if !validSum(sum) {
		writeJSON(w, http.StatusBadRequest, errorBody{"onnxrunner: a model is named by the hex SHA-256 of its bytes"})
		return
	}
	switch {
	case verb == "" && r.Method == http.MethodPut:
		s.load(w, r, sum)
	case verb == "run" && r.Method == http.MethodPost:
		s.run(w, r, sum)
	default:
		w.Header().Set("Allow", "PUT, POST")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"onnxrunner: PUT /v1/models/{sha256} or POST /v1/models/{sha256}:run"})
	}
}

func validSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// load keeps the model the body holds, and answers its ports.
func (s *Server) load(w http.ResponseWriter, r *http.Request, sum string) {
	if err := s.engine.Init(); err != nil {
		s.fail(w, err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxModelBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"onnxrunner: could not read the model: " + err.Error()})
		return
	}
	if len(data) > MaxModelBytes {
		writeJSON(w, http.StatusBadRequest, errorBody{"the ONNX model is larger than the maximum model size, 450 MB"})
		return
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != sum {
		writeJSON(w, http.StatusBadRequest, errorBody{"onnxrunner: the model's bytes do not have the SHA-256 its path names"})
		return
	}
	in, out, err := s.engine.Inspect(data)
	if err != nil {
		s.fail(w, &Error{Status: modelStatus(err), Message: err.Error()})
		return
	}
	s.keep(sum, data)
	writeJSON(w, http.StatusOK, ports{Inputs: in, Outputs: out})
}

// keep adds the model, evicting the least recently used until the cache
// fits. A model larger than the whole cache is still kept, alone, so the
// run that follows its load finds it.
func (s *Server) keep(sum string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if e, ok := s.models[sum]; ok {
		s.lru.MoveToFront(e)
		return
	}
	s.models[sum] = s.lru.PushFront(&cached{sum: sum, data: data})
	s.bytes += int64(len(data))
	for s.bytes > s.maxBytes && s.lru.Len() > 1 {
		e := s.lru.Back()
		c := e.Value.(*cached)
		s.lru.Remove(e)
		delete(s.models, c.sum)
		s.bytes -= int64(len(c.data))
	}
}

// get is a held model's bytes, marking it used.
func (s *Server) get(sum string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.models[sum]
	if !ok {
		return nil, false
	}
	s.lru.MoveToFront(e)
	return e.Value.(*cached).data, true
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, sum string) {
	data, ok := s.get(sum)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{"onnxrunner: model " + sum + " is not loaded"})
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"onnxrunner: could not read the run: " + err.Error()})
		return
	}
	res, err := s.engine.Run(data, req.Inputs, fromWire(req.Tensors), req.Outputs)
	if err != nil {
		s.fail(w, &Error{Status: modelStatus(err), Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, runResponse{Tensors: toWire(res)})
}

// modelStatus is 501 for a runner with no runtime, else 400: the model,
// or the input it was given, failed.
func modelStatus(err error) int {
	if errors.Is(err, onnxmodel.ErrNotCompiled) {
		return http.StatusNotImplemented
	}
	return http.StatusBadRequest
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		// Init: the runtime is not compiled in, or does not load.
		e = &Error{Status: http.StatusNotImplemented, Message: err.Error()}
	}
	writeJSON(w, e.Status, errorBody{e.Message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(errorBody{fmt.Sprintf("onnxrunner: could not write the answer: %v", err)})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
