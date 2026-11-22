package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// The Kubernetes side: an app is an app.k8s.io/v1beta1 Application, the
// Kubernetes Applications CRD (kubernetes-sigs/application) that Google
// Cloud Marketplace records each Kubernetes app with, whose components are
// selected by the app.kubernetes.io/name label and owned by it.

// Annotations the Application carries.
const (
	AnnotationSource  = "marketplace.cloudburrow.dev/source"
	AnnotationProduct = "marketplace.cloudburrow.dev/product"
	AnnotationImage   = "marketplace.cloudburrow.dev/image"
	// AnnotationRelease names a Helm release the app is.
	AnnotationRelease = "marketplace.cloudburrow.dev/helm-release"
	// LabelNameKey is the label an Application selects its components by.
	LabelNameKey = "app.kubernetes.io/name"
)

// AppResource is the Application kind as kubectl names it.
const AppResource = "applications.app.k8s.io"

// AppLabels are the labels every component carries.
func AppLabels(name string) map[string]any {
	return map[string]any{LabelNameKey: name, "app.kubernetes.io/managed-by": "cloudburrow-marketplace"}
}

// ApplicationCRD is the Application CRD, app.k8s.io/v1beta1, its schema
// open (x-kubernetes-preserve-unknown-fields) as the fields are the
// Application API's (spec.descriptor, componentKinds, selector,
// addOwnerRef, assemblyPhase; status).
func ApplicationCRD() map[string]any {
	open := map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	return map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": AppResource, "annotations": map[string]any{
			"api-approved.kubernetes.io": "https://github.com/kubernetes-sigs/application/pull/2"}},
		"spec": map[string]any{
			"group": "app.k8s.io", "scope": "Namespaced",
			"names": map[string]any{"kind": "Application", "listKind": "ApplicationList", "plural": "applications",
				"singular": "application", "shortNames": []any{"app"}, "categories": []any{"all"}},
			"versions": []any{map[string]any{
				"name": "v1beta1", "served": true, "storage": true,
				"subresources": map[string]any{"status": map[string]any{}},
				"additionalPrinterColumns": []any{
					map[string]any{"name": "Type", "type": "string", "jsonPath": ".spec.descriptor.type"},
					map[string]any{"name": "Version", "type": "string", "jsonPath": ".spec.descriptor.version"},
					map[string]any{"name": "Age", "type": "date", "jsonPath": ".metadata.creationTimestamp"},
				},
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{
					"apiVersion": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string"},
					"metadata": map[string]any{"type": "object"}, "spec": open, "status": open,
				}}},
			}},
		},
	}
}

// Kube runs the deploys and reads through internal/k8s.
type Kube struct {
	// Invoker defaults to kubectl.
	Invoker k8s.Invoker
	// Wait bounds the wait for the CRD to be established.
	Wait time.Duration
}

func (k Kube) runner(kubeconfig, namespace string) *k8s.Runner {
	inv := k.Invoker
	if inv == nil {
		inv = k8s.Subprocess{}
	}
	return k8s.NewWith(inv, kubeconfig, "", namespace)
}

// cause is what kubectl printed, or its error.
func cause(err error) error {
	var re *k8s.RunError
	if errors.As(err, &re) && re.Stderr != "" {
		return errors.New(re.Stderr)
	}
	return err
}

// DNSLabel is a Kubernetes name: an app instance's or a namespace's.
var DNSLabel = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Target is a cluster to deploy to.
type Target struct {
	Cluster    string `json:"cluster"`
	Kubeconfig string `json:"-"`
}

// DeployRequest is the Configure page's Deploy.
type DeployRequest struct {
	Source          string            `json:"source"`
	Product         string            `json:"product"`
	Version         string            `json:"version"`
	Cluster         string            `json:"cluster"`
	Namespace       string            `json:"namespace"`
	CreateNamespace bool              `json:"createNamespace"`
	Name            string            `json:"name"`
	Values          map[string]string `json:"values"`
}

