package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/releaseutil"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// A chart's deploy is a Helm release (install, upgrade, uninstall through
// the Helm SDK, its records in the namespace's Secrets as the helm CLI
// keeps them), recorded as an Application whose components are the
// release's objects.

// HelmConfigFunc makes the Helm SDK's configuration for a cluster and
// namespace; a test passes one with an in-memory store.
type HelmConfigFunc func(kubeconfig, namespace string) (*action.Configuration, error)

func defaultHelmConfig(kubeconfig, namespace string) (*action.Configuration, error) {
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kubeconfig
	flags.Namespace = &namespace
	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret", func(string, ...any) {}); err != nil {
		return nil, fmt.Errorf("helm: %w", err)
	}
	return cfg, nil
}

func (k Kube) helm(t Target, namespace string) (*action.Configuration, error) {
	if k.Helm != nil {
		return k.Helm(t.Kubeconfig, namespace)
	}
	return defaultHelmConfig(t.Kubeconfig, namespace)
}

// manifestKinds are the group/kinds of a release's objects.
func manifestKinds(manifest string) []GroupKind {
	seen := map[GroupKind]bool{}
	var out []GroupKind
	for _, m := range releaseutil.SplitManifests(manifest) {
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if yaml.Unmarshal([]byte(m), &head) != nil || head.Kind == "" {
			continue
		}
		group := ""
		if g, _, ok := strings.Cut(head.APIVersion, "/"); ok {
			group = g
		}
		gk := GroupKind{Group: group, Kind: head.Kind}
		if !seen[gk] {
			seen[gk] = true
			out = append(out, gk)
		}
	}
	return out
}

type manifestObject struct {
	GroupKind
	Name string
}

func manifestObjects(manifest string) []manifestObject {
	var out []manifestObject
	for _, m := range releaseutil.SplitManifests(manifest) {
		var o struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(m), &o) != nil || o.Kind == "" {
			continue
		}
		group := ""
		if g, _, ok := strings.Cut(o.APIVersion, "/"); ok {
			group = g
		}
		out = append(out, manifestObject{GroupKind: GroupKind{Group: group, Kind: o.Kind}, Name: o.Metadata.Name})
	}
	return out
}

// deployChart installs the chart as release app.Name and records it.
func (k Kube) deployChart(ctx context.Context, t Target, app AppMeta, createNamespace bool, plan Plan) error {
	cfg, err := k.helm(t, app.Namespace)
	if err != nil {
		return err
	}
	install := action.NewInstall(cfg)
	install.ReleaseName = app.Name
	install.Namespace = app.Namespace
	install.CreateNamespace = createNamespace
	install.Timeout = 5 * time.Minute
	rel, err := install.RunWithContext(ctx, plan.Chart.Chart, plan.Chart.Values)
	if err != nil {
		return fmt.Errorf("helm install %s: %w", app.Name, err)
	}
	return k.recordRelease(ctx, t, app, plan, rel.Manifest)
}

// recordRelease creates or updates the release's Application.
func (k Kube) recordRelease(ctx context.Context, t Target, app AppMeta, plan Plan, manifest string) error {
	kinds := []any{}
	for _, gk := range manifestKinds(manifest) {
		kinds = append(kinds, map[string]any{"group": gk.Group, "kind": gk.Kind})
	}
	ann := map[string]any{AnnotationRelease: app.Name}
	for k, v := range plan.Annotations {
		ann[k] = v
	}
	application := map[string]any{
		"apiVersion": "app.k8s.io/v1beta1", "kind": "Application",
		"metadata": map[string]any{"name": app.Name, "namespace": app.Namespace, "annotations": ann,
			"labels": map[string]any{"app.kubernetes.io/instance": app.Name, "app.kubernetes.io/managed-by": "Helm"}},
		"spec": map[string]any{
			"descriptor":     plan.Descriptor,
			"componentKinds": kinds,
			"selector":       map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/instance": app.Name}},
			"assemblyPhase":  "Succeeded",
		},
	}
	b, _ := json.Marshal(application)
	if err := k.runner(t.Kubeconfig, app.Namespace).Apply(ctx, string(b), k8s.ApplyOptions{}); err != nil {
		return fmt.Errorf("record application %s: %w", app.Name, cause(err))
	}
	return nil
}

