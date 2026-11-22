package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

// The APIs & Services pages (#1866): Enabled APIs & services (the
// dashboard and its table), the API Library and each API's page. Their
// calls are the serviceusage provider's Studio, so they read and change the
// same serviceusage.Server gcloud and Terraform call, and count calls from
// the instance's own request counters (/metrics), which are kept per
// service and per project; the pages read their one-minute history
// (consoleapicharts.go).

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
	"cloudquotas.googleapis.com":          {"Management tools", "Lists the quotas of the APIs a project uses and adjusts them with quota preferences.", ""},
	"siteverification.googleapis.com":     {"Developer tools", "Verifies that you own a website or a domain, and lists what you have verified.", ""},
	"cloudbilling.googleapis.com":         {"Management tools", "Manages billing accounts, links projects to them and lists the services and SKUs of the pricing catalog.", ""},
	"billingbudgets.googleapis.com":       {"Management tools", "Creates and manages budgets and their alert thresholds for a billing account.", ""},
	"storage.googleapis.com":              {"Storage", "Stores and retrieves objects in buckets.", "storage"},
	"pubsub.googleapis.com":               {"Big data", "Sends and receives messages between independent applications.", "pubsub"},
	"cloudtasks.googleapis.com":           {"Developer tools", "Manages the execution of large numbers of distributed requests.", "tasks"},
	"run.googleapis.com":                  {"Compute", "Deploys and manages containerized services and jobs.", "run"},
	"secretmanager.googleapis.com":        {"Security", "Stores API keys, passwords, certificates and other sensitive data.", "secretmanager"},
	"parametermanager.googleapis.com":     {"Security", "Stores, versions and renders configuration parameters, in plain text, JSON or YAML, with references to Secret Manager secrets.", "parametermanager"},
	"dataform.googleapis.com":             {"Big data", "Develops, version controls and runs SQL workflows in BigQuery with SQLX.", "dataform"},
	"apphub.googleapis.com":               {"Management tools", "Discovers a boundary's services and workloads and groups them into applications.", "apphub"},
	"cloudtrace.googleapis.com":           {"Management tools", "Sends application latency data to Cloud Trace for viewing, and reads traces back.", "trace"},
	"telemetry.googleapis.com":            {"Management tools", "Ingests OpenTelemetry (OTLP) traces into Cloud Trace.", "trace"},
	"cloudkms.googleapis.com":             {"Security", "Manages cryptographic keys and performs operations with them.", "kms"},
	"kmsinventory.googleapis.com":         {"Security", "Reports which resources Cloud KMS keys protect.", "kms"},
	"cloudscheduler.googleapis.com":       {"Developer tools", "Runs jobs on a schedule: HTTP targets, Pub/Sub topics and App Engine.", "scheduler"},
	"logging.googleapis.com":              {"Management tools", "Writes and queries log entries, and routes them with sinks.", "logging"},
	"observability.googleapis.com":        {"Management tools", "Keeps a project's default log scope, the one the Logs Explorer opens on.", ""},
	"monitoring.googleapis.com":           {"Management tools", "Manages your Cloud Monitoring data and configurations: metrics, alerting policies, uptime checks, groups, services and dashboards.", ""},
	"firestore.googleapis.com":            {"Databases", "Accesses a Firestore database, documents and indexes.", "firestore"},
	"firebaserules.googleapis.com":        {"Security", "Creates and manages rules that determine when a Firebase Rules-enabled service should permit a request.", "firestore"},
	"datastore.googleapis.com":            {"Databases", "Accesses a schemaless NoSQL database in Datastore mode.", "datastore"},
	"bigtable.googleapis.com":             {"Databases", "Reads and writes rows in Bigtable tables.", "bigtable"},
	"bigtableadmin.googleapis.com":        {"Databases", "Administers Bigtable instances, clusters, tables and backups.", "bigtable"},
	"spanner.googleapis.com":              {"Databases", "Manages Spanner instances and databases, and runs transactions on them.", "spanner"},
	"bigquery.googleapis.com":             {"Big data", "Creates datasets and tables, loads data and runs queries.", "bigquery"},
	"bigquerystorage.googleapis.com":      {"Big data", "Reads and writes BigQuery table data in streams.", "bigquery"},
	"bigquerydatatransfer.googleapis.com": {"Big data", "Schedules queries and copies datasets on a recurring schedule.", "bigquery"},
	"analyticshub.googleapis.com":         {"Big data", "Shares BigQuery datasets through data exchanges, listings and subscriptions.", "bigquery"},
	"redis.googleapis.com":                {"Databases", "Creates and manages Redis instances.", "memorystore"},
	"sqladmin.googleapis.com":             {"Databases", "Administers Cloud SQL instances, databases, users and backups.", ""},
	"backupdr.googleapis.com":             {"Storage", "Manages backup vaults, backup plans and the backups in them.", ""},
	"privateca.googleapis.com":            {"Security", "Creates and manages private certificate authorities and the certificates they issue.", ""},
	"workflows.googleapis.com":            {"Developer tools", "Manages workflow definitions: orchestrates and automates Google Cloud and HTTP-based API services with serverless workflows.", "workflows"},
	"workflowexecutions.googleapis.com":   {"Developer tools", "Executes workflows, lists their executions, their step entries and callbacks, and cancels them.", "workflows"},
	"compute.googleapis.com":              {"Compute", "Creates and runs virtual machines on Google Cloud Platform; CloudBurrow serves its VPC networking: networks, subnets, firewall rules, routes and IP addresses.", ""},
	"vpcaccess.googleapis.com":            {"Networking", "Manages Serverless VPC Access connectors, which let serverless services such as Cloud Run reach a VPC network; CloudBurrow records them, and carries no traffic through them.", ""},
	"container.googleapis.com":            {"Compute", "Builds and manages container-based applications, powered by the open source Kubernetes technology.", ""},
	"containeranalysis.googleapis.com":    {"Developer tools", "Stores and reads the notes and occurrences of software artifacts, vulnerabilities among them.", ""},
	"containerscanning.googleapis.com":    {"Security", "Scans the container images pushed to Artifact Registry for vulnerabilities.", ""},

	// The Cloud Storage companion APIs (#1957).
	"storagebatchoperations.googleapis.com": {"Storage", "Runs jobs that delete, update or rewrite many Cloud Storage objects at once.", ""},
	"storageinsights.googleapis.com":        {"Storage", "Writes scheduled inventory reports of the objects in a bucket.", ""},
	"storagetransfer.googleapis.com":        {"Storage", "Transfers data between Cloud Storage buckets and file systems.", ""},
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