// EnsureCRD applies the Application CRD and waits for it to be served.
func (k Kube) EnsureCRD(ctx context.Context, t Target) error {
	r := k.runner(t.Kubeconfig, "")
	b, _ := json.Marshal(ApplicationCRD())
	if err := r.Apply(ctx, string(b), k8s.ApplyOptions{}); err != nil {
		return fmt.Errorf("install the Application CRD: %w", cause(err))
	}
	wait := k.Wait
	if wait == 0 {
		wait = 60 * time.Second
	}
	if _, err := r.Do(ctx, "", "wait", "--for", "condition=established", "crd/"+AppResource, "--timeout", strconv.Itoa(int(wait.Seconds()))+"s"); err != nil {
		return fmt.Errorf("wait for the Application CRD: %w", cause(err))
	}
	return nil
}

// Namespaces are the cluster's namespaces.
func (k Kube) Namespaces(ctx context.Context, t Target) ([]string, error) {
	out, err := k.runner(t.Kubeconfig, "").Do(ctx, "", "get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, cause(err)
	}
	ns := strings.Fields(out)
	sort.Strings(ns)
	return ns, nil
}

// Deploy applies the plan as an Application named app.Name in
// app.Namespace: the CRD, the namespace when asked, the Application, then
// its components owned by it, and its assemblyPhase Succeeded.
func (k Kube) Deploy(ctx context.Context, t Target, app AppMeta, createNamespace bool, plan Plan) error {
	if !DNSLabel.MatchString(app.Name) {
		return fmt.Errorf("%w: app instance name %q: lowercase letters, digits and dashes, starting with a letter, at most 63", ErrBadRequest, app.Name)
	}
	if !DNSLabel.MatchString(app.Namespace) {
		return fmt.Errorf("%w: namespace %q is not a valid namespace name", ErrBadRequest, app.Namespace)
	}
	if err := k.EnsureCRD(ctx, t); err != nil {
		return err
	}
	root := k.runner(t.Kubeconfig, "")
	if _, err := root.Get(ctx, "namespace", app.Namespace, "name"); err != nil {
		if !k8s.IsNotFound(err) {
			return fmt.Errorf("read namespace %s: %w", app.Namespace, cause(err))
		}
		if !createNamespace {
			return fmt.Errorf("%w: namespace %s does not exist; choose Create a namespace", ErrBadRequest, app.Namespace)
		}
		ns, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": app.Namespace}})
		if err := root.Apply(ctx, string(ns), k8s.ApplyOptions{}); err != nil {
			return fmt.Errorf("create namespace %s: %w", app.Namespace, cause(err))
		}
	}
	r := k.runner(t.Kubeconfig, app.Namespace)
	if _, err := r.Get(ctx, AppResource, app.Name, "name"); err == nil {
		return fmt.Errorf("%w: an application named %s already exists in namespace %s; the name must be unique within the namespace", ErrBadRequest, app.Name, app.Namespace)
	} else if !k8s.IsNotFound(err) {
		return fmt.Errorf("read application %s: %w", app.Name, cause(err))
	}
	kinds := make([]any, 0, len(plan.Kinds))
	for _, gk := range plan.Kinds {
		kinds = append(kinds, map[string]any{"group": gk.Group, "kind": gk.Kind})
	}
	ann := map[string]any{}
	for k, v := range plan.Annotations {
		ann[k] = v
	}
	application := map[string]any{
		"apiVersion": "app.k8s.io/v1beta1", "kind": "Application",
		"metadata": map[string]any{"name": app.Name, "namespace": app.Namespace, "labels": AppLabels(app.Name), "annotations": ann},
		"spec": map[string]any{
			"descriptor":     plan.Descriptor,
			"componentKinds": kinds,
			"selector":       map[string]any{"matchLabels": map[string]any{LabelNameKey: app.Name}},
			"addOwnerRef":    true,
			"assemblyPhase":  "Pending",
		},
	}
	b, _ := json.Marshal(application)
	if err := r.Create(ctx, string(b)); err != nil {
		return fmt.Errorf("create application %s: %w", app.Name, cause(err))
	}
	uid, err := r.Get(ctx, AppResource, app.Name, "jsonpath={.metadata.uid}")
	if err != nil {
		return fmt.Errorf("read application %s: %w", app.Name, cause(err))
	}
	owner := map[string]any{"apiVersion": "app.k8s.io/v1beta1", "kind": "Application", "name": app.Name,
		"uid": strings.TrimSpace(uid), "blockOwnerDeletion": true}
	items := make([]any, 0, len(plan.Manifests))
	for _, m := range plan.Manifests {
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			m["metadata"] = meta
		}
		meta["namespace"] = app.Namespace
		meta["ownerReferences"] = []any{owner}
		items = append(items, m)
	}
	if len(items) > 0 {
		list, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
		if err := r.Apply(ctx, string(list), k8s.ApplyOptions{}); err != nil {
			phase, _ := json.Marshal(map[string]any{"spec": map[string]any{"assemblyPhase": "Failed"}})
			_ = r.Patch(ctx, AppResource, app.Name, string(phase))
			return fmt.Errorf("deploy %s's components: %w", app.Name, cause(err))
		}
	}
	phase, _ := json.Marshal(map[string]any{"spec": map[string]any{"assemblyPhase": "Succeeded"}})
	if err := r.Patch(ctx, AppResource, app.Name, string(phase)); err != nil {
		return fmt.Errorf("mark application %s assembled: %w", app.Name, cause(err))
	}
	return nil
}

