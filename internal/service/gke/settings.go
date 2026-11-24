package gke

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cluster and node pool settings (#2100), each with the effect it has on
// GKE, made on the local cluster by the Backend's Controller:
//   - SetLegacyAbac turns the API server's ABAC authorizer on or off;
//   - SetNetworkPolicy turns NetworkPolicy enforcement on or off (a
//     Standard cluster enforces none until it is enabled, an Autopilot
//     cluster always does, as GKE Dataplane V2 does);
//   - SetAddonsConfig records the network policy add-on, which enforcement
//     needs ("Enabling only the add-on without enabling enforcement on the
//     nodes will not enforce any policies", re-read 2026-11-21:
//     https://docs.cloud.google.com/kubernetes-engine/docs/how-to/network-policy);
//     the other add-ons have no local counterpart and are refused when they
//     would change;
//   - SetNodePoolManagement's auto-upgrade is what automatic upgrades obey
//     (upgrade.go) and its auto-repair what the repair loop (autoscale.go)
//     obeys; SetNodePoolAutoscaling's limits are what the cluster
//     autoscaler (autoscale.go) scales a pool within;
//   - GetJSONWebKeys is the cluster's own service account issuer keys, and
//     CheckAutopilotCompatibility reads the cluster's own pods.

// Controller is what a Backend does for the settings.
type Controller interface {
	// Get is a GET of the cluster's Kubernetes API at path.
	Get(ctx context.Context, kind, path string) ([]byte, error)
	// SetLegacyAbac turns the API server's legacy ABAC authorizer on or off.
	SetLegacyAbac(ctx context.Context, kind string, enabled bool) error
	// SetNetworkPolicy turns NetworkPolicy enforcement on or off.
	SetNetworkPolicy(ctx context.Context, kind string, enabled bool) error
	// RepairNode replaces a pool's node by a new one of the same name.
	RepairNode(ctx context.Context, kind, image, pool, name string) error
}

func (s *Server) controller() (Controller, error) {
	c, ok := s.backend.(Controller)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Changing cluster settings is not available with this backend.")
	}
	return c, nil
}

// startSettingLocked runs work on a ready cluster, RECONCILING while it
// runs, and applies the change to the record when it succeeds.
func (s *Server) startSettingLocked(p parsed, e *entry, typ containerpb.Operation_Type, work func(ctx context.Context) error, apply func()) *containerpb.Operation {
	c := e.cluster
	c.Status = containerpb.Cluster_RECONCILING
	return s.startOpLocked(p, typ, c.SelfLink, e, work, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		apply()
	})
}

// SetLegacyAbac enables or disables the API server's legacy ABAC
// authorizer, which grants the service accounts full access after RBAC
// (internal/cluster's abacPolicy). Autopilot clusters refuse it ("can't be
// enabled in Autopilot clusters", re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/hardening-your-cluster).
func (s *Server) SetLegacyAbac(_ context.Context, req *containerpb.SetLegacyAbacRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	ctl, err := s.controller()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	enabled := req.GetEnabled()
	if enabled && e.cluster.GetAutopilot().GetEnabled() {
		// UNVERIFIED wording; the refusal is documented.
		// Docs re-read 2026-11-21: nothing more is published.
		return nil, status.Error(codes.InvalidArgument, "Legacy ABAC can't be enabled in Autopilot clusters.")
	}
	kind := e.kind
	return s.startSettingLocked(p, e, containerpb.Operation_UPDATE_CLUSTER, func(ctx context.Context) error {
		if err := ctl.SetLegacyAbac(ctx, kind, enabled); err != nil {
			return status.Errorf(codes.Internal, "Setting legacy ABAC failed: %v", err)
		}
		return nil
	}, func() { e.cluster.LegacyAbac = &containerpb.LegacyAbac{Enabled: enabled} }), nil
}

// networkPolicyAddon is whether the network policy add-on is enabled.
func networkPolicyAddon(c *containerpb.Cluster) bool {
	cfg := c.GetAddonsConfig().GetNetworkPolicyConfig()
	return cfg != nil && !cfg.GetDisabled()
}

