package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	ltype "google.golang.org/genproto/googleapis/logging/type"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

const gkePodsJSON = `{"items":[
 {"metadata":{"name":"web-1","namespace":"default","uid":"u1","labels":{"app":"web"}},
  "spec":{"nodeName":"k-1234-1","containers":[{"name":"app"}]}},
 {"metadata":{"name":"coredns-x","namespace":"kube-system","uid":"u2","labels":{"k8s-app":"kube-dns"}},
  "spec":{"nodeName":"k-1234-1","containers":[{"name":"coredns"}]}},
 {"metadata":{"name":"kube-apiserver-k-control-plane","namespace":"kube-system","uid":"u3","labels":{"tier":"control-plane","component":"kube-apiserver"}},
  "spec":{"nodeName":"k-control-plane","containers":[{"name":"kube-apiserver"}]}},
 {"metadata":{"name":"etcd-k-control-plane","namespace":"kube-system","uid":"u4","labels":{"tier":"control-plane","component":"etcd"}},
  "spec":{"nodeName":"k-control-plane","containers":[{"name":"etcd"}]}},
 {"metadata":{"name":"pending","namespace":"default","uid":"u5"},"spec":{"containers":[{"name":"app"}]}}
]}`

// TestGKELogTargets (#2100): workload and system containers are shipped
// as their components say, on k8s_container with GKE's labels; the API
// server only with APISERVER, and etcd never.
func TestGKELogTargets(t *testing.T) {
	tt := gke.TelemetryTarget{Project: "p", Location: "us-central1-a", Cluster: "c1", Kind: "k",
		SystemLogs: true, WorkloadLogs: true, ControlPlaneLogs: map[string]bool{}}
	srcs, err := gkeLogTargets(tt, []byte(gkePodsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %+v, want web-1's and coredns's", srcs)
	}
	w := srcs[0]
	if w.category != "workload" || w.node != "k-1234-1" || w.dir != "/var/log/pods/default_web-1_u1/app" {
		t.Errorf("workload source = %+v", w)
	}
	want := map[string]string{"project_id": "p", "location": "us-central1-a", "cluster_name": "c1",
		"namespace_name": "default", "pod_name": "web-1", "container_name": "app"}
	if w.resource.Type != "k8s_container" || len(w.resource.Labels) != len(want) {
		t.Errorf("resource = %v", w.resource)
	}
	for k, v := range want {
		if w.resource.Labels[k] != v {
			t.Errorf("resource label %s = %q, want %q", k, w.resource.Labels[k], v)
		}
	}
	if w.labels["k8s-pod/app"] != "web" || w.labels["compute.googleapis.com/resource_name"] != "k-1234-1" {
		t.Errorf("entry labels = %v", w.labels)
	}
	if srcs[1].category != "system" {
		t.Errorf("coredns category = %q", srcs[1].category)
	}

	tt.SystemLogs, tt.ControlPlaneLogs["apiserver"] = false, true
	srcs, _ = gkeLogTargets(tt, []byte(gkePodsJSON))
	if len(srcs) != 2 || srcs[1].resource.Type != "k8s_control_plane_component" || srcs[1].logName != "container.googleapis.com/apiserver" {
		t.Fatalf("with APISERVER and no system logs: %+v", srcs)
	}
	e := gkeLogEntry(srcs[1], "stderr", time.Unix(1, 0), "x")
	if e.LogName != "projects/p/logs/container.googleapis.com%2Fapiserver" {
		t.Errorf("control plane log = %q", e.LogName)
	}
	tt.WorkloadLogs, tt.ControlPlaneLogs = false, map[string]bool{}
	if srcs, _ = gkeLogTargets(tt, []byte(gkePodsJSON)); len(srcs) != 0 {
		t.Errorf("nothing enabled: %+v", srcs)
	}
}

func TestGKELogEntry(t *testing.T) {
	tt := gke.TelemetryTarget{Project: "p", Location: "l", Cluster: "c", WorkloadLogs: true}
	srcs, _ := gkeLogTargets(tt, []byte(gkePodsJSON))
	s := srcs[0]
	e := gkeLogEntry(s, "stdout", time.Unix(5, 0), "hello")
	if e.LogName != "projects/p/logs/stdout" || e.GetTextPayload() != "hello" || e.Severity != ltype.LogSeverity_INFO {
		t.Errorf("stdout entry = %v", e)
	}
	if e := gkeLogEntry(s, "stderr", time.Unix(5, 0), "bad"); e.LogName != "projects/p/logs/stderr" || e.Severity != ltype.LogSeverity_ERROR {
		t.Errorf("stderr entry = %v", e)
	}
	e = gkeLogEntry(s, "stderr", time.Unix(5, 0), `{"severity":"WARNING","message":"m"}`)
	if e.Severity != ltype.LogSeverity_WARNING || e.GetJsonPayload().GetFields()["message"].GetStringValue() != "m" {
		t.Errorf("structured entry = %v", e)
	}
}

// gkeFeederFor is a feeder over one cluster whose pod web-1 wrote line,
// with a clock starting at at that each listing moves on five seconds.
func gkeFeederFor(at, changed time.Time, line string, written chan string) *gkeLogFeeder {
	var mu sync.Mutex
	clock := at
	file := podLogDir("default", "web-1", "u1") + "/app/0.log"
	return &gkeLogFeeder{
		targets: func() []gke.TelemetryTarget {
			return []gke.TelemetryTarget{{Project: "p", Location: "us-central1-a", Cluster: "c1", Kind: "k",
				SystemLogs: true, WorkloadLogs: true, ControlPlaneLogs: map[string]bool{}, LoggingChanged: changed}}
		},
		kubectl: func(context.Context, string, ...string) (string, error) {
			mu.Lock()
			clock = clock.Add(5 * time.Second)
			mu.Unlock()
			return gkePodsJSON, nil
		},
		docker: func(_ context.Context, args ...string) (io.ReadCloser, func() error, error) {
			if slices.Contains(args, "find") {
				return io.NopCloser(strings.NewReader(file + "\n")), func() error { return nil }, nil
			}
			return io.NopCloser(strings.NewReader(line)), func() error { return nil }, nil
		},
		write: func(_ context.Context, req *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
			for _, e := range req.GetEntries() {
				written <- e.GetTextPayload()
			}
			return &loggingpb.WriteLogEntriesResponse{}, nil
		},
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		},
		since: map[string]time.Time{}, follows: map[string]*runLogFollow{},
	}
}