// App is one Application's row and page.
type App struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Cluster     string            `json:"cluster"`
	Type        string            `json:"type"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	Created     string            `json:"created"`
	Phase       string            `json:"phase"`
	Status      string            `json:"status"`
	Detail      string            `json:"detail,omitempty"`
	Source      string            `json:"source,omitempty"`
	Product     string            `json:"product,omitempty"`
	Image       string            `json:"image,omitempty"`
	Release     string            `json:"release,omitempty"`
	Links       []AppLink         `json:"links,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Kinds       []GroupKind       `json:"componentKinds,omitempty"`
	Components  []Component       `json:"components,omitempty"`
	Pods        []Pod             `json:"pods,omitempty"`
	Object      json.RawMessage   `json:"object,omitempty"`
}

// Component is one of an app's objects.
type Component struct {
	Kind      string   `json:"kind"`
	Group     string   `json:"group,omitempty"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Detail    string   `json:"detail,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
}

// Pod is one of the app's pods, for the progress view.
type Pod struct {
	Name   string `json:"name"`
	Phase  string `json:"phase"`
	Ready  string `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// The statuses an app shows, as Google's Applications page has them.
const (
	StatusOK      = "OK"
	StatusPending = "Pending"
	StatusFailed  = "Error"
)

type object = map[string]any

func mget(m object, path ...string) object {
	for _, p := range path {
		n, _ := m[p].(map[string]any)
		if n == nil {
			return object{}
		}
		m = n
	}
	return m
}

func sget(m object, key string) string { s, _ := m[key].(string); return s }

func appOf(item object, cluster string) App {
	md, spec := mget(item, "metadata"), mget(item, "spec")
	d := mget(spec, "descriptor")
	ann := mget(md, "annotations")
	a := App{Name: sget(md, "name"), Namespace: sget(md, "namespace"), Cluster: cluster,
		Type: sget(d, "type"), Version: sget(d, "version"), Description: sget(d, "description"),
		Created: sget(md, "creationTimestamp"), Phase: sget(spec, "assemblyPhase"),
		Source: sget(ann, AnnotationSource), Product: sget(ann, AnnotationProduct), Image: sget(ann, AnnotationImage),
		Release: sget(ann, AnnotationRelease), Labels: map[string]string{}}
	for k, v := range mget(md, "labels") {
		if s, ok := v.(string); ok {
			a.Labels[k] = s
		}
	}
	if links, ok := d["links"].([]any); ok {
		for _, l := range links {
			if lm, ok := l.(map[string]any); ok {
				if u := safeURL(sget(lm, "url")); u != "" {
					a.Links = append(a.Links, AppLink{Description: sget(lm, "description"), URL: u})
				}
			}
		}
	}
	if ks, ok := spec["componentKinds"].([]any); ok {
		for _, k := range ks {
			if km, ok := k.(map[string]any); ok {
				a.Kinds = append(a.Kinds, GroupKind{Group: sget(km, "group"), Kind: sget(km, "kind")})
			}
		}
	}
	return a
}

func noAppCRD(err error) bool {
	s := err.Error()
	return strings.Contains(s, "the server doesn't have a resource type") || strings.Contains(s, "no matches for kind")
}

// Applications are the cluster's Applications, with each one's status.
func (k Kube) Applications(ctx context.Context, t Target) ([]App, error) {
	out, err := k.runner(t.Kubeconfig, "").Do(ctx, "", "get", AppResource, "--all-namespaces", "-o", "json")
	if err != nil {
		if noAppCRD(cause(err)) {
			return []App{}, nil
		}
		return nil, cause(err)
	}
	var list struct {
		Items []object `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode applications: %w", err)
	}
	apps := make([]App, 0, len(list.Items))
	for _, it := range list.Items {
		a := appOf(it, t.Cluster)
		k.fill(ctx, t, &a)
		a.Components, a.Pods = nil, nil
		apps = append(apps, a)
	}
	return apps, nil
}

