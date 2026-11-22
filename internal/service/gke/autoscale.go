package gke

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The cluster autoscaler and node auto-repair (#2100), run by the Server
// over each RUNNING Standard cluster, as GKE runs them for its node pools:
//
//   - Scale-up: "If Pods fail to be scheduled on any of the current nodes,"
//     a node is added (one per zone of the pool) to an autoscaled pool
//     whose maximum allows it, the pool the pod asks for by
//     cloud.google.com/gke-nodepool, else the first. As GKE's, the decision
//     is on resource requests: a pod no new node could hold (its requests
//     above a node's allocatable, or a node selector no pool's nodes have)
//     adds none (re-read 2026-11-21:
//     https://docs.cloud.google.com/kubernetes-engine/docs/concepts/cluster-autoscaler).
//   - Scale-down: a pool's node that has run no pod but DaemonSets' and
//     static pods for ScaleDownUnneeded (the open-source cluster
//     autoscaler's --scale-down-unneeded-time, ten minutes) is drained and
//     removed, never below the pool's minimum. GKE also moves the pods of
//     an underused node to make it empty; locally only empty nodes go.
//   - Auto-repair: a node of a pool with auto-repair that has been NotReady
//     (or has reported no status) for RepairAfter, "approximately 10
//     minutes" (re-read 2026-11-21:
//     https://docs.cloud.google.com/kubernetes-engine/docs/how-to/node-auto-repair),
//     is re-created with the same name, in an AUTO_REPAIR_NODES operation.
//
// GKE's autoscaler makes no operation; its resizes are not either.

// DefaultScaleDownUnneeded is how long a node is empty before the
// autoscaler removes it.
const DefaultScaleDownUnneeded = 10 * time.Minute

// DefaultRepairAfter is how long a node is NotReady before it is repaired.
const DefaultRepairAfter = 10 * time.Minute

// scaleUpCooldown is how long after a scale-up the autoscaler waits before
// another, so the new node's pods are scheduled first.
const scaleUpCooldown = 30 * time.Second

// autoscalerBusy marks a cluster the autoscaler is resizing: other changes
// wait, as they do for an operation.
const autoscalerBusy = "cluster-autoscaler"

// autoscaleState is what the loop remembers of a cluster.
type autoscaleState struct {
	lastScaleUp time.Time
	// emptySince is when each node was first seen empty.
	emptySince map[string]time.Time
}

type k8sNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

type k8sPodState struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		Annotations     map[string]string `json:"annotations"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName     string            `json:"nodeName"`
		NodeSelector map[string]string `json:"nodeSelector"`
		Containers   []struct {
			Resources struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

func (p k8sPodState) unschedulable() bool {
	if p.Status.Phase != "Pending" || p.Spec.NodeName != "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			return true
		}
	}
	return false
}

// occupies is whether the pod keeps its node from being removed: anything
// running but a DaemonSet's pod or a static (mirror) pod.
func (p k8sPodState) occupies() bool {
	if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
		return false
	}
	if _, ok := p.Metadata.Annotations["kubernetes.io/config.mirror"]; ok {
		return false
	}
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

// requests is the pod's summed CPU (millicores) and memory (bytes).
func (p k8sPodState) requests() (int64, int64) {
	var cpu, mem int64
	for _, c := range p.Spec.Containers {
		cpu += milli(c.Resources.Requests["cpu"])
		mem += bytesOf(c.Resources.Requests["memory"])
	}
	return cpu, mem
}

// milli is a CPU quantity in millicores.
func milli(q string) int64 {
	if q == "" {
		return 0
	}
	if strings.HasSuffix(q, "m") {
		return int64(parseNum(strings.TrimSuffix(q, "m")))
	}
	return int64(parseNum(q) * 1000)
}

// bytesOf is a memory quantity in bytes.
func bytesOf(q string) int64 {
	if q == "" {
		return 0
	}
	units := []struct {
		suffix string
		mult   float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			return int64(parseNum(strings.TrimSuffix(q, u.suffix)) * u.mult)
		}
	}
	return int64(parseNum(q))
}

func parseNum(s string) float64 {
	var f float64
	_ = json.Unmarshal([]byte(s), &f)
	return f
}

// ready is the node's Ready condition and when it last changed; a node
// with none has reported no status.
func (n k8sNode) ready() (bool, time.Time, bool) {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True", c.LastTransitionTime, true
		}
	}
	return false, time.Time{}, false
}

// poolLimits are an autoscaled pool's total minimum and maximum nodes.
func poolLimits(np *containerpb.NodePool) (int, int) {
	a := np.GetAutoscaling()
	zones := len(np.GetLocations())
	if a.GetTotalMaxNodeCount() != 0 || a.GetTotalMinNodeCount() != 0 {
		return int(a.GetTotalMinNodeCount()), int(a.GetTotalMaxNodeCount())
	}
	return int(a.GetMinNodeCount()) * zones, int(a.GetMaxNodeCount()) * zones
}

// autoscaleLoop runs Autoscale every interval until the Server closes.
func (s *Server) autoscaleLoop(every time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-t.C:
			s.Autoscale(s.bg)
		}
	}
}

// Autoscale is one pass of the cluster autoscaler and auto-repair over
// every cluster; it returns the auto-repair operations it started.
func (s *Server) Autoscale(ctx context.Context) []*containerpb.Operation {
	ctl, ok := s.backend.(Controller)
	if !ok {
		return nil
	}
	type work struct {
		key  string
		kind string
	}
	s.mu.Lock()
	var todo []work
	for k, e := range s.clusters {
		c := e.cluster
		if c.GetAutopilot().GetEnabled() || c.Status != containerpb.Cluster_RUNNING || e.busy != "" || e.access.Endpoint == "" {
			continue
		}
		for _, np := range c.NodePools {
			if np.GetAutoscaling().GetEnabled() || np.GetManagement().GetAutoRepair() {
				todo = append(todo, work{k, e.kind})
				break
			}
		}
	}
	s.mu.Unlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i].key < todo[j].key })
	var ops []*containerpb.Operation
	for _, w := range todo {
		nb, err := ctl.Get(ctx, w.kind, "/api/v1/nodes")
		if err != nil {
			continue
		}
		pb, err := ctl.Get(ctx, w.kind, "/api/v1/pods")
		if err != nil {
			continue
		}
		var nodes struct {
			Items []k8sNode `json:"items"`
		}
		var pods struct {
			Items []k8sPodState `json:"items"`
		}
		if json.Unmarshal(nb, &nodes) != nil || json.Unmarshal(pb, &pods) != nil {
			continue
		}
		if op := s.repairOnce(w.key, ctl, nodes.Items); op != nil {
			ops = append(ops, op)
			continue
		}
		s.scaleOnce(ctx, w.key, nodes.Items, pods.Items)
	}
	return ops
}

// repairOnce starts the repair of the first node of an auto-repair pool
// NotReady for RepairAfter, or returns nil.
func (s *Server) repairOnce(key string, ctl Controller, nodes []k8sNode) *containerpb.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.clusters[key]
	if e == nil || e.busy != "" || e.cluster.Status != containerpb.Cluster_RUNNING {
		return nil
	}
	// The wall clock, not s.now(): the kubelet stamps Ready's
	// lastTransitionTime in real time, and AdvanceClock (the maintenance
	// windows' clock) moves s.now() ahead of it, which made a node
	// NotReady for a moment read as NotReady for hours and repaired at
	// once, holding the cluster busy ("Cluster is running incompatible
	// operation").
	now := s.opts.Now()
	for _, n := range nodes {
		np := findPool(e.cluster, n.Metadata.Labels["cloud.google.com/gke-nodepool"])
		if np == nil || !np.GetManagement().GetAutoRepair() || np.Status != containerpb.NodePool_RUNNING {
			continue
		}
		ok, since, reported := n.ready()
		if ok || (reported && now.Sub(since) < s.repairAfter()) {
			continue
		}
		p, err := parseName(key, "", "", "", "", "", "cluster")
		if err != nil {
			return nil
		}
		c := e.cluster
		c.Status = containerpb.Cluster_RECONCILING
		np.Status = containerpb.NodePool_RECONCILING
		kind, image, pool, name := e.kind, s.NodeImage(np.Version), np.Name, n.Metadata.Name
		return s.startOpLocked(p, containerpb.Operation_AUTO_REPAIR_NODES, np.SelfLink, e, func(ctx context.Context) error {
			s.progress(ctx, func(op *containerpb.Operation) {
				op.Detail = "Repairing node " + name + ": it has been NotReady since " + since.UTC().Format(time.RFC3339) + "."
			})
			if err := ctl.RepairNode(ctx, kind, image, pool, name); err != nil {
				return status.Errorf(codes.Internal, "Repairing node %s failed: %v", name, err)
			}
			return nil
		}, func(err error) {
			c.Status = containerpb.Cluster_RUNNING
			np.Status = containerpb.NodePool_RUNNING
			if err != nil {
				np.Status = containerpb.NodePool_RUNNING_WITH_ERROR
				np.StatusMessage = status.Convert(err).Message()
			}
		})
	}
	return nil
}

func (s *Server) repairAfter() time.Duration {
	if s.opts.RepairAfter > 0 {
		return s.opts.RepairAfter
	}
	return DefaultRepairAfter
}

func (s *Server) scaleDownUnneeded() time.Duration {
	if s.opts.ScaleDownUnneeded > 0 {
		return s.opts.ScaleDownUnneeded
	}
	return DefaultScaleDownUnneeded
}

// fits is whether a pod could run on a new node of pool np, like nodes.
func fits(pod k8sPodState, np *containerpb.NodePool, nodes []k8sNode) bool {
	var model *k8sNode
	for i := range nodes {
		if nodes[i].Metadata.Labels["cloud.google.com/gke-nodepool"] == np.Name {
			model = &nodes[i]
			break
		}
	}
	for k, v := range pod.Spec.NodeSelector {
		switch k {
		case "cloud.google.com/gke-nodepool":
			if v != np.Name {
				return false
			}
		default:
			if model == nil || model.Metadata.Labels[k] != v {
				return false
			}
		}
	}
	if model == nil {
		return true
	}
	cpu, mem := pod.requests()
	if a := milli(model.Status.Allocatable["cpu"]); a > 0 && cpu > a {
		return false
	}
	if a := bytesOf(model.Status.Allocatable["memory"]); a > 0 && mem > a {
		return false
	}
	return true
}

// scaleOnce grows one pool for unschedulable pods, or removes the empty
// nodes of pools above their minimum.
func (s *Server) scaleOnce(ctx context.Context, key string, nodes []k8sNode, pods []k8sPodState) {
	s.mu.Lock()
	e := s.clusters[key]
	if e == nil || e.busy != "" || e.cluster.Status != containerpb.Cluster_RUNNING {
		s.mu.Unlock()
		return
	}
	if e.scale == nil {
		e.scale = &autoscaleState{emptySince: map[string]time.Time{}}
	}
	st := e.scale
	c := e.cluster
	now := s.now()

	// Scale up.
	var grow *containerpb.NodePool
	if now.Sub(st.lastScaleUp) >= scaleUpCooldown {
		for _, pod := range pods {
			if !pod.unschedulable() {
				continue
			}
			for _, np := range c.NodePools {
				if !np.GetAutoscaling().GetEnabled() || np.Status != containerpb.NodePool_RUNNING {
					continue
				}
				zones := len(np.Locations)
				_, hi := poolLimits(np)
				if poolNodes(np, zones)+zones > hi || clusterNodes(c)+zones > MaxNodesPerCluster || !fits(pod, np, nodes) {
					continue
				}
				grow = np
				break
			}
			if grow != nil {
				break
			}
		}
	}
	if grow != nil && s.checkDisk() == nil {
		zones := len(grow.Locations)
		count := grow.InitialNodeCount + 1
		e.busy = autoscalerBusy
		c.Status = containerpb.Cluster_RECONCILING
		kind, image, pool := e.kind, s.NodeImage(grow.Version), grow.Name
		st.lastScaleUp = now
		// The pool's size first, as GKE resizes the pool's instance group
		// before its new node registers: the node is Ready, and its pods
		// run, a moment before Resize returns (join polls Ready every two
		// seconds), and a GetNodePool then read the old size (main run
		// 45023269046, TestGKEClusterSettings: "initialNodeCount 1" with
		// the pool's two nodes running the deployment).
		grow.InitialNodeCount = count
		_ = s.saveLocked()
		s.mu.Unlock()
		err := s.backend.Resize(ctx, kind, image, pool, int(count)*zones)
		s.mu.Lock()
		e.busy = ""
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			grow.InitialNodeCount = count - 1
		}
		c.CurrentNodeCount = int32(clusterNodes(c))
		st.lastScaleUp = s.now()
		_ = s.saveLocked()
		s.mu.Unlock()
		return
	}

	// Scale down: the empty nodes, by pool.
	busy := map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeName != "" && pod.occupies() {
			busy[pod.Spec.NodeName] = true
		}
	}
	anyPending := false
	for _, pod := range pods {
		if pod.unschedulable() {
			anyPending = true
		}
	}
	seen := map[string]bool{}
	var remove []string
	var shrink *containerpb.NodePool
	for _, n := range nodes {
		name := n.Metadata.Name
		seen[name] = true
		np := findPool(c, n.Metadata.Labels["cloud.google.com/gke-nodepool"])
		if np == nil || !np.GetAutoscaling().GetEnabled() || busy[name] {
			delete(st.emptySince, name)
			continue
		}
		if _, ok := st.emptySince[name]; !ok {
			st.emptySince[name] = now
		}
		if anyPending || shrink != nil && shrink != np || now.Sub(st.emptySince[name]) < s.scaleDownUnneeded() {
			continue
		}
		zones := len(np.Locations)
		lo, _ := poolLimits(np)
		if poolNodes(np, zones)-zones < lo || len(remove) == zones {
			continue
		}
		shrink = np
		remove = append(remove, name)
	}
	for name := range st.emptySince {
		if !seen[name] {
			delete(st.emptySince, name)
		}
	}
	u, isUpgrader := s.backend.(Upgrader)
	if shrink == nil || !isUpgrader || len(remove) != len(shrink.Locations) {
		s.mu.Unlock()
		return
	}
	e.busy = autoscalerBusy
	c.Status = containerpb.Cluster_RECONCILING
	kind := e.kind
	s.mu.Unlock()
	var failed error
	for _, name := range remove {
		if err := u.DrainNode(ctx, kind, name); err != nil {
			failed = err
			break
		}
		if err := u.RemoveNode(ctx, kind, name); err != nil {
			failed = err
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.busy = ""
	c.Status = containerpb.Cluster_RUNNING
	if failed == nil {
		shrink.InitialNodeCount--
		for _, name := range remove {
			delete(st.emptySince, name)
		}
	}
	c.CurrentNodeCount = int32(clusterNodes(c))
	_ = s.saveLocked()
}
