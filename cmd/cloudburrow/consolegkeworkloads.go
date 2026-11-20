package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A workload's actions on the Workloads page and a workload's page, as
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/stateless-apps
// and the Workloads dashboard describe them (#1857): Actions > Scale > Edit
// replicas, Autoscale, Expose and Rolling update, Delete, and the YAML
// tab's Edit. Each runs against the instance's own cluster through kubectl,
// as Google's console calls the cluster's Kubernetes API.

// gkeWorkloadKinds are the top-level kinds the Workloads page lists, as
// kubectl names them.
const gkeWorkloadKinds = "deployments,statefulsets,daemonsets,jobs,cronjobs"

// gkeWorkload is one workload found by name: its object, kind and
// namespace.
type gkeWorkload struct {
	item      map[string]any
	kind, ns  string
	name      string
	resource  string // kind/name, as kubectl takes it
	scalable  bool
	rollable  bool
	exposable bool
}

func gkeWorkloadOf(item map[string]any) gkeWorkload {
	m := meta(item)
	w := gkeWorkload{item: item, kind: str(item, "kind"), ns: str(m, "namespace"), name: str(m, "name")}
	w.resource = strings.ToLower(w.kind) + "/" + w.name
	switch w.kind {
	case "Deployment", "StatefulSet":
		w.scalable, w.rollable, w.exposable = true, true, true
	case "DaemonSet":
		w.rollable, w.exposable = true, true
	}
	return w
}

// find reads the workload named name: the first top-level object of that
// name, as the workload's page shows it.
func (p gkeWorkloadsProvider) find(ctx context.Context, name string) (gkeWorkload, error) {
	raw, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, gkeWorkloadKinds)
	if err != nil {
		return gkeWorkload{}, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return gkeWorkload{}, fmt.Errorf("decode workloads: %w", err)
	}
	for _, it := range list.Items {
		if str(meta(it), "name") != name {
			continue
		}
		if str(it, "kind") == "Job" && ownerKind(it) == "CronJob" {
			continue
		}
		return gkeWorkloadOf(it), nil
	}
	return gkeWorkload{}, fmt.Errorf("no workload named %s", name)
}

// gkeContainers are a workload's containers' names and images.
func gkeContainers(item map[string]any) [][2]string {
	spec := nested(item, "spec", "template", "spec")
	if str(item, "kind") == "CronJob" {
		spec = nested(item, "spec", "jobTemplate", "spec", "template", "spec")
	}
	cs, _ := spec["containers"].([]any)
	var out [][2]string
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		out = append(out, [2]string{str(cm, "name"), str(cm, "image")})
	}
	return out
}

func gkeIntOr(v any, def string) string {
	switch x := v.(type) {
	case float64:
		return strconv.Itoa(int(x))
	case string:
		return x
	}
	return def
}

