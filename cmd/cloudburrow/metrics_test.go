package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// parseMetrics parses /metrics the way Prometheus does.
func parseMetrics(t *testing.T, body string) map[string]*dto.MetricFamily {
	t.Helper()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the exposition does not parse: %v\n%s", err, body)
	}
	return fams
}

func counter(fams map[string]*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	f, ok := fams[name]
	if !ok {
		return 0, false
	}
next:
	for _, m := range f.GetMetric() {
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		for k, v := range labels {
			if got[k] != v {
				continue next
			}
		}
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue(), true
		}
		if m.GetGauge() != nil {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// TestCreateTaskCallsAreCounted: N CreateTask calls through the official
// Cloud Tasks client, on the server wired as `up` wires it, make
// cloudburrow_requests_total{service="tasks",method="CreateTask",code="OK"}
// exactly N, in output the Prometheus parser accepts.
func TestCreateTaskCallsAreCounted(t *testing.T) {
	reg := metrics.New("storage", "pubsub")
	srv := grpctransport.New("127.0.0.1:0")
	srv.Observe(callEvents(nil, reg, "tasks"))
	if err := srv.Register(func(g *grpc.Server) { tasks.NewGRPCServer(tasks.NewStore(store.NewMemory())).Register(g) }); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ctx := context.Background()
	c, err := cloudtasks.NewClient(ctx, option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := "projects/metrics-proj/locations/us-central1"
	q, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: parent, Queue: &taskspb.Queue{Name: parent + "/queues/q"}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 7
	for i := 0; i < n; i++ {
		if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.Name, Task: &taskspb.Task{
			Name:        fmt.Sprintf("%s/tasks/t%d", q.Name, i),
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:9/x"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: parent + "/queues/absent"})

	h := httptest.NewServer(metricsHandler(reg))
	defer h.Close()
	resp, err := http.Get(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	if _, err := fmt.Fprint(&b, readAll(resp)); err != nil {
		t.Fatal(err)
	}
	fams := parseMetrics(t, b.String())
	if v, ok := counter(fams, "cloudburrow_requests_total", map[string]string{"service": "tasks", "method": "CreateTask", "code": "OK"}); !ok || v != n {
		t.Errorf("CreateTask OK = %v (found %v), want %d", v, ok, n)
	}
	if v, _ := counter(fams, "cloudburrow_requests_total", map[string]string{"service": "tasks", "method": "GetQueue", "code": "NOT_FOUND"}); v != 1 {
		t.Errorf("GetQueue NOT_FOUND = %v, want 1", v)
	}
	if _, ok := fams["cloudburrow_request_duration_seconds"]; !ok {
		t.Error("no latency histogram")
	}
	for _, s := range []string{"storage", "pubsub"} {
		if v, ok := counter(fams, "cloudburrow_service_measured", map[string]string{"service": s}); !ok || v != 0 {
			t.Errorf("%s is not reported as unmeasured", s)
		}
	}
}

func readAll(resp *http.Response) string {
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

// /metrics is served on the control port, which binds loopback whatever the
// bind address, and on no service port: a workload that can reach Secret
// Manager's API cannot read the instance's traffic figures through it.
func TestMetricsAreServedOnTheControlPortOnly(t *testing.T) {
	reg := metrics.New()
	control := lifecycle.NewControlServer(0, lifecycle.New(time.Second))
	control.Mount(func(mux *http.ServeMux) { mux.Handle("GET /metrics", metricsHandler(reg)) })
	if err := control.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Stop(context.Background()) })
	if host, _, _ := net.SplitHostPort(control.Addr()); host != "127.0.0.1" {
		t.Errorf("the control port bound %s, not loopback", host)
	}

	_, secretsAddr, _ := startObservedSecrets(t)
	for addr, want := range map[string]int{control.Addr(): http.StatusOK, secretsAddr: http.StatusNotFound} {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(resp)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s/metrics = %d, want %d", addr, resp.StatusCode, want)
		}
		if want != http.StatusOK && strings.Contains(body, "cloudburrow_") {
			t.Errorf("a service port served metrics:\n%s", body)
		}
	}
}

// Storage is measured and not in the unmeasured list (#513).
func TestMetricsStorageIsMeasured(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub}
	if got := unmeasuredServices(cfg); contains(got, "storage") {
		t.Errorf("unmeasured = %v; storage is measured", got)
	}
	reg := metrics.New(unmeasuredServices(cfg)...)
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Observe: storageEvents(nil, reg)})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	defer h.Close()
	resp, _ := http.Post(h.URL+"/storage/v1/b?project=p", "application/json", strings.NewReader(`{"name":"measured"}`))
	resp.Body.Close()
	var b strings.Builder
	_ = reg.Write(&b)
	fams := parseMetrics(t, b.String())
	if v, ok := counter(fams, "cloudburrow_requests_total", map[string]string{"service": "storage", "method": "storage.buckets.insert", "code": "200"}); !ok || v != 1 {
		t.Errorf("no storage.buckets.insert series in /metrics:\n%s", b.String())
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
