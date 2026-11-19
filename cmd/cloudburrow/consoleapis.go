package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

// The APIs & Services pages (#1866): Enabled APIs & services (the
// dashboard and its table), the API Library and each API's page. Their
// calls are the serviceusage provider's Studio, so they read and change the
// same serviceusage.Server gcloud and Terraform call, and count calls from
// the instance's own request counters (/metrics), which are kept per
// service, not per project.

// apiLibraryInfo is what the Library shows of an API beside its name and
// title: Google's Library category and a one-line summary.
type apiLibraryInfo struct {
	Category, Summary string
	// Metrics is the request counters' service name; empty when its calls
	// are not counted.
	Metrics string
}

var apiLibrary = map[string]apiLibraryInfo{
	"cloudresourcemanager.googleapis.com": {"Management tools", "Creates, reads and updates projects, folders and the organization.", "resourcemanager"},
	"serviceusage.googleapis.com":         {"Management tools", "Lists, enables and disables the APIs a project uses.", ""},
	"apikeys.googleapis.com":              {"Security", "Manages the API keys of a project and their restrictions.", ""},
	"storage.googleapis.com":              {"Storage", "Stores and retrieves objects in buckets.", "storage"},
	"pubsub.googleapis.com":               {"Big data", "Sends and receives messages between independent applications.", "pubsub"},
	"cloudtasks.googleapis.com":           {"Developer tools", "Manages the execution of large numbers of distributed requests.", "tasks"},
	"run.googleapis.com":                  {"Compute", "Deploys and manages containerized services and jobs.", "run"},
	"secretmanager.googleapis.com":        {"Security", "Stores API keys, passwords, certificates and other sensitive data.", "secretmanager"},
	"cloudkms.googleapis.com":             {"Security", "Manages cryptographic keys and performs operations with them.", "kms"},
	"cloudscheduler.googleapis.com":       {"Developer tools", "Runs jobs on a schedule: HTTP targets, Pub/Sub topics and App Engine.", "scheduler"},
	"logging.googleapis.com":              {"Management tools", "Writes and queries log entries, and routes them with sinks.", "logging"},
	"firestore.googleapis.com":            {"Databases", "Accesses a Firestore database, documents and indexes.", "firestore"},
	"datastore.googleapis.com":            {"Databases", "Accesses a schemaless NoSQL database in Datastore mode.", "datastore"},
	"bigtable.googleapis.com":             {"Databases", "Reads and writes rows in Bigtable tables.", "bigtable"},
	"bigtableadmin.googleapis.com":        {"Databases", "Administers Bigtable instances, clusters, tables and backups.", "bigtable"},
	"spanner.googleapis.com":              {"Databases", "Manages Spanner instances and databases, and runs transactions on them.", "spanner"},
	"bigquery.googleapis.com":             {"Big data", "Creates datasets and tables, loads data and runs queries.", "bigquery"},
	"bigquerystorage.googleapis.com":      {"Big data", "Reads and writes BigQuery table data in streams.", "bigquery"},
	"redis.googleapis.com":                {"Databases", "Creates and manages Redis instances.", "memorystore"},
	"sqladmin.googleapis.com":             {"Databases", "Administers Cloud SQL instances, databases, users and backups.", ""},
	"backupdr.googleapis.com":             {"Storage", "Manages backup vaults, backup plans and the backups in them.", ""},
	"privateca.googleapis.com":            {"Security", "Creates and manages private certificate authorities and the certificates they issue.", ""},
	"containeranalysis.googleapis.com":    {"Developer tools", "Stores and reads the notes and occurrences of software artifacts, vulnerabilities among them.", ""},
	"containerscanning.googleapis.com":    {"Security", "Scans the container images pushed to Artifact Registry for vulnerabilities.", ""},
}

// apiTotals is an API's calls since the instance started, from the request
// counters.
type apiTotals struct {
	Measured bool     `json:"measured"`
	Requests uint64   `json:"requests"`
	Errors   uint64   `json:"errors"`
	P50MS    *float64 `json:"p50Ms,omitempty"`
	P95MS    *float64 `json:"p95Ms,omitempty"`
}

// apiMethod is one method's line of an API's Metrics tab.
type apiMethod struct {
	Method string `json:"method"`
	apiTotals
}

func failedCode(code string) bool {
	if code == "OK" {
		return false
	}
	if len(code) == 3 && code[0] >= '0' && code[0] <= '9' {
		return code[0] >= '4'
	}
	return true
}

// quantileMS interpolates the q-th call's latency, in milliseconds, from
// cumulative bucket counts, as the console's request charts do.
func quantileMS(q float64, cumulative []uint64, total uint64) *float64 {
	if total == 0 {
		return nil
	}
	rank := q * float64(total)
	lower, prev := 0.0, uint64(0)
	v := metrics.Buckets[len(metrics.Buckets)-1]
	for i, c := range cumulative {
		upper := metrics.Buckets[i]
		if float64(c) >= rank {
			in := c - prev
			v = upper
			if in > 0 {
				v = lower + (upper-lower)*(rank-float64(prev))/float64(in)
			}
			break
		}
		lower, prev = upper, c
	}
	ms := v * 1000
	return &ms
}

type totalsAcc struct {
	n, errs uint64
	buckets []uint64
}

func (a *totalsAcc) add(p metrics.Point) {
	if a.buckets == nil {
		a.buckets = make([]uint64, len(metrics.Buckets))
	}
	a.n += p.Count
	if failedCode(p.Code) {
		a.errs += p.Count
	}
	for i, b := range p.Buckets {
		if i < len(a.buckets) {
			a.buckets[i] += b
		}
	}
}