// enforcesNetworkPolicy is whether the cluster enforces NetworkPolicy:
// Autopilot and Dataplane V2 clusters always do, a Standard one when
// network policy is enabled.
func enforcesNetworkPolicy(c *containerpb.Cluster) bool {
	return c.GetAutopilot().GetEnabled() || c.GetNetworkPolicy().GetEnabled() ||
		c.GetNetworkConfig().GetDatapathProvider() == containerpb.DatapathProvider_ADVANCED_DATAPATH
}

// SetNetworkPolicy enables or disables NetworkPolicy enforcement. Enabling
// it needs the network policy add-on first, as Google's two steps are
// (re-read 2026-11-21: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/network-policy).
// GKE re-creates the nodes; locally the CNI (kindnet) is restarted with
// enforcement on or off on every node, which is the change the nodes'
// re-creation makes, and the nodes' connection tracking is flushed, so no
// connection made before the change outlives it, as on new nodes.
func (s *Server) SetNetworkPolicy(_ context.Context, req *containerpb.SetNetworkPolicyRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	np := req.GetNetworkPolicy()
	if np == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"network_policy\" is missing.")
	}
	ctl, err := s.controller()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	c := e.cluster
	// UNVERIFIED wordings below; the rules are the documented ones.
	// Docs re-read 2026-11-21: nothing more is published.
	if c.GetAutopilot().GetEnabled() || c.GetNetworkConfig().GetDatapathProvider() == containerpb.DatapathProvider_ADVANCED_DATAPATH {
		return nil, status.Error(codes.InvalidArgument, "Network policy enforcement is built into GKE Dataplane V2, which this cluster uses: it cannot be changed.")
	}
	if np.GetEnabled() && !networkPolicyAddon(c) {
		return nil, status.Error(codes.FailedPrecondition, "Network policy cannot be enabled while the NetworkPolicy add-on is disabled: enable addons_config.network_policy_config first.")
	}
	want := &containerpb.NetworkPolicy{Enabled: np.GetEnabled(), Provider: np.GetProvider()}
	if want.Enabled && want.Provider == containerpb.NetworkPolicy_PROVIDER_UNSPECIFIED {
		want.Provider = containerpb.NetworkPolicy_CALICO
	}
	kind := e.kind
	return s.startSettingLocked(p, e, containerpb.Operation_SET_NETWORK_POLICY, func(ctx context.Context) error {
		if err := ctl.SetNetworkPolicy(ctx, kind, want.Enabled); err != nil {
			return status.Errorf(codes.Internal, "Setting network policy enforcement failed: %v", err)
		}
		return nil
	}, func() { c.NetworkPolicy = want }), nil
}

// SetAddonsConfig sets the cluster's add-ons. The network policy and the
// Filestore CSI driver add-ons have local counterparts; any other add-on
// is accepted only as it already is.
func (s *Server) SetAddonsConfig(_ context.Context, req *containerpb.SetAddonsConfigRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	in := req.GetAddonsConfig()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"addons_config\" is missing.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	c := e.cluster
	cur := c.GetAddonsConfig()
	if cur == nil {
		cur = &containerpb.AddonsConfig{}
	}
	var unsupported []string
	curR := cur.ProtoReflect()
	in.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Name() == "network_policy_config" || fd.Name() == "gcp_filestore_csi_driver_config" {
			return true
		}
		if !curR.Has(fd) || !proto.Equal(curR.Get(fd).Message().Interface(), v.Message().Interface()) {
			unsupported = append(unsupported, "addons_config."+string(fd.Name()))
		}
		return true
	})
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, status.Errorf(codes.Unimplemented, "Not available locally: changing %s.", strings.Join(unsupported, ", "))
	}
	if npc := in.GetNetworkPolicyConfig(); npc != nil {
		if c.GetAutopilot().GetEnabled() {
			return nil, status.Error(codes.InvalidArgument, "Autopilot clusters enforce network policy with GKE Dataplane V2: the NetworkPolicy add-on cannot be changed.")
		}
		if npc.GetDisabled() && c.GetNetworkPolicy().GetEnabled() {
			// UNVERIFIED wording: enforcement is disabled before the add-on.
			// Docs re-read 2026-11-21: nothing more is published.
			return nil, status.Error(codes.FailedPrecondition, "The NetworkPolicy add-on cannot be disabled while network policy is enabled: disable network policy first.")
		}
	}
	next := proto.Clone(cur).(*containerpb.AddonsConfig)
	if npc := in.GetNetworkPolicyConfig(); npc != nil {
		next.NetworkPolicyConfig = proto.Clone(npc).(*containerpb.NetworkPolicyConfig)
	}
	if fc := in.GetGcpFilestoreCsiDriverConfig(); fc != nil {
		next.GcpFilestoreCsiDriverConfig = proto.Clone(fc).(*containerpb.GcpFilestoreCsiDriverConfig)
	}
	c.AddonsConfig = next
	return s.doneOpLocked(p, containerpb.Operation_UPDATE_CLUSTER, c.SelfLink), nil
}

