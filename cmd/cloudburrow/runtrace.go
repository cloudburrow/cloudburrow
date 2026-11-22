package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Cloud Run's automatic request traces (#2118): "Incoming requests to Cloud
// Run services automatically generate traces", and "The standard W3C trace
// context propagation header traceparent is automatically populated for
// Cloud Run requests" (docs.cloud.google.com/run/docs/trace, read
// 2026-11-21). Locally a request reaches a revision through Knative's
// queue-proxy, which makes a span of each request and propagates W3C
// trace context to the container when Knative's tracing is on. runTracing
// turns it on, exporting over OTLP/HTTP to this instance's Cloud Trace
// through cloudburrow-host, so each request is a span in Cloud Trace and
// the container is handed a traceparent naming it.
//
// Choices Google does not document, or documents otherwise (UNVERIFIED
// (docs re-read 2026-11-21: https://docs.cloud.google.com/run/docs/trace)):
// Google samples "at a maximum rate of 0.1 requests per second for each
// instance"; Knative samples by ratio, so every request is sampled here.
// The spans are queue-proxy's, named and attributed as Knative names them,
// The spans of a Cloud Run service's requests go to the service's project
// (spanRouter); Knative's own spans (its webhooks, the activator's
// internals) are not a Cloud Run service's and are dropped, as Cloud Run
// shows none of its infrastructure's.
type runTracing struct {
	cfg   config.Config
	host  *clusterHost
	out   io.Writer
	trace *traceService

	mu sync.Mutex
	// services maps a Knative Service name to its Cloud Run project.
	services map[string]string
	listed   time.Time
}

func (r *runTracing) Name() string { return "run-tracing" }

// runTracingPatch is the config-observability patch exporting to endpoint;
// "" turns tracing off.
func runTracingPatch(endpoint string) (string, error) {
	data := map[string]string{"tracing-protocol": "none", "tracing-endpoint": "", "tracing-sampling-rate": "0"}
	if endpoint != "" {
		data = map[string]string{"tracing-protocol": "http/protobuf", "tracing-endpoint": "http://" + endpoint + "/v1/traces", "tracing-sampling-rate": "1"}
	}
	b, err := json.Marshal(map[string]any{"data": data})
	return string(b), err
}

func (r *runTracing) Start(ctx context.Context) error {
	// Without Cloud Trace, tracing is turned off, so a cluster that had it
	// does not keep exporting to a port nobody answers.
	addr := r.host.InCluster("trace")
	patch, err := runTracingPatch(addr)
	if err != nil {
		return err
	}
	if r.trace != nil && r.trace.api != nil && addr != "" {
		r.trace.api.SetRouter(r.spanRouter)
	}
	k := kubeRunner(r.cfg.KubeconfigPath(), "knative-serving")
	if err := k.Patch(ctx, "configmap", "config-observability", patch); err != nil {
		// A cluster whose Knative refuses the patch still serves; its
		// requests are not traced.
		fmt.Fprintf(r.out, "  Cloud Run request traces: %v\n", strings.TrimSpace(err.Error()))
		return nil
	}
	return nil
}

func (r *runTracing) Stop(context.Context) error { return nil }

// spanRouter places a span Knative exported: one naming a Cloud Run
// service, in its kn.service.name attribute or as the service.name of a
// workload pod, goes to that service's project; another span from the
// cluster is dropped. A span from outside the cluster (no
// k8s.namespace.name) keeps its project.
func (r *runTracing) spanRouter(attrs map[string]string) (string, bool) {
	ns := attrs["k8s.namespace.name"]
	if ns == "" {
		return "", true
	}
	svc := attrs["kn.service.name"]
	if svc == "" && ns == runadapter.WorkloadNamespace {
		svc = attrs["service.name"]
	}
	if svc == "" {
		return "", false
	}
	if p := r.serviceProject(svc); p != "" {
		return p, true
	}
	return "", false
}

// serviceProject is a Knative Service's Cloud Run project, listing the
// workload namespace's Services again on a miss, at most every 2 seconds.
func (r *runTracing) serviceProject(ksvc string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.services[ksvc]; ok {
		return p
	}
	if time.Since(r.listed) < 2*time.Second {
		return ""
	}
	r.listed = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := kubeRunner(r.cfg.KubeconfigPath(), "").Do(ctx, "", "-n", runadapter.WorkloadNamespace, "get", "services.serving.knative.dev", "-o", "json")
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return ""
	}
	services := map[string]string{}
	for _, it := range list.Items {
		name := it.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]
		if name == "" || it.Metadata.Labels[k8s.OwnedLabel] != "true" {
			continue
		}
		if n, err := resource.Parse(name); err == nil {
			services[it.Metadata.Name] = n.Project
		}
	}
	r.services = services
	return services[ksvc]
}
