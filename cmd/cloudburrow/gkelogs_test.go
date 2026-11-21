package main

import (
	"testing"
	"time"

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