// SetNodePoolManagement sets a pool's auto-upgrade and auto-repair. A pool
// of a cluster on a release channel keeps auto-upgrade ("Node auto-upgrade
// can't be disabled for clusters enrolled in a release channel", UNVERIFIED
// wording, re-read 2026-11-21: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-auto-upgrades).
func (s *Server) SetNodePoolManagement(_ context.Context, req *containerpb.SetNodePoolManagementRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	m := req.GetManagement()
	if m == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"management\" is missing.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.poolLocked(p); err != nil {
		return nil, err
	}
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	if !m.GetAutoUpgrade() && e.cluster.GetReleaseChannel().GetChannel() != containerpb.ReleaseChannel_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "Auto-upgrade must be enabled for node pools of a cluster enrolled in a release channel.")
	}
	np := findPool(e.cluster, p.pool)
	next := &containerpb.NodeManagement{AutoUpgrade: m.GetAutoUpgrade(), AutoRepair: m.GetAutoRepair(), UpgradeOptions: np.GetManagement().GetUpgradeOptions()}
	np.Management = next
	return s.doneOpLocked(p, containerpb.Operation_SET_NODE_POOL_MANAGEMENT, np.SelfLink), nil
}

// checkAutoscaling refuses limits outside Google's rules, and a pool that
// could grow past the local cluster's nodes. zones is the pool's zone
// count, others the cluster's other pools' nodes.
func checkAutoscaling(a *containerpb.NodePoolAutoscaling, zones, others int) error {
	if !a.GetEnabled() {
		return nil
	}
	// UNVERIFIED wordings; the limits are the documented ones (re-read 2026-11-21:
	// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/cluster-autoscaler).
	if a.GetAutoprovisioned() {
		return status.Error(codes.Unimplemented, "Node auto-provisioning is not available locally.")
	}
	perZone := a.GetMinNodeCount() != 0 || a.GetMaxNodeCount() != 0
	total := a.GetTotalMinNodeCount() != 0 || a.GetTotalMaxNodeCount() != 0
	if perZone && total {
		return status.Error(codes.InvalidArgument, "Per-zone (min_node_count, max_node_count) and total (total_min_node_count, total_max_node_count) limits cannot both be set.")
	}
	lo, hi := int(a.GetMinNodeCount())*zones, int(a.GetMaxNodeCount())*zones
	if total {
		lo, hi = int(a.GetTotalMinNodeCount()), int(a.GetTotalMaxNodeCount())
	}
	if lo < 0 || hi < 0 {
		return status.Error(codes.InvalidArgument, "Autoscaling node counts must not be negative.")
	}
	if hi < 1 {
		return status.Error(codes.InvalidArgument, "The maximum node count must be at least 1.")
	}
	if lo > hi {
		return status.Error(codes.InvalidArgument, "The minimum node count must not exceed the maximum node count.")
	}
	if others+hi > MaxNodesPerCluster {
		return status.Errorf(codes.ResourceExhausted, "The node pool could grow to %d nodes, making the cluster %d; a local cluster may have at most %d.", hi, others+hi, MaxNodesPerCluster)
	}
	return nil
}

// normAutoscaling is the autoscaling a pool records: BALANCED when no
// location policy is given, and nothing but disabled when it is off.
func normAutoscaling(a *containerpb.NodePoolAutoscaling) *containerpb.NodePoolAutoscaling {
	if !a.GetEnabled() {
		return &containerpb.NodePoolAutoscaling{}
	}
	out := proto.Clone(a).(*containerpb.NodePoolAutoscaling)
	if out.LocationPolicy == containerpb.NodePoolAutoscaling_LOCATION_POLICY_UNSPECIFIED {
		out.LocationPolicy = containerpb.NodePoolAutoscaling_BALANCED
	}
	return out
}