// actions are what Google's console offers on this workload: Scale >
// Edit replicas and Autoscale for a Deployment or Stateful Set, Expose and
// Rolling update for those and a Daemon Set, and Delete for every one.
func (w gkeWorkload) actions(system bool) []console.Action {
	if system {
		why := "Namespace " + w.ns + " holds Kubernetes system objects, which are not changed from the console."
		return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, Unavailable: why}}
	}
	var out []console.Action
	spec := nested(w.item, "spec")
	if w.scalable {
		out = append(out,
			console.Action{ID: "scale", Label: "Edit replicas", Title: "Scale", Submit: "Scale", Fields: []console.Field{
				{Name: "replicas", Label: "Replicas", Type: "text", Pattern: `^[0-9]+$`, Default: gkeIntOr(spec["replicas"], "1"), Required: true},
			}},
			console.Action{ID: "autoscale", Label: "Autoscale", Submit: "Save", Fields: []console.Field{
				{Name: "min", Label: "Minimum number of replicas", Type: "text", Pattern: `^[0-9]+$`, Default: "1", Required: true},
				{Name: "max", Label: "Maximum number of replicas", Type: "text", Pattern: `^[0-9]+$`, Default: "3", Required: true},
				{Name: "cpu", Label: "CPU utilization target (%)", Type: "text", Pattern: `^[0-9]+$`, Default: "80",
					Help: "A HorizontalPodAutoscaler on CPU; it scales once the cluster serves resource metrics (metrics-server)."},
			}})
	}
	if w.exposable {
		out = append(out, console.Action{ID: "expose", Label: "Expose", Title: "Expose", Submit: "Expose", Fields: []console.Field{
			{Name: "port", Label: "Port", Type: "text", Pattern: `^[0-9]+$`, Default: "80", Required: true, Section: "Port mapping"},
			{Name: "targetPort", Label: "Target port", Type: "text", Pattern: `^[0-9]+$`, Default: "80", Required: true, Section: "Port mapping"},
			{Name: "protocol", Label: "Protocol", Type: "select", Options: []string{"TCP", "UDP"}, Default: "TCP", Section: "Port mapping"},
			{Name: "type", Label: "Service type", Type: "select", Options: []string{"Cluster IP", "Node port", "Load balancer"}, Default: "Load balancer",
				Help: "A load balancer's external IP is the kind cluster's own: reachable from this machine through `kubectl port-forward`."},
			{Name: "service", Label: "Service name", Type: "text", Default: w.name + "-service", Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
		}})
	}
	if w.rollable {
		fields := []console.Field{}
		for _, c := range gkeContainers(w.item) {
			fields = append(fields, console.Field{Name: "image-" + c[0], Label: "Image of container " + c[0], Type: "text", Default: c[1], Section: "Container images"})
		}
		if w.kind == "Deployment" {
			ru := nested(w.item, "spec", "strategy", "rollingUpdate")
			fields = append(fields,
				console.Field{Name: "minReady", Label: "Minimum seconds ready", Type: "text", Pattern: `^[0-9]+$`, Default: gkeIntOr(spec["minReadySeconds"], "0"), Section: "Update strategy"},
				console.Field{Name: "maxSurge", Label: "Maximum surge", Type: "text", Pattern: `^[0-9]+%?$`, Default: gkeIntOr(ru["maxSurge"], "25%"), Section: "Update strategy"},
				console.Field{Name: "maxUnavailable", Label: "Maximum unavailable", Type: "text", Pattern: `^[0-9]+%?$`, Default: gkeIntOr(ru["maxUnavailable"], "25%"), Section: "Update strategy"},
			)
		}
		out = append(out, console.Action{ID: "rollingupdate", Label: "Rolling update", Submit: "Update", Fields: fields})
	}
	return append(out, gkeWorkloadDelete(false))
}

// gkeWorkloadDelete is a workload's Delete, on its page and in its row's
// menu, or (selected) the Workloads list's Delete for the checked ones.
// From https://docs.cloud.google.com/kubernetes-engine/docs/how-to/stateless-apps
// (re-read 2026-11-20): "In the workloads list, select one or more
// Deployments to delete. Click Delete. When prompted to confirm, click
// Delete." Nothing is typed back.
func gkeWorkloadDelete(selected bool) console.Action {
	if selected {
		return console.Action{ID: gkeDeleteSelected, Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
			SelectionField: "workloads", Fields: []console.Field{{Name: "workloads", Label: "Workloads", Type: "hidden"}}}
	}
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Leaves: true}
}

// gkeDeleteSelected is the Workloads list's Delete over the checked rows.
const gkeDeleteSelected = "delete-selected"

// ListSelectActions is the Workloads list's Delete for the checked
// workloads (#2070), as Google's list has "select one or more Deployments
// to delete. Click Delete."
func (p gkeWorkloadsProvider) ListSelectActions(context.Context, string) []console.Action {
	return []console.Action{gkeWorkloadDelete(true)}
}

// List is the Workloads list, its rows named workloads, so the list's
// Delete asks "Delete 2 workloads?".
func (p gkeWorkloadsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l, err := p.gkeObjects.List(ctx, project)
	l.Noun = "workloads"
	return l, err
}

