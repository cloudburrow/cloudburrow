package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	tfconvertimage "github.com/cloudburrow/cloudburrow/deploy/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// tfConverterEnabled is whether `up` serves the TensorFlow SavedModel
// converter (#1775): with BigQuery enabled, whose front imports
// MODEL_TYPE='TENSORFLOW' models through it.
func tfConverterEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.TFConvert >= 0
}

// tfConverterService serves internal/tfconvert on --port-tfconvert: on
// loopback, and to the BigQuery front through cloudburrow-host. Each
// conversion is `docker run --network none` of the converter image
// (deploy/tfconvert), built on the first import; the CLI itself runs no
// TensorFlow and stays static.
type tfConverterService struct {
	addr string
	out  io.Writer
	srv  *http.Server
	ln   net.Listener
	h    http.Handler
}

// newTFConverter is the converter, or nil when tfConverterEnabled is false.
func newTFConverter(cfg config.Config, out io.Writer) *tfConverterService {
	if !tfConverterEnabled(cfg) {
		return nil
	}
	engine := &tfconvert.Docker{Image: tfconvertimage.Tag(), Context: tfconvertimage.Tar}
	return &tfConverterService{addr: net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.TFConvert)), out: out,
		h: tfconvert.NewServer(engine, "")}
}

func (s *tfConverterService) Name() string { return "tf-converter" }

func (s *tfConverterService) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("tensorflow converter: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	// Loopback-only, but a rebound or cross-site page reaches loopback
	// too (#676); the front names cloudburrow-host, which Wrap accepts.
	s.srv = &http.Server{Handler: hostguard.Wrap(s.h), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *tfConverterService) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, "" before Start or for a nil converter.
func (s *tfConverterService) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}
