package logagent

import (
	"encoding/json"
	"time"

	"google.golang.org/genproto/googleapis/api/monitoredres"
)

// The cluster objects the agent is deployed as and reads.
const (
	Namespace = "kube-system"
	// Name is the DaemonSet's, its ServiceAccount's, ClusterRole's and
	// pods' k8s-app label: GKE's agent's (observed on GKE nodes as
	// kube-system/fluentbit-gke; UNVERIFIED, re-read 2026-11-22:
	// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs
	// names a fluentbit-based agent and no DaemonSet).
	Name = "fluentbit-gke"
	// ConfigMap is the configuration the CLI writes; ConfigKey its key.
	ConfigMap = "fluentbit-gke-config"
	ConfigKey = "config.json"
	// PositionDir is the directory on the node the position file is kept
	// in, a hostPath beside the node's other state.
	PositionDir = "/var/lib/google-fluentbit/pos-files"
)

// The log categories: system and workload containers, and the control
// plane's components by GKE's names.
const (
	System   = "system"
	Workload = "workload"
)

// SystemNamespaces are the namespaces whose pods' logs are system logs
// (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs).
var SystemNamespaces = map[string]bool{
	"kube-system": true, "istio-system": true, "knative-serving": true,
	"gke-system": true, "config-management-system": true,
}

// ControlPlane are the control plane's static pods' components (their
// "component" label) and GKE's component names; etcd's logs are never
// shipped.
var ControlPlane = map[string]string{
	"kube-apiserver": "apiserver", "kube-scheduler": "scheduler", "kube-controller-manager": "controller-manager", "etcd": "",
}

// Config is what the agent ships, the ConfigMap's config.json.
type Config struct {
	// Project, Location and Cluster label the entries' resources.
	Project  string `json:"project"`
	Location string `json:"location"`
	Cluster  string `json:"cluster"`
	// Categories are the log categories (System, Workload, a control plane
	// component), each with when it was last turned on or off. One that is
	// absent has never been on.
	Categories map[string]Window `json:"categories"`
}

// Window is a category's state since its last change.
type Window struct {
	// On is whether the category is shipped.
	On bool `json:"on"`
	// Since is when it was turned on, or off. A line of a category on is
	// shipped if it is later than Since, of one off if it is not: written
	// before it was turned off. Zero, while on, is since the cluster began.
	Since time.Time `json:"since"`
}

// Ships is whether a line of category at ts is shipped.
func (c Config) Ships(category string, ts time.Time) bool {
	w, ok := c.Categories[category]
	switch {
	case !ok:
		return false
	case w.On:
		return ts.After(w.Since)
	default:
		return !ts.After(w.Since)
	}
}

// Any is whether any category is on.
func (c Config) Any() bool {
	for _, w := range c.Categories {
		if w.On {
			return true
		}
	}
	return false
}

// Next is the configuration with the categories on now: a category whose
// state changed takes changed as its Since (when changed is zero or not
// after its previous Since, now), and the others keep theirs. A category
// on for the first time is shipped from changed: from the cluster's start
// when it is zero, as for a cluster made with it on.
func (c Config) Next(on map[string]bool, changed, now time.Time) Config {
	out := Config{Project: c.Project, Location: c.Location, Cluster: c.Cluster, Categories: map[string]Window{}}
	for k, w := range c.Categories {
		out.Categories[k] = w
	}
	for k, v := range on {
		w, ok := out.Categories[k]
		switch {
		case !ok && !v:
			continue
		case !ok:
			out.Categories[k] = Window{On: true, Since: changed}
		case w.On != v:
			at := changed
			if at.IsZero() || !at.After(w.Since) {
				at = now
			}
			out.Categories[k] = Window{On: v, Since: at}
		}
	}
	for k, w := range out.Categories {
		if _, ok := on[k]; !ok && w.On {
			at := changed
			if at.IsZero() || !at.After(w.Since) {
				at = now
			}
			out.Categories[k] = Window{Since: at}
		}
	}
	return out
}

// Marshal is the ConfigMap's value.
func (c Config) Marshal() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// ParseConfig reads a ConfigMap's value.
func ParseConfig(s string) (Config, error) {
	var c Config
	err := json.Unmarshal([]byte(s), &c)
	return c, err
}

// Pod is what the agent knows of a pod from the Kubernetes API.
type Pod struct {
	Namespace, Name, UID string
	Labels               map[string]string
}

// Source is one container's entries: their category, log, resource and
// labels. Log is empty for stdout or stderr by stream.
type Source struct {
	Category string
	Log      string
	Resource *monitoredres.MonitoredResource
	Labels   map[string]string
}

// Classify is the source of container c of pod p on node, or false for
// one never shipped (etcd).
func (c Config) Classify(p Pod, container, node string) (Source, bool) {
	category := Workload
	if SystemNamespaces[p.Namespace] {
		category = System
	}
	if p.Namespace == "kube-system" && p.Labels["tier"] == "control-plane" {
		if comp, ok := ControlPlane[p.Labels["component"]]; ok {
			if comp == "" {
				return Source{}, false
			}
			return Source{Category: comp, Log: "container.googleapis.com/" + comp,
				Resource: &monitoredres.MonitoredResource{Type: "k8s_control_plane_component", Labels: map[string]string{
					"project_id": c.Project, "location": c.Location, "cluster_name": c.Cluster,
					"component_name": comp, "component_location": c.Location}},
				Labels: map[string]string{}}, true
		}
	}
	labels := map[string]string{"compute.googleapis.com/resource_name": node}
	for k, v := range p.Labels {
		labels["k8s-pod/"+k] = v
	}
	return Source{Category: category,
		Resource: &monitoredres.MonitoredResource{Type: "k8s_container", Labels: map[string]string{
			"project_id": c.Project, "location": c.Location, "cluster_name": c.Cluster,
			"namespace_name": p.Namespace, "pod_name": p.Name, "container_name": container}},
		Labels: labels}, true
}
