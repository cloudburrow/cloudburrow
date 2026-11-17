package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/onnxrunner"
)

// onnxRunnerEnabled is whether `up` serves the ONNX runner (#1711): in the
// onnx build (make build-onnx), whose binary has ONNX Runtime, with
// BigQuery enabled, whose front runs imported ONNX models on it. The
// default build never binds --port-onnx.
func onnxRunnerEnabled(cfg config.Config) bool {
	return onnxmodel.Available && serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.ONNX >= 0
}

// onnxRunnerService serves internal/onnxrunner on --port-onnx: on loopback,
// and to the BigQuery front through cloudburrow-host.
type onnxRunnerService struct {
	addr   string
	out    io.Writer
	runner *onnxrunner.Server
	engine onnxrunner.Engine
	ln     net.Listener
	srv    *http.Server
}

// newONNXRunner is the runner, or nil when onnxRunnerEnabled is false.
func newONNXRunner(cfg config.Config, out io.Writer) *onnxRunnerService {
	if !onnxRunnerEnabled(cfg) {
		return nil
	}
	engine := onnxrunner.Local{}
	return &onnxRunnerService{addr: net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.ONNX)), out: out,
		runner: onnxrunner.NewServer(engine, 0), engine: engine}
}

func (s *onnxRunnerService) Name() string { return "onnx-runner" }

// Start binds the port. ONNX Runtime not loading does not stop `up`: the
// runner answers each model 501 with the reason, which is said here too.
func (s *onnxRunnerService) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("onnx runner: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	// Loopback-only, but a rebound or cross-site page reaches loopback
	// too (#676); the front names cloudburrow-host, which Wrap accepts.
	s.srv = &http.Server{Handler: hostguard.Wrap(s.runner), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	if err := s.engine.Init(); err != nil {
		fmt.Fprintf(s.out, "BigQuery ML: ONNX Runtime did not load, so imported ONNX models are 501: %v\n", err)
	}
	return nil
}

func (s *onnxRunnerService) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, "" before Start or for a nil runner.
func (s *onnxRunnerService) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}