// Application is one app's page: its components, pods and object.
func (k Kube) Application(ctx context.Context, t Target, namespace, name string) (App, error) {
	if !DNSLabel.MatchString(namespace) || !DNSLabel.MatchString(name) {
		return App{}, fmt.Errorf("%w: %s/%s is not a namespace and an application name", ErrBadRequest, namespace, name)
	}
	r := k.runner(t.Kubeconfig, namespace)
	out, err := r.Get(ctx, AppResource, name, "json")
	if err != nil {
		if k8s.IsNotFound(err) || noAppCRD(cause(err)) {
			return App{}, fmt.Errorf("%w: no application %s in namespace %s of cluster %s", ErrNotFound, name, namespace, t.Cluster)
		}
		return App{}, cause(err)
	}
	var item object
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return App{}, fmt.Errorf("decode application: %w", err)
	}
	a := appOf(item, t.Cluster)
	k.fill(ctx, t, &a)
	a.Object = json.RawMessage(out)
	return a, nil
}

// kindResource is kubectl's name for a component kind.
func kindResource(gk GroupKind) string {
	r := strings.ToLower(gk.Kind)
	if gk.Group != "" && gk.Group != "core" {
		r += "." + gk.Group
	}
	return r
}

// fill reads the components and pods and sets the app's status.
func (k Kube) fill(ctx context.Context, t Target, a *App) {
	r := k.runner(t.Kubeconfig, a.Namespace)
	sel := LabelNameKey + "=" + a.Name
	pending, failed := false, ""
	for _, gk := range a.Kinds {
		out, err := r.List(ctx, kindResource(gk), sel, "json")
		if err != nil {
			a.Components = append(a.Components, Component{Kind: gk.Kind, Group: gk.Group, Status: StatusFailed, Detail: cause(err).Error()})
			continue
		}
		var list struct {
			Items []object `json:"items"`
		}
		_ = json.Unmarshal([]byte(out), &list)
		for _, it := range list.Items {
			c := componentOf(gk, it)
			if c.Status == StatusPending {
				pending = true
			}
			a.Components = append(a.Components, c)
		}
	}
	if out, err := r.List(ctx, "pods", sel, "json"); err == nil {
		var list struct {
			Items []object `json:"items"`
		}
		_ = json.Unmarshal([]byte(out), &list)
		for _, it := range list.Items {
			p := podOf(it)
			if bad(p.Reason) && failed == "" {
				failed = p.Name + ": " + p.Reason
			}
			a.Pods = append(a.Pods, p)
		}
	}
	switch {
	case a.Phase == "Failed":
		a.Status, a.Detail = StatusFailed, "The application's components could not be deployed."
	case failed != "":
		a.Status, a.Detail = StatusFailed, failed
	case a.Phase == "Pending" || pending:
		a.Status = StatusPending
	default:
		a.Status = StatusOK
	}
}

func bad(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "Error", "OOMKilled":
		return true
	}
	return false
}

