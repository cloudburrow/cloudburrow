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
// conversion is a `docker run` of the converter image, on an --internal network,
// (deploy/tfconvert), built on the first import; a model TensorFlow Lite
// cannot take predicts in a long-lived container of the same image there
// (internal/tfconvert's predict.go). The CLI itself runs no TensorFlow and
// stays static.
type tfConverterService struct {
	addr string
	out  io.Writer
	srv  *http.Server
	ln   net.Listener
	h    *tfconvert.Server
}

// newTFConverter is the converter, or nil when tfConverterEnabled is false.
func newTFConverter(cfg config.Config, out io.Writer) *tfConverterService {
	if !tfConverterEnabled(cfg) {
		return nil
	}
	return &tfConverterService{addr: net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.TFConvert)), out: out,
		h: tfconvert.NewServer(tfConverterEngine(out), "")}
}

// tfConverterEngine is the converter image, built on its first use, with
// the build's start and outcome written to out (up.log, or prefetch's
// output).
func tfConverterEngine(out io.Writer) *tfconvert.Docker {
	return &tfconvert.Docker{Image: tfconvertimage.Tag(), Context: tfconvertimage.Tar,
		Logf: func(format string, args ...any) { fmt.Fprintf(out, "cloudburrow: "+format+"\n", args...) }}
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
	err := s.srv.Shutdown(ctx)
	// The TensorFlow runtime's predictor containers (predict.go) go with it.
	if cerr := s.h.Close(); err == nil {
		err = cerr
	}
	return err
}

// Addr is the bound address, "" before Start or for a nil converter.
func (s *tfConverterService) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}