// deleteSelected deletes the checked workloads, one name per line. A
// Kubernetes system object among them refuses the whole selection before
// anything is deleted, naming it with the reason the row's Delete gives.
func (p gkeWorkloadsProvider) deleteSelected(ctx context.Context, names string) error {
	var found []gkeWorkload
	seen := map[string]bool{}
	for _, n := range strings.Split(names, "\n") {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		w, err := p.find(ctx, n)
		if err != nil {
			return err
		}
		if p.system(w) {
			return fmt.Errorf("%s: namespace %s holds Kubernetes system objects, which are not changed from the console; nothing was deleted", w.name, w.ns)
		}
		found = append(found, w)
	}
	if len(found) == 0 {
		return errors.New("select the workloads to delete")
	}
	kube := kubeRunner(p.kubeconfig, "")
	for _, w := range found {
		if _, err := kube.Do(ctx, "", "delete", w.resource, "-n", w.ns); err != nil {
			return fmt.Errorf("%s: %w", w.name, kubectlCause(err))
		}
	}
	return nil
}

// yamlAction is the YAML tab's Edit: the object as kubectl prints it,
// saved with kubectl replace.
func (p gkeWorkloadsProvider) yamlAction(ctx context.Context, w gkeWorkload) (console.Action, bool) {
	out, err := kubeRunner(p.kubeconfig, "").Do(ctx, "", "get", w.resource, "-n", w.ns, "-o", "yaml")
	if err != nil {
		return console.Action{}, false
	}
	// Without its resourceVersion, which the controllers move on while the
	// form is open (a Deployment's status), the copy replaces the object
	// whole, as kubectl edit's retry does.
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  resourceVersion:") {
			keep = append(keep, line)
		}
	}
	return console.Action{ID: "edityaml", Label: "Edit", Title: "Edit YAML", Submit: "Save", Fields: []console.Field{
		{Name: "yaml", Label: "YAML", Type: "textarea", Default: strings.Join(keep, "\n"), Required: true,
			Help: "Saved with kubectl replace, in the workload's namespace."},
	}}, true
}

func (p gkeWorkloadsProvider) system(w gkeWorkload) bool {
	return gkeSystemNamespace(w.ns, p.ownNamespace)
}

// Detail is the workload's page with the YAML tab's Edit.
func (p gkeWorkloadsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	d, err := p.gkeObjects.Detail(ctx, project, path)
	if err != nil || d.Unavailable != "" || len(path) != 1 {
		return d, err
	}
	w, err := p.find(ctx, path[0])
	if err != nil {
		return d, nil
	}
	d = p.withUtilization(w, d)
	if p.system(w) {
		return d, nil
	}
	if a, ok := p.yamlAction(ctx, w); ok {
		for i := range d.Sections {
			if d.Sections[i].ID == "object" {
				d.Sections[i].Actions = []console.Action{a}
			}
		}
	}
	return d, nil
}

func (p gkeWorkloadsProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	w, err := p.find(ctx, path[0])
	if err != nil {
		return nil
	}
	out := w.actions(p.system(w))
	if !p.system(w) {
		if a, ok := p.yamlAction(ctx, w); ok {
			a.Linked = true // drawn on the YAML tab, not beside the title
			out = append(out, a)
		}
	}
	return out
}