// UpgradeApplication upgrades a chart's release to plan, as `helm
// upgrade` does, and updates its Application.
func (k Kube) UpgradeApplication(ctx context.Context, t Target, namespace, name string, plan Plan) error {
	if plan.Chart == nil {
		return fmt.Errorf("%w: only a Helm chart's application is upgraded", ErrBadRequest)
	}
	cfg, err := k.helm(t, namespace)
	if err != nil {
		return err
	}
	up := action.NewUpgrade(cfg)
	up.Namespace = namespace
	up.Timeout = 5 * time.Minute
	rel, err := up.RunWithContext(ctx, name, plan.Chart.Chart, plan.Chart.Values)
	if err != nil {
		return fmt.Errorf("helm upgrade %s: %w", name, err)
	}
	return k.recordRelease(ctx, t, AppMeta{Name: name, Namespace: namespace}, plan, rel.Manifest)
}

// ReleaseValues are a release's user-supplied values, as YAML, and its
// chart's version.
func (k Kube) ReleaseValues(t Target, namespace, name string) (string, string, error) {
	cfg, err := k.helm(t, namespace)
	if err != nil {
		return "", "", err
	}
	rel, err := action.NewGet(cfg).Run(name)
	if err != nil {
		return "", "", fmt.Errorf("%w: helm release %s: %v", ErrNotFound, name, err)
	}
	b, _ := yaml.Marshal(rel.Config)
	if len(rel.Config) == 0 {
		b = nil
	}
	return string(b), chartVersion(rel.Chart), nil
}

func chartVersion(c *chart.Chart) string {
	if c == nil || c.Metadata == nil {
		return ""
	}
	return c.Metadata.Version
}

// releaseComponents reads a release's objects from the cluster.
func (k Kube) releaseComponents(ctx context.Context, t Target, a *App) (pending bool) {
	cfg, err := k.helm(t, a.Namespace)
	if err != nil {
		a.Components = append(a.Components, Component{Kind: "Release", Name: a.Release, Status: StatusFailed, Detail: err.Error()})
		return false
	}
	rel, err := action.NewGet(cfg).Run(a.Release)
	if err != nil {
		a.Components = append(a.Components, Component{Kind: "Release", Name: a.Release, Status: StatusFailed, Detail: err.Error()})
		return false
	}
	a.ReleaseStatus = rel.Info.Status.String()
	a.Revision = rel.Version
	r := k.runner(t.Kubeconfig, a.Namespace)
	for _, o := range manifestObjects(rel.Manifest) {
		out, err := r.Get(ctx, kindResource(o.GroupKind), o.Name, "json")
		if err != nil {
			st := StatusFailed
			detail := cause(err).Error()
			if k8s.IsNotFound(err) {
				st, detail, pending = StatusPending, "Not created yet", true
			}
			a.Components = append(a.Components, Component{Kind: o.Kind, Group: o.Group, Name: o.Name, Status: st, Detail: detail})
			continue
		}
		var item object
		_ = json.Unmarshal([]byte(out), &item)
		c := componentOf(o.GroupKind, item)
		if c.Status == StatusPending {
			pending = true
		}
		a.Components = append(a.Components, c)
	}
	return pending
}

// uninstall removes a chart's release.
func (k Kube) uninstall(t Target, namespace, release string) error {
	cfg, err := k.helm(t, namespace)
	if err != nil {
		return err
	}
	// An already-uninstalled release is done.
	if _, err := action.NewUninstall(cfg).Run(release); err != nil && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("helm uninstall %s: %w", release, err)
	}
	return nil
}
