package components

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Retired is an object an earlier CloudBurrow installed in the instance's
// namespace and this one no longer manages (#780). `up` removes it: left
// running, it keeps doing what it did against state this release now owns.
type Retired struct {
	// Kind is the kubectl resource, "deployment" or "service".
	Kind string
	Name string
	// Selector is the labels the object must carry to be removed, so an
	// object of the same name a developer made is never touched.
	Selector string
	// Why is the reason the message names.
	Why string
}

// RetiredComponents are the objects earlier releases installed for
// instance that this one removes.
//
// storage-internal was the second Cloud Storage Deployment before the
// builtin server (#519), serving in-cluster clients from the same
// storage-data volume. `up` over such a cluster left it running as root
// beside the builtin server, which it took for a set of buckets, writing
// root-owned files into the builtin server's store until that server could
// not open its own log (#780). Its Deployment carried the instance label;
// its Service, like every Service then, carried only the ownership label.
func RetiredComponents(instance string) []Retired {
	const why = "the in-cluster Cloud Storage server CloudBurrow ran before its builtin server (#519); " +
		"the storage Deployment now serves the cluster too"
	deployment := k8s.OwnedSelector + "," + k8s.InstanceLabel + "=" + instance
	return []Retired{
		{Kind: "deployment", Name: "storage-internal", Selector: deployment, Why: why},
		{Kind: "service", Name: "storage-internal", Selector: k8s.OwnedSelector, Why: why},
	}
}

// RemoveRetired deletes each of RetiredComponents present in the namespace,
// naming it and why, and returns the ones it removed. A Deployment is
// deleted in the foreground and its pods waited for, so none is still
// writing to a volume when the next backend starts on it. It runs through
// internal/k8s's Runner, scoped to the instance's namespace.
func (i *Installer) RemoveRetired(ctx context.Context, timeout time.Duration) ([]Retired, error) {
	inv := i.Kube
	if inv == nil {
		inv = k8s.Subprocess{}
	}
	kube := k8s.NewWith(inv, i.Kubeconfig, "", i.Namespace)
	secs := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	var removed []Retired
	for _, r := range RetiredComponents(i.Instance) {
		out, err := kube.List(ctx, r.Kind, r.Selector, "name")
		if err != nil {
			return removed, fmt.Errorf("%w: look for the retired %s %s: %w", ErrInstallFailed, r.Kind, r.Name, err)
		}
		if !listed(out, r.Name) {
			continue
		}
		i.logf("  removing %s %s: %s\n", r.Kind, r.Name, r.Why)
		args := []string{"delete", r.Kind, r.Name, "--ignore-not-found", "--wait=true", secs}
		if r.Kind == "deployment" {
			args = append(args, "--cascade=foreground")
		}
		if _, err := kube.Do(ctx, "", args...); err != nil {
			return removed, fmt.Errorf("%w: remove the retired %s %s: %w", ErrInstallFailed, r.Kind, r.Name, err)
		}
		if r.Kind == "deployment" {
			// The foreground delete returns once the pods are gone; this
			// makes sure of it. Every Deployment CloudBurrow made selects
			// its pods by app. kubectl wait fails when nothing matches, so
			// it runs only while a pod is still listed.
			pods := "app=" + r.Name
			if left, err := kube.List(ctx, "pod", pods, "name"); err == nil && strings.TrimSpace(left) != "" {
				if _, err := kube.Do(ctx, "", "wait", "--for=delete", "pod", "-l", pods, secs); err != nil &&
					!strings.Contains(err.Error(), "no matching resources") {
					return removed, fmt.Errorf("%w: the retired %s %s's pods did not stop: %w", ErrInstallFailed, r.Kind, r.Name, err)
				}
			}
		}
		removed = append(removed, r)
	}
	return removed, nil
}

// listed reports whether kubectl's -o name output names name, as
// "deployment.apps/storage-internal" or "service/storage-internal".
func listed(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if _, n, ok := strings.Cut(strings.TrimSpace(line), "/"); ok && n == name {
			return true
		}
	}
	return false
}