// SetNodePoolAutoscaling sets the limits the cluster autoscaler scales a
// pool within (autoscale.go).
func (s *Server) SetNodePoolAutoscaling(_ context.Context, req *containerpb.SetNodePoolAutoscalingRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	a := req.GetAutoscaling()
	if a == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"autoscaling\" is missing.")
	}
	if _, err := s.controller(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.poolLocked(p); err != nil {
		return nil, err
	}
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	np := findPool(e.cluster, p.pool)
	if err := checkAutoscaling(a, len(np.Locations), clusterNodes(e.cluster)-poolNodes(np, len(np.Locations))); err != nil {
		return nil, err
	}
	np.Autoscaling = normAutoscaling(a)
	return s.doneOpLocked(p, containerpb.Operation_UPDATE_CLUSTER, np.SelfLink), nil
}

// GetJSONWebKeys is the public half of the keys the cluster signs its
// service account tokens with: its API server's JWKS (/openid/v1/jwks).
func (s *Server) GetJSONWebKeys(ctx context.Context, req *containerpb.GetJSONWebKeysRequest) (*containerpb.GetJSONWebKeysResponse, error) {
	p, err := parseName(req.GetParent(), "", "", "", "", "", "cluster")
	if err != nil {
		return nil, err
	}
	ctl, err := s.controller()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	e, err := s.lookupLocked(p)
	var kind string
	if err == nil {
		kind = e.kind
		if e.access.Endpoint == "" {
			err = busyErr(e)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b, err := ctl.Get(ctx, kind, "/openid/v1/jwks")
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "The cluster's API server did not answer: %v", err)
	}
	var doc struct {
		Keys []struct {
			Kty, Alg, Use, Kid, N, E, X, Y, Crv string
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, status.Errorf(codes.Internal, "The cluster's JWKS is not JSON: %v", err)
	}
	resp := &containerpb.GetJSONWebKeysResponse{}
	for _, k := range doc.Keys {
		resp.Keys = append(resp.Keys, &containerpb.Jwk{Kty: k.Kty, Alg: k.Alg, Use: k.Use, Kid: k.Kid, N: k.N, E: k.E, X: k.X, Y: k.Y, Crv: k.Crv})
	}
	return resp, nil
}

// ---- CheckAutopilotCompatibility ----

// autopilotCapabilities are the Linux capabilities Autopilot lets a
// container add: the container runtime's defaults and SYS_PTRACE (re-read
// 2026-11-21: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security).
var autopilotCapabilities = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true, "KILL": true, "MKNOD": true,
	"NET_BIND_SERVICE": true, "NET_RAW": true, "SETFCAP": true, "SETGID": true, "SETPCAP": true, "SETUID": true, "SYS_CHROOT": true, "SYS_PTRACE": true,
}

// systemNamespace is a namespace of the cluster's own components, which
// the check leaves out.
func systemNamespace(ns string) bool {
	return ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease" || ns == "local-path-storage" || strings.HasPrefix(ns, "gke-")
}

type k8sPod struct {
	Metadata struct {
		Name, Namespace string
	} `json:"metadata"`
	Spec struct {
		HostNetwork bool `json:"hostNetwork"`
		HostPID     bool `json:"hostPID"`
		HostIPC     bool `json:"hostIPC"`
		Volumes     []struct {
			Name     string `json:"name"`
			HostPath *struct {
				Path string `json:"path"`
			} `json:"hostPath"`
		} `json:"volumes"`
		Containers     []k8sContainer `json:"containers"`
		InitContainers []k8sContainer `json:"initContainers"`
	} `json:"spec"`
}