func (p gkeWorkloadsProvider) ActAt(ctx context.Context, _ string, path []string, action string, v map[string]string) error {
	if len(path) == 0 && action == gkeDeleteSelected {
		return p.deleteSelected(ctx, v["workloads"])
	}
	if len(path) != 1 {
		return fmt.Errorf("no workload at %s", strings.Join(path, "/"))
	}
	w, err := p.find(ctx, path[0])
	if err != nil {
		return err
	}
	if p.system(w) {
		return fmt.Errorf("namespace %s holds Kubernetes system objects, which are not changed from the console", w.ns)
	}
	offered := false
	for _, a := range w.actions(false) {
		offered = offered || a.ID == action
	}
	if !offered && action != "edityaml" {
		return fmt.Errorf("%s is not offered on a %s", action, gkeKindLabel(w.kind))
	}
	kube := kubeRunner(p.kubeconfig, "")
	run := func(args ...string) error {
		if _, err := kube.Do(ctx, "", append(args, "-n", w.ns)...); err != nil {
			return kubectlCause(err)
		}
		return nil
	}
	num := func(key, label string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(v[key]))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s %q is not a whole number", label, v[key])
		}
		return n, nil
	}
	switch action {
	case "scale":
		n, err := num("replicas", "replicas")
		if err != nil {
			return err
		}
		return run("scale", w.resource, "--replicas="+strconv.Itoa(n))
	case "autoscale":
		lo, err := num("min", "minimum number of replicas")
		if err != nil {
			return err
		}
		hi, err := num("max", "maximum number of replicas")
		if err != nil {
			return err
		}
		if lo < 1 || hi < lo {
			return fmt.Errorf("the maximum (%d) must be at least the minimum (%d), which must be at least 1", hi, lo)
		}
		args := []string{"autoscale", w.resource, "--min=" + strconv.Itoa(lo), "--max=" + strconv.Itoa(hi)}
		if strings.TrimSpace(v["cpu"]) != "" {
			cpu, err := num("cpu", "CPU utilization target")
			if err != nil {
				return err
			}
			args = append(args, "--cpu-percent="+strconv.Itoa(cpu))
		}
		return run(args...)
	case "expose":
		port, err := num("port", "port")
		if err != nil {
			return err
		}
		target, err := num("targetPort", "target port")
		if err != nil {
			return err
		}
		typ := map[string]string{"Cluster IP": "ClusterIP", "Node port": "NodePort", "Load balancer": "LoadBalancer"}[v["type"]]
		if typ == "" {
			typ = "ClusterIP"
		}
		proto := v["protocol"]
		if proto != "UDP" {
			proto = "TCP"
		}
		args := []string{"expose", w.resource, "--port=" + strconv.Itoa(port), "--target-port=" + strconv.Itoa(target),
			"--protocol=" + proto, "--type=" + typ}
		if s := strings.TrimSpace(v["service"]); s != "" {
			if !gkeNamePattern.MatchString(s) {
				return fmt.Errorf("service name %q: lowercase letters, digits and dashes, at most 63", s)
			}
			args = append(args, "--name="+s)
		}
		return run(args...)
	case "rollingupdate":
		var images []string
		for _, c := range gkeContainers(w.item) {
			if img := strings.TrimSpace(v["image-"+c[0]]); img != "" && img != c[1] {
				if strings.ContainsAny(img, " \t\n") {
					return fmt.Errorf("image %q of container %s is not an image path", img, c[0])
				}
				images = append(images, c[0]+"="+img)
			}
		}
		if w.kind == "Deployment" {
			patch := map[string]any{}
			if s := strings.TrimSpace(v["minReady"]); s != "" {
				n, err := num("minReady", "minimum seconds ready")
				if err != nil {
					return err
				}
				patch["minReadySeconds"] = n
			}
			ru := map[string]any{}
			for _, k := range []string{"maxSurge", "maxUnavailable"} {
				if s := strings.TrimSpace(v[k]); s != "" {
					if n, err := strconv.Atoi(s); err == nil {
						ru[k] = n
					} else {
						ru[k] = s
					}
				}
			}
			if len(ru) > 0 {
				patch["strategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": ru}
			}
			if len(patch) > 0 {
				b, _ := json.Marshal(map[string]any{"spec": patch})
				if err := run("patch", w.resource, "--type", "merge", "-p", string(b)); err != nil {
					return err
				}
			}
		}
		if len(images) > 0 {
			return run(append([]string{"set", "image", w.resource}, images...)...)
		}
		return nil
	case "delete":
		return run("delete", w.resource)
	case "edityaml":
		text := v["yaml"]
		if strings.TrimSpace(text) == "" {
			return errors.New("the YAML is empty")
		}
		if _, err := kube.Do(ctx, text, "replace", "-n", w.ns, "-f", "-"); err != nil {
			return kubectlCause(err)
		}
		return nil
	}
	return fmt.Errorf("unknown action %s", action)
}

// withSeries gives Workloads the retained kubelet readings its Overview
// tab charts (#2070), the ones a pod's Metrics charts read.
func (p gkeWorkloadsProvider) withSeries(series *console.Series) gkeWorkloadsProvider {
	p.series = series
	return p
}

// gkeNoSeries is why a workload's utilization has no chart.
const gkeNoSeries = "Not available: this instance keeps no kubelet readings (the console's metrics sampler is not running)."

