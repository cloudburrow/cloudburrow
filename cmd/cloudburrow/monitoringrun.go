package main

// Cloud Run's built-in metrics (#2049) are read from the cluster:
//
//   - requests and their latencies from net-kourier's gateway, the Envoy
//     every request to a service's URL passes through on its way to the
//     revision (or to the activator, which holds it while the revision
//     scales from zero). Its stats listener (port 9000, /stats/prometheus,
//     in kourier.yaml's bootstrap) is read through the API server's pod
//     proxy, so nothing on this machine listens for it: each revision is
//     an Envoy cluster named after the revision's Kubernetes service, whose
//     envoy_cluster_upstream_rq counts by response code and
//     envoy_cluster_upstream_rq_time is the time in milliseconds from the
//     request reaching the revision to its answer;
//   - instances from the revision's pods: a running pod whose containers
//     are ready is an active instance, one not yet (or no longer) ready an
//     idle one.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

const (
	kourierNamespace      = "kourier-system"
	kourierGatewayPrefix  = "3scale-kourier-gateway"
	kourierStatsPath      = ":9000/proxy/stats/prometheus"
	runRevisionLabel      = "serving.knative.dev/revision"
	runServiceLabel       = "serving.knative.dev/service"
	runConfigurationLabel = "serving.knative.dev/configuration"
	runNameAnnotation     = "cloudburrow.dev/cloud-run-name"
)

// monitoringRun is monitoring.RunSource over the instance's cluster.
type monitoringRun struct {
	kubeconfig, namespace, project, region string
}

// envoyRevision is one Envoy cluster's counters.
type envoyRevision struct {
	codes   map[int]uint64
	bounds  []float64
	buckets []uint64
	count   uint64
	sum     float64
}

// promLabels parses a Prometheus sample's {k="v",...} labels.
func promLabels(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return out
		}
		key := strings.TrimLeft(s[:eq], ", ")
		var val strings.Builder
		i := eq + 2
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			val.WriteByte(s[i])
		}
		out[key] = val.String()
		s = s[min(i+1, len(s)):]
	}
	return out
}

// envoyRevisionName is the revision an Envoy cluster name routes to:
// "ns/rev", or the service host rev.ns.svc..., in namespace.
func envoyRevisionName(cluster, namespace string) string {
	if rest, ok := strings.CutPrefix(cluster, namespace+"/"); ok {
		cluster = rest
	} else if strings.Contains(cluster, "/") {
		return ""
	}
	if i := strings.IndexAny(cluster, "/.:"); i >= 0 {
		cluster = cluster[:i]
	}
	return strings.TrimSuffix(cluster, "-private")
}

// parseEnvoyStats adds an Envoy's /stats/prometheus to by, by revision.
func parseEnvoyStats(text, namespace string, by map[string]*envoyRevision) {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "envoy_cluster_upstream_rq") {
			continue
		}
		open, end := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}')
		if open < 0 || end < open {
			continue
		}
		name, labels := line[:open], promLabels(line[open+1:end])
		v, err := strconv.ParseFloat(strings.TrimSpace(line[end+1:]), 64)
		if err != nil {
			continue
		}
		rev := envoyRevisionName(labels["envoy_cluster_name"], namespace)
		if rev == "" {
			continue
		}
		r := by[rev]
		if r == nil {
			r = &envoyRevision{codes: map[int]uint64{}}
			by[rev] = r
		}
		switch name {
		case "envoy_cluster_upstream_rq":
			if code, err := strconv.Atoi(labels["envoy_response_code"]); err == nil {
				r.codes[code] += uint64(v)
			}
		case "envoy_cluster_upstream_rq_time_bucket":
			le := labels["le"]
			if le == "+Inf" {
				continue
			}
			bound, err := strconv.ParseFloat(le, 64)
			if err != nil {
				continue
			}
			i := sort.SearchFloat64s(r.bounds, bound)
			if i == len(r.bounds) || r.bounds[i] != bound {
				r.bounds = append(r.bounds[:i], append([]float64{bound}, r.bounds[i:]...)...)
				r.buckets = append(r.buckets[:i], append([]uint64{0}, r.buckets[i:]...)...)
			}
			r.buckets[i] += uint64(v)
		case "envoy_cluster_upstream_rq_time_sum":
			r.sum += v
		case "envoy_cluster_upstream_rq_time_count":
			r.count += uint64(v)
		}
	}
}

