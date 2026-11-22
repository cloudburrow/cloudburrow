package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Cloud Run's traffic onto VPC networks (#2140): a service's or job's
// Serverless VPC Access connector or Direct VPC egress carries its pods'
// connections to the network's VMs, as on Google
// (vpc/docs/serverless-vpc-access, run/docs/configuring/vpc-direct-vpc).
//
// Every few seconds the loop below:
//   - lists Cloud Run's running pods that carry cloudburrow.dev/vpc-egress
//     (internal/adapter/run/vpcaccess.go);
//   - gives the compute API each distinct connector or Direct VPC egress
//     setting they use as a gateway (internal/service/compute/gateways.go),
//     which is given an address of the network;
//   - runs one gateway pod for each: the VM image's own vpc agent, which
//     joins the network's VXLAN segment at that address and forwards;
//   - routes, on the kind nodes, what each such pod sends to the network's
//     ranges (or, for ALL_TRAFFIC, everywhere but the cluster and this
//     machine) to its gateway pod, by policy routing on the pod's source
//     address, so no other pod reaches the network.

// runEgressLabel marks a gateway pod.
const runEgressLabel = "cloudburrow.dev/run-gateway"

// runEgressRulePref is where the nodes' policy rules for Cloud Run's pods
// start; each gateway has a routing table of its own above runEgressTable.
const (
	runEgressRulePref = 1789
	runEgressTable    = 17890
)

type runVPCEgress struct {
	svc  *computeService
	last string // the nodes' last applied routing script
}

func (s *computeService) startRunEgress(ctx context.Context) {
	e := &runVPCEgress{svc: s}
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := e.sync(ctx); err != nil && ctx.Err() == nil {
				fmt.Println("cloudburrow: Cloud Run VPC egress:", err)
			}
		}
	}()
}

// runPod is a Cloud Run pod with VPC egress.
type runPod struct {
	ip, name string
	access   *runpb.VpcAccess
}

func (e *runVPCEgress) sync(ctx context.Context) error {
	api, va, vms := e.svc.API(), e.svc.VPCAccess(), e.svc.vms
	if api == nil || va == nil || vms == nil {
		return nil
	}
	out, err := vms.installer.Kubectl(ctx, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return err
	}
	pods, gwPods := runEgressPods(out)
	var gws []compute.Gateway
	byKey := map[string]int{}
	podGW := map[string]string{} // pod IP -> gateway key
	for _, p := range pods {
		g, ok := runGateway(p, func(name string) (network, cidr, subnet, region string, ok bool) {
			c, ok := va.Connector(name)
			if !ok {
				return "", "", "", "", false
			}
			cp := strings.Split(name, "/")
			project := cp[1]
			if c.GetSubnet().GetName() != "" {
				sp := c.GetSubnet().GetProjectId()
				if sp == "" {
					sp = project
				}
				return "", "", "projects/" + sp + "/regions/" + cp[3] + "/subnetworks/" + c.GetSubnet().GetName(), cp[3], true
			}
			return compute.Rel("global/networks/"+c.GetNetwork(), project), c.GetIpCidrRange(), "", cp[3], true
		})
		if !ok {
			continue
		}
		if _, seen := byKey[g.Key]; !seen {
			byKey[g.Key] = len(gws)
			gws = append(gws, g)
		}
		podGW[p.ip] = g.Key
	}
	states := api.SetGateways(gws)
	want := map[string]compute.GatewayState{}
	for _, st := range states {
		want["cb-gw-"+st.Token[2:14]] = st
	}
	// The gateway pods: those wanted made, the rest deleted.
	ref := ""
	for name, st := range want {
		if gwPods[name] {
			continue
		}
		if ref == "" {
			img, ok := gatewayImage()
			if !ok {
				return fmt.Errorf("no Debian image to run gateways")
			}
			if ref, err = vms.ensureImage(ctx, img); err != nil {
				return err
			}
		}
		if err := vms.installer.Apply(ctx, gatewayPodManifest(vms, name, st, ref)); err != nil {
			return err
		}
	}
	for name := range gwPods {
		if _, ok := want[name]; !ok {
			_, _ = vms.kubectl(ctx, "delete", "pod", name, "--wait=false", "--ignore-not-found")
		}
	}
	// The nodes' routes.
	cur := api.Gateways()
	script := runEgressScript(pods, podGW, cur, api.GatewayRanges)
	if script == e.last {
		return nil
	}
	if err := e.applyNodes(ctx, script); err != nil {
		return err
	}
	e.last = script
	return nil
}