// window is project's request history over the range, or nil without one.
func (p serviceUsageProvider) window(project, rangeID string) (*apiWindow, error) {
	r, err := lookupAPIRange(rangeID)
	if err != nil {
		return nil, err
	}
	if p.history == nil {
		return nil, nil
	}
	// The calls made up to now are a sample before the window ends, so a
	// call just made is in it.
	p.history.Sample()
	now := time.Now()
	start := now.Add(-r.Span)
	return &apiWindow{Range: r, Start: start, End: now, Began: p.history.Began(), Deltas: p.history.Deltas(project, start, now)}, nil
}

// apiCounters sums the calls per metrics service, and per method of each:
// the window's, or every project's since the instance started without one.
func (p serviceUsageProvider) apiCounters(w *apiWindow) (map[string]*totalsAcc, map[string]map[string]*totalsAcc) {
	byService := map[string]*totalsAcc{}
	byMethod := map[string]map[string]*totalsAcc{}
	var points []metrics.Point
	switch {
	case w != nil:
		for _, d := range w.Deltas {
			points = append(points, deltaPoint(d))
		}
	case p.metrics != nil:
		points = p.metrics.Snapshot()
	}
	for _, pt := range points {
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

func (p serviceUsageProvider) entries(project string, w *apiWindow) ([]apiEntry, map[string]map[string]*totalsAcc, error) {
	services, err := p.api.Services(project)
	if err != nil {
		return nil, nil, err
	}
	byService, byMethod := p.apiCounters(w)
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
func (p serviceUsageProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var req struct {
		Service string `json:"service"`
		// Range is the time range's ID (apiRanges); empty is the first.
		Range string `json:"range"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %v", err)
		}
	}
	if strings.HasPrefix(call, "key") {
		return p.keysCall(project, call, body)
	}
	if strings.HasPrefix(call, "oauth-") {
		return p.oauthCall(project, call, body)
	}
	if strings.HasPrefix(call, "quota") || strings.HasPrefix(call, "domain") {
		return p.quotasCall(ctx, project, call, body)
	}
	switch call {
	case "apis", "api":
		w, err := p.window(project, req.Range)
		if err != nil {
			return nil, err
		}
		list, byMethod, err := p.entries(project, w)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"project": project, "ranges": apiRanges, "countersNote": p.countersNote(project)}
		if w != nil {
			out["range"] = w.Range.ID
		}
		if call == "apis" {
			out["apis"] = list
			if w != nil {
				counted := map[string]bool{}
				for _, e := range list {
					if e.State == "ENABLED" && e.MetricsService != "" {
						counted[e.MetricsService] = true
					}
				}
				out["charts"] = w.dashboardCharts(counted)
			}
			return out, nil
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
			out["api"], out["methods"] = e, methods
			if w != nil && e.MetricsService != "" {
				out["charts"] = w.apiPageCharts(map[string]bool{e.MetricsService: true})
			}
			return out, nil
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

// countersNote says what the counts are.
func (p serviceUsageProvider) countersNote(project string) string {
	if p.history == nil {
		return "Calls this instance has served since it started, from its own request counters (/metrics). " +
			"They are counted per API for every project together, not per project."
	}
	note := fmt.Sprintf("Calls this instance served in project %s over the time range, sampled every minute from its request counters, "+
		"as Cloud Monitoring's serviceruntime.googleapis.com/api metrics are.", project)
	if p.history.Project() == project {
		note += " A call that named no project counts in this project, the instance's default."
	}
	return note + " History starts when the instance started."
}
