package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/relabel"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// An exposition's families become Managed Service for Prometheus' types:
// a counter's start is when it was first read, and moves when it resets;
// a histogram's buckets are de-accumulated; a series' namespace places it;
// metric relabelling drops what it drops.
func TestPromReadings(t *testing.T) {
	text := `# TYPE req_total counter
req_total{code="200",namespace="shop"} 5
# TYPE temp gauge
temp 21.5
# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 2
lat_seconds_bucket{le="1"} 5
lat_seconds_bucket{le="+Inf"} 6
lat_seconds_sum 3.5
lat_seconds_count 6
# TYPE drop_me gauge
drop_me 1
`
	starts := newPromStarts()
	t0 := time.Unix(1000, 0)
	re, _ := relabel.NewRegexp("drop_me")
	cfg := relabel.DefaultRelabelConfig
	cfg.SourceLabels, cfg.Regex, cfg.Action = model.LabelNames{"__name__"}, re, relabel.Drop
	sc := promScrape{resource: map[string]string{"project_id": "p", "cluster": "c", "namespace": "default", "job": "j", "instance": "pod:metrics"},
		relabel: []*relabel.Config{&cfg}, extra: map[string]string{"pod": "pod"}}
	pts, err := promReadings(strings.NewReader(text), sc, starts, t0)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]monitoring.PromPoint{}
	for _, p := range pts {
		byType[p.Type] = p
	}
	if _, ok := byType["prometheus.googleapis.com/drop_me/gauge"]; ok {
		t.Error("the relabelling's drop was kept")
	}
	c := byType["prometheus.googleapis.com/req_total/counter"]
	if c.Value != 5 || c.Resource["namespace"] != "shop" || c.Labels["code"] != "200" || c.Labels["pod"] != "pod" || !c.Start.Before(t0) {
		t.Errorf("counter %+v", c)
	}
	if g := byType["prometheus.googleapis.com/temp/gauge"]; g.Value != 21.5 || g.Resource["namespace"] != "default" {
		t.Errorf("gauge %+v", g)
	}
	h := byType["prometheus.googleapis.com/lat_seconds/histogram"].Histogram
	if h == nil || h.Count != 6 || h.Sum != 3.5 || len(h.Bounds) != 2 || h.Counts[0] != 2 || h.Counts[1] != 3 || h.Counts[2] != 1 {
		t.Errorf("histogram %+v", h)
	}
	// Read again later, reset: the start moves.
	t1 := t0.Add(time.Minute)
	pts, _ = promReadings(strings.NewReader("# TYPE req_total counter\nreq_total{code=\"200\",namespace=\"shop\"} 1\n"), sc, starts, t1)
	if len(pts) != 1 || !pts[0].Start.After(t0) {
		t.Errorf("after a reset: %+v", pts)
	}
}

// Kube state metrics skip the system namespaces and read a Deployment's
// replicas.
func TestKubeStatePoints(t *testing.T) {
	raw := `{"items":[
 {"kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":3},"status":{"availableReplicas":2,"updatedReplicas":3}},
 {"kind":"Deployment","metadata":{"name":"coredns","namespace":"kube-system"},"spec":{"replicas":2},"status":{}},
 {"kind":"Pod","metadata":{"name":"web-1","namespace":"shop","uid":"u1"},"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":true,"state":{}}]}}]}`
	pts := ksmPoints(gke.TelemetryTarget{Project: "p", Location: "l", Cluster: "c"}, []byte(raw), time.Unix(0, 0))
	got := map[string]float64{}
	for _, p := range pts {
		if p.Resource["namespace"] == "kube-system" {
			t.Errorf("a system namespace's %s", p.Type)
		}
		key := strings.TrimPrefix(p.Type, "prometheus.googleapis.com/")
		if ph := p.Labels["phase"]; ph != "" {
			key += "," + ph
		}
		got[key] = p.Value
	}
	if got["kube_deployment_spec_replicas/gauge"] != 3 || got["kube_deployment_status_replicas_available/gauge"] != 2 ||
		got["kube_pod_status_phase/gauge,Running"] != 1 || got["kube_pod_status_phase/gauge,Pending"] != 0 || got["kube_pod_container_status_ready/gauge"] != 1 {
		t.Errorf("kube state %v", got)
	}
}

// The managed collection manifest is four documents, each standing alone
// (a YAML alias does not cross documents): both CRDs carry their schema.
func TestGMPManifest(t *testing.T) {
	dec := yaml.NewDecoder(strings.NewReader(gmpManifest))
	var got []string
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema map[string]any `yaml:"openAPIV3Schema"`
					} `yaml:"schema"`
				} `yaml:"versions"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "CustomResourceDefinition" && (len(doc.Spec.Versions) != 1 || doc.Spec.Versions[0].Schema.OpenAPIV3Schema["properties"] == nil) {
			t.Errorf("%s has no schema", doc.Metadata.Name)
		}
		got = append(got, doc.Kind+"/"+doc.Metadata.Name)
	}
	want := "Namespace/gmp-system Namespace/gmp-public CustomResourceDefinition/podmonitorings.monitoring.googleapis.com CustomResourceDefinition/clusterpodmonitorings.monitoring.googleapis.com"
	if strings.Join(got, " ") != want {
		t.Errorf("manifest = %v", got)
	}
	if strings.Contains(gmpManifest, "&") || strings.Contains(gmpManifest, ": *") {
		t.Error("the manifest uses a YAML anchor or alias")
	}
}

// An endpoint's named port is scraped on its number, which is all the
// API server's pod proxy takes.
func TestGMPPodPort(t *testing.T) {
	var p gmpPod
	if err := json.Unmarshal([]byte(`{"spec":{"containers":[{"name":"coredns","ports":[{"name":"dns","containerPort":53},{"name":"metrics","containerPort":9153}]}]}}`), &p); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ port, ctr, number string }{{"metrics", "coredns", "9153"}, {"9153", "coredns", "9153"}, {"8080", "", "8080"}} {
		if ctr, number, ok := p.container(c.port); !ok || ctr != c.ctr || number != c.number {
			t.Errorf("container(%q) = %q %q %v", c.port, ctr, number, ok)
		}
	}
	if _, _, ok := p.container("nope"); ok {
		t.Error("an unknown port name was found")
	}
}