type k8sContainer struct {
	Name            string `json:"name"`
	SecurityContext *struct {
		Privileged   *bool `json:"privileged"`
		Capabilities *struct {
			Add []string `json:"add"`
		} `json:"capabilities"`
	} `json:"securityContext"`
	VolumeMounts []struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"readOnly"`
	} `json:"volumeMounts"`
}

// autopilotChecks are the Autopilot constraints the check applies, by
// constraint name as Autopilot's admission names them in its denials.
var autopilotChecks = []struct {
	constraint, description string
	violates                func(p k8sPod) bool
}{
	{"autogke-disallow-privilege", "Autopilot does not allow privileged containers.", func(p k8sPod) bool {
		for _, c := range append(p.Spec.Containers, p.Spec.InitContainers...) {
			if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
				return true
			}
		}
		return false
	}},
	{"autogke-disallow-hostnamespaces", "Autopilot does not allow the host's network, PID or IPC namespace (hostNetwork, hostPID, hostIPC).", func(p k8sPod) bool {
		return p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC
	}},
	{"autogke-no-write-mode-hostpath", "Autopilot allows hostPath volumes only read-only, under /var/log/.", func(p k8sPod) bool {
		host := map[string]string{}
		for _, v := range p.Spec.Volumes {
			if v.HostPath != nil {
				host[v.Name] = v.HostPath.Path
			}
		}
		for _, c := range append(p.Spec.Containers, p.Spec.InitContainers...) {
			for _, m := range c.VolumeMounts {
				if path, ok := host[m.Name]; ok && (!m.ReadOnly || !strings.HasPrefix(strings.TrimSuffix(path, "/")+"/", "/var/log/")) {
					return true
				}
			}
		}
		return false
	}},
	{"autogke-default-linux-capabilities", "Autopilot allows only the default Linux capabilities and SYS_PTRACE.", func(p k8sPod) bool {
		for _, c := range append(p.Spec.Containers, p.Spec.InitContainers...) {
			if sc := c.SecurityContext; sc != nil && sc.Capabilities != nil {
				for _, a := range sc.Capabilities.Add {
					if !autopilotCapabilities[strings.TrimPrefix(strings.ToUpper(a), "CAP_")] {
						return true
					}
				}
			}
		}
		return false
	}},
}

// CheckAutopilotCompatibility reads a Standard cluster's pods and reports
// what Autopilot would refuse of them: an INCOMPATIBILITY issue per
// constraint, naming the pods (namespace/name) that break it.
func (s *Server) CheckAutopilotCompatibility(ctx context.Context, req *containerpb.CheckAutopilotCompatibilityRequest) (*containerpb.CheckAutopilotCompatibilityResponse, error) {
	p, err := parseName(req.GetName(), "", "", "", "", "", "cluster")
	if err != nil {
		return nil, err
	}
	ctl, err := s.controller()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	e, err := s.lookupLocked(p)
	var kind string
	if err == nil {
		kind = e.kind
		if e.cluster.GetAutopilot().GetEnabled() {
			// UNVERIFIED wording.
			// Docs re-read 2026-11-21: nothing more is published.
			err = status.Error(codes.FailedPrecondition, "The cluster is already an Autopilot cluster.")
		} else if e.access.Endpoint == "" {
			err = busyErr(e)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b, err := ctl.Get(ctx, kind, "/api/v1/pods")
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "The cluster's API server did not answer: %v", err)
	}
	var list struct {
		Items []k8sPod `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, status.Errorf(codes.Internal, "The cluster's pods are not JSON: %v", err)
	}
	now := timestamppb.New(s.now())
	resp := &containerpb.CheckAutopilotCompatibilityResponse{}
	for _, chk := range autopilotChecks {
		var subjects []string
		for _, pod := range list.Items {
			if !systemNamespace(pod.Metadata.Namespace) && chk.violates(pod) {
				subjects = append(subjects, pod.Metadata.Namespace+"/"+pod.Metadata.Name)
			}
		}
		if len(subjects) == 0 {
			continue
		}
		sort.Strings(subjects)
		resp.Issues = append(resp.Issues, &containerpb.AutopilotCompatibilityIssue{
			LastObservation:     now,
			ConstraintType:      chk.constraint,
			IncompatibilityType: containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY,
			Subjects:            subjects,
			DocumentationUrl:    "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security",
			Description:         chk.description,
		})
	}
	// UNVERIFIED wording.
	// Docs re-read 2026-11-21: nothing more is published.
	if len(resp.Issues) == 0 {
		resp.Summary = "No incompatibilities found: the cluster's workloads can run on Autopilot."
	} else {
		resp.Summary = strconv.Itoa(len(resp.Issues)) + " Autopilot incompatibilities found."
	}
	return resp, nil
}