// runEgressPods are the running Cloud Run pods with VPC egress, and the
// gateway pods that exist, by name.
func runEgressPods(kubectlJSON string) ([]runPod, map[string]bool) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
				PodIP string `json:"podIP"`
			} `json:"status"`
		} `json:"items"`
	}
	gw := map[string]bool{}
	if json.Unmarshal([]byte(kubectlJSON), &list) != nil {
		return nil, gw
	}
	var out []runPod
	for _, it := range list.Items {
		if it.Metadata.Labels[runEgressLabel] != "" {
			gw[it.Metadata.Name] = true
			continue
		}
		raw := it.Metadata.Annotations["cloudburrow.dev/vpc-egress"]
		if raw == "" || it.Status.Phase != "Running" || it.Status.PodIP == "" {
			continue
		}
		var a struct {
			Name      string          `json:"name"`
			VpcAccess json.RawMessage `json:"vpcAccess"`
		}
		if json.Unmarshal([]byte(raw), &a) != nil {
			continue
		}
		v := &runpb.VpcAccess{}
		if protojson.Unmarshal(a.VpcAccess, v) != nil {
			continue
		}
		out = append(out, runPod{ip: it.Status.PodIP, name: a.Name, access: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ip < out[j].ip })
	return out, gw
}

// runGateway is the gateway p's traffic goes through: its connector's
// (connector looks it up) or its Direct VPC egress's.
func runGateway(p runPod, connector func(name string) (network, cidr, subnet, region string, ok bool)) (compute.Gateway, bool) {
	parts := strings.Split(p.name, "/")
	if len(parts) < 4 {
		return compute.Gateway{}, false
	}
	project, region := parts[1], parts[3]
	all := p.access.GetEgress() == runpb.VpcAccess_ALL_TRAFFIC
	if c := p.access.GetConnector(); c != "" {
		if !strings.Contains(c, "/") {
			c = "projects/" + project + "/locations/" + region + "/connectors/" + c
		}
		network, cidr, subnet, r, ok := connector(c)
		if !ok {
			return compute.Gateway{}, false
		}
		key := "connector:" + c
		if all {
			key += ":all"
		}
		// The connector's instances carry its network tag, which its tagfw
		// firewall rule lets into the network (connectorfw.go).
		return compute.Gateway{Key: key, Connector: true, Network: network, Range: cidr, Subnetwork: subnet, Region: r, AllTraffic: all,
			Tags: []string{compute.ConnectorTag(r, c[strings.LastIndex(c, "/")+1:])}}, true
	}
	nis := p.access.GetNetworkInterfaces()
	if len(nis) == 0 {
		return compute.Gateway{}, false
	}
	ni := nis[0]
	sub := ni.GetSubnetwork()
	if sub == "" {
		sub = ni.GetNetwork()[strings.LastIndex(ni.GetNetwork(), "/")+1:]
	}
	subRel := compute.Rel(sub, project)
	if !strings.Contains(subRel, "/") {
		subRel = "projects/" + project + "/regions/" + region + "/subnetworks/" + subRel
	}
	tags := append([]string(nil), ni.GetTags()...)
	sort.Strings(tags)
	key := "direct:" + subRel + ":" + strings.Join(tags, ",")
	if all {
		key += ":all"
	}
	return compute.Gateway{Key: key, Subnetwork: subRel, Region: region, Tags: tags, AllTraffic: all}, true
}

// gatewayImage is the Debian image a gateway pod runs: the VM image,
// whose vpc agent it runs.
func gatewayImage() (compute.PublicImage, bool) {
	for _, img := range compute.PublicImages {
		if img.Family == "debian-12" {
			return img, true
		}
	}
	return compute.PublicImage{}, false
}

