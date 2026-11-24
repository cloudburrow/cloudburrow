package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Load Balancing's pages (#2153), over the Compute Engine API's
// load-balancing collections, as Google's console has them
// (load-balancing/docs/https/setup-global-ext-https-compute,
// .../setup-global-ext-https-serverless, .../load-balancing/docs/tcp/set-up-ext-reg-tcp-proxy-zonal,
// .../network/setting-up-network-backend-service, read 2026-11-22):
// Network services › Load balancing, with its Load balancers, Backends and
// Frontends tabs; Create load balancer's steps (the load balancer's type,
// then its frontend, backend and routing rules, then Create); a load
// balancer's page (Frontend, Routing rules, Backend) and Delete; Compute
// Engine's Health checks and Network endpoint groups; and Certificate
// Manager's Classic Certificates. The lists' columns are the ones the docs
// name where they name them (UNVERIFIED for the rest, re-read 2026-11-22).

const lbNote = "Each forwarding rule listens on 127.0.0.1 at the port shown as its address (cloudburrowAddress): requests to it are routed by its URL map to its backends — instance groups' VMs, Cloud Run through serverless network endpoint groups, buckets — as health checks, balancing modes and session affinity decide. " +
	"Google-managed certificates are issued by this instance's own certificate authority (/cloudburrow/lb/ca.pem). " +
	"Cloud Armor policies, Cloud CDN and Identity-Aware Proxy act on each request through the proxy, as a load balancer's Edit sets them."

// lbScopePath is a resource's [scope, name] path: scope "global" or its
// region.
func lbScopePath(o map[string]any) []string {
	scope := "global"
	if r := vpcLast(vpcStr(o, "region")); r != "" {
		scope = r
	}
	return []string{scope, vpcStr(o, "name")}
}

func lbScopeColl(scope, coll string) string {
	if scope == "global" || scope == "" {
		return "global/" + coll
	}
	return "regions/" + scope + "/" + coll
}

func lbScopeText(scope string) string {
	if scope == "global" {
		return "Global"
	}
	return scope
}

