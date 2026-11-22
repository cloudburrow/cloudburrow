package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Cloud NAT's address translation (#2140; internal/service/compute/natgateways.go).
// Every few seconds the loop below:
//   - runs one pod per Cloud NAT gateway that translates a VM: the VM
//     image's own vpc agent, which joins the network's VXLAN segment at the
//     subnet's gateway address and translates its VMs' traffic to their NAT
//     IPs and port ranges;
//   - routes each NAT IP, on every kind node, to its gateway's pod, so
//     what comes back to a NAT IP reaches the pod that translated it;
//   - reads each logging gateway's conntrack table and the set of what it
//     dropped for want of ports, and writes each new connection as a
//     compute.googleapis.com/nat_flows entry, as Cloud NAT logs them
//     (nat/docs/monitoring), per the gateway's logConfig filter.

// natGatewayLabel marks a NAT gateway pod.
const natGatewayLabel = "cloudburrow.dev/nat-gateway"

// natRouteProto marks the nodes' NAT IP routes (ip route ... proto).
const natRouteProto = 177

type natTranslation struct {
	svc  *computeService
	last string // the nodes' last applied routing script
	// seen are the connections already logged, by gateway key.
	seen map[string]map[string]bool
}

func (s *computeService) startNAT(ctx context.Context) {
	n := &natTranslation{svc: s, seen: map[string]map[string]bool{}}
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := n.sync(ctx); err != nil && ctx.Err() == nil {
				fmt.Println("cloudburrow: Cloud NAT:", err)
			}
		}
	}()
}

func natPodName(g compute.NATGateway) string { return "cb-nat-" + g.Token[2:14] }

