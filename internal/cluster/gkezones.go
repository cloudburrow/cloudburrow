package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The zones of a Kubernetes Engine cluster's nodes (#2100). GKE gives every
// node pool its node count in each of the cluster's zones and labels each
// node with its zone (topology.kubernetes.io/zone and region); removing a
// zone deletes that zone's nodes, and the others keep theirs (re-read
// 2026-11-22: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managing-clusters,
// "nodes are immediately added or removed"). Locally a zone is a label: each
// pool keeps its zones and the zone each node was given, in the cluster's
// directory. A new node goes to the zone with the fewest of the pool's
// nodes; a pool made smaller loses nodes of zones it no longer has first,
// then of the zone with the most nodes, so the counts per zone stay even.

// zoneMu serialises the zone files' read-modify-write.
var zoneMu sync.Mutex

// poolZones is a pool's zone file: its zones, and each node's; and the
// Kubernetes labels and taints of its nodes (ConfigureNodes).
type poolZones struct {
	Zones  []string          `json:"zones"`
	Nodes  map[string]string `json:"nodes,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Taints []NodeTaint       `json:"taints,omitempty"`
	// Kubelet is the pool's kubelet settings, KubeletConfiguration
	// fields its nodes join with.
	Kubelet map[string]any `json:"kubelet,omitempty"`
	// Machine is the machine type's resources its nodes are given.
	Machine *NodeMachine `json:"machine,omitempty"`
}

// NodeTaint is a Kubernetes taint of a pool's nodes: Effect is NoSchedule,
// PreferNoSchedule or NoExecute.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

func (t NodeTaint) spec() string {
	if t.Value == "" {
		return t.Key + ":" + t.Effect
	}
	return t.Key + "=" + t.Value + ":" + t.Effect
}

// poolNodeConfig is the kubelet node-labels suffix and the kubeadm taints
// a new node of the pool registers with.
func (g *GKE) poolNodeConfig(kind, pool string) (string, []NodeTaint) {
	zoneMu.Lock()
	pz := g.readZones(kind, pool)
	zoneMu.Unlock()
	keys := make([]string, 0, len(pz.Labels))
	for k := range pz.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("," + k + "=" + pz.Labels[k])
	}
	return b.String(), pz.Taints
}

// ConfigureKubelet records pool's kubelet settings, KubeletConfiguration
// fields (GKE's kubelet_config, #2100), for the nodes it gets from now on:
// GKE re-creates the nodes for a change, which the caller does.
func (g *GKE) ConfigureKubelet(_ context.Context, kind, pool string, settings map[string]any) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	zoneMu.Lock()
	defer zoneMu.Unlock()
	pz := g.readZones(kind, pool)
	pz.Kubelet = settings
	return g.writeZones(kind, pool, pz)
}

// ConfigureMachine records the machine pool's nodes get from now on
// (#2100): the node container's CPU and memory limits, and the kubelet's
// reservations, so the node's allocatable resources are a GKE node's of
// that machine type.
func (g *GKE) ConfigureMachine(_ context.Context, kind, pool string, m NodeMachine) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	zoneMu.Lock()
	defer zoneMu.Unlock()
	pz := g.readZones(kind, pool)
	pz.Machine = &m
	return g.writeZones(kind, pool, pz)
}

// poolMachine is pool's recorded machine, nil for none.
func (g *GKE) poolMachine(kind, pool string) *NodeMachine {
	zoneMu.Lock()
	defer zoneMu.Unlock()
	return g.readZones(kind, pool).Machine
}

// poolKubelet is pool's recorded kubelet settings.
func (g *GKE) poolKubelet(kind, pool string) map[string]any {
	zoneMu.Lock()
	defer zoneMu.Unlock()
	return g.readZones(kind, pool).Kubelet
}

// ConfigureNodes sets the Kubernetes labels and taints of pool's nodes,
// as GKE's node_config.labels and taints (#2100): recorded for the nodes
// it gets later, which register with them, and, when apply, set on its
// nodes now, the labels and taints it no longer has removed. A change the
// nodes are recreated for (a machine type) is recorded only.
func (g *GKE) ConfigureNodes(ctx context.Context, kind, pool string, labels map[string]string, taints []NodeTaint, apply bool) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	zoneMu.Lock()
	pz := g.readZones(kind, pool)
	old, oldTaints := pz.Labels, pz.Taints
	pz.Labels, pz.Taints = labels, taints
	err := g.writeZones(kind, pool, pz)
	zoneMu.Unlock()
	if err != nil || !apply {
		return err
	}
	nodes, err := g.poolNodes(ctx, kind, pool)
	if err != nil {
		return err
	}
	label := []string{"--overwrite"}
	for k := range old {
		if _, ok := labels[k]; !ok {
			label = append(label, k+"-")
		}
	}
	for k, v := range labels {
		label = append(label, k+"="+v)
	}
	var untaint []string
	for _, t := range oldTaints {
		if !slices.Contains(taints, t) {
			untaint = append(untaint, t.Key+":"+t.Effect+"-")
		}
	}
	var taint []string
	for _, t := range taints {
		taint = append(taint, t.spec())
	}
	kc := g.kubeconfig(kind)
	for _, n := range nodes {
		if len(label) > 1 {
			if _, err := g.runner().Run(ctx, "kubectl", append([]string{"--kubeconfig", kc, "label", "node", n}, label...)...); err != nil {
				return fmt.Errorf("label node %s: %w", n, err)
			}
		}
		if len(untaint) > 0 {
			// A taint already gone is not an error worth stopping for.
			_, _ = g.runner().Run(ctx, "kubectl", append([]string{"--kubeconfig", kc, "taint", "node", n}, untaint...)...)
		}
		if len(taint) > 0 {
			if _, err := g.runner().Run(ctx, "kubectl", append([]string{"--kubeconfig", kc, "taint", "node", n, "--overwrite"}, taint...)...); err != nil {
				return fmt.Errorf("taint node %s: %w", n, err)
			}
		}
	}
	return nil
}

// zonesFile keeps a pool's zones and its nodes' (PlaceNodes).
func (g *GKE) zonesFile(kind, pool string) string {
	sum := sha256.Sum256([]byte(pool))
	return filepath.Join(g.Dir, kind, "zones-"+hex.EncodeToString(sum[:])[:8])
}

// readZones reads a pool's zone file; one written before nodes had their
// own zones (a list of zones, one per line) has no node map.
func (g *GKE) readZones(kind, pool string) poolZones {
	var pz poolZones
	b, err := os.ReadFile(g.zonesFile(kind, pool))
	if err != nil {
		return pz
	}
	if json.Unmarshal(b, &pz) != nil {
		pz = poolZones{Zones: strings.Fields(string(b))}
	}
	return pz
}

func (g *GKE) writeZones(kind, pool string, pz poolZones) error {
	b, err := json.Marshal(pz)
	if err != nil {
		return err
	}
	return os.WriteFile(g.zonesFile(kind, pool), b, 0o600)
}

// leastZone is the zone of pz's with the fewest nodes, other than skip;
// the first of the zones on a tie.
func (pz poolZones) leastZone(skip string) string {
	count := map[string]int{}
	for n, z := range pz.Nodes {
		if n != skip {
			count[z]++
		}
	}
	best := ""
	for _, z := range pz.Zones {
		if best == "" || count[z] < count[best] {
			best = z
		}
	}
	return best
}

// nodeZone is the zone of a pool's node: the one it was given, or, for a
// node with none (a new one), the pool's zone with the fewest nodes,
// recorded. "" when the pool has no zones recorded.
func (g *GKE) nodeZone(kind, pool, name string) string {
	zoneMu.Lock()
	defer zoneMu.Unlock()
	pz := g.readZones(kind, pool)
	if len(pz.Zones) == 0 {
		return ""
	}
	if z, ok := pz.Nodes[name]; ok {
		return z
	}
	z := pz.leastZone(name)
	if pz.Nodes == nil {
		pz.Nodes = map[string]string{}
	}
	pz.Nodes[name] = z
	_ = g.writeZones(kind, pool, pz)
	return z
}

// forgetNode drops a removed node from its pool's zone file.
func (g *GKE) forgetNode(kind, pool, name string) {
	zoneMu.Lock()
	defer zoneMu.Unlock()
	pz := g.readZones(kind, pool)
	if _, ok := pz.Nodes[name]; ok {
		delete(pz.Nodes, name)
		_ = g.writeZones(kind, pool, pz)
	}
}

// zoneLabels is the kubelet node-labels suffix of a node's zone.
func (g *GKE) zoneLabels(kind, pool, name string) string {
	z := g.nodeZone(kind, pool, name)
	if z == "" {
		return ""
	}
	return ",topology.kubernetes.io/zone=" + z + ",topology.kubernetes.io/region=" + regionOfZone(z)
}

func regionOfZone(z string) string { return z[:strings.LastIndex(z, "-")] }

// shrinkOrder is the order Resize removes a pool's nodes in (have, by
// index): first the nodes of zones the pool no longer has, then one at a
// time from the zone with the most nodes left, its highest index first.
// With no zones recorded it is the highest index first.
func (g *GKE) shrinkOrder(kind, pool string, have []string) []string {
	zoneMu.Lock()
	pz := g.readZones(kind, pool)
	zoneMu.Unlock()
	left := slices.Clone(have)
	var out []string
	if len(pz.Zones) == 0 {
		for i := len(left) - 1; i >= 0; i-- {
			out = append(out, left[i])
		}
		return out
	}
	zone := func(n string) string { return pz.Nodes[n] }
	for i := len(left) - 1; i >= 0; i-- {
		if z := zone(left[i]); z != "" && !slices.Contains(pz.Zones, z) {
			out = append(out, left[i])
			left = slices.Delete(left, i, i+1)
		}
	}
	for len(left) > 0 {
		count := map[string]int{}
		for _, n := range left {
			count[zone(n)]++
		}
		pick := len(left) - 1
		for i := len(left) - 1; i >= 0; i-- {
			if count[zone(left[i])] > count[zone(left[pick])] {
				pick = i
			}
		}
		out = append(out, left[pick])
		left = slices.Delete(left, pick, pick+1)
	}
	return out
}

// outsideZones are pool's nodes (have) given a zone the pool no longer
// has: a zone removed with SetLocations, whose nodes GKE deletes.
func (g *GKE) outsideZones(kind, pool string, have []string) []string {
	zoneMu.Lock()
	pz := g.readZones(kind, pool)
	zoneMu.Unlock()
	var out []string
	for _, n := range have {
		if z := pz.Nodes[n]; z != "" && len(pz.Zones) > 0 && !slices.Contains(pz.Zones, z) {
			out = append(out, n)
		}
	}
	return out
}

// PlaceNodes records pool's zones, which the nodes it gets later are
// given in turn, and labels its nodes now with theirs
// (topology.kubernetes.io/zone and topology.kubernetes.io/region). A node
// keeps the zone it has while the pool still has it; one in a zone the
// pool no longer has keeps that zone's label until Resize deletes it; one
// with no zone yet (a cluster made before nodes had their own) is given
// the zone with the fewest nodes.
func (g *GKE) PlaceNodes(ctx context.Context, kind, pool string, zones []string) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	nodes, err := g.poolNodes(ctx, kind, pool)
	if err != nil {
		return err
	}
	zoneMu.Lock()
	pz := g.readZones(kind, pool)
	pz.Zones = slices.Clone(zones)
	live := map[string]bool{}
	for _, n := range nodes {
		live[n] = true
	}
	for n := range pz.Nodes {
		if !live[n] {
			delete(pz.Nodes, n)
		}
	}
	err = g.writeZones(kind, pool, pz)
	zoneMu.Unlock()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		z := g.nodeZone(kind, pool, n)
		if z == "" {
			continue
		}
		if _, err := g.runner().Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "label", "node", n, "--overwrite",
			"topology.kubernetes.io/zone="+z, "topology.kubernetes.io/region="+regionOfZone(z)); err != nil {
			return fmt.Errorf("label node %s's zone: %w", n, err)
		}
	}
	return nil
}