// withUtilization puts Google's resource utilization on a workload's
// Overview tab (#2070): "high level details about the workload, including
// its resource utilization" (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/dashboards,
// re-read 2026-11-20). CPU, memory and disk are the kubelet's readings of
// the workload's managed pods, summed: the same history a pod's Metrics
// charts draw, never estimated.
func (p gkeWorkloadsProvider) withUtilization(w gkeWorkload, d console.Detail) console.Detail {
	var pods []string
	for _, s := range d.Sections {
		if s.ID == "pods" {
			for _, it := range s.Listing.Items {
				pods = append(pods, w.ns+"/"+it.Name)
			}
		}
	}
	sec := console.Section{ID: "utilization", Label: "Utilization", Kind: console.KindChart,
		Series: gkeUtilization(p.series, pods),
		Note: "The sum over the workload's managed pods, as the kubelet measured them, retained in memory only, " +
			"so a restart of CloudBurrow begins again from nothing. A break in a line is a sample in which a pod had no reading. " +
			"Disk is the pods' ephemeral storage: their containers' writable layers, logs and emptyDir volumes."}
	out := make([]console.Section, 0, len(d.Sections)+1)
	placed := false
	for _, s := range d.Sections {
		if !placed && s.Group == "Overview" {
			sec.Group = "Overview"
			out = append(out, sec)
			placed = true
		}
		out = append(out, s)
	}
	if !placed {
		// A workload with no Overview tab of its own (a Job's page) keeps
		// its tabs; the charts lead them.
		out = append([]console.Section{sec}, out...)
	}
	d.Sections = out
	return d
}

// gkeUtilization is the CPU, Memory and Disk series of the pods named
// (namespace/name), summed per sample. A sample where one of the pods
// reported has a point only if every pod it reported has a reading of that
// metric, so a sum is never a part of the pods passed off as all of them;
// CPU is each pod's rate between its consecutive counters, as podCPURate.
func gkeUtilization(series *console.Series, pods []string) []console.ChartSeries {
	cpu := console.ChartSeries{Label: "CPU", Unit: "cores"}
	memory := console.ChartSeries{Label: "Memory", Unit: "bytes"}
	disk := console.ChartSeries{Label: "Disk", Unit: "bytes"}
	switch {
	case series == nil:
		cpu.Unavailable, memory.Unavailable, disk.Unavailable = gkeNoSeries, gkeNoSeries, gkeNoSeries
		return []console.ChartSeries{cpu, memory, disk}
	case len(pods) == 0:
		why := "Not available: this workload has no managed pods to measure."
		cpu.Unavailable, memory.Unavailable, disk.Unavailable = why, why, why
		return []console.ChartSeries{cpu, memory, disk}
	}
	want := map[string]bool{}
	for _, k := range pods {
		want[k] = true
	}
	samples, _, _ := series.Window()
	prev := map[string]*console.PodMetrics{}
	anyDisk := false
	for _, s := range samples {
		at := s.At.UTC().Format(time.RFC3339)
		var c, m, dk float64
		seen, cOK, mOK, dOK := 0, true, true, true
		now := map[string]*console.PodMetrics{}
		for i := range s.Pods {
			r := s.Pods[i]
			if !want[r.Key()] {
				continue
			}
			seen++
			if rate := podCPURate(prev[r.Key()], &r); rate != nil {
				c += *rate
			} else {
				cOK = false
			}
			if r.MemoryWorkingSetBytes > 0 {
				m += float64(r.MemoryWorkingSetBytes)
			} else {
				mOK = false
			}
			if r.EphemeralStorageUsedBytes > 0 {
				dk += float64(r.EphemeralStorageUsedBytes)
				anyDisk = true
			} else {
				dOK = false
			}
			if r.CPUCoreNanoSeconds > 0 {
				now[r.Key()] = &r
			}
		}
		prev = now
		point := func(ok bool, v float64) console.ChartPoint {
			if seen == 0 || !ok {
				return console.ChartPoint{At: at}
			}
			return console.ChartPoint{At: at, Value: &v}
		}
		cpu.Points = append(cpu.Points, point(cOK, c))
		memory.Points = append(memory.Points, point(mOK, m))
		disk.Points = append(disk.Points, point(dOK, dk))
	}
	if len(samples) > 0 && !anyDisk {
		disk.Points = nil
		disk.Unavailable = "Not available: the kubelet reported no ephemeral storage for this workload's pods."
	}
	return []console.ChartSeries{cpu, memory, disk}
}