func (n *natTranslation) sync(ctx context.Context) error {
	api, vms := n.svc.API(), n.svc.vms
	if api == nil || vms == nil {
		return nil
	}
	gws := api.NATGateways()
	out, err := vms.kubectl(ctx, "get", "pods", "-l", natGatewayLabel, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, name := range strings.Fields(out) {
		have[name] = true
	}
	want := map[string]bool{}
	ref := ""
	for _, g := range gws {
		name := natPodName(g)
		want[name] = true
		if have[name] {
			continue
		}
		if ref == "" {
			img, ok := gatewayImage()
			if !ok {
				return fmt.Errorf("no Debian image to run NAT gateways")
			}
			if ref, err = vms.ensureImage(ctx, img); err != nil {
				return err
			}
		}
		if err := vms.installer.Apply(ctx, natPodManifest(vms, name, g, ref)); err != nil {
			return err
		}
	}
	for name := range have {
		if !want[name] {
			_, _ = vms.kubectl(ctx, "delete", "pod", name, "--wait=false", "--ignore-not-found")
		}
	}
	if script := natRouteScript(gws); script != n.last {
		if err := n.svc.applyNodes(ctx, script); err != nil {
			return err
		}
		n.last = script
	}
	n.logFlows(ctx, gws)
	return nil
}

func natPodManifest(b *computeVMs, name string, g compute.NATGateway, ref string) string {
	guest := ""
	if b.guestBase != nil {
		if base := b.guestBase(); base != "" {
			guest = base + compute.GuestPrefix + g.Token
		}
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(),
			"labels":      map[string]string{"cloudburrow.dev/owned": "true", natGatewayLabel: g.Token},
			"annotations": map[string]string{"cloudburrow.dev/nat-gateway-key": g.Key}},
		"spec": map[string]any{"restartPolicy": "Always", "terminationGracePeriodSeconds": 1,
			"automountServiceAccountToken": false, "enableServiceLinks": false,
			"containers": []any{map[string]any{"name": "nat", "image": ref, "imagePullPolicy": "Never",
				"command":         []string{"/usr/lib/cloudburrow/vpc"},
				"securityContext": map[string]any{"privileged": true},
				"env": []any{map[string]string{"name": "CLOUDBURROW_GUEST_METADATA", "value": guest},
					map[string]string{"name": "CLOUDBURROW_INTERNAL_IP", "value": g.IP}},
				"resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}}}}}}
	raw, _ := json.Marshal(pod)
	return string(raw)
}

// natRouteScript sets the nodes' NAT IP routes: each gateway's NAT IPs
// to its pod (through the node that runs it, from any other node). The
// earlier ones are flushed first, so the script states the whole set.
func natRouteScript(gws []compute.NATGateway) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ip route flush proto %d 2>/dev/null || true\n", natRouteProto)
	for _, g := range gws {
		if g.Pod == "" {
			continue
		}
		fmt.Fprintf(&b, "r=$(ip -o route get %s)\n", g.Pod)
		b.WriteString("d=$(echo \"$r\" | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p'); v=$(echo \"$r\" | sed -n 's/.* via \\([0-9.]*\\).*/\\1/p')\n")
		for _, ip := range g.NatIPs {
			fmt.Fprintf(&b, "if [ -n \"$v\" ]; then ip route replace %s/32 via \"$v\" dev \"$d\" proto %d; else ip route replace %s/32 via %s dev \"$d\" onlink proto %d; fi\n",
				ip, natRouteProto, ip, g.Pod, natRouteProto)
		}
	}
	return b.String()
}

// applyNodes runs script on each node of the instance's kind cluster.
func (s *computeService) applyNodes(ctx context.Context, script string) error {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+s.cfg.ClusterName(),
		"--format", "{{.Names}}").Output()
	if err != nil {
		return err
	}
	for _, node := range strings.Fields(string(out)) {
		if b, err := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", script).CombinedOutput(); err != nil {
			return fmt.Errorf("route NAT IPs on %s: %w: %s", node, err, vmTail(string(b), 3))
		}
	}
	return nil
}

// natConn is one connection a gateway translated or dropped.
type natConn struct {
	proto                    int
	srcIP, natIP, destIP     string
	srcPort, natPort, dstPrt int
	dropped                  bool
}

func (c natConn) key() string {
	return fmt.Sprintf("%t/%d/%s:%d/%s:%d", c.dropped, c.proto, c.srcIP, c.srcPort, c.destIP, c.dstPrt)
}

// natConntrack are the TCP and UDP connections of a /proc/net/nf_conntrack
// listing whose original source is one of vms and whose reply goes to one
// of natIPs: those the gateway translated.
func natConntrack(listing string, vms, natIPs map[string]bool) []natConn {
	var out []natConn
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "ipv4" {
			continue
		}
		proto, _ := strconv.Atoi(f[3])
		if proto != 6 && proto != 17 {
			continue
		}
		var kv [][2]string
		for _, x := range f[4:] {
			if k, v, ok := strings.Cut(x, "="); ok {
				kv = append(kv, [2]string{k, v})
			}
		}
		get := func(k string, nth int) string {
			for _, p := range kv {
				if p[0] == k {
					if nth == 0 {
						return p[1]
					}
					nth--
				}
			}
			return ""
		}
		c := natConn{proto: proto, srcIP: get("src", 0), destIP: get("dst", 0), natIP: get("dst", 1)}
		c.srcPort, _ = strconv.Atoi(get("sport", 0))
		c.dstPrt, _ = strconv.Atoi(get("dport", 0))
		c.natPort, _ = strconv.Atoi(get("dport", 1))
		if vms[c.srcIP] && natIPs[c.natIP] {
			out = append(out, c)
		}
	}
	return out
}

// natDropped are the elements of the gateway's dropped set, as
// `nft -j list set` prints it.
func natDropped(raw string) []natConn {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	var out []natConn
	for _, it := range doc.Nftables {
		if it.Set == nil {
			continue
		}
		for _, e := range it.Set.Elem {
			var el struct {
				Concat []any `json:"concat"`
				Elem   struct {
					Val struct {
						Concat []any `json:"concat"`
					} `json:"val"`
				} `json:"elem"`
			}
			if json.Unmarshal(e, &el) != nil {
				continue
			}
			v := el.Concat
			if v == nil {
				v = el.Elem.Val.Concat
			}
			if len(v) != 5 {
				continue
			}
			num := func(x any) int {
				switch t := x.(type) {
				case float64:
					return int(t)
				case string:
					return map[string]int{"tcp": 6, "udp": 17}[t]
				}
				return 0
			}
			src, _ := v[0].(string)
			dst, _ := v[2].(string)
			out = append(out, natConn{dropped: true, srcIP: src, srcPort: num(v[1]), destIP: dst, dstPrt: num(v[3]), proto: num(v[4])})
		}
	}
	return out
}

// natLogs reports whether the gateway's logConfig records c.
func natLogs(g compute.NATGateway, c natConn) bool {
	if !g.Log {
		return false
	}
	switch g.LogFilter {
	case "ERRORS_ONLY":
		return c.dropped
	case "TRANSLATIONS_ONLY":
		return !c.dropped
	}
	return true
}

