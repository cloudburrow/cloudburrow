package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Cloud KMS is reported wherever the other in-process services are (#386).

func kmsConfig(t *testing.T, services string) config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{
		Args:   []string{"--name", "kmsstatus", "--state-dir", t.TempDir(), "--services", services, "--port-kms", "9118"},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startedKMS runs the service on an OS-assigned port over an in-memory store.
func startedKMS(t *testing.T, cfg config.Config, reg *metrics.Registry) *kmsService {
	t.Helper()
	cfg.Endpoints.KMS = 0
	svc := &kmsService{cfg: cfg, db: store.NewMemory()}
	if reg != nil {
		svc.calls = callEvents(nil, reg, "kms")
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return svc
}

func kmsStatus(r statusReport) (statusService, bool) {
	for _, s := range r.Services {
		if s.ID == string(config.ServiceKMS) {
			return s, true
		}
	}
	return statusService{}, false
}

func TestStatusJSONReportsTheKMSEndpoint(t *testing.T) {
	cfg := kmsConfig(t, "storage,kms")
	r, _ := buildStatusReport(cfg, nil, "stopped", "")
	if s, ok := kmsStatus(r); !ok || !strings.HasSuffix(s.Endpoint, ":9118") {
		t.Errorf("stopped: kms = %+v (listed %v), want the configured port 9118", s, ok)
	}
	live := &liveState{info: runtimeInfo{PID: 1, Control: "127.0.0.1:49152", Endpoints: map[string]string{
		"control": "127.0.0.1:49152", "kms": "127.0.0.1:49999"}},
		readiness: &readiness{Ready: true, State: "running", Components: map[string]bool{"control": true, "cluster": true, "kms": true}}}
	r, _ = buildStatusReport(cfg, live, "running", "")
	if s, ok := kmsStatus(r); !ok || s.Endpoint != "127.0.0.1:49999" {
		t.Errorf("running: kms = %+v (listed %v), want the live port", s, ok)
	}
}

func TestStartupBannerListsKMSOnlyWhenEnabled(t *testing.T) {
	svc := startedKMS(t, kmsConfig(t, "storage,kms"), nil)
	eps := startupEndpoints(svc.cfg, nil, nil, nil, nil, svc, "")
	found := false
	for _, e := range eps {
		if e.Service == "kms" {
			found = e.Host == svc.Addr()
		}
	}
	if !found {
		t.Errorf("banner endpoints %+v lack kms at %s", eps, svc.Addr())
	}
	for _, e := range startupEndpoints(kmsConfig(t, "storage"), nil, nil, nil, nil, nil, "") {
		if e.Service == "kms" {
			t.Errorf("kms listed while disabled: %+v", e)
		}
	}
}

func TestKMSCallsAreMeasured(t *testing.T) {
	cfg := kmsConfig(t, "storage,kms")
	reg := metrics.New(unmeasuredServices(cfg)...)
	svc := startedKMS(t, cfg, reg)
	ctx := context.Background()
	c, err := kmsapi.NewKeyManagementClient(ctx, option.WithEndpoint(svc.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/demo-project/locations/global", KeyRingId: "r"}); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(metricsHandler(reg))
	defer h.Close()
	resp, err := http.Get(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	fams := parseMetrics(t, readAll(resp))
	if v, ok := counter(fams, "cloudburrow_requests_total", map[string]string{"service": "kms", "method": "CreateKeyRing", "code": "OK"}); !ok || v != 1 {
		t.Errorf("CreateKeyRing OK = %v (found %v), want 1", v, ok)
	}
	if _, ok := counter(fams, "cloudburrow_service_measured", map[string]string{"service": "kms"}); ok {
		t.Error("kms is reported as unmeasured")
	}
}

func TestDoctorChecksTheKMSPortWhenEnabled(t *testing.T) {
	if _, ok := doctorOptions(kmsConfig(t, "storage")).Ports["kms"]; ok {
		t.Error("doctor checks the kms port while kms is disabled")
	}
	opts := doctorOptions(kmsConfig(t, "storage,kms"))
	if opts.Ports["kms"] != 9118 {
		t.Fatalf("doctor kms port = %d, want 9118", opts.Ports["kms"])
	}
	env := doctor.Env{
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		PortFree: func(_ string, port int) error {
			if port == 9118 {
				return errors.New("address already in use")
			}
			return nil
		},
		DiskFree: func(p string) (uint64, string, error) { return 1 << 40, p, nil },
		HomeDir:  os.UserHomeDir,
		GOOS:     "linux",
	}
	for _, r := range doctor.Run(context.Background(), env, opts).Results {
		if r.Name == "port kms" {
			if r.Level != doctor.LevelFail || !strings.Contains(r.Detail, fmt.Sprint(9118)) {
				t.Errorf("port kms = %+v, want a failure naming 9118", r)
			}
			return
		}
	}
	t.Error("doctor reported nothing for a busy kms port")
}