// ---- ListUsableSubnetworks ----

// UsableSubnet is a Compute Engine subnetwork a cluster can use.
type UsableSubnet struct {
	// Subnetwork is projects/P/regions/R/subnetworks/S, Network
	// projects/P/global/networks/N.
	Subnetwork, Network, IPCIDRRange string
	// Secondary are its secondary ranges, name and range.
	Secondary [][2]string
}

// SubnetLister is a Networks that lists a project's subnetworks.
type SubnetLister interface {
	UsableSubnetworks(project string) []UsableSubnet
}

// maxUsablePage is ListUsableSubnetworks' largest and default page.
const maxUsablePage = 500

// ListUsableSubnetworks lists the subnetworks of the filter's project
// (networkProjectId=P, the parent's when absent) that clusters can use,
// with the status of each secondary range: a range a cluster of the
// parent's project uses for its pods or services is IN_USE_*.
func (s *Server) ListUsableSubnetworks(_ context.Context, req *containerpb.ListUsableSubnetworksRequest) (*containerpb.ListUsableSubnetworksResponse, error) {
	parts := strings.Split(req.GetParent(), "/")
	if len(parts) != 2 || parts[0] != "projects" || parts[1] == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid resource name %q: want projects/PROJECT.", req.GetParent())
	}
	project := parts[1]
	network := project
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		k, v, ok := strings.Cut(f, "=")
		if !ok || strings.TrimSpace(k) != "networkProjectId" || strings.TrimSpace(v) == "" {
			// "Only networkProjectId is supported" (re-read 2026-11-21:
			// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.aggregated.usableSubnetworks/list).
			return nil, status.Errorf(codes.InvalidArgument, "Invalid filter %q: only networkProjectId=[PROJECTID] is supported.", f)
		}
		network = strings.Trim(strings.TrimSpace(v), `"`)
	}
	size := int(req.GetPageSize())
	if size < 0 || size > maxUsablePage {
		return nil, status.Errorf(codes.InvalidArgument, "page_size must be between 0 and %d.", maxUsablePage)
	}
	if size == 0 {
		size = maxUsablePage
	}
	start := 0
	if t := req.GetPageToken(); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid page_token %q.", t)
		}
		start = n
	}
	var subnets []UsableSubnet
	if s.opts.Networks != nil {
		if l, ok := s.opts.Networks().(SubnetLister); ok {
			subnets = l.UsableSubnetworks(network)
		}
	}
	sort.Slice(subnets, func(i, j int) bool { return subnets[i].Subnetwork < subnets[j].Subnetwork })
	// The secondary ranges the project's clusters use.
	use := map[string]containerpb.UsableSubnetworkSecondaryRange_Status{}
	s.mu.Lock()
	for _, e := range s.clusters {
		if e.project != project {
			continue
		}
		c := e.cluster
		sub := c.GetNetworkConfig().GetSubnetwork()
		if ip := c.GetIpAllocationPolicy(); ip != nil {
			if n := ip.GetServicesSecondaryRangeName(); n != "" {
				use[sub+"#"+n] = containerpb.UsableSubnetworkSecondaryRange_IN_USE_SERVICE
			}
			if n := ip.GetClusterSecondaryRangeName(); n != "" {
				use[sub+"#"+n] = containerpb.UsableSubnetworkSecondaryRange_IN_USE_SHAREABLE_POD
			}
		}
	}
	s.mu.Unlock()
	resp := &containerpb.ListUsableSubnetworksResponse{}
	for i := start; i < len(subnets) && len(resp.Subnetworks) < size; i++ {
		sn := subnets[i]
		u := &containerpb.UsableSubnetwork{Subnetwork: sn.Subnetwork, Network: sn.Network, IpCidrRange: sn.IPCIDRRange}
		for _, r := range sn.Secondary {
			st, ok := use[sn.Subnetwork+"#"+r[0]]
			if !ok {
				st = containerpb.UsableSubnetworkSecondaryRange_UNUSED
			}
			u.SecondaryIpRanges = append(u.SecondaryIpRanges, &containerpb.UsableSubnetworkSecondaryRange{RangeName: r[0], IpCidrRange: r[1], Status: st})
		}
		resp.Subnetworks = append(resp.Subnetworks, u)
		if i+1 < len(subnets) && len(resp.Subnetworks) == size {
			resp.NextPageToken = strconv.Itoa(i + 1)
		}
	}
	return resp, nil
}