func gatewayPodManifest(b *computeVMs, name string, st compute.GatewayState, ref string) string {
	guest := ""
	if b.guestBase != nil {
		if base := b.guestBase(); base != "" {
			guest = base + compute.GuestPrefix + st.Token
		}
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(),
			"labels":      map[string]string{"cloudburrow.dev/owned": "true", runEgressLabel: st.Token},
			"annotations": map[string]string{"cloudburrow.dev/run-gateway-key": st.Key}},
		"spec": map[string]any{"restartPolicy": "Always", "terminationGracePeriodSeconds": 1,
			"automountServiceAccountToken": false, "enableServiceLinks": false,
			"containers": []any{map[string]any{"name": "gateway", "image": ref, "imagePullPolicy": "Never",
				"command":         []string{"/usr/lib/cloudburrow/vpc"},
				"securityContext": map[string]any{"privileged": true},
				"env": []any{map[string]string{"name": "CLOUDBURROW_GUEST_METADATA", "value": guest},
					map[string]string{"name": "CLOUDBURROW_INTERNAL_IP", "value": st.IP}},
				"resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}}}}}}
	raw, _ := json.Marshal(pod)
	return string(raw)
}

// runEgressTableOf is a gateway's routing table on the nodes.
func runEgressTableOf(key string) int {
	h := sha256.Sum256([]byte(key))
	return runEgressTable + int(binary.BigEndian.Uint16(h[:2])%2000)
}

// runEgressScript is the shell script that sets the nodes' routes: for each
// gateway with a pod, a table routing its network's ranges (ranges gives
// them) through the gateway pod, every address but the cluster's and
// private ones too for ALL_TRAFFIC; and a rule per Cloud Run pod sending
// its traffic to its gateway's table. Earlier rules and tables are
// flushed first, so the script states the whole set.
func runEgressScript(pods []runPod, podGW map[string]string, gws []compute.GatewayState, ranges func(key string) []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "while ip rule del pref %d 2>/dev/null; do :; done\n", runEgressRulePref)
	tables := map[string]int{}
	for _, g := range gws {
		if g.Pod == "" {
			continue
		}
		t := runEgressTableOf(g.Key)
		tables[g.Key] = t
		fmt.Fprintf(&b, "ip route flush table %d 2>/dev/null || true\n", t)
		// The gateway pod's veth, which its /32 route on the node names.
		fmt.Fprintf(&b, "d=$(ip -o route get %s | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p')\n", g.Pod)
		for _, c := range compute.ClusterCIDRs {
			fmt.Fprintf(&b, "ip route replace throw %s table %d\n", c, t)
		}
		for _, r := range ranges(g.Key) {
			fmt.Fprintf(&b, "ip route replace %s via %s dev $d table %d onlink\n", r, g.Pod, t)
		}
		if g.AllTraffic {
			fmt.Fprintf(&b, "ip route replace default via %s dev $d table %d onlink\n", g.Pod, t)
			// This machine and the node's own network stay direct: Cloud
			// Run's local APIs are there.
			b.WriteString("for c in $(ip -4 -o addr show dev eth0 | awk '{print $4}'); do ip route replace throw $c table " + fmt.Sprint(t) + "; done\n")
			b.WriteString("h=$(getent ahostsv4 host.docker.internal | awk 'NR==1{print $1}'); [ -n \"$h\" ] && ip route replace throw $h/32 table " + fmt.Sprint(t) + " || true\n")
		}
	}
	for _, p := range pods {
		if t, ok := tables[podGW[p.ip]]; ok {
			fmt.Fprintf(&b, "ip rule add from %s/32 lookup %d pref %d\n", p.ip, t, runEgressRulePref)
		}
	}
	return b.String()
}

// applyNodes runs script on each node of the instance's kind cluster.
func (e *runVPCEgress) applyNodes(ctx context.Context, script string) error {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+e.svc.cfg.ClusterName(),
		"--format", "{{.Names}}").Output()
	if err != nil {
		return err
	}
	for _, node := range strings.Fields(string(out)) {
		if b, err := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", script).CombinedOutput(); err != nil {
			return fmt.Errorf("route Cloud Run's pods on %s: %w: %s", node, err, vmTail(string(b), 3))
		}
	}
	return nil
}