func wantShipped(t *testing.T, what string, written chan string, want bool) {
	t.Helper()
	select {
	case got := <-written:
		if !want || got != "hello" {
			t.Errorf("%s: shipped %q", what, got)
		}
	case <-time.After(2 * time.Second):
		if want {
			t.Errorf("%s: the line was not shipped", what)
		}
	}
}

// TestGKELogFeederShipsFromWhenLoggingWasOn (#2100): a category turned on
// is shipped from when the cluster's logging change turned it on, so a
// line a pod writes half a second after the change, before the shipper's
// next sweep, is shipped (TestGKELoggingAndMonitoring's lg3 line was
// dropped by 115ms); without a change made by this server, from the
// start of the sweep that first sees it, not from when its slow listing
// ends; a line older than both is not.
func TestGKELogFeederShipsFromWhenLoggingWasOn(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	line := t0.Add(time.Second).Format(time.RFC3339Nano) + " stdout F hello\n"
	written := make(chan string, 4)
	gkeFeederFor(t0.Add(2*time.Second), t0, line, written).sweep(context.Background())
	wantShipped(t, "a line after the change, before the sweep", written, true)
	written = make(chan string, 4)
	gkeFeederFor(t0, time.Time{}, line, written).sweep(context.Background())
	wantShipped(t, "a line during the sweep's listing", written, true)
	written = make(chan string, 4)
	gkeFeederFor(t0.Add(2*time.Second), time.Time{}, line, written).sweep(context.Background())
	wantShipped(t, "a line before the sweep, no change known", written, false)
}