// checkCreateSettings checks and completes a new cluster's network policy,
// add-ons and legacy ABAC: the network policy add-on is enabled with
// enforcement (as gcloud's --enable-network-policy sets both) and disabled
// otherwise, as a Standard cluster's default is.
func checkCreateSettings(c *containerpb.Cluster) error {
	autopilot := c.GetAutopilot().GetEnabled()
	// The Filestore CSI driver is "enabled by default on new Autopilot and
	// Standard clusters" (filestore-csi-driver, read 2026-11-23); it runs
	// against the served Filestore API (#2192, internal/filestorecsi).
	if c.AddonsConfig == nil {
		c.AddonsConfig = &containerpb.AddonsConfig{}
	}
	if c.AddonsConfig.GcpFilestoreCsiDriverConfig == nil {
		c.AddonsConfig.GcpFilestoreCsiDriverConfig = &containerpb.GcpFilestoreCsiDriverConfig{Enabled: true}
	}
	if c.GetLegacyAbac().GetEnabled() && autopilot {
		return status.Error(codes.InvalidArgument, "Legacy ABAC can't be enabled in Autopilot clusters.")
	}
	if autopilot {
		return nil
	}
	if np := c.GetNetworkPolicy(); np.GetEnabled() && np.GetProvider() == containerpb.NetworkPolicy_PROVIDER_UNSPECIFIED {
		np.Provider = containerpb.NetworkPolicy_CALICO
	}
	if c.AddonsConfig == nil {
		c.AddonsConfig = &containerpb.AddonsConfig{}
	}
	if c.AddonsConfig.NetworkPolicyConfig == nil {
		c.AddonsConfig.NetworkPolicyConfig = &containerpb.NetworkPolicyConfig{Disabled: !c.GetNetworkPolicy().GetEnabled()}
	}
	if c.GetNetworkPolicy().GetEnabled() && c.AddonsConfig.NetworkPolicyConfig.Disabled {
		return status.Error(codes.InvalidArgument, "Network policy cannot be enabled while the NetworkPolicy add-on is disabled.")
	}
	return nil
}

// applyCreateSettings makes a new cluster's network policy enforcement and
// legacy ABAC, with a Controller.
func (s *Server) applyCreateSettings(ctx context.Context, kind string, enforce, abac bool) error {
	ctl, ok := s.backend.(Controller)
	if !ok {
		return nil
	}
	if err := ctl.SetNetworkPolicy(ctx, kind, enforce); err != nil {
		return status.Errorf(codes.Internal, "Setting network policy enforcement failed: %v", err)
	}
	if abac {
		if err := ctl.SetLegacyAbac(ctx, kind, true); err != nil {
			return status.Errorf(codes.Internal, "Enabling legacy ABAC failed: %v", err)
		}
	}
	return nil
}

// FilestoreCSICluster is a running cluster whose Filestore CSI driver
// add-on is enabled (#2192).
type FilestoreCSICluster struct {
	// Name is projects/P/locations/L/clusters/C; Kind its kind cluster.
	Name, Kind                 string
	Project, Location, Network string
}

// FilestoreCSIClusters are the running clusters whose
// GcpFilestoreCsiDriver add-on is enabled.
func (s *Server) FilestoreCSIClusters() []FilestoreCSICluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FilestoreCSICluster
	for _, e := range s.clusters {
		c := e.cluster
		if c.GetStatus() != containerpb.Cluster_RUNNING || !c.GetAddonsConfig().GetGcpFilestoreCsiDriverConfig().GetEnabled() {
			continue
		}
		network := c.GetNetwork()
		if network == "" {
			network = "default"
		}
		out = append(out, FilestoreCSICluster{Name: "projects/" + e.project + "/locations/" + c.GetLocation() + "/clusters/" + c.GetName(),
			Kind: e.kind, Project: e.project, Location: c.GetLocation(), Network: network})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