// lbAll is a collection's records, global and regional.
func (c computeScreen) lbAll(project, coll string, global, regional bool) []map[string]any {
	var out []map[string]any
	if global {
		if l, err := c.list(project, "global/"+coll); err == nil {
			out = append(out, l...)
		}
	}
	if regional {
		if m, err := c.call("GET", project, "aggregated/"+coll, nil, nil); err == nil {
			for _, o := range aggregatedItems(m, coll) {
				if vpcStr(o, "region") != "" {
					out = append(out, o)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return vpcStr(out[i], "name") < vpcStr(out[j], "name") })
	return out
}

// lbRel is a link's projects/... part.
func lbRel(link string) string { return pscRel(link) }

// lbSchemeText is a scheme's access type and generation.
func lbAccess(scheme string) string {
	if strings.HasPrefix(scheme, "INTERNAL") {
		return "Internal"
	}
	return "External"
}

// lbModel is one load balancer as the console groups it: an Application
// Load Balancer is its URL map; a proxy or passthrough Network Load
// Balancer its backend service; a target pool-based one its pool.
type lbModel struct {
	path     []string // scope, name
	kind     string   // urlMap, backendService, targetPool
	typ      string
	access   string
	rules    []map[string]any
	proxies  []map[string]any
	services []string // backend services and buckets, relative
	self     map[string]any
}

// lbModels are project's load balancers.
func (c computeScreen) lbModels(project string) []lbModel {
	rules := c.lbAll(project, "forwardingRules", true, true)
	maps := c.lbAll(project, "urlMaps", true, true)
	bss := c.lbAll(project, "backendServices", true, true)
	pools := c.lbAll(project, "targetPools", false, true)
	var proxies []map[string]any
	for _, coll := range []string{"targetHttpProxies", "targetHttpsProxies", "targetTcpProxies", "targetSslProxies"} {
		proxies = append(proxies, c.lbAll(project, coll, true, coll != "targetSslProxies")...)
	}
	rulesTo := func(target string) []map[string]any {
		var out []map[string]any
		for _, r := range rules {
			if lbRel(vpcStr(r, "target")) == target || lbRel(vpcStr(r, "backendService")) == target {
				out = append(out, r)
			}
		}
		return out
	}
	var out []lbModel
	inMap := map[string]bool{}
	for _, m := range maps {
		rel := lbRel(vpcStr(m, "selfLink"))
		lm := lbModel{path: lbScopePath(m), kind: "urlMap", typ: "Application", self: m}
		for _, s := range urlMapServiceLinks(m) {
			inMap[s] = true
			if !hasString(lm.services, s) {
				lm.services = append(lm.services, s)
			}
		}
		for _, p := range proxies {
			if lbRel(vpcStr(p, "urlMap")) == rel {
				lm.proxies = append(lm.proxies, p)
				lm.rules = append(lm.rules, rulesTo(lbRel(vpcStr(p, "selfLink")))...)
			}
		}
		scheme := ""
		for _, s := range bss {
			if hasString(lm.services, lbRel(vpcStr(s, "selfLink"))) {
				scheme = vpcStr(s, "loadBalancingScheme")
			}
		}
		if scheme == "EXTERNAL" && lm.path[0] == "global" {
			lm.typ = "Application (Classic)"
		}
		lm.access = lbAccess(scheme)
		out = append(out, lm)
	}
	for _, s := range bss {
		rel := lbRel(vpcStr(s, "selfLink"))
		if inMap[rel] {
			continue
		}
		lm := lbModel{path: lbScopePath(s), kind: "backendService", services: []string{rel}, self: s, access: lbAccess(vpcStr(s, "loadBalancingScheme"))}
		lm.rules = rulesTo(rel)
		for _, p := range proxies {
			if lbRel(vpcStr(p, "service")) == rel {
				lm.proxies = append(lm.proxies, p)
				lm.rules = append(lm.rules, rulesTo(lbRel(vpcStr(p, "selfLink")))...)
			}
		}
		switch {
		case len(lm.proxies) > 0:
			lm.typ = "Network (Proxy)"
		case len(lm.rules) > 0:
			lm.typ = "Network (Passthrough)"
		default:
			continue // a backend service of no load balancer: Backends tab only
		}
		out = append(out, lm)
	}
	for _, p := range pools {
		lm := lbModel{path: lbScopePath(p), kind: "targetPool", typ: "Network (Passthrough, target pool-based)", self: p, access: "External"}
		lm.rules = rulesTo(lbRel(vpcStr(p, "selfLink")))
		out = append(out, lm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path[1] < out[j].path[1] })
	return out
}

// urlMapServiceLinks are every backend service or bucket a URL map names.
func urlMapServiceLinks(m map[string]any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				switch k {
				case "defaultService", "service", "backendService":
					if s, ok := e.(string); ok && s != "" {
						s = lbRel(s)
						if !hasString(out, s) {
							out = append(out, s)
						}
					}
				default:
					walk(e)
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(m)
	sort.Strings(out)
	return out
}

func (lm lbModel) protocols() string {
	var ps []string
	for _, r := range lm.rules {
		p := "TCP"
		switch {
		case strings.Contains(vpcStr(r, "target"), "/targetHttpsProxies/"):
			p = "HTTPS"
		case strings.Contains(vpcStr(r, "target"), "/targetHttpProxies/"):
			p = "HTTP"
		case strings.Contains(vpcStr(r, "target"), "/targetSslProxies/"):
			p = "SSL"
		case vpcStr(r, "IPProtocol") != "":
			p = vpcStr(r, "IPProtocol")
		}
		if !hasString(ps, p) {
			ps = append(ps, p)
		}
	}
	return vpcDash(strings.Join(ps, ", "))
}

func (lm lbModel) backendsText() string {
	if lm.kind == "targetPool" {
		n := len(vpcStrs(lm.self, "instances"))
		return fmt.Sprintf("1 target pool (%d instance%s)", n, lbPlural(n))
	}
	bs, bb := 0, 0
	for _, s := range lm.services {
		if strings.Contains(s, "/backendBuckets/") {
			bb++
		} else {
			bs++
		}
	}
	var parts []string
	if bs > 0 {
		parts = append(parts, fmt.Sprintf("%d backend service%s", bs, lbPlural(bs)))
	}
	if bb > 0 {
		parts = append(parts, fmt.Sprintf("%d backend bucket%s", bb, lbPlural(bb)))
	}
	return vpcDash(strings.Join(parts, ", "))
}

func lbPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// lbAddress is a rule's address as the console shows it: its IP and
// ports, and the local address that takes its traffic.
func lbAddress(r map[string]any) string {
	a := vpcStr(r, "IPAddress")
	if p := vpcStr(r, "portRange"); p != "" {
		a += ":" + strings.Replace(p, "-", "-", 1)
		if lo, hi, ok := strings.Cut(p, "-"); ok && lo == hi {
			a = vpcStr(r, "IPAddress") + ":" + lo
		}
	} else if ps := vpcStrs(r, "ports"); len(ps) > 0 {
		a += ":" + strings.Join(ps, ",")
	}
	return a
}

// --- Load balancers -----------------------------------------------------------

type computeLBProvider struct{ computeScreen }

func (computeLBProvider) ID() string    { return "netservices-lb" }
func (computeLBProvider) Title() string { return "Load balancing" }

func lbColumns() []string {
	return []string{"Load balancer type", "Access type", "Protocols", "Region", "Backends"}
}

func lbDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete load balancer " + path[1] + "? Its forwarding rules and target proxies are deleted; its backend services, buckets and health checks are kept."}
}

func (p computeLBProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: lbColumns(), NameColumn: "Name", Noun: "load balancers", Note: lbNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Load balancers are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, lm := range p.lbModels(project) {
		path := lm.path
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Load balancer type": lm.typ, "Access type": lm.access, "Protocols": lm.protocols(),
				"Region": lbScopeText(path[0]), "Backends": lm.backendsText()},
			Actions: []console.Action{lbDelete(path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeLBProvider) CreateOnPage() bool  { return true }
func (computeLBProvider) CreateTitle() string { return "Create load balancer" }

func (computeLBProvider) CreateForm() (string, []console.Field) {
	return "Create load balancer", lbCreateFields(nil, nil, nil)
}

func (p computeLBProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	var groups, hcs []string
	if m, err := p.call("GET", project, "aggregated/instanceGroups", nil, nil); err == nil {
		for _, g := range aggregatedItems(m, "instanceGroups") {
			scope := vpcLast(vpcStr(g, "zone"))
			if scope == "" {
				scope = vpcLast(vpcStr(g, "region"))
			}
			groups = append(groups, scope+"/"+vpcStr(g, "name"))
		}
	}
	for _, h := range p.lbAll(project, "healthChecks", true, false) {
		hcs = append(hcs, vpcStr(h, "name"))
	}
	var buckets []string
	for _, b := range p.lbAll(project, "backendBuckets", true, false) {
		buckets = append(buckets, vpcStr(b, "name"))
	}
	sort.Strings(groups)
	return lbCreateFields(groups, hcs, buckets)
}

// lbCreateFields are Create load balancer's steps: the load balancer's
// type (Application, proxy or passthrough Network; public or internal;
// global or single region), its frontend (name, protocol, port, IP
// address, certificate), backend (instance group, Cloud Run service or
// bucket, with its health check) and routing rules (host and path rules
// to other backend services or buckets).
func lbCreateFields(groups, hcs, buckets []string) []console.Field {
	sel := func(f console.Field, opts []string) console.Field {
		if opts != nil {
			f.Type, f.Options = "select", opts
		}
		return f
	}
	app := map[string]string{"lbType": "application"}
	const s1, s2, s3, s4 = "Load balancer type", "Frontend configuration", "Backend configuration", "Routing rules"
	return []console.Field{
		{Name: "lbType", Label: "Type of load balancer", Type: "radio", Default: "application", Section: s1,
			Options: []string{"application", "network-proxy", "network-passthrough"},
			OptionLabels: map[string]string{"application": "Application Load Balancer (HTTP/HTTPS)", "network-proxy": "Network Load Balancer (TCP/SSL), proxy",
				"network-passthrough": "Network Load Balancer (TCP), passthrough"}},
		{Name: "facing", Label: "Public facing or internal", Type: "radio", Default: "external", Section: s1, Options: []string{"external", "internal"},
			OptionLabels: map[string]string{"external": "Public facing (external)", "internal": "Internal"}},
		{Name: "deployment", Label: "Global or single region deployment", Type: "radio", Default: "global", Section: s1, Options: []string{"global", "regional"},
			OptionLabels: map[string]string{"global": "Best for global workloads", "regional": "Best for regional workloads"},
			Help:         "A passthrough Network Load Balancer is always regional; an internal one in this form is regional too."},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, Section: s1,
			ShowWhen: "deployment=regional|lbType=network-passthrough|facing=internal"},
		{Name: "network", Label: "Network", Type: "text", Default: "default", Section: s1, VisibleWhen: map[string]string{"facing": "internal"},
			Help: "An internal load balancer's network; a regional Application Load Balancer needs a proxy-only subnet in its region."},

		{Name: "name", Label: "Load balancer name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp, Section: s2},
		{Name: "protocol", Label: "Protocol", Type: "select", Default: "HTTP", Section: s2, Options: []string{"HTTP", "HTTPS", "TCP", "SSL"},
			Help: "HTTP or HTTPS for an Application Load Balancer; TCP or SSL for a proxy Network Load Balancer; TCP for a passthrough one."},
		{Name: "port", Label: "Port", Type: "text", Default: "80", Section: s2, Pattern: `^[0-9]{1,5}(-[0-9]{1,5})?$`},
		{Name: "ipAddress", Label: "IP address", Type: "text", Section: s2,
			Help: "Empty for Ephemeral (127.0.0.1 on this machine), or the name of a reserved static address of the load balancer's scope."},
		{Name: "certificate", Label: "Certificate", Type: "text", Section: s2, ShowWhen: "protocol=HTTPS|protocol=SSL",
			Help: "An SSL certificate's name, or empty to create a Google-managed one for the domains below."},
		{Name: "domains", Label: "Domains", Type: "text", Section: s2, ShowWhen: "protocol=HTTPS|protocol=SSL",
			Help: "A new Google-managed certificate's domains, comma separated, such as www.example.com. It is issued by this instance's certificate authority."},

		{Name: "backendType", Label: "Backend type", Type: "radio", Default: "instanceGroup", Section: s3,
			Options: []string{"instanceGroup", "serverless", "bucket"},
			OptionLabels: map[string]string{"instanceGroup": "Instance group", "serverless": "Serverless network endpoint group (Cloud Run)",
				"bucket": "Backend bucket (Cloud Storage)"},
			Help: "A serverless network endpoint group and a backend bucket are an Application Load Balancer's only."},
		sel(console.Field{Name: "instanceGroup", Label: "Instance group", Type: "text", Section: s3, VisibleWhen: map[string]string{"backendType": "instanceGroup"},
			Help: "ZONE/NAME, or REGION/NAME for a regional managed group."}, groups),
		{Name: "portName", Label: "Named port", Type: "text", Default: "http", Section: s3, VisibleWhen: map[string]string{"backendType": "instanceGroup"},
			Help: "The instance group's named port the backend service serves."},
		{Name: "balancingMode", Label: "Balancing mode", Type: "select", Default: "UTILIZATION", Section: s3, Options: []string{"UTILIZATION", "RATE", "CONNECTION", "CUSTOM_METRICS"},
			OptionLabels: map[string]string{"UTILIZATION": "Utilization", "RATE": "Rate", "CONNECTION": "Connection", "CUSTOM_METRICS": "Custom metrics"},
			VisibleWhen:  map[string]string{"backendType": "instanceGroup"}},
		{Name: "maxRatePerInstance", Label: "Maximum RPS per instance", Type: "text", Section: s3, VisibleWhen: map[string]string{"balancingMode": "RATE"}},
		{Name: "customMetricName", Label: "Custom metric name", Type: "text", Section: s3, VisibleWhen: map[string]string{"balancingMode": "CUSTOM_METRICS"},
			Pattern: `^[a-z]([\-_.a-z0-9]*[a-z0-9])?$`,
			Help:    "The metric your backends report in their ORCA load report (the endpoint-load-metrics response header), such as orca.cpu_utilization or a named metric."},
		{Name: "customMetricMaxUtilization", Label: "Max utilization", Type: "text", Default: "0.8", Section: s3, VisibleWhen: map[string]string{"balancingMode": "CUSTOM_METRICS"},
			Help: "The target value of the metric, between 0.0 and 1.0."},
		{Name: "customMetricDryRun", Label: "Dry run", Type: "checkbox", Section: s3, VisibleWhen: map[string]string{"balancingMode": "CUSTOM_METRICS"},
			Help: "Report the metric without using it to balance traffic."},
		sel(console.Field{Name: "healthCheck", Label: "Health check", Type: "text", Section: s3, VisibleWhen: map[string]string{"backendType": "instanceGroup"},
			Help: "An existing health check's name, or empty to create an HTTP (TCP for a Network Load Balancer) health check on the serving port."}, append([]string{""}, hcs...)),
		{Name: "cloudRunService", Label: "Cloud Run service", Type: "text", Section: s3, VisibleWhen: map[string]string{"backendType": "serverless"},
			Help: "The Cloud Run service in the region below."},
		{Name: "cloudRunRegion", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, Section: s3,
			VisibleWhen: map[string]string{"backendType": "serverless"}},
		sel(console.Field{Name: "bucket", Label: "Cloud Storage bucket", Type: "text", Section: s3, VisibleWhen: map[string]string{"backendType": "bucket"},
			Help: "An existing backend bucket's name, or a Cloud Storage bucket to make one of."}, nil),
		{Name: "sessionAffinity", Label: "Session affinity", Type: "select", Default: "NONE", Section: s3,
			Options: []string{"NONE", "CLIENT_IP", "GENERATED_COOKIE", "HEADER_FIELD", "HTTP_COOKIE"}},

		{Name: "routing", Label: "Host and path rules", Type: "map", Section: s4, VisibleWhen: app,
			KeyHelp: "A path pattern, such as /api/*, or host|path such as example.com|/images/*", ValueHelp: "A backend service or backend bucket's name",
			Help: "Requests that match no rule go to the backend above."},
	}
}

func (p computeLBProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Load balancer name: give it a name")
	}
	lbType, internal := vpcOr(v["lbType"], "application"), v["facing"] == "internal"
	regional := v["deployment"] == "regional" || lbType == "network-passthrough" || internal
	region := vpcOr(v["region"], "us-central1")
	var customMetric map[string]any
	if v["balancingMode"] == "CUSTOM_METRICS" && vpcOr(v["backendType"], "instanceGroup") == "instanceGroup" {
		customMetric = map[string]any{"name": strings.TrimSpace(v["customMetricName"])}
		if customMetric["name"] == "" {
			return "", errors.New("Custom metric name: give the metric your backends report")
		}
		u, err := strconv.ParseFloat(vpcOr(strings.TrimSpace(v["customMetricMaxUtilization"]), "0.8"), 64)
		if err != nil || u < 0 || u > 1 {
			return "", errors.New("Max utilization: a number between 0.0 and 1.0")
		}
		customMetric["maxUtilization"] = u
		if v["customMetricDryRun"] == "true" || v["customMetricDryRun"] == "on" {
			customMetric["dryRun"] = true
		}
		if lbType != "application" {
			return "", errors.New("Balancing mode: custom metrics are for Application Load Balancers")
		}
	}
	scope := "global"
	if regional {
		scope = region
	}
	scheme := "EXTERNAL_MANAGED"
	switch {
	case internal:
		scheme = "INTERNAL_MANAGED"
		if lbType == "network-passthrough" {
			scheme = "INTERNAL"
		}
	case lbType == "network-passthrough":
		scheme = "EXTERNAL"
	}
	protocol := vpcOr(v["protocol"], "HTTP")
	switch lbType {
	case "application":
		if protocol != "HTTP" && protocol != "HTTPS" {
			return "", errors.New("Protocol: an Application Load Balancer's is HTTP or HTTPS")
		}
	case "network-proxy":
		if protocol != "TCP" && protocol != "SSL" {
			return "", errors.New("Protocol: a proxy Network Load Balancer's is TCP or SSL")
		}
	default:
		protocol = "TCP"
	}
	coll := func(c string) string { return lbScopeColl(scope, c) }
	post := func(c string, body map[string]any) error {
		_, err := p.call("POST", project, coll(c), nil, body)
		return err
	}
	var network string
	if internal {
		network = "projects/" + project + "/global/networks/" + vpcOr(strings.TrimSpace(v["network"]), "default")
	}

	// The backend.
	backend := ""
	switch v["backendType"] {
	case "bucket":
		if lbType != "application" {
			return "", errors.New("Backend type: a backend bucket is an Application Load Balancer's only")
		}
		bucket := strings.TrimSpace(v["bucket"])
		if bucket == "" {
			return "", errors.New("Cloud Storage bucket: give one")
		}
		if _, err := p.call("GET", project, "global/backendBuckets/"+bucket, nil, nil); err == nil {
			backend = "global/backendBuckets/" + bucket
		} else {
			if err := post("backendBuckets", map[string]any{"name": name + "-bucket", "bucketName": bucket}); err != nil {
				return "", err
			}
			backend = "global/backendBuckets/" + name + "-bucket"
		}
	case "serverless":
		if lbType != "application" {
			return "", errors.New("Backend type: a serverless network endpoint group is an Application Load Balancer's only")
		}
		svc := strings.TrimSpace(v["cloudRunService"])
		if svc == "" {
			return "", errors.New("Cloud Run service: give one")
		}
		rr := vpcOr(v["cloudRunRegion"], region)
		if regional && rr != region {
			return "", errors.New("Region: a regional load balancer's Cloud Run service is in its region")
		}
		neg := name + "-neg"
		if _, err := p.call("POST", project, "regions/"+rr+"/networkEndpointGroups", nil, map[string]any{"name": neg,
			"networkEndpointType": "SERVERLESS", "cloudRun": map[string]any{"service": svc}}); err != nil {
			return "", err
		}
		bs := map[string]any{"name": name + "-backend-service", "loadBalancingScheme": scheme, "protocol": "HTTPS",
			"backends": []any{map[string]any{"group": "regions/" + rr + "/networkEndpointGroups/" + neg}}}
		if err := post("backendServices", bs); err != nil {
			return "", err
		}
		backend = coll("backendServices") + "/" + name + "-backend-service"
	default:
		ig := strings.TrimSpace(v["instanceGroup"])
		where, gname, ok := strings.Cut(ig, "/")
		if !ok || gname == "" {
			return "", errors.New("Instance group: ZONE/NAME or REGION/NAME")
		}
		group := "zones/" + where + "/instanceGroups/" + gname
		if strings.Count(where, "-") == 1 {
			group = "regions/" + where + "/instanceGroups/" + gname
		}
		hc := strings.TrimSpace(v["healthCheck"])
		if hc == "" {
			hc = name + "-health-check"
			typ := "HTTP"
			if lbType != "application" {
				typ = "TCP"
			}
			body := map[string]any{"name": hc, "type": typ, strings.ToLower(typ) + "HealthCheck": map[string]any{"portSpecification": "USE_SERVING_PORT"}}
			if lbType == "network-passthrough" {
				body[strings.ToLower(typ)+"HealthCheck"] = map[string]any{"port": lbFirstPort(v["port"])}
			}
			if err := post("healthChecks", body); err != nil {
				return "", err
			}
		}
		bp := "HTTP"
		if lbType != "application" {
			bp = "TCP"
		}
		mode := vpcOr(v["balancingMode"], "UTILIZATION")
		if lbType == "network-passthrough" {
			mode = "CONNECTION"
		}
		be := map[string]any{"group": group, "balancingMode": mode}
		if mode == "RATE" {
			r, err := strconv.ParseFloat(strings.TrimSpace(v["maxRatePerInstance"]), 64)
			if err != nil || r <= 0 {
				return "", errors.New("Maximum RPS per instance: a number above 0")
			}
			be["maxRatePerInstance"] = r
		}
		if mode == "CUSTOM_METRICS" {
			be["customMetrics"] = []any{customMetric}
		}
		bs := map[string]any{"name": name + "-backend-service", "loadBalancingScheme": scheme, "protocol": bp,
			"healthChecks": []any{coll("healthChecks") + "/" + hc}, "backends": []any{be}}
		if lbType != "network-passthrough" {
			bs["portName"] = vpcOr(strings.TrimSpace(v["portName"]), "http")
		}
		if network != "" && lbType == "network-passthrough" {
			bs["network"] = network
		}
		if a := vpcOr(v["sessionAffinity"], "NONE"); a != "NONE" {
			bs["sessionAffinity"] = a
		}
		if err := post("backendServices", bs); err != nil {
			return "", err
		}
		backend = coll("backendServices") + "/" + name + "-backend-service"
	}

	// The frontend.
	rule := map[string]any{"name": name + "-forwarding-rule", "loadBalancingScheme": scheme, "IPProtocol": "TCP"}
	port := vpcOr(strings.TrimSpace(v["port"]), map[bool]string{true: "443", false: "80"}[protocol == "HTTPS" || protocol == "SSL"])
	rule["portRange"] = port
	if ip := strings.TrimSpace(v["ipAddress"]); ip != "" {
		rule["IPAddress"] = coll("addresses") + "/" + ip
	}
	if network != "" {
		rule["network"] = network
	}
	var certs []any
	if protocol == "HTTPS" || protocol == "SSL" {
		cert := strings.TrimSpace(v["certificate"])
		if cert == "" {
			ds := vpcList(v["domains"])
			if len(ds) == 0 {
				return "", errors.New("Certificate: choose one, or give the domains of a Google-managed certificate")
			}
			if regional {
				return "", errors.New("Certificate: a Google-managed certificate is global only; give a regional self-managed certificate's name")
			}
			cert = name + "-cert"
			if err := post("sslCertificates", map[string]any{"name": cert, "type": "MANAGED", "managed": map[string]any{"domains": ds}}); err != nil {
				return "", err
			}
		}
		certs = []any{coll("sslCertificates") + "/" + cert}
	}
	switch lbType {
	case "application":
		um := map[string]any{"name": name, "defaultService": backend}
		rules, err := lbRoutingRules(v["routing"], scope)
		if err != nil {
			return "", err
		}
		if len(rules) > 0 {
			um["hostRules"], um["pathMatchers"] = lbHostRules(rules, backend)
		}
		if err := post("urlMaps", um); err != nil {
			return "", err
		}
		proxyColl := "targetHttpProxies"
		proxy := map[string]any{"name": name + "-target-proxy", "urlMap": coll("urlMaps") + "/" + name}
		if protocol == "HTTPS" {
			proxyColl, proxy["sslCertificates"] = "targetHttpsProxies", certs
		}
		if err := post(proxyColl, proxy); err != nil {
			return "", err
		}
		rule["target"] = coll(proxyColl) + "/" + name + "-target-proxy"
	case "network-proxy":
		proxyColl := "targetTcpProxies"
		proxy := map[string]any{"name": name + "-target-proxy", "service": backend}
		if protocol == "SSL" {
			if regional {
				return "", errors.New("Protocol: a target SSL proxy is global only")
			}
			proxyColl, proxy["sslCertificates"] = "targetSslProxies", certs
		}
		if err := post(proxyColl, proxy); err != nil {
			return "", err
		}
		rule["target"] = coll(proxyColl) + "/" + name + "-target-proxy"
	default:
		rule["backendService"] = backend
	}
	if err := post("forwardingRules", rule); err != nil {
		return "", err
	}
	return name, nil
}

func lbFirstPort(p string) float64 {
	lo, _, _ := strings.Cut(strings.TrimSpace(p), "-")
	n, err := strconv.Atoi(lo)
	if err != nil {
		return 80
	}
	return float64(n)
}

type lbRule struct{ host, path, service string }

// lbRoutingRules reads the routing map's entries (path, or host|path, to
// a backend service or bucket).
func lbRoutingRules(raw, scope string) ([]lbRule, error) {
	m, err := console.ParseMap(raw)
	if err != nil {
		return nil, fmt.Errorf("Host and path rules: %w", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []lbRule
	for _, k := range keys {
		svc := strings.TrimSpace(m[k])
		if svc == "" {
			continue
		}
		host, path := "*", strings.TrimSpace(k)
		if h, pth, ok := strings.Cut(path, "|"); ok {
			host, path = strings.TrimSpace(h), strings.TrimSpace(pth)
		}
		if !strings.Contains(svc, "/") {
			svc = lbScopeColl(scope, "backendServices") + "/" + svc
		}
		out = append(out, lbRule{host, path, svc})
	}
	return out, nil
}

func lbHostRules(rules []lbRule, def string) ([]any, []any) {
	byHost := map[string][]lbRule{}
	var hosts []string
	for _, r := range rules {
		if _, ok := byHost[r.host]; !ok {
			hosts = append(hosts, r.host)
		}
		byHost[r.host] = append(byHost[r.host], r)
	}
	var hrs, pms []any
	for i, h := range hosts {
		pm := "path-matcher-" + strconv.Itoa(i+1)
		hrs = append(hrs, map[string]any{"hosts": []any{h}, "pathMatcher": pm})
		var prs []any
		for _, r := range byHost[h] {
			prs = append(prs, map[string]any{"paths": []any{r.path}, "service": r.service})
		}
		pms = append(pms, map[string]any{"name": pm, "defaultService": def, "pathRules": prs})
	}
	return hrs, pms
}

func (p computeLBProvider) model(project string, path []string) (lbModel, error) {
	if len(path) != 2 {
		return lbModel{}, fmt.Errorf("a load balancer is addressed by its scope and name, not %v", path)
	}
	for _, lm := range p.lbModels(project) {
		if lm.path[0] == path[0] && lm.path[1] == path[1] {
			return lm, nil
		}
	}
	return lbModel{}, fmt.Errorf("load balancer %s was not found", path[1])
}

func (p computeLBProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	lm, err := p.model(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Load balancer type", Value: lm.typ},
		{Label: "Access type", Value: lm.access}, {Label: "Region", Value: lbScopeText(path[0])}}
	front := console.Listing{Columns: []string{"Protocol", "IP:Port", "Certificate", "Network tier", "Local address"}, NameColumn: "Name", Noun: "frontends"}
	for _, r := range lm.rules {
		var certs []string
		for _, px := range lm.proxies {
			if lbRel(vpcStr(px, "selfLink")) == lbRel(vpcStr(r, "target")) {
				for _, c := range vpcStrs(px, "sslCertificates") {
					certs = append(certs, vpcLast(c))
				}
			}
		}
		front.Items = append(front.Items, console.Resource{Name: vpcStr(r, "name"), Fields: map[string]string{
			"Protocol": lbModel{rules: []map[string]any{r}}.protocols(), "IP:Port": lbAddress(r), "Certificate": vpcDash(strings.Join(certs, ", ")),
			"Network tier": vpcTitle(strings.ToLower(vpcOr(vpcStr(r, "networkTier"), "PREMIUM"))), "Local address": vpcDash(vpcStr(r, "cloudburrowAddress"))}})
	}
	front.Total = len(front.Items)
	routes := console.Listing{Columns: []string{"Hosts", "Paths", "Backend"}, NameColumn: "Rule", Noun: "routing rules"}
	if lm.kind == "urlMap" {
		m := lm.self
		matchers := map[string]map[string]any{}
		for _, pm := range vmMaps(m, "pathMatchers") {
			matchers[vpcStr(pm, "name")] = pm
		}
		n := 0
		addRow := func(hosts, paths, backend string) {
			n++
			routes.Items = append(routes.Items, console.Resource{Name: strconv.Itoa(n), Fields: map[string]string{"Hosts": hosts, "Paths": paths, "Backend": backend}})
		}
		for _, hr := range vmMaps(m, "hostRules") {
			hosts := strings.Join(vpcStrs(hr, "hosts"), ", ")
			pm := matchers[vpcStr(hr, "pathMatcher")]
			for _, pr := range vmMaps(pm, "pathRules") {
				target := vpcLast(vpcStr(pr, "service"))
				if vpcStr(pr, "service") == "" {
					target = "Redirect"
				}
				addRow(hosts, strings.Join(vpcStrs(pr, "paths"), ", "), target)
			}
			if len(vmMaps(pm, "routeRules")) > 0 {
				addRow(hosts, fmt.Sprintf("%d route rules", len(vmMaps(pm, "routeRules"))), "Advanced")
			}
			addRow(hosts, "/*", vpcLast(vpcOr(vpcStr(pm, "defaultService"), vpcStr(m, "defaultService"))))
		}
		addRow("All unmatched (default)", "All unmatched (default)", vpcDash(vpcLast(vpcStr(m, "defaultService"))))
	}
	routes.Total = len(routes.Items)
	backs := console.Listing{Columns: []string{"Endpoint protocol", "Named port", "Timeout", "Health check", "Session affinity", "Backends"}, NameColumn: "Backend service", Noun: "backends"}
	for _, s := range lm.services {
		o, err := p.call("GET", project, strings.SplitN(s, "/", 3)[2], nil, nil)
		if err != nil {
			continue
		}
		var groups []string
		for _, b := range vmMaps(o, "backends") {
			groups = append(groups, vpcLast(vpcStr(b, "group")))
		}
		row := console.Resource{Name: vpcStr(o, "name"), Fields: map[string]string{"Endpoint protocol": vpcDash(vpcStr(o, "protocol")),
			"Named port": vpcDash(vpcStr(o, "portName")), "Timeout": vpcDash(vpcStr(o, "timeoutSec")) + map[bool]string{true: " seconds", false: ""}[vpcStr(o, "timeoutSec") != ""],
			"Health check": vpcDash(vpcLast(strings.Join(vpcStrs(o, "healthChecks"), ""))), "Session affinity": vpcDash(vpcStr(o, "sessionAffinity")),
			"Backends": vpcDash(strings.Join(groups, ", "))}}
		if vpcStr(o, "kind") == "compute#backendBucket" {
			row.Fields["Endpoint protocol"], row.Fields["Backends"] = "Cloud Storage", vpcStr(o, "bucketName")
		}
		backs.Items = append(backs.Items, row)
	}
	if lm.kind == "targetPool" {
		var insts []string
		for _, i := range vpcStrs(lm.self, "instances") {
			insts = append(insts, vpcLast(i))
		}
		backs.Items = append(backs.Items, console.Resource{Name: vpcStr(lm.self, "name"), Fields: map[string]string{"Endpoint protocol": "TCP",
			"Health check": vpcDash(vpcLast(strings.Join(vpcStrs(lm.self, "healthChecks"), ""))), "Session affinity": vpcDash(vpcStr(lm.self, "sessionAffinity")),
			"Backends": vpcDash(strings.Join(insts, ", "))}})
	}
	backs.Total = len(backs.Items)
	return console.Detail{Title: path[1], Heading: "Load balancer details", Edit: p.lbEditForm(project, lm),
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "frontend", Label: "Frontend", Listing: front},
			{ID: "routing", Label: "Routing rules", Listing: routes},
			{ID: "backend", Label: "Backend", Listing: backs}}}, nil
}

func (p computeLBProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	if _, err := p.model(project, path); err != nil {
		return nil
	}
	a := lbDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

// ActAt deletes a load balancer: its forwarding rules, then its target
// proxies, then its URL map, keeping its backend services, buckets and
// health checks, as Google's Delete keeps what is not ticked.
func (p computeLBProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	lm, err := p.model(project, path)
	if err != nil {
		return err
	}
	del := func(self string) error {
		rel := lbRel(self)
		_, err := p.call("DELETE", project, strings.SplitN(rel, "/", 3)[2], nil, nil)
		return err
	}
	for _, r := range lm.rules {
		if err := del(vpcStr(r, "selfLink")); err != nil {
			return err
		}
	}
	for _, px := range lm.proxies {
		if err := del(vpcStr(px, "selfLink")); err != nil {
			return err
		}
	}
	if lm.kind == "urlMap" {
		return del(vpcStr(lm.self, "selfLink"))
	}
	return nil
}

// --- Backends ----------------------------------------------------------------

type computeLBBackendsProvider struct{ computeScreen }

func (computeLBBackendsProvider) ID() string    { return "netservices-lb-backends" }
func (computeLBBackendsProvider) Title() string { return "Load balancing" }

func (p computeLBBackendsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Backend type", "Protocol", "Scope", "Load balancing scheme", "Health check", "In use by"}, NameColumn: "Name", Noun: "backends", Note: lbNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Backends are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, o := range append(p.lbAll(project, "backendServices", true, true), p.lbAll(project, "backendBuckets", true, false)...) {
		path := lbScopePath(o)
		bucket := vpcStr(o, "kind") == "compute#backendBucket"
		kind := "Backend service"
		if bucket {
			kind = "Backend bucket"
		}
		var users []string
		for _, u := range vpcStrs(o, "usedBy") {
			users = append(users, vpcLast(u))
		}
		f := map[string]string{"Backend type": kind, "Protocol": vpcDash(vpcStr(o, "protocol")), "Scope": lbScopeText(path[0]),
			"Load balancing scheme": vpcDash(vpcStr(o, "loadBalancingScheme")), "Health check": vpcDash(vpcLast(strings.Join(vpcStrs(o, "healthChecks"), ""))),
			"In use by": vpcDash(strings.Join(users, ", "))}
		if bucket {
			f["Protocol"] = "Cloud Storage: " + vpcStr(o, "bucketName")
		}
		target := []string{path[0], map[bool]string{true: "backendBuckets", false: "backendServices"}[bucket], path[1]}
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Fields: f, Target: target,
			Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete " + strings.ToLower(kind) + " " + path[1] + "?"}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeLBBackendsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 3 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, lbScopeColl(path[0], path[1])+"/"+path[2], nil, nil)
	return err
}

// --- Frontends ---------------------------------------------------------------

type computeLBFrontendsProvider struct{ computeScreen }

func (computeLBFrontendsProvider) ID() string    { return "netservices-lb-frontends" }
func (computeLBFrontendsProvider) Title() string { return "Load balancing" }

func (p computeLBFrontendsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Protocol", "Scope", "Address", "Network tier", "Target", "Local address"}, NameColumn: "Name", Noun: "frontends", Note: lbNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Frontends are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, r := range p.lbAll(project, "forwardingRules", true, true) {
		if vpcStr(r, "loadBalancingScheme") == "" {
			continue // a Private Service Connect endpoint: its own page
		}
		path := lbScopePath(r)
		target := vpcLast(vpcOr(vpcStr(r, "target"), vpcStr(r, "backendService")))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Protocol": lbModel{rules: []map[string]any{r}}.protocols(), "Scope": lbScopeText(path[0]), "Address": lbAddress(r),
				"Network tier": vpcTitle(strings.ToLower(vpcOr(vpcStr(r, "networkTier"), "PREMIUM"))), "Target": vpcDash(target),
				"Local address": vpcDash(vpcStr(r, "cloudburrowAddress"))},
			Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete forwarding rule " + path[1] + "?"}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeLBFrontendsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, lbScopeColl(path[0], "forwardingRules")+"/"+path[1], nil, nil)
	return err
}

// --- Health checks -----------------------------------------------------------

// computeHealthChecksProvider is Compute Engine's Health checks page
// (load-balancing/docs/health-checks, "Create a health check"): Name,
// Scope, Protocol, Port, Check interval, Timeout, thresholds; Create
// health check; Delete.
type computeHealthChecksProvider struct{ computeScreen }

func (computeHealthChecksProvider) ID() string    { return "compute-healthchecks" }
func (computeHealthChecksProvider) Title() string { return "Health checks" }

func hcPort(o map[string]any) string {
	for _, k := range []string{"httpHealthCheck", "httpsHealthCheck", "http2HealthCheck", "tcpHealthCheck", "sslHealthCheck", "grpcHealthCheck"} {
		if m, ok := o[k].(map[string]any); ok {
			if vpcStr(m, "portSpecification") == "USE_SERVING_PORT" {
				return "Serving port"
			}
			return vpcOr(vpcStr(m, "port"), vpcStr(m, "portName"))
		}
	}
	return vpcStr(o, "port")
}

func (p computeHealthChecksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Scope", "Protocol", "Port", "Interval", "Timeout", "Healthy threshold", "Unhealthy threshold"}, NameColumn: "Name", Noun: "health checks",
		Note: "A health check probes its backends from Google's probe ranges, 35.191.0.0/16 and 130.211.0.0/22: an instance's network needs a firewall rule allowing both on the probed port, as on Google."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Health checks are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, o := range p.lbAll(project, "healthChecks", true, true) {
		path := lbScopePath(o)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Scope": lbScopeText(path[0]), "Protocol": vpcStr(o, "type"), "Port": vpcDash(hcPort(o)),
				"Interval": vpcStr(o, "checkIntervalSec") + " seconds", "Timeout": vpcStr(o, "timeoutSec") + " seconds",
				"Healthy threshold": vpcStr(o, "healthyThreshold"), "Unhealthy threshold": vpcStr(o, "unhealthyThreshold")},
			Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete health check " + path[1] + "?"}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeHealthChecksProvider) CreateOnPage() bool  { return true }
func (computeHealthChecksProvider) CreateTitle() string { return "Create a health check" }
func (computeHealthChecksProvider) CreateLayout() string {
	return console.CreateLayoutPage
}

func (computeHealthChecksProvider) CreateForm() (string, []console.Field) {
	return "Create a health check", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "scope", Label: "Scope", Type: "radio", Default: "global", Options: []string{"global", "regional"},
			OptionLabels: map[string]string{"global": "Global", "regional": "Regional"}},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, VisibleWhen: map[string]string{"scope": "regional"}},
		{Name: "protocol", Label: "Protocol", Type: "select", Default: "TCP", Options: []string{"TCP", "SSL", "HTTP", "HTTPS", "HTTP2", "GRPC", "GRPC_WITH_TLS"}},
		{Name: "port", Label: "Port", Type: "text", Default: "80", Pattern: `^[0-9]{1,5}$`},
		{Name: "requestPath", Label: "Request path", Type: "text", Default: "/", ShowWhen: "protocol=HTTP|protocol=HTTPS|protocol=HTTP2"},
		{Name: "response", Label: "Response", Type: "text", Help: "The start of the body a healthy backend answers; empty accepts any."},
		{Name: "proxyHeader", Label: "Proxy protocol", Type: "select", Default: "NONE", Options: []string{"NONE", "PROXY_V1"}},
		{Name: "checkIntervalSec", Label: "Check interval", Type: "text", Default: "5", Help: "Seconds."},
		{Name: "timeoutSec", Label: "Timeout", Type: "text", Default: "5", Help: "Seconds; no more than the check interval."},
		{Name: "healthyThreshold", Label: "Healthy threshold", Type: "text", Default: "2"},
		{Name: "unhealthyThreshold", Label: "Unhealthy threshold", Type: "text", Default: "2"},
	}
}

func (p computeHealthChecksProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	proto := vpcOr(v["protocol"], "TCP")
	spec := map[string]any{"proxyHeader": vpcOr(v["proxyHeader"], "NONE")}
	if n, err := strconv.Atoi(strings.TrimSpace(v["port"])); err == nil {
		spec["port"] = float64(n)
	}
	if strings.HasPrefix(proto, "HTTP") {
		spec["requestPath"] = vpcOr(strings.TrimSpace(v["requestPath"]), "/")
	}
	if r := strings.TrimSpace(v["response"]); r != "" {
		spec["response"] = r
	}
	if strings.HasPrefix(proto, "GRPC") {
		delete(spec, "proxyHeader")
	}
	key := map[string]string{"TCP": "tcpHealthCheck", "SSL": "sslHealthCheck", "HTTP": "httpHealthCheck", "HTTPS": "httpsHealthCheck",
		"HTTP2": "http2HealthCheck", "GRPC": "grpcHealthCheck", "GRPC_WITH_TLS": "grpcTlsHealthCheck"}[proto]
	body := map[string]any{"name": name, "type": proto, key: spec}
	for _, k := range []string{"checkIntervalSec", "timeoutSec", "healthyThreshold", "unhealthyThreshold"} {
		if n, err := strconv.Atoi(strings.TrimSpace(v[k])); err == nil {
			body[k] = float64(n)
		}
	}
	scope := "global"
	if v["scope"] == "regional" {
		scope = vpcOr(v["region"], "us-central1")
	}
	if _, err := p.call("POST", project, lbScopeColl(scope, "healthChecks"), nil, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeHealthChecksProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a health check is addressed by its scope and name"}, nil
	}
	o, err := p.call("GET", project, lbScopeColl(path[0], "healthChecks")+"/"+path[1], nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Scope", Value: lbScopeText(path[0])}, {Label: "Protocol", Value: vpcStr(o, "type")},
		{Label: "Port", Value: vpcDash(hcPort(o))}, {Label: "Check interval", Value: vpcStr(o, "checkIntervalSec") + " seconds"},
		{Label: "Timeout", Value: vpcStr(o, "timeoutSec") + " seconds"}, {Label: "Healthy threshold", Value: vpcStr(o, "healthyThreshold")},
		{Label: "Unhealthy threshold", Value: vpcStr(o, "unhealthyThreshold")}}
	return console.Detail{Title: path[1], Heading: "Health check details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}}}, nil
}

func (p computeHealthChecksProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete health check " + path[1] + "?", Leaves: true}}
}

func (p computeHealthChecksProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, lbScopeColl(path[0], "healthChecks")+"/"+path[1], nil, nil)
	return err
}

// --- Network endpoint groups -------------------------------------------------

// computeNEGsProvider is Compute Engine's Network endpoint groups page
// (load-balancing/docs/negs): zonal (GCE_VM_IP_PORT), serverless (Cloud
// Run), internet (FQDN or IP and port) and Private Service Connect groups.
type computeNEGsProvider struct{ computeScreen }

func (computeNEGsProvider) ID() string    { return "compute-negs" }
func (computeNEGsProvider) Title() string { return "Network endpoint groups" }

func negTypeText(t string) string {
	return map[string]string{"GCE_VM_IP_PORT": "Zonal (GCE_VM_IP_PORT)", "GCE_VM_IP": "Zonal (GCE_VM_IP)", "NON_GCP_PRIVATE_IP_PORT": "Hybrid connectivity",
		"SERVERLESS": "Serverless", "INTERNET_FQDN_PORT": "Internet (FQDN and port)", "INTERNET_IP_PORT": "Internet (IP and port)",
		"PRIVATE_SERVICE_CONNECT": "Private Service Connect"}[t]
}

func (p computeNEGsProvider) negs(project string) []map[string]any {
	out := p.lbAll(project, "networkEndpointGroups", true, false)
	if m, err := p.call("GET", project, "aggregated/networkEndpointGroups", nil, nil); err == nil {
		for _, o := range aggregatedItems(m, "networkEndpointGroups") {
			if vpcStr(o, "region") != "" || vpcStr(o, "zone") != "" {
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return vpcStr(out[i], "name") < vpcStr(out[j], "name") })
	return out
}

func negPath(o map[string]any) []string {
	switch {
	case vpcStr(o, "zone") != "":
		return []string{"zones", vpcLast(vpcStr(o, "zone")), vpcStr(o, "name")}
	case vpcStr(o, "region") != "":
		return []string{"regions", vpcLast(vpcStr(o, "region")), vpcStr(o, "name")}
	}
	return []string{"global", "global", vpcStr(o, "name")}
}

func negColl(path []string) string {
	if path[0] == "global" {
		return "global/networkEndpointGroups/" + path[2]
	}
	return path[0] + "/" + path[1] + "/networkEndpointGroups/" + path[2]
}

func (p computeNEGsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Network endpoint type", "Scope", "Network", "Size", "In use by"}, NameColumn: "Name", Noun: "network endpoint groups", Note: lbNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Network endpoint groups are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	bss := p.lbAll(project, "backendServices", true, true)
	for _, o := range p.negs(project) {
		path := negPath(o)
		rel := lbRel(vpcStr(o, "selfLink"))
		var users []string
		for _, b := range bss {
			for _, be := range vmMaps(b, "backends") {
				if lbRel(vpcStr(be, "group")) == rel {
					users = append(users, vpcStr(b, "name"))
				}
			}
		}
		listing.Items = append(listing.Items, console.Resource{Name: path[2], Opens: path, Target: path,
			Fields: map[string]string{"Network endpoint type": negTypeText(vpcStr(o, "networkEndpointType")), "Scope": path[1],
				"Network": vpcDash(vpcLast(vpcStr(o, "network"))), "Size": vpcOr(vpcStr(o, "size"), "0"), "In use by": vpcDash(strings.Join(users, ", "))},
			Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete network endpoint group " + path[2] + "?"}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeNEGsProvider) CreateOnPage() bool   { return true }
func (computeNEGsProvider) CreateTitle() string  { return "Create network endpoint group" }
func (computeNEGsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (computeNEGsProvider) CreateForm() (string, []console.Field) {
	zonal := map[string]string{"type": "GCE_VM_IP_PORT"}
	return "Create network endpoint group", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "type", Label: "Network endpoint group type", Type: "select", Default: "GCE_VM_IP_PORT",
			Options: []string{"GCE_VM_IP_PORT", "SERVERLESS", "INTERNET_FQDN_PORT", "INTERNET_IP_PORT", "PRIVATE_SERVICE_CONNECT"},
			OptionLabels: map[string]string{"GCE_VM_IP_PORT": "Zonal network endpoint group", "SERVERLESS": "Serverless network endpoint group",
				"INTERNET_FQDN_PORT": "Internet network endpoint group (FQDN)", "INTERNET_IP_PORT": "Internet network endpoint group (IP)",
				"PRIVATE_SERVICE_CONNECT": "Private Service Connect network endpoint group"}},
		{Name: "zone", Label: "Zone", Type: "text", Default: "us-central1-a", VisibleWhen: zonal},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, ShowWhen: "type=SERVERLESS|type=PRIVATE_SERVICE_CONNECT"},
		{Name: "network", Label: "Network", Type: "text", Default: "default", VisibleWhen: zonal},
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Default: "default", VisibleWhen: zonal},
		{Name: "defaultPort", Label: "Default port", Type: "text", Default: "80", ShowWhen: "type=GCE_VM_IP_PORT|type=INTERNET_FQDN_PORT|type=INTERNET_IP_PORT"},
		{Name: "cloudRunService", Label: "Cloud Run service", Type: "text", VisibleWhen: map[string]string{"type": "SERVERLESS"}},
		{Name: "endpoint", Label: "Endpoint", Type: "text", ShowWhen: "type=GCE_VM_IP_PORT|type=INTERNET_FQDN_PORT|type=INTERNET_IP_PORT",
			Help: "A zonal group's VM instance name, or an internet group's fully qualified domain name or IP address; empty adds none."},
		{Name: "targetService", Label: "Target service", Type: "text", VisibleWhen: map[string]string{"type": "PRIVATE_SERVICE_CONNECT"},
			Help: "A service attachment's URI, or a Google API's host such as storage.us-central1.p.rep.googleapis.com."},
	}
}

func (p computeNEGsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	typ := vpcOr(v["type"], "GCE_VM_IP_PORT")
	body := map[string]any{"name": name, "networkEndpointType": typ}
	port, _ := strconv.Atoi(strings.TrimSpace(v["defaultPort"]))
	var coll string
	switch typ {
	case "SERVERLESS":
		coll = "regions/" + vpcOr(v["region"], "us-central1") + "/networkEndpointGroups"
		body["cloudRun"] = map[string]any{"service": strings.TrimSpace(v["cloudRunService"])}
	case "PRIVATE_SERVICE_CONNECT":
		coll = "regions/" + vpcOr(v["region"], "us-central1") + "/networkEndpointGroups"
		body["pscTargetService"] = strings.TrimSpace(v["targetService"])
	case "GCE_VM_IP_PORT":
		zone := vpcOr(strings.TrimSpace(v["zone"]), "us-central1-a")
		coll = "zones/" + zone + "/networkEndpointGroups"
		body["network"] = "global/networks/" + vpcOr(strings.TrimSpace(v["network"]), "default")
		body["subnetwork"] = "regions/" + zone[:strings.LastIndex(zone, "-")] + "/subnetworks/" + vpcOr(strings.TrimSpace(v["subnetwork"]), "default")
		if port > 0 {
			body["defaultPort"] = float64(port)
		}
	default:
		coll = "global/networkEndpointGroups"
		if port > 0 {
			body["defaultPort"] = float64(port)
		}
	}
	if _, err := p.call("POST", project, coll, nil, body); err != nil {
		return "", err
	}
	if ep := strings.TrimSpace(v["endpoint"]); ep != "" && (typ == "GCE_VM_IP_PORT" || strings.HasPrefix(typ, "INTERNET")) {
		e := map[string]any{}
		switch typ {
		case "GCE_VM_IP_PORT":
			e["instance"] = ep
		case "INTERNET_FQDN_PORT":
			e["fqdn"] = ep
		default:
			e["ipAddress"] = ep
		}
		if _, err := p.call("POST", project, coll+"/"+name+"/attachNetworkEndpoints", nil, map[string]any{"networkEndpoints": []any{e}}); err != nil {
			return name, err
		}
	}
	return name, nil
}

func (p computeNEGsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 3 {
		return console.Detail{Unavailable: "a network endpoint group is addressed by its scope and name"}, nil
	}
	o, err := p.call("GET", project, negColl(path), nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[2]}, {Label: "Network endpoint type", Value: negTypeText(vpcStr(o, "networkEndpointType"))},
		{Label: "Scope", Value: path[1]}, {Label: "Network", Value: vpcDash(vpcLast(vpcStr(o, "network")))},
		{Label: "Default port", Value: vpcDash(vpcStr(o, "defaultPort"))}}
	if cr, ok := o["cloudRun"].(map[string]any); ok {
		props = append(props, console.Property{Label: "Cloud Run service", Value: vpcDash(vpcStr(cr, "service"))})
	}
	if t := vpcStr(o, "pscTargetService"); t != "" {
		props = append(props, console.Property{Label: "Target service", Value: t})
	}
	eps := console.Listing{Columns: []string{"IP address", "Port", "Health"}, NameColumn: "Endpoint", Noun: "network endpoints"}
	if typ := vpcStr(o, "networkEndpointType"); typ != "SERVERLESS" && typ != "PRIVATE_SERVICE_CONNECT" {
		if m, err := p.call("POST", project, negColl(path)+"/listNetworkEndpoints", nil, nil); err == nil {
			for _, e := range computeItems(m) {
				ne, _ := e["networkEndpoint"].(map[string]any)
				health := "—"
				for _, h := range vmMaps(e, "healths") {
					health = vpcTitle(strings.ToLower(vpcStr(h, "healthState")))
				}
				eps.Items = append(eps.Items, console.Resource{Name: vpcOr(vpcOr(vpcStr(ne, "instance"), vpcStr(ne, "fqdn")), vpcStr(ne, "ipAddress")),
					Fields: map[string]string{"IP address": vpcDash(vpcStr(ne, "ipAddress")), "Port": vpcDash(vpcStr(ne, "port")), "Health": health}})
			}
		}
	}
	eps.Total = len(eps.Items)
	return console.Detail{Title: path[2], Heading: "Network endpoint group details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "endpoints", Label: "Network endpoints in this group", Listing: eps}}}, nil
}

func (p computeNEGsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 3 {
		return nil
	}
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete network endpoint group " + path[2] + "?", Leaves: true}}
}

func (p computeNEGsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 3 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, negColl(path), nil, nil)
	return err
}

// --- Classic certificates ------------------------------------------------------

// computeLBCertsProvider is Certificate Manager's Classic Certificates tab
// (load-balancing/docs/ssl-certificates): Compute Engine SSL certificates,
// Google-managed (issued here by this instance's certificate authority
// once a load balancer serves them) and self-managed.
type computeLBCertsProvider struct{ computeScreen }

func (computeLBCertsProvider) ID() string    { return "netservices-lb-certs" }
func (computeLBCertsProvider) Title() string { return "Certificate Manager" }

func (p computeLBCertsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Scope", "Domains", "Expires", "In use by", "Status"}, NameColumn: "Name", Noun: "certificates", AlwaysStatus: true,
		Note: "A Google-managed certificate is PROVISIONING until a load balancer's forwarding rule serves it; it is then issued by this instance's certificate authority, whose certificate is at /cloudburrow/lb/ca.pem on the Compute Engine API's port."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Certificates are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	proxies := append(p.lbAll(project, "targetHttpsProxies", true, true), p.lbAll(project, "targetSslProxies", true, false)...)
	for _, o := range p.lbAll(project, "sslCertificates", true, true) {
		path := lbScopePath(o)
		var users []string
		for _, px := range proxies {
			for _, c := range vpcStrs(px, "sslCertificates") {
				if lbRel(c) == lbRel(vpcStr(o, "selfLink")) {
					users = append(users, vpcStr(px, "name"))
				}
			}
		}
		status := "Active"
		typ := "Self-managed"
		if vpcStr(o, "type") == "MANAGED" {
			typ = "Google-managed"
			m, _ := o["managed"].(map[string]any)
			status = vpcTitle(strings.ToLower(vpcStr(m, "status")))
		}
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Status: status, Target: path,
			Fields: map[string]string{"Type": typ, "Scope": lbScopeText(path[0]), "Domains": vpcDash(strings.Join(vpcStrs(o, "subjectAlternativeNames"), ", ")),
				"Expires": vpcDash(vpcStr(o, "expireTime")), "In use by": vpcDash(strings.Join(users, ", ")), "Status": status},
			Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete certificate " + path[1] + "?"}}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeLBCertsProvider) CreateOnPage() bool   { return true }
func (computeLBCertsProvider) CreateTitle() string  { return "Create a classic SSL certificate" }
func (computeLBCertsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (computeLBCertsProvider) CreateForm() (string, []console.Field) {
	self := map[string]string{"type": "SELF_MANAGED"}
	return "Create", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "type", Label: "Create mode", Type: "radio", Default: "MANAGED", Options: []string{"MANAGED", "SELF_MANAGED"},
			OptionLabels: map[string]string{"MANAGED": "Create Google-managed certificate", "SELF_MANAGED": "Upload my certificate"}},
		{Name: "domains", Label: "Domains", Type: "text", VisibleWhen: map[string]string{"type": "MANAGED"}, Help: "Comma separated; no wildcards."},
		{Name: "region", Label: "Region", Type: "select", Default: "global", Options: append([]string{"global"}, computeRegionField("").Options...), VisibleWhen: self},
		{Name: "certificate", Label: "Certificate", Type: "textarea", VisibleWhen: self, Help: "PEM: the certificate, then its chain."},
		{Name: "privateKey", Label: "Private key", Type: "textarea", VisibleWhen: self, Help: "PEM: an RSA key of at least 2048 bits, or an ECDSA P-256 or P-384 key."},
	}
}

func (p computeLBCertsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	body := map[string]any{"name": name}
	scope := "global"
	if v["type"] == "SELF_MANAGED" {
		scope = vpcOr(v["region"], "global")
		body["type"], body["certificate"], body["privateKey"] = "SELF_MANAGED", v["certificate"], v["privateKey"]
	} else {
		body["type"], body["managed"] = "MANAGED", map[string]any{"domains": vpcList(v["domains"])}
	}
	if _, err := p.call("POST", project, lbScopeColl(scope, "sslCertificates"), nil, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeLBCertsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, lbScopeColl(path[0], "sslCertificates")+"/"+path[1], nil, nil)
	return err
}
