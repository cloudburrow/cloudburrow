package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Kubernetes Engine console parity (#1857).
//
// CloudBurrow serves no Kubernetes Engine API (container.googleapis.com, #1913):
// its one cluster is the instance's own kind cluster, and these screens read
// that cluster's real Kubernetes API through kubectl. The pages follow
// Google's (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/dashboards):
// Kubernetes clusters, Workloads, Gateways, Services & Ingress, Secrets &
// ConfigMaps, Storage (Persistent volume claims, Storage classes) and Object
// Browser. What would need the Kubernetes Engine API (creating, editing,
// upgrading or deleting a cluster) is drawn in Google's place and refused
// with its reason.

// gkeNoAPI is why a cluster cannot be created, edited, upgraded or deleted.
const gkeNoAPI = "Not available locally: CloudBurrow does not serve the Kubernetes Engine API (container.googleapis.com, #1913). " +
	"Its one cluster is this instance's own kind cluster, made by `cloudburrow up` and removed by `cloudburrow down`."

// gkeNoMonitoring is why an Observability tab is empty.
const gkeNoMonitoring = "Not available locally: GKE's Observability dashboards read Cloud Monitoring metrics, which CloudBurrow does not emulate. " +
	"A pod's page has Metrics charts read from the kubelet."

// gkeClusterScope is the query parameter the Kubernetes object screens carry
// their cluster in, and gkeSystemFilter the one for Kubernetes system objects.
const (
	gkeClusterScope = "cluster"
	gkeSystemFilter = "system"
)

// gkeSystemNamespace reports whether a namespace holds Kubernetes system
// objects, which Google's Workloads, Services and Secrets & ConfigMaps pages
// filter out by default ("By default, Kubernetes system objects are filtered
// out"). CloudBurrow's own components, in its namespace, are system objects
// of this instance's cluster; Cloud Run's services run in default and are not.
func gkeSystemNamespace(ns, own string) bool {
	switch {
	case ns == "kube-system", ns == "kube-public", ns == "kube-node-lease",
		ns == "local-path-storage", ns == "kourier-system", ns == "gmp-system", ns == "gke-managed-system":
		return true
	case strings.HasPrefix(ns, "knative-"), strings.HasPrefix(ns, "gke-"):
		return true
	case own != "" && ns == own:
		return true
	}
	return false
}

// gkeObjects is one of the Kubernetes object screens (Workloads, Services,
// Ingress, Secrets & ConfigMaps, Persistent volume claims): the cluster
// picker and the system-objects filter over a kubeProvider, with its detail
// regrouped into Google's tabs.
type gkeObjects struct {
	kubeProvider
	cluster, ownNamespace string
	// clusterColumn adds Google's Cluster column.
	tabs func(d console.Detail, item map[string]any) console.Detail
}

// gkeWorkloadsProvider is Workloads, the one Kubernetes object screen with a
// create: Deploy.
type gkeWorkloadsProvider struct{ gkeObjects }

func (p gkeObjects) ScopeKey() string     { return gkeClusterScope }
func (p gkeObjects) FilterKeys() []string { return []string{gkeSystemFilter} }

func (p gkeObjects) List(ctx context.Context, project string) (console.Listing, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, p.kind)
	if err != nil {
		return console.Listing{}, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Listing{}, fmt.Errorf("decode %s: %w", p.kind, err)
	}
	showSystem := console.ScopeValue(ctx, gkeSystemFilter) == "shown"
	items := make([]console.Resource, 0, len(list.Items))
	for _, raw := range list.Items {
		if !showSystem && gkeSystemNamespace(str(meta(raw), "namespace"), p.ownNamespace) {
			continue
		}
		r, ok := p.row(raw)
		if !ok {
			continue
		}
		if r.Fields == nil {
			r.Fields = map[string]string{}
		}
		r.Fields["Cluster"] = p.cluster
		items = append(items, r)
	}
	if p.enrich != nil {
		p.enrich(ctx, items)
	}
	columns := append(append([]string{}, p.columns...), "Cluster")
	return console.Listing{
		Columns: columns, Items: items, Total: len(items), AlwaysStatus: p.alwaysStatus,
		Scope: &console.Scope{Key: gkeClusterScope, Label: "Cluster", Selected: p.cluster, Default: p.cluster,
			Options: []console.ScopeOption{{ID: p.cluster, Label: p.cluster}}},
		Filters: []*console.Scope{{Key: gkeSystemFilter, Label: "Kubernetes system objects", Selected: map[bool]string{true: "shown", false: "hidden"}[showSystem],
			Default: "hidden", Options: []console.ScopeOption{{ID: "hidden", Label: "Hidden"}, {ID: "shown", Label: "Shown"}}}},
		Note: "Not scoped by project: these are the objects of this instance's own cluster, read from its Kubernetes API, " +
			"whatever project the toolbar has selected. Kubernetes system objects (kube-system and CloudBurrow's own components) are hidden " +
			"unless Kubernetes system objects is Shown.",
	}, nil
}

func (p gkeObjects) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	d, err := p.kubeProvider.Detail(ctx, project, path)
	if err != nil || d.Unavailable != "" || p.tabs == nil {
		return d, err
	}
	return p.tabs(d, nil), nil
}

