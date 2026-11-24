package gke

import (
	"context"
	"regexp"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A node pool's Kubernetes labels and taints (#2100): node_config.labels
// and node_config.taints when the pool is made, UpdateNodePool's labels
// and taints later (gcloud's --node-labels and --node-taints), which
// replace them. GKE "applies [them] to all nodes in the node pool", the
// nodes it adds later too (re-read 2026-11-22:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/update-existing-nodepools
// and how-to/node-taints): locally each node registers with them, and an
// update relabels and retaints the nodes it has.

// NodeTaint is a taint of a pool's nodes, in Kubernetes' words: Effect is
// NoSchedule, PreferNoSchedule or NoExecute.
type NodeTaint struct{ Key, Value, Effect string }

// NodeConfigurer is what a Backend does for node labels and taints.
type NodeConfigurer interface {
	// ConfigureNodes sets the labels and taints of the nodes pool gets
	// later and, when apply, of its nodes now.
	ConfigureNodes(ctx context.Context, kind, pool string, labels map[string]string, taints []NodeTaint, apply bool) error
}

// taintEffects are the API's effects in Kubernetes' words.
var taintEffects = map[containerpb.NodeTaint_Effect]string{
	containerpb.NodeTaint_NO_SCHEDULE:        "NoSchedule",
	containerpb.NodeTaint_PREFER_NO_SCHEDULE: "PreferNoSchedule",
	containerpb.NodeTaint_NO_EXECUTE:         "NoExecute",
}

// Kubernetes' label rules: a key is an optional DNS subdomain prefix and
// a name of at most 63 characters; a value at most 63 characters, empty
// or alphanumeric at both ends (re-read 2026-11-22: kubernetes.io,
// "Labels and Selectors", Syntax and character set).
var (
	labelNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelPrefixRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	kubeValueRE   = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

func checkLabelKey(field, k string) error {
	name := k
	if prefix, n, ok := strings.Cut(k, "/"); ok {
		if len(prefix) > 253 || !labelPrefixRE.MatchString(prefix) {
			return status.Errorf(codes.InvalidArgument, "%s: key %q has an invalid prefix: it must be a DNS subdomain.", field, k)
		}
		name = n
	}
	if !labelNameRE.MatchString(name) {
		return status.Errorf(codes.InvalidArgument,
			"%s: key %q is invalid: a name must be 63 characters or less, begin and end with an alphanumeric character, and contain only dashes, underscores, dots and alphanumerics.", field, k)
	}
	return nil
}

// checkNodeLabels refuses labels Kubernetes refuses. UNVERIFIED wording;
// the rule is Kubernetes' (docs.cloud.google.com/kubernetes-engine/docs/how-to/node-taints,
// re-read 2026-11-22, quotes no error).
func checkNodeLabels(field string, labels map[string]string) error {
	for k, v := range labels {
		if err := checkLabelKey(field, k); err != nil {
			return err
		}
		if !kubeValueRE.MatchString(v) {
			return status.Errorf(codes.InvalidArgument, "%s: value %q of key %q is invalid: it must be 63 characters or less, and empty or begin and end with an alphanumeric character.", field, v, k)
		}
	}
	return nil
}

// nodeTaints checks taints and turns them into Kubernetes'. UNVERIFIED
// wording; the rules are Kubernetes' and the API's effects (the node-taints
// page, re-read 2026-11-22, quotes no error).
func nodeTaints(field string, in []*containerpb.NodeTaint) ([]NodeTaint, error) {
	var out []NodeTaint
	seen := map[string]bool{}
	for _, t := range in {
		if t.GetKey() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "%s: a taint's key is required.", field)
		}
		if err := checkLabelKey(field, t.GetKey()); err != nil {
			return nil, err
		}
		if !kubeValueRE.MatchString(t.GetValue()) {
			return nil, status.Errorf(codes.InvalidArgument, "%s: taint value %q is invalid.", field, t.GetValue())
		}
		effect, ok := taintEffects[t.GetEffect()]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "%s: taint %q needs an effect: NO_SCHEDULE, PREFER_NO_SCHEDULE or NO_EXECUTE.", field, t.GetKey())
		}
		if seen[t.GetKey()+":"+effect] {
			return nil, status.Errorf(codes.InvalidArgument, "%s: taint %q with effect %s is listed twice.", field, t.GetKey(), t.GetEffect())
		}
		seen[t.GetKey()+":"+effect] = true
		out = append(out, NodeTaint{Key: t.GetKey(), Value: t.GetValue(), Effect: effect})
	}
	return out, nil
}

// The labels GKE gives each node with its machine type and boot disk type
// (re-read 2026-11-22: Kubernetes' well-known labels,
// node.kubernetes.io/instance-type, and GKE's cloud.google.com/gke-boot-disk,
// which how-to/node-auto-provisioning and the node labels GKE sets name).
const (
	instanceTypeLabel = "node.kubernetes.io/instance-type"
	bootDiskLabel     = "cloud.google.com/gke-boot-disk"
)

// nodeLabels are the labels a pool's nodes carry: its own, and its
// machine type's and boot disk type's.
func nodeLabels(cfg *containerpb.NodeConfig) map[string]string {
	out := make(map[string]string, len(cfg.GetLabels())+2)
	for k, v := range cfg.GetLabels() {
		out[k] = v
	}
	if m := cfg.GetMachineType(); m != "" {
		out[instanceTypeLabel] = m
	}
	if d := cfg.GetDiskType(); d != "" {
		out[bootDiskLabel] = d
	}
	return out
}

// configurePool applies a pool's labels and taints through the backend
// (apply: to its nodes now too); a backend with no nodes of its own does
// nothing.
func (s *Server) configurePool(ctx context.Context, kind, pool string, labels map[string]string, taints []NodeTaint, apply bool) error {
	nc, ok := s.backend.(NodeConfigurer)
	if !ok {
		return nil
	}
	if err := nc.ConfigureNodes(ctx, kind, pool, labels, taints, apply); err != nil {
		return status.Errorf(codes.Internal, "Setting node pool %q's node labels and taints failed: %v", pool, err)
	}
	return nil
}

// configurePoolOf applies np's node_config labels, taints, machine type
// label and kubelet configuration, checked when the pool was made.
func (s *Server) configurePoolOf(ctx context.Context, kind string, np *containerpb.NodePool) error {
	t, err := nodeTaints("node_config.taints", np.GetConfig().GetTaints())
	if err != nil {
		return err
	}
	if kc := np.GetConfig().GetKubeletConfig(); kc != nil {
		k, err := kubeletSettings("node_config.kubelet_config", kc)
		if err != nil {
			return err
		}
		if err := s.configureKubelet(ctx, kind, np.GetName(), k); err != nil {
			return err
		}
	}
	if err := s.configureMachine(ctx, kind, np.GetName(), np.GetConfig().GetMachineType()); err != nil {
		return err
	}
	return s.configurePool(ctx, kind, np.GetName(), nodeLabels(np.GetConfig()), t, true)
}
