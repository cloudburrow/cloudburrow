package main

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// TestONNXRunnerOnlyInTheOnnxBuild (#1711): --port-onnx is bound, and
// checked by up and doctor, only by the onnx build with BigQuery enabled;
// the default build never binds it, so an instance from before the port
// existed comes up as it did.
func TestONNXRunnerOnlyInTheOnnxBuild(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceBigQuery}
	_, bound := hostPorts(cfg)["onnx"]
	if bound != onnxmodel.Available || (newONNXRunner(cfg, io.Discard) != nil) != onnxmodel.Available {
		t.Errorf("onnx build %v: port bound %v", onnxmodel.Available, bound)
	}
	cfg.Services = []config.Service{config.ServiceStorage}
	if _, bound := hostPorts(cfg)["onnx"]; bound || newONNXRunner(cfg, io.Discard) != nil {
		t.Error("the ONNX runner without BigQuery")
	}
	if cfg.Endpoints.ONNX != 9025 {
		t.Errorf("default port %d, want 9025", cfg.Endpoints.ONNX)
	}
}

// TestONNXRunnerServes: the runner on its port, behind the Host check, and
// answering whether ONNX Runtime loads (501 in the default build).
func TestONNXRunnerServes(t *testing.T) {
	if !onnxmodel.Available {
		t.Skip("the default build serves no ONNX runner")
	}
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceBigQuery}
	cfg.Endpoints.ONNX = 0
	s := newONNXRunner(cfg, io.Discard)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(context.Background())
	resp, err := http.Get("http://" + s.Addr() + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d", resp.StatusCode)
	}
}