func num(m object, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func componentOf(gk GroupKind, it object) Component {
	md, spec, st := mget(it, "metadata"), mget(it, "spec"), mget(it, "status")
	c := Component{Kind: gk.Kind, Group: gk.Group, Name: sget(md, "name"), Status: StatusOK}
	switch gk.Kind {
	case "Deployment", "StatefulSet", "ReplicaSet":
		want := 1
		if v, ok := spec["replicas"].(float64); ok {
			want = int(v)
		}
		ready := num(st, "readyReplicas")
		c.Detail = fmt.Sprintf("%d/%d pods ready", ready, want)
		if ready < want {
			c.Status = StatusPending
		}
	case "DaemonSet":
		ready, want := num(st, "numberReady"), num(st, "desiredNumberScheduled")
		c.Detail = fmt.Sprintf("%d/%d pods ready", ready, want)
		if ready < want {
			c.Status = StatusPending
		}
	case "Job":
		if num(st, "succeeded") == 0 {
			c.Status = StatusPending
		}
	case "Service":
		typ := sget(spec, "type")
		c.Detail = typ
		ip := sget(spec, "clusterIP")
		var lbs []string
		if ing, ok := mget(st, "loadBalancer")["ingress"].([]any); ok {
			for _, i := range ing {
				if im, ok := i.(map[string]any); ok {
					lbs = append(lbs, or(sget(im, "ip"), sget(im, "hostname")))
				}
			}
		}
		if ports, ok := spec["ports"].([]any); ok {
			for _, p := range ports {
				pm, _ := p.(map[string]any)
				port := num(pm, "port")
				proto := or(sget(pm, "protocol"), "TCP")
				for _, lb := range lbs {
					c.Endpoints = append(c.Endpoints, fmt.Sprintf("%s:%d", lb, port))
				}
				if ip != "" && ip != "None" {
					c.Endpoints = append(c.Endpoints, fmt.Sprintf("%s:%d %s", ip, port, proto))
				}
				if np := num(pm, "nodePort"); np > 0 && typ != "ClusterIP" {
					c.Endpoints = append(c.Endpoints, fmt.Sprintf("node port %d", np))
				}
			}
		}
		if typ == "LoadBalancer" && len(lbs) == 0 {
			c.Detail = "LoadBalancer, external IP pending"
		}
	}
	return c
}

func podOf(it object) Pod {
	md, st := mget(it, "metadata"), mget(it, "status")
	p := Pod{Name: sget(md, "name"), Phase: sget(st, "phase")}
	cs, _ := st["containerStatuses"].([]any)
	ready := 0
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		if b, _ := cm["ready"].(bool); b {
			ready++
		}
		if w := mget(cm, "state", "waiting"); sget(w, "reason") != "" && p.Reason == "" {
			p.Reason = sget(w, "reason")
		}
		if w := mget(cm, "state", "terminated"); sget(w, "reason") != "" && p.Reason == "" {
			p.Reason = sget(w, "reason")
		}
	}
	p.Ready = fmt.Sprintf("%d/%d", ready, len(cs))
	if p.Reason == "" {
		conds, _ := st["conditions"].([]any)
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if sget(cm, "type") == "PodScheduled" && sget(cm, "status") == "False" {
				p.Reason = or(sget(cm, "reason"), "Unschedulable")
			}
		}
	}
	return p
}

// DeleteApplication removes an Application and its components.
func (k Kube) DeleteApplication(ctx context.Context, t Target, namespace, name string) error {
	a, err := k.Application(ctx, t, namespace, name)
	if err != nil {
		return err
	}
	r := k.runner(t.Kubeconfig, namespace)
	for _, gk := range a.Kinds {
		if err := r.DeleteSelected(ctx, kindResource(gk), LabelNameKey+"="+name, true); err != nil {
			return fmt.Errorf("delete %s's %s: %w", name, gk.Kind, cause(err))
		}
	}
	if err := r.Delete(ctx, AppResource, name, true); err != nil {
		return fmt.Errorf("delete application %s: %w", name, cause(err))
	}
	return nil
}