type kubeItems struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func (m monitoringRun) list(ctx context.Context, namespace, kind string) (kubeItems, error) {
	var out kubeItems
	raw, err := kubectlJSON(ctx, m.kubeconfig, namespace, kind)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// RunRevisions implements monitoring.RunSource.
func (m monitoringRun) RunRevisions() ([]monitoring.RunRevision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svcs, err := m.list(ctx, m.namespace, "ksvc")
	if err != nil {
		return nil, err
	}
	fullName := map[string]string{}
	for _, s := range svcs.Items {
		n := s.Metadata.Annotations[runNameAnnotation]
		if n == "" {
			n = fmt.Sprintf("projects/%s/locations/%s/services/%s", m.project, m.region, s.Metadata.Name)
		}
		fullName[s.Metadata.Name] = n
	}
	revs, err := m.list(ctx, m.namespace, "revisions")
	if err != nil {
		return nil, err
	}
	stats := map[string]*envoyRevision{}
	if gw, err := m.list(ctx, kourierNamespace, "pods"); err == nil {
		for _, p := range gw.Items {
			if !strings.HasPrefix(p.Metadata.Name, kourierGatewayPrefix) || p.Status.Phase != "Running" {
				continue
			}
			raw, err := kubectlRaw(ctx, m.kubeconfig, "/api/v1/namespaces/"+kourierNamespace+"/pods/"+p.Metadata.Name+kourierStatsPath)
			if err != nil {
				continue
			}
			parseEnvoyStats(string(raw), m.namespace, stats)
		}
	}
	active, idle := map[string]int64{}, map[string]int64{}
	if pods, err := m.list(ctx, m.namespace, "pods"); err == nil {
		for _, p := range pods.Items {
			rev := p.Metadata.Labels[runRevisionLabel]
			if rev == "" || (p.Status.Phase != "Running" && p.Status.Phase != "Pending") {
				continue
			}
			ready := p.Status.Phase == "Running" && len(p.Status.ContainerStatuses) > 0
			for _, c := range p.Status.ContainerStatuses {
				ready = ready && c.Ready
			}
			if ready {
				active[rev]++
			} else {
				idle[rev]++
			}
		}
	}
	var out []monitoring.RunRevision
	for _, r := range revs.Items {
		kname := r.Metadata.Labels[runServiceLabel]
		name, ok := fullName[kname]
		if !ok {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) != 6 {
			continue
		}
		rv := monitoring.RunRevision{Project: parts[1], Location: parts[3], Service: parts[5], Configuration: parts[5],
			Revision: runadapter.RevisionID(name, kname, r.Metadata.Name), Requests: map[int]uint64{},
			Active: active[r.Metadata.Name], Idle: idle[r.Metadata.Name]}
		if st := stats[r.Metadata.Name]; st != nil {
			rv.Requests, rv.LatencyBounds, rv.LatencyBuckets = st.codes, st.bounds, st.buckets
			rv.LatencyCount, rv.LatencySum = st.count, st.sum
		}
		out = append(out, rv)
	}
	return out, nil
}

// sampleRunMetrics adds Cloud Run's metrics to api and samples them every
// requestSampleInterval until stop. The first read, which sets where the
// counters start, is made here rather than in Start, so a cluster still
// starting never holds up Cloud Monitoring.
func sampleRunMetrics(api *monitoring.Server, src monitoring.RunSource, stop chan struct{}) {
	b := monitoring.NewRunBuiltin(src, time.Now)
	api.AddBuiltin(b)
	t := time.NewTicker(requestSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.Sample()
		case <-stop:
			return
		}
	}
}
