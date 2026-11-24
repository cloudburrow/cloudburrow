package compute

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Custom metrics in Cloud Monitoring (#2160). The custom metrics page
// (load-balancing/docs/https/applb-custom-metrics, read 2026-11-22) says
// that "when a metric is configured with dryRun set to true, the metric
// is reported to Monitoring but isn't actually used by the load
// balancer", and the Application Load Balancers' monitoring pages
// (https/https-reg-logging-monitoring, cdn/docs/cdn-logging-monitoring)
// name the metric it is reported as:
// network.googleapis.com/loadbalancer/backend/lb_custom_metric, "the
// current utilization by each backend group, based on your defined custom
// metrics". CustomMetricSamples are its points: for every custom metric
// of a backend (CUSTOM_METRICS balancing mode) or of a backend service
// (WEIGHTED_ROUND_ROBIN), dry run or not, the mean of the values its
// backend group's healthy endpoints last reported in their ORCA load
// reports.

// CustomMetricSample is one backend group's current value of one custom
// metric.
type CustomMetricSample struct {
	Project, BackendService, BackendServiceID string
	// Region is the backend service's region, "global" for a global one.
	Region string
	// Group is the backend group's relative name; GroupType is
	// INSTANCE_GROUP or NETWORK_ENDPOINT_GROUP; Scope is its zone or region
	// and ScopeType ZONE or REGION.
	Group, GroupName, GroupType, Scope, ScopeType string
	Metric                                        string
	DryRun                                        bool
	// Value is the mean reported value; Reporting is how many healthy
	// endpoints reported it (the mean is over them).
	Value     float64
	Reporting int
	At        time.Time
}

// CustomMetricSamples are the current values of every custom metric of
// the backend services, for each backend group with at least one healthy
// endpoint that reported the metric.
func (s *Server) CustomMetricSamples() []CustomMetricSample {
	now := s.opts.Now()
	s.mu.Lock()
	var rels []string
	for k := range s.res {
		if p := strings.Split(k, "/"); len(p) >= 5 && p[len(p)-2] == "backendServices" {
			rels = append(rels, k)
		}
	}
	sort.Strings(rels)
	type job struct {
		rel string
		set lbBackendSet
	}
	var jobs []job
	for _, rel := range rels {
		if set, ok := s.backendSetLocked(rel); ok {
			jobs = append(jobs, job{rel, set})
		}
	}
	s.mu.Unlock()
	var out []CustomMetricSample
	for _, j := range jobs {
		p := strings.Split(j.rel, "/")
		region := "global"
		if len(p) > 3 && p[2] == "regions" {
			region = p[3]
		}
		svcMetrics := listOf(j.set.bs["customMetrics"])
		for _, g := range j.set.groups {
			metrics := append([]any(nil), listOf(g.backend["customMetrics"])...)
			metrics = append(metrics, svcMetrics...)
			if len(metrics) == 0 {
				continue
			}
			grp := Rel(str(g.backend, "group"), "")
			gp := strings.Split(grp, "/")
			gt, scope, scopeType := "INSTANCE_GROUP", "", ""
			if strings.Contains(grp, "/networkEndpointGroups/") {
				gt = "NETWORK_ENDPOINT_GROUP"
			}
			if len(gp) > 3 {
				scope = gp[3]
				scopeType = "ZONE"
				if gp[2] == "regions" {
					scopeType = "REGION"
				}
			}
			seen := map[string]bool{}
			for _, e := range metrics {
				m := objOf(e)
				name := str(m, "name")
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				sum, n := 0.0, 0
				for _, ep := range g.healthy {
					if r, ok := s.lb.bal.orca.get(ep.id, now); ok {
						if v, ok := r.value(name); ok {
							sum += v
							n++
						}
					}
				}
				if n == 0 {
					continue
				}
				out = append(out, CustomMetricSample{Project: p[1], BackendService: str(j.set.bs, "name"),
					BackendServiceID: fmt.Sprint(j.set.bs["id"]), Region: region, Group: grp, GroupName: gp[len(gp)-1],
					GroupType: gt, Scope: scope, ScopeType: scopeType, Metric: name, DryRun: m["dryRun"] == true,
					Value: sum / float64(n), Reporting: n, At: now})
			}
		}
	}
	return out
}