// gkeRegroup puts a detail's sections in Google's tabs: plan names each
// section's ID with the label and the tab (Group) it goes on; a section the
// plan does not name keeps its place after them, so nothing becomes
// unreachable.
func gkeRegroup(d console.Detail, plan [][3]string, extra map[string]console.Section) console.Detail {
	byID := map[string]console.Section{}
	for _, s := range d.Sections {
		byID[s.ID] = s
	}
	for id, s := range extra {
		byID[id] = s
	}
	used := map[string]bool{}
	var out []console.Section
	for _, step := range plan {
		s, ok := byID[step[0]]
		if !ok {
			continue
		}
		if step[1] != "" {
			s.Label = step[1]
		}
		s.Group = step[2]
		used[step[0]] = true
		out = append(out, s)
	}
	for _, s := range d.Sections {
		if !used[s.ID] {
			out = append(out, s)
		}
	}
	d.Sections = out
	return d
}

func gkeObservability() console.Section {
	return console.Section{ID: "observability", Label: "Observability", Unavailable: gkeNoMonitoring}
}

func gkeLogs() console.Section {
	return console.Section{ID: "logs", Label: "Logs", Kind: console.KindLogs}
}

// gkeWorkloadTabs are Google's workload tabs: Overview (containers and
// managed pods), Details, Observability, Revision history, Events, Logs, YAML.
func gkeWorkloadTabs(d console.Detail, _ map[string]any) console.Detail {
	return gkeRegroup(d, [][3]string{
		{"containers", "Containers", "Overview"},
		{"pods", "Managed pods", "Overview"},
		{"configuration", "Details", ""},
		{"observability", "", ""},
		{"revisions", "", ""},
		{"events", "", ""},
		{"logs", "", ""},
		{"object", "", ""},
	}, map[string]console.Section{"observability": gkeObservability(), "logs": gkeLogs()})
}

// gkeServiceTabs are Google's Service tabs: Details (the Service's ports,
// routing and serving pods), Events, Logs, YAML.
func gkeServiceTabs(d console.Detail, _ map[string]any) console.Detail {
	return gkeRegroup(d, [][3]string{
		{"routing", "Details", "Details"},
		{"ports", "Ports", "Details"},
		{"endpoints", "Serving pods", "Details"},
		{"events", "", ""},
		{"logs", "", ""},
		{"object", "", ""},
	}, map[string]console.Section{"logs": gkeLogs()})
}

// gkeDetailsTabs are a configuration object's tabs: Details, Events, YAML.
func gkeDetailsTabs(d console.Detail, _ map[string]any) console.Detail {
	return gkeRegroup(d, [][3]string{{"details", "Details", ""}, {"events", "", ""}, {"object", "", ""}}, nil)
}

// gkeWorkloads is Google's Workloads page: Deployments, StatefulSets,
// DaemonSets, Jobs and CronJobs, the top-level objects only (not the
// ReplicaSets or Pods they own), with Name, Status, Type, Pods, Namespace
// and Cluster.
func gkeWorkloads(kubeconfig, cluster, own string) gkeWorkloadsProvider {
	base := workloadsProvider(kubeconfig)
	base.kind = "deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs"
	base.columns = []string{"Type", "Pods", "Namespace"}
	inner := base.row
	base.row = func(item map[string]any) (console.Resource, bool) {
		kind := str(item, "kind")
		if kind == "Job" || kind == "CronJob" {
			// A Job a CronJob made is the CronJob's run, not a workload of
			// its own, as a ReplicaSet is a Deployment's.
			if kind == "Job" && ownerKind(item) == "CronJob" {
				return console.Resource{}, false
			}
			return gkeJobRow(item), true
		}
		r, ok := inner(item)
		if !ok {
			return r, false
		}
		state := "OK"
		switch r.Status {
		case "Pending", "Updating":
			state = "Pods are pending"
		case "Scaled to zero":
			state = "Scaled to zero"
		}
		return console.Resource{Name: r.Name, Status: state, Fields: map[string]string{
			"Type": gkeKindLabel(kind), "Pods": r.Fields["Ready"], "Namespace": r.Fields["Namespace"],
		}}, true
	}
	inDetail := base.detail
	base.detail = func(item map[string]any) console.Detail {
		if k := str(item, "kind"); k == "Job" || k == "CronJob" {
			return jobDetail(item)
		}
		return inDetail(item)
	}
	return gkeWorkloadsProvider{gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own, tabs: gkeWorkloadTabs}}
}

// gkeKindLabel is the Type column's wording: Deployment, Stateful Set,
// Daemon Set, Job, Cron Job, as Google's Workloads page names them.
func gkeKindLabel(kind string) string {
	switch kind {
	case "StatefulSet":
		return "Stateful Set"
	case "DaemonSet":
		return "Daemon Set"
	case "ReplicaSet":
		return "Replica Set"
	case "CronJob":
		return "Cron Job"
	}
	return kind
}