func (a *totalsAcc) totals() apiTotals {
	return apiTotals{Measured: true, Requests: a.n, Errors: a.errs,
		P50MS: quantileMS(0.5, a.buckets, a.n), P95MS: quantileMS(0.95, a.buckets, a.n)}
}

// apiCounters sums the request counters per metrics service, and per
// method of each.
func (p serviceUsageProvider) apiCounters() (map[string]*totalsAcc, map[string]map[string]*totalsAcc) {
	byService := map[string]*totalsAcc{}
	byMethod := map[string]map[string]*totalsAcc{}
	if p.metrics == nil {
		return byService, byMethod
	}
	for _, pt := range p.metrics.Snapshot() {
		if byService[pt.Service] == nil {
			byService[pt.Service] = &totalsAcc{}
			byMethod[pt.Service] = map[string]*totalsAcc{}
		}
		byService[pt.Service].add(pt)
		m := byMethod[pt.Service][pt.Method]
		if m == nil {
			m = &totalsAcc{}
			byMethod[pt.Service][pt.Method] = m
		}
		m.add(pt)
	}
	return byService, byMethod
}

func (p serviceUsageProvider) totalsFor(name string, byService map[string]*totalsAcc) apiTotals {
	svc := apiLibrary[name].Metrics
	if svc == "" || !p.metrics.Measured(svc) {
		return apiTotals{}
	}
	if a := byService[svc]; a != nil {
		return a.totals()
	}
	return apiTotals{Measured: true}
}

func apiTitle(name string) string {
	for _, e := range serviceUsageAPIs {
		if e.api.Name == name {
			return e.api.Title
		}
	}
	return name
}

// apiEntry is one API as the pages read it.
type apiEntry struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Summary  string `json:"summary"`
	// State is ENABLED or DISABLED for a served API, empty for one this
	// instance does not serve.
	State string `json:"state,omitempty"`
	// Unavailable is why an API CloudBurrow can serve is not served here.
	Unavailable string `json:"unavailable,omitempty"`
	// MetricsService is the request charts' series this API's calls are in.
	MetricsService string `json:"metricsService,omitempty"`
	// SharedWith names the other APIs counted in the same series.
	SharedWith []string  `json:"sharedWith,omitempty"`
	Totals     apiTotals `json:"totals"`
}

func (p serviceUsageProvider) entries(project string) ([]apiEntry, map[string]map[string]*totalsAcc, error) {
	services, err := p.api.Services(project)
	if err != nil {
		return nil, nil, err
	}
	byService, byMethod := p.apiCounters()
	var out []apiEntry
	for _, s := range services {
		cfg, _ := s["config"].(map[string]any)
		name, _ := cfg["name"].(string)
		title, _ := cfg["title"].(string)
		state, _ := s["state"].(string)
		info := apiLibrary[name]
		e := apiEntry{Name: name, Title: title, Category: info.Category, Summary: info.Summary, State: state,
			Totals: p.totalsFor(name, byService)}
		if e.Totals.Measured {
			e.MetricsService = info.Metrics
		}
		out = append(out, e)
	}
	for name, why := range p.api.Unserved() {
		info := apiLibrary[name]
		out = append(out, apiEntry{Name: name, Title: apiTitle(name), Category: info.Category, Summary: info.Summary, Unavailable: why})
	}
	for i := range out {
		for _, o := range out {
			if o.Name != out[i].Name && out[i].MetricsService != "" && apiLibrary[o.Name].Metrics == out[i].MetricsService && o.Unavailable == "" {
				out[i].SharedWith = append(out[i].SharedWith, o.Name)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, byMethod, nil
}

// StudioCall implements console.Studio for the APIs & Services pages:
//
//   - apis: every API, served (with its state and counters) or not;
//   - api {service}: one API, with its methods' counters;
//   - enable / disable {service}: services.enable and services.disable.
func (p serviceUsageProvider) StudioCall(_ context.Context, project, call string, body json.RawMessage) (any, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var req struct {
		Service string `json:"service"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %v", err)
		}
	}
	if strings.HasPrefix(call, "key") {
		return p.keysCall(project, call, body)
	}
	switch call {
	case "apis":
		list, _, err := p.entries(project)
		if err != nil {
			return nil, err
		}
		return map[string]any{"project": project, "apis": list, "countersNote": apiCountersNote}, nil
	case "api":
		list, byMethod, err := p.entries(project)
		if err != nil {
			return nil, err
		}
		for _, e := range list {
			if e.Name != req.Service {
				continue
			}
			methods := []apiMethod{}
			if e.MetricsService != "" {
				for m, a := range byMethod[e.MetricsService] {
					methods = append(methods, apiMethod{Method: m, apiTotals: a.totals()})
				}
				sort.Slice(methods, func(i, j int) bool { return methods[i].Method < methods[j].Method })
			}
			return map[string]any{"project": project, "api": e, "methods": methods, "countersNote": apiCountersNote}, nil
		}
		return nil, fmt.Errorf("CloudBurrow does not serve %s", strings.TrimSpace(req.Service))
	case "enable", "disable":
		if req.Service == "" {
			return nil, errors.New("a service is required")
		}
		if err := p.api.SetState(project, req.Service, call == "enable"); err != nil {
			return nil, err
		}
		return map[string]any{"service": req.Service, "state": map[bool]string{true: "ENABLED", false: "DISABLED"}[call == "enable"]}, nil
	}
	return nil, fmt.Errorf("unknown call %q", call)
}

const apiCountersNote = "Calls this instance has served since it started, from its own request counters (/metrics). " +
	"They are counted per API for every project together, not per project."