// logFlows writes each logging gateway's new connections.
func (n *natTranslation) logFlows(ctx context.Context, gws []compute.NATGateway) {
	live := map[string]bool{}
	for _, g := range gws {
		live[g.Key] = true
		if !g.Log || g.Pod == "" || n.svc.natLogs == nil {
			delete(n.seen, g.Key)
			continue
		}
		out, err := n.svc.vms.kubectl(ctx, "exec", natPodName(g), "--", "sh", "-c",
			"cat /proc/net/nf_conntrack; echo '#dropped'; nft -j list set inet cloudburrow "+compute.NATDroppedSet)
		if err != nil {
			continue
		}
		ct, dropped, _ := strings.Cut(out, "#dropped")
		vmIPs, natIPs := map[string]bool{}, map[string]bool{}
		byIP := map[string]compute.NATVM{}
		for _, v := range g.VMs {
			vmIPs[v.IP], byIP[v.IP] = true, v
		}
		for _, ip := range g.NatIPs {
			natIPs[ip] = true
		}
		conns := append(natConntrack(ct, vmIPs, natIPs), natDropped(dropped)...)
		prev := n.seen[g.Key]
		cur := map[string]bool{}
		var entries []*loggingpb.LogEntry
		for _, c := range conns {
			k := c.key()
			cur[k] = true
			if prev[k] || !natLogs(g, c) {
				continue
			}
			vm, ok := byIP[c.srcIP]
			if !ok {
				continue
			}
			if c.dropped {
				c.natIP = vm.NatIP
			}
			entries = append(entries, natFlowEntry(g, vm, c, time.Now()))
		}
		if len(entries) > 0 {
			if err := n.svc.natLogs(ctx, &loggingpb.WriteLogEntriesRequest{Entries: entries}); err != nil {
				// Not marked seen: written on the next pass.
				continue
			}
		}
		n.seen[g.Key] = cur
	}
	for k := range n.seen {
		if !live[k] {
			delete(n.seen, k)
		}
	}
}

// natFlowEntry is the nat_flows entry of a connection of vm's through g,
// in Cloud NAT's format: the nat_gateway resource, the connection's
// 7-tuple and allocation_status, the gateway, the VM and its network.
func natFlowEntry(g compute.NATGateway, vm compute.NATVM, c natConn, ts time.Time) *loggingpb.LogEntry {
	status := "OK"
	if c.dropped {
		status = "DROPPED"
	}
	network := g.Network[strings.LastIndex(g.Network, "/")+1:]
	subnet := vm.Subnetwork[strings.LastIndex(vm.Subnetwork, "/")+1:]
	region := g.Region
	conn := map[string]any{"src_ip": c.srcIP, "src_port": c.srcPort, "dest_ip": c.destIP, "dest_port": c.dstPrt, "protocol": c.proto}
	if c.natIP != "" {
		conn["nat_ip"] = c.natIP
	}
	if !c.dropped {
		conn["nat_port"] = c.natPort
	}
	payload, _ := structpb.NewStruct(map[string]any{
		"connection":          conn,
		"allocation_status":   status,
		"gateway_identifiers": map[string]any{"gateway_name": g.Name, "router_name": g.Router, "region": region},
		"endpoint":            map[string]any{"project_id": g.Project, "vm_name": vm.Name, "region": region, "zone": vm.Zone},
		"vpc":                 map[string]any{"project_id": g.Project, "vpc_name": network, "subnetwork_name": subnet},
		"destination":         map[string]any{},
	})
	labels := map[string]string{"nat.googleapis.com/instance_name": vm.Name, "nat.googleapis.com/instance_zone": vm.Zone,
		"nat.googleapis.com/network_name": network, "nat.googleapis.com/router_name": g.Router,
		"nat.googleapis.com/subnetwork_name": subnet}
	if c.natIP != "" {
		labels["nat.googleapis.com/nat_ip"] = c.natIP
	}
	return &loggingpb.LogEntry{
		LogName: "projects/" + g.Project + "/logs/compute.googleapis.com%2Fnat_flows",
		Resource: &monitoredres.MonitoredResource{Type: "nat_gateway", Labels: map[string]string{
			"project_id": g.Project, "region": region, "router_id": g.RouterID, "gateway_name": g.Name}},
		Timestamp: timestamppb.New(ts),
		Labels:    labels,
		Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
	}
}