func gkeJobRow(item map[string]any) console.Resource {
	m := meta(item)
	st := nested(item, "status")
	num := func(key string) int {
		if v, ok := st[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	state := "OK"
	pods := fmt.Sprintf("%d/%d", num("succeeded"), num("succeeded")+num("failed")+num("active"))
	switch {
	case str(item, "kind") == "CronJob":
		active, _ := st["active"].([]any)
		pods = fmt.Sprintf("%d active", len(active))
	case num("failed") > 0:
		state = "Failed"
	case num("active") > 0:
		state = "Running"
	}
	return console.Resource{Name: str(m, "name"), Status: state, Fields: map[string]string{
		"Type": gkeKindLabel(str(item, "kind")), "Pods": pods, "Namespace": str(m, "namespace"),
	}}
}

// CreateForm is the Workloads page's Deploy (Create a deployment): a
// container image, then the application's name, namespace and labels, as
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/stateless-apps
// describes it. Google's Deploy makes three replicas.
func (p gkeWorkloadsProvider) CreateForm() (string, []console.Field) {
	return "Deploy", []console.Field{
		{Name: "image", Label: "Image path", Type: "text", Required: true, Section: "Container",
			Help: "The container image, such as nginx:latest or a repository of this instance's Artifact Registry."},
		{Name: "name", Label: "Application name", Type: "text", Required: true, Section: "Configuration",
			Pattern: `^[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$`, Help: "Lowercase letters, digits and dashes, at most 63."},
		{Name: "namespace", Label: "Namespace", Type: "text", Default: "default", Section: "Configuration"},
		{Name: "replicas", Label: "Replicas", Type: "text", Default: "3", Section: "Configuration", Pattern: `^[0-9]+$`},
		{Name: "cluster", Label: "Kubernetes cluster", Type: "select", Options: []string{p.cluster}, Default: p.cluster, Section: "Configuration"},
	}
}

func (p gkeWorkloadsProvider) CreateOnPage() bool   { return true }
func (p gkeWorkloadsProvider) CreateTitle() string  { return "Create deployment" }
func (p gkeWorkloadsProvider) CreateLayout() string { return "page" }
func (p gkeWorkloadsProvider) CreateSubmit() string { return "Deploy" }

var gkeNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$`)

// Create deploys the Deployment through the cluster's own API (kubectl
// create deployment), labelled app=NAME as Google's Deploy labels it.
func (p gkeWorkloadsProvider) Create(ctx context.Context, _ string, v map[string]string) (string, error) {
	name, image := strings.TrimSpace(v["name"]), strings.TrimSpace(v["image"])
	ns := strings.TrimSpace(v["namespace"])
	if ns == "" {
		ns = "default"
	}
	if !gkeNamePattern.MatchString(name) {
		return "", fmt.Errorf("application name %q: lowercase letters, digits and dashes, starting and ending with a letter or digit, at most 63", name)
	}
	if !gkeNamePattern.MatchString(ns) {
		return "", fmt.Errorf("namespace %q is not a valid namespace name", ns)
	}
	if image == "" || strings.ContainsAny(image, " \t\n") {
		return "", fmt.Errorf("image path %q: a container image is required", image)
	}
	if gkeSystemNamespace(ns, p.ownNamespace) {
		return "", fmt.Errorf("namespace %s holds Kubernetes system objects; deploy to another namespace", ns)
	}
	replicas := 3
	if s := strings.TrimSpace(v["replicas"]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 100 {
			return "", fmt.Errorf("replicas %q: a whole number from 0 to 100", s)
		}
		replicas = n
	}
	if c := v["cluster"]; c != "" && c != p.cluster {
		return "", fmt.Errorf("no cluster named %s: this instance has one, %s", c, p.cluster)
	}
	if p.kubeconfig == "" {
		return "", errors.New("no kubeconfig: the cluster has not started")
	}
	if _, err := kubeRunner(p.kubeconfig, "").Do(ctx, "", "create", "deployment", name, "--image="+image,
		"--replicas="+strconv.Itoa(replicas), "-n", ns); err != nil {
		return "", fmt.Errorf("deploy %s: %w", name, kubectlCause(err))
	}
	return name, nil
}

// gkeServices is the Services tab of Gateways, Services & Ingress: Name,
// Status, Type, Endpoints, Pods, Namespace, Cluster.
func gkeServices(kubeconfig, cluster, own string) gkeObjects {
	base := servicesProvider(kubeconfig)
	base.title = "Services"
	base.columns = []string{"Type", "Endpoints", "Namespace"}
	base.alwaysStatus = true
	base.row = func(item map[string]any) (console.Resource, bool) {
		m := meta(item)
		spec := nested(item, "spec")
		endpoint := str(spec, "clusterIP")
		if ports := servicePorts(spec); endpoint != "" && endpoint != "None" && len(ports) > 0 {
			endpoint += ":" + strings.Join(ports, ", ")
		}
		return console.Resource{Name: str(m, "name"), Status: "OK", Fields: map[string]string{
			"Type": gkeServiceType(str(spec, "type")), "Endpoints": endpoint, "Namespace": str(m, "namespace"),
		}}, true
	}
	return gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own, tabs: gkeServiceTabs}
}

// gkeServiceType is a Service type as Google's list words it.
func gkeServiceType(t string) string {
	switch t {
	case "ClusterIP", "":
		return "Cluster IP"
	case "NodePort":
		return "Node Port"
	case "LoadBalancer":
		return "External load balancer"
	case "ExternalName":
		return "External name"
	}
	return t
}

// gkeIngress is the Ingress tab: Name, Status, Type, Frontends, Namespace,
// Cluster.
func gkeIngress(kubeconfig, cluster, own string) gkeObjects {
	base := kubeProvider{
		id: "k8singress", title: "Ingress", kind: "ingresses", kubeconfig: kubeconfig,
		columns: []string{"Type", "Frontends", "Namespace"}, alwaysStatus: true,
		detail: gkeIngressDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			return console.Resource{Name: str(m, "name"), Status: "OK", Fields: map[string]string{
				"Type": "Ingress", "Frontends": strings.Join(gkeIngressHosts(item), ", "), "Namespace": str(m, "namespace"),
			}}, true
		},
	}
	return gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own, tabs: gkeDetailsTabs}
}

func gkeIngressHosts(item map[string]any) []string {
	var hosts []string
	rules, _ := nested(item, "spec")["rules"].([]any)
	for _, r := range rules {
		if rm, ok := r.(map[string]any); ok {
			h := str(rm, "host")
			if h == "" {
				h = "*"
			}
			hosts = append(hosts, h)
		}
	}
	lbs, _ := nested(item, "status", "loadBalancer")["ingress"].([]any)
	for _, lb := range lbs {
		if lm, ok := lb.(map[string]any); ok {
			if ip := str(lm, "ip"); ip != "" {
				hosts = append(hosts, ip)
			} else if h := str(lm, "hostname"); h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

func gkeIngressDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")
	rules := console.Listing{Columns: []string{"Path", "Backend"}, NameColumn: "Host", Noun: "rules"}
	rs, _ := spec["rules"].([]any)
	for _, r := range rs {
		rm, _ := r.(map[string]any)
		host := str(rm, "host")
		if host == "" {
			host = "*"
		}
		paths, _ := nested(rm, "http")["paths"].([]any)
		for _, pth := range paths {
			pm, _ := pth.(map[string]any)
			svc := nested(pm, "backend", "service")
			port := nested(svc, "port")
			backend := str(svc, "name")
			if n, ok := port["number"].(float64); ok {
				backend += ":" + strconv.Itoa(int(n))
			} else if s := str(port, "name"); s != "" {
				backend += ":" + s
			}
			rules.Items = append(rules.Items, console.Resource{Name: host, Fields: map[string]string{"Path": str(pm, "path"), "Backend": backend}})
		}
	}
	rules.Total = len(rules.Items)
	return console.Detail{
		Summary: stored(
			console.Property{Label: "Namespace", Value: str(m, "namespace")},
			console.Property{Label: "Ingress class", Value: str(spec, "ingressClassName")},
			console.Property{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
		),
		Sections: []console.Section{{ID: "details", Label: "Details", Listing: rules}},
	}
}

// gkeGateways is the Gateways tab: the Gateway API's Gateways, when the
// cluster serves that API.
func gkeGateways(kubeconfig, cluster, own string) gkeGatewaysProvider {
	base := kubeProvider{
		id: "k8sgateways", title: "Gateways", kind: "gateways.gateway.networking.k8s.io", kubeconfig: kubeconfig,
		columns: []string{"Gateway class", "Addresses", "Namespace"}, alwaysStatus: true,
		detail: func(item map[string]any) console.Detail {
			return console.Detail{Summary: stored(console.Property{Label: "Namespace", Value: str(meta(item), "namespace")},
				console.Property{Label: "Gateway class", Value: str(nested(item, "spec"), "gatewayClassName")})}
		},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			var addrs []string
			as, _ := nested(item, "status")["addresses"].([]any)
			for _, a := range as {
				if am, ok := a.(map[string]any); ok {
					addrs = append(addrs, str(am, "value"))
				}
			}
			return console.Resource{Name: str(m, "name"), Status: "OK", Fields: map[string]string{
				"Gateway class": str(nested(item, "spec"), "gatewayClassName"), "Addresses": strings.Join(addrs, ", "),
				"Namespace": str(m, "namespace"),
			}}, true
		},
	}
	return gkeGatewaysProvider{gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own}}
}

// List reports a cluster with no Gateway API as such, rather than as a
// failure: kind installs none.
func (p gkeGatewaysProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l, err := p.gkeObjects.List(ctx, project)
	if err != nil && strings.Contains(err.Error(), "the server doesn't have a resource type") {
		return console.Listing{Columns: append(append([]string{}, p.columns...), "Cluster"),
			Unavailable: "Not available locally: this instance's cluster serves no Gateway API (gateway.networking.k8s.io), " +
				"which GKE installs and kind does not. Services and Ingress are on their tabs."}, nil
	}
	return l, err
}

type gkeGatewaysProvider struct{ gkeObjects }

// gkeConfig is Secrets & ConfigMaps: Name, Type, Namespace, Cluster. A
// Secret's values are never shown, only its keys, as in Google's.
func gkeConfig(kubeconfig, cluster, own string) gkeObjects {
	base := kubeProvider{
		id: "k8sconfig", title: "Secrets & ConfigMaps", kind: "secrets,configmaps", kubeconfig: kubeconfig,
		columns: []string{"Type", "Namespace"},
		detail:  gkeConfigDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			kind := str(item, "kind")
			if kind == "ConfigMap" && str(m, "name") == "kube-root-ca.crt" {
				// Kubernetes puts this ConfigMap in every namespace; it is
				// a system object wherever it is.
				return console.Resource{}, false
			}
			if kind == "Secret" && str(item, "type") == "kubernetes.io/service-account-token" {
				return console.Resource{}, false
			}
			typ := "Config Map"
			if kind == "Secret" {
				typ = "Secret: " + str(item, "type")
			}
			return console.Resource{Name: str(m, "name"), Fields: map[string]string{"Type": typ, "Namespace": str(m, "namespace")}}, true
		},
	}
	return gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own, tabs: gkeDetailsTabs}
}

func gkeConfigDetail(item map[string]any) console.Detail {
	m := meta(item)
	kind := str(item, "kind")
	keys := console.Listing{Columns: []string{"Value"}, NameColumn: "Key", Noun: "keys"}
	for _, field := range []string{"data", "binaryData"} {
		data, _ := item[field].(map[string]any)
		names := make([]string, 0, len(data))
		for k := range data {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			value := "Hidden"
			if kind == "ConfigMap" && field == "data" {
				value, _ = data[k].(string)
			} else if kind == "Secret" {
				value = "Hidden: Secret data is not shown"
			}
			keys.Items = append(keys.Items, console.Resource{Name: k, Fields: map[string]string{"Value": value}})
		}
	}
	keys.Total = len(keys.Items)
	if keys.Total == 0 {
		keys.Note = "This object holds no data."
	}
	typ := "Config Map"
	if kind == "Secret" {
		typ = "Secret: " + str(item, "type")
	}
	return console.Detail{
		Summary: stored(
			console.Property{Label: "Type", Value: typ},
			console.Property{Label: "Namespace", Value: str(m, "namespace")},
			console.Property{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
		),
		Sections: []console.Section{{ID: "details", Label: "Details", Listing: keys}},
	}
}

// gkeClaims is the Storage page's Persistent volume claims tab.
func gkeClaims(kubeconfig, cluster, own string) gkeObjects {
	base := clusterStorageProvider(kubeconfig)
	base.title = "Persistent volume claims"
	base.columns = []string{"Phase", "Volume", "Storage class", "Namespace"}
	inner := base.row
	base.row = func(item map[string]any) (console.Resource, bool) {
		r, ok := inner(item)
		if !ok {
			return r, false
		}
		r.Fields["Phase"] = r.Status
		return r, true
	}
	return gkeObjects{kubeProvider: base, cluster: cluster, ownNamespace: own}
}

// gkeStorageClasses is the Storage page's Storage classes tab: Name,
// Provisioner, Parameters, Reclaim policy.
func gkeStorageClasses(kubeconfig, cluster string) kubeProvider {
	return kubeProvider{
		id: "k8sstorageclasses", title: "Storage classes", kind: "storageclasses", kubeconfig: kubeconfig,
		columns: []string{"Provisioner", "Parameters", "Reclaim policy", "Volume binding mode", "Cluster"},
		detail: func(item map[string]any) console.Detail {
			return console.Detail{Summary: stored(
				console.Property{Label: "Provisioner", Value: str(item, "provisioner")},
				console.Property{Label: "Reclaim policy", Value: str(item, "reclaimPolicy")},
				console.Property{Label: "Volume binding mode", Value: str(item, "volumeBindingMode")},
			)}
		},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			params := sortedPairs(stringMap(item["parameters"]))
			parts := make([]string, 0, len(params))
			for _, kv := range params {
				parts = append(parts, kv.Label+": "+kv.Value)
			}
			name := str(m, "name")
			if v, _ := stringMap(m["annotations"])["storageclass.kubernetes.io/is-default-class"]; v == "true" {
				name += " (default)"
			}
			return console.Resource{Name: str(m, "name"), Fields: map[string]string{
				"Provisioner": str(item, "provisioner"), "Parameters": strings.Join(parts, ", "),
				"Reclaim policy": str(item, "reclaimPolicy"), "Volume binding mode": str(item, "volumeBindingMode"),
				"Cluster": cluster,
			}}, true
		},
	}
}

// --- Kubernetes clusters ---------------------------------------------

// gkeClustersProvider is the Kubernetes clusters page: this instance's one
// cluster, read from its nodes, with Google's columns. Nothing here invents
// GKE metadata the kind cluster does not have (a tier, a release channel):
// those columns say "—".
type gkeClustersProvider struct {
	name, kubeconfig, own string
	// svc is the Kubernetes Engine API (#1913), nil unless served, and
	// gkeDir where its clusters' kubeconfigs are.
	svc    *gkeService
	gkeDir string
}

func (p gkeClustersProvider) ID() string    { return "gkeclusters" }
func (p gkeClustersProvider) Title() string { return "Kubernetes clusters" }

type gkeClusterFacts struct {
	nodes       []map[string]any
	ready       int
	cpu         float64
	memory      int64
	version     string
	created     string
	unavailable string
}

func (p gkeClustersProvider) facts(ctx context.Context) gkeClusterFacts {
	var f gkeClusterFacts
	raw, err := kubectlJSON(ctx, p.kubeconfig, "", "nodes")
	if err != nil {
		f.unavailable = err.Error()
		return f
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		f.unavailable = "decode nodes: " + err.Error()
		return f
	}
	f.nodes = list.Items
	for _, n := range list.Items {
		if strings.HasPrefix(nodeStatus(n), "Ready") {
			f.ready++
		}
		capacity := stringMap(nested(n, "status")["capacity"])
		f.cpu += gkeCores(capacity["cpu"])
		f.memory += gkeBytes(capacity["memory"])
		if v := str(nested(n, "status", "nodeInfo"), "kubeletVersion"); v != "" && f.version == "" {
			f.version = v
		}
		if c := str(meta(n), "creationTimestamp"); f.created == "" || (c != "" && c < f.created) {
			f.created = c
		}
	}
	return f
}

func gkeMemory(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<30), 'f', 2, 64) + " GB"
}

// gkeCores reads a Kubernetes CPU quantity ("8", "500m") as cores.
func gkeCores(q string) float64 {
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "m") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(q, "m"), 64)
		return v / 1000
	}
	v, _ := strconv.ParseFloat(q, 64)
	return v
}

// gkeCoresText is a core count as Google's Total vCPUs shows it.
func gkeCoresText(c float64) string { return strconv.FormatFloat(c, 'f', -1, 64) }

// gkeBytes reads a Kubernetes memory quantity ("16336332Ki", "2Gi", "1G").
func gkeBytes(q string) int64 {
	q = strings.TrimSpace(q)
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}} {
		if strings.HasSuffix(q, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(q, u.suffix), 64)
			if err != nil {
				return 0
			}
			return int64(v * u.mult)
		}
	}
	v, _ := strconv.ParseFloat(q, 64)
	return int64(v)
}

func (f gkeClusterFacts) status() string {
	switch {
	case f.unavailable != "":
		return "Unavailable"
	case len(f.nodes) > 0 && f.ready == len(f.nodes):
		return "Running"
	case f.ready > 0:
		return "Degraded"
	}
	return "Error"
}

func (p gkeClustersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	f := p.facts(ctx)
	columns := []string{"Location", "Tier", "Mode", "Number of nodes", "Total vCPUs", "Total memory", "Notifications", "Labels"}
	row := console.Resource{Name: p.name, Status: f.status(), Fields: map[string]string{
		"Location": "local", "Tier": "—", "Mode": "kind (local)", "Number of nodes": strconv.Itoa(len(f.nodes)),
		"Total vCPUs": gkeCoresText(f.cpu), "Total memory": gkeMemory(f.memory), "Notifications": "—", "Labels": "—",
	}}
	if f.unavailable != "" {
		row.Fields["Number of nodes"] = "—"
		row.Fields["Total vCPUs"] = "—"
		row.Fields["Total memory"] = "—"
	}
	if p.gkeAPI() != nil {
		items := []console.Resource{row}
		for _, c := range p.served(ctx, project) {
			if c.GetName() != p.name {
				items = append(items, p.servedRow(c))
			}
		}
		return console.Listing{Columns: columns, Items: items, Total: len(items), AlwaysStatus: true, Noun: "clusters",
			Note: "The first cluster is this instance's own, a kind cluster made by `cloudburrow up`; the others are Kubernetes Engine " +
				"clusters of this project, each a kind cluster of its own, which Create cluster makes."}, nil
	}
	return console.Listing{
		Columns: columns, Items: []console.Resource{row}, Total: 1, AlwaysStatus: true, Noun: "clusters",
		Note: "This instance's own Kubernetes cluster, a kind cluster made by `cloudburrow up`. CloudBurrow does not serve the " +
			"Kubernetes Engine API (#1913), so it is the only cluster, and it cannot be created, edited, upgraded or deleted here.",
	}, nil
}

func (p gkeClustersProvider) CanDrill() bool { return true }

func (p gkeClustersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if c := p.servedCluster(ctx, project, path[0]); c != nil {
		return p.servedDetail(ctx, project, c), nil
	}
	if path[0] != p.name {
		return console.Detail{Unavailable: "no cluster named " + path[0] + ": this instance has one, " + p.name}, nil
	}
	f := p.facts(ctx)
	endpoint := gkeKubeServer(p.kubeconfig)
	basics := console.PropertyGroup{Heading: "Cluster basics", Properties: []console.Property{
		{Label: "Name", Value: p.name},
		{Label: "Location type", Value: "Local (this machine)"},
		{Label: "Control plane location", Value: "local"},
		{Label: "Release channel", Value: "—"},
		{Label: "Version", Value: unsetAs(f.version, "—")},
		{Label: "Total size", Value: strconv.Itoa(len(f.nodes))},
		{Label: "External endpoint", Value: unsetAs(endpoint, "—")},
		{Label: "Created", Value: unsetAs(shortTime(f.created), "—")},
	}}
	networking := console.PropertyGroup{Heading: "Networking", Properties: []console.Property{
		{Label: "Network", Value: "kind (Docker network \"kind\")"},
	}}
	details := console.Section{ID: "details", Label: "Details", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{basics, networking}}

	nodes := console.Listing{Columns: []string{"Roles", "CPU requested", "CPU allocatable", "Memory allocatable", "Storage allocatable", "Version"},
		NameColumn: "Name", Noun: "nodes", AlwaysStatus: true, Unavailable: f.unavailable}
	for _, n := range f.nodes {
		m := meta(n)
		alloc := stringMap(nested(n, "status")["allocatable"])
		nodes.Items = append(nodes.Items, console.Resource{
			Name: str(m, "name"), Status: nodeStatus(n), Link: "/kubernetes/nodes/" + str(m, "name"),
			Fields: map[string]string{
				"Roles": nodeRoles(m), "CPU requested": p.cpuRequested(ctx, str(m, "name")),
				"CPU allocatable": alloc["cpu"], "Memory allocatable": alloc["memory"],
				"Storage allocatable": alloc["ephemeral-storage"],
				"Version":             str(nested(n, "status", "nodeInfo"), "kubeletVersion"),
			},
		})
	}
	nodes.Total = len(nodes.Items)

	storage := console.Listing{Columns: []string{"Status", "Claim", "Capacity", "Storage class"}, NameColumn: "Persistent volume", Noun: "persistent volumes"}
	if raw, err := kubectlJSON(ctx, p.kubeconfig, "", "persistentvolumes"); err != nil {
		storage.Unavailable = err.Error()
	} else {
		var list struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.Unmarshal(raw, &list)
		for _, pv := range list.Items {
			claim := nested(pv, "spec", "claimRef")
			storage.Items = append(storage.Items, console.Resource{Name: str(meta(pv), "name"), Fields: map[string]string{
				"Status": str(nested(pv, "status"), "phase"), "Claim": strings.Trim(str(claim, "namespace")+"/"+str(claim, "name"), "/"),
				"Capacity": stringMap(nested(pv, "spec")["capacity"])["storage"], "Storage class": str(nested(pv, "spec"), "storageClassName"),
			}})
		}
		storage.Total = len(storage.Items)
		if storage.Total == 0 {
			storage.Note = "No persistent volume is provisioned. Claims and storage classes are on the Storage page."
		}
	}

	return console.Detail{
		Summary: stored(
			console.Property{Label: "Status", Value: f.status()},
			console.Property{Label: "Location", Value: "local"},
			console.Property{Label: "Version", Value: f.version},
		),
		Sections: []console.Section{
			details,
			{ID: "nodes", Label: "Nodes", Listing: nodes},
			{ID: "storage", Label: "Storage", Listing: storage},
			gkeObservability(),
			gkeLogs(),
		},
	}, nil
}

// cpuRequested sums the CPU requests of the pods on a node, Google's Nodes
// tab's "CPU requested".
func (p gkeClustersProvider) cpuRequested(ctx context.Context, node string) string {
	raw, err := kubeRunner(p.kubeconfig, "").Do(ctx, "", "get", "pods", "--all-namespaces", "-o", "json",
		"--field-selector=spec.nodeName="+node)
	if err != nil {
		return "—"
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return "—"
	}
	var total float64
	for _, pod := range list.Items {
		cs, _ := nested(pod, "spec")["containers"].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			total += gkeCores(stringMap(nested(cm, "resources", "requests"))["cpu"])
		}
	}
	return gkeCoresText(total) + " CPU"
}

// gkeKubeServer is the API server address the instance's kubeconfig names.
func gkeKubeServer(kubeconfig string) string {
	if kubeconfig == "" {
		return ""
	}
	b, err := os.ReadFile(kubeconfig)
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`(?m)^\s*server:\s*(\S+)`).FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// gkeConnectCommands is Connect's "Command-line access". Google's is
// `gcloud container clusters get-credentials`, which calls the Kubernetes
// Engine API this instance does not serve (#1913); the local equivalent
// points kubectl at the instance's own kubeconfig.
func (p gkeClustersProvider) gkeConnectCommands() string {
	return "export KUBECONFIG=" + p.kubeconfig + "\nkubectl config use-context kind-" + p.name + "\nkubectl get nodes"
}

func (p gkeClustersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) == 1 {
		if c := p.servedCluster(ctx, project, path[0]); c != nil {
			return p.servedActions(ctx, project, c)
		}
	}
	if len(path) != 1 || path[0] != p.name {
		return nil
	}
	refusal := gkeNoAPI
	if p.gkeAPI() != nil {
		refusal = gkeOwnClusterNote
	}
	return []console.Action{
		{ID: "connect", Label: "Connect", Submit: "OK", Fields: []console.Field{
			{Name: "command", Label: "Command-line access", Type: "textarea", Immutable: true, Default: p.gkeConnectCommands(),
				Help: "Google's Connect gives `gcloud container clusters get-credentials " + p.name + "`, which reads the Kubernetes Engine API; " +
					"CloudBurrow does not serve it (#1913), so point kubectl at this instance's kubeconfig instead. The API server is " +
					unsetAs(gkeKubeServer(p.kubeconfig), "not started") + "."},
		}},
		{ID: "edit", Label: "Edit", Unavailable: refusal},
		{ID: "upgrade", Label: "Upgrade", Unavailable: refusal},
		{ID: "delete", Label: "Delete", Destructive: true, Unavailable: refusal},
	}
}

func (p gkeClustersProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) == 1 {
		if c := p.servedCluster(ctx, project, path[0]); c != nil {
			return p.servedAct(ctx, project, c, action, v)
		}
	}
	if len(path) != 1 || path[0] != p.name {
		return fmt.Errorf("no cluster named %s", strings.Join(path, "/"))
	}
	if action == "connect" {
		return nil
	}
	if p.gkeAPI() != nil {
		return errors.New(gkeOwnClusterNote)
	}
	return errors.New(gkeNoAPI)
}

// CreateForm is Create cluster: Google's Autopilot or Standard choice, then
// Cluster basics, Fleet registration, Networking and, for Standard, the
// default-pool node pool. Both modes are drawn and refused with the reason.
func (p gkeClustersProvider) CreateForm() (string, []console.Field) {
	var refused map[string]string
	if p.gkeAPI() == nil {
		refused = map[string]string{"Autopilot": gkeNoAPI, "Standard": gkeNoAPI}
	}
	return "Create", []console.Field{
		{Name: "mode", Label: "Cluster mode", Type: "radio", Options: []string{"Autopilot", "Standard"}, Default: "Autopilot",
			Section: "Cluster mode", UnavailableOptions: refused,
			OptionLabels: map[string]string{"Autopilot": "Autopilot: Google manages your cluster (Recommended)", "Standard": "Standard: You manage your cluster"}},
		{Name: "name", Label: "Name", Type: "text", Default: "autopilot-cluster-1", Section: "Cluster basics", Pattern: `^[a-z]([a-z0-9\-]{0,38}[a-z0-9])?$`},
		{Name: "locationType", Label: "Location type", Type: "radio", Options: []string{"Regional", "Zonal"}, Default: "Regional", Section: "Cluster basics"},
		{Name: "region", Label: "Region", Type: "text", Default: "us-central1", Section: "Cluster basics", ShowWhen: "locationType=Regional"},
		{Name: "zone", Label: "Zone", Type: "text", Default: "us-central1-a", Section: "Cluster basics", ShowWhen: "locationType=Zonal"},
		{Name: "channel", Label: "Release channel", Type: "select", Options: []string{"Rapid channel", "Regular channel", "Stable channel", "Extended channel"}, Default: "Regular channel", Section: "Cluster basics"},
		{Name: "version", Label: "Version", Type: "text", Section: "Cluster basics", Help: "The control plane version; the channel's default when empty."},
		{Name: "fleet", Label: "Register to the fleet of this project", Type: "checkbox", Section: "Fleet registration",
			Unavailable: "Not available locally: fleets are the GKE Hub API's, which CloudBurrow does not serve."},
		{Name: "network", Label: "Network", Type: "text", Default: "default", Section: "Networking"},
		{Name: "subnet", Label: "Node subnet", Type: "text", Default: "default", Section: "Networking"},
		{Name: "nodes", Label: "Number of nodes", Type: "text", Default: "3", Section: "Node pools: default-pool", ShowWhen: "mode=Standard", Pattern: `^[0-9]+$`},
		{Name: "machine", Label: "Machine type", Type: "text", Default: "e2-medium", Section: "Node pools: default-pool", ShowWhen: "mode=Standard"},
		{Name: "clientCert", Label: "Issue a client certificate", Type: "checkbox", Section: "Security",
			Help: "The legacy admin client certificate, in the cluster's masterAuth: what kubectl authenticates with locally, since gke-gcloud-auth-plugin mints Google tokens."},
	}
}

func (p gkeClustersProvider) CreateOnPage() bool   { return true }
func (p gkeClustersProvider) CreateTitle() string  { return "Create cluster" }
func (p gkeClustersProvider) CreateLayout() string { return "page" }

func (p gkeClustersProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if p.gkeAPI() != nil {
		return p.servedCreate(ctx, project, v)
	}
	return "", errors.New(gkeNoAPI)
}

// --- Object Browser ---------------------------------------------------

// gkeObjectBrowser is Object Browser: every listable resource type of the
// cluster by API group, each opening its objects, each object its YAML.
type gkeObjectBrowser struct{ kubeconfig string }

func (p gkeObjectBrowser) ID() string    { return "k8sobjects" }
func (p gkeObjectBrowser) Title() string { return "Object Browser" }

type gkeAPIResource struct{ name, group, kind string }

func (p gkeObjectBrowser) resources(ctx context.Context) ([]gkeAPIResource, error) {
	if p.kubeconfig == "" {
		return nil, errors.New("no kubeconfig: the cluster has not started")
	}
	out, err := kubeRunner(p.kubeconfig, "").Do(ctx, "", "api-resources", "--verbs=list", "-o", "wide", "--no-headers=false")
	if err != nil {
		return nil, fmt.Errorf("kubectl api-resources: %w", kubectlCause(err))
	}
	return parseAPIResources(out), nil
}

// parseAPIResources reads `kubectl api-resources -o wide`, whose columns
// are NAME SHORTNAMES APIVERSION NAMESPACED KIND VERBS ...; SHORTNAMES may be
// empty, so the columns are found from the header's offsets.
func parseAPIResources(out string) []gkeAPIResource {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		return nil
	}
	head := lines[0]
	col := func(name string) int { return strings.Index(head, name) }
	at := func(line string, from, to int) string {
		if from < 0 || from >= len(line) {
			return ""
		}
		if to < 0 || to > len(line) {
			to = len(line)
		}
		return strings.TrimSpace(line[from:to])
	}
	ver, ns, kind, verbs := col("APIVERSION"), col("NAMESPACED"), col("KIND"), col("VERBS")
	var out2 []gkeAPIResource
	for _, l := range lines[1:] {
		name := strings.Fields(l)
		if len(name) == 0 {
			continue
		}
		group := at(l, ver, ns)
		if i := strings.LastIndex(group, "/"); i >= 0 {
			group = group[:i]
		} else {
			group = "core"
		}
		out2 = append(out2, gkeAPIResource{name: name[0], group: group, kind: at(l, kind, verbs)})
	}
	return out2
}

func (r gkeAPIResource) qualified() string {
	if r.group == "core" {
		return r.name
	}
	return r.name + "." + r.group
}

func (p gkeObjectBrowser) List(ctx context.Context, _ string) (console.Listing, error) {
	rs, err := p.resources(ctx)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"API group", "Kind"}, NameColumn: "Resource type", Noun: "resource types", RowsOpenable: true,
		Note: "Every resource type this instance's cluster lists, by API group. Open one for its objects, and an object for its YAML."}
	for _, r := range rs {
		l.Items = append(l.Items, console.Resource{Name: r.qualified(), Fields: map[string]string{"API group": r.group, "Kind": r.kind}})
	}
	sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].Fields["API group"] < l.Items[j].Fields["API group"] })
	l.Total = len(l.Items)
	return l, nil
}

func (p gkeObjectBrowser) CanDrill() bool { return true }

var gkeResourceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.\-]*[a-z0-9])?$`)

func (p gkeObjectBrowser) Detail(ctx context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	typ := path[0]
	if !gkeResourceName.MatchString(typ) {
		return console.Detail{Unavailable: "no resource type " + typ}, nil
	}
	raw, err := kubectlJSON(ctx, p.kubeconfig, "", typ)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return console.Detail{Unavailable: "decode " + typ + ": " + err.Error()}, nil
	}
	if len(path) == 1 {
		objs := console.Listing{Columns: []string{"Namespace", "Age"}, NameColumn: "Name", Noun: "objects", RowsOpenable: true}
		for _, it := range list.Items {
			m := meta(it)
			id := str(m, "name")
			if ns := str(m, "namespace"); ns != "" {
				id = ns + "/" + id
			}
			objs.Items = append(objs.Items, console.Resource{Name: id, Fields: map[string]string{
				"Namespace": str(m, "namespace"), "Age": shortAge(str(m, "creationTimestamp"))}})
		}
		objs.Total = len(objs.Items)
		return console.Detail{Title: typ, Sections: []console.Section{{ID: "objects", Label: "Objects", Listing: objs}}}, nil
	}
	for _, it := range list.Items {
		m := meta(it)
		id := str(m, "name")
		if ns := str(m, "namespace"); ns != "" {
			id = ns + "/" + id
		}
		if id == path[1] {
			return console.Detail{Title: path[1], Summary: stored(
				console.Property{Label: "Kind", Value: str(it, "kind")},
				console.Property{Label: "Namespace", Value: str(m, "namespace")},
				console.Property{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
			), Sections: []console.Section{objectSection(it)}}, nil
		}
	}
	return console.Detail{Unavailable: "no " + typ + " named " + path[1]}, nil
}
