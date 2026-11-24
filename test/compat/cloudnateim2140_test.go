//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// covers: compute.routers.insert, compute.routers.patch, compute.routers.get, compute.routers.getNatMappingInfo, compute.routers.delete
//
// TestCloudNATEndpointIndependentMappingAndDynamicPorts2140 (#2140), the
// official compute/apiv1 REST client, as nat/docs/ports-and-addresses
// describes the two: a VM without an external IP behind a public Cloud
// NAT gateway sends UDP from one source port to six destination ports of
// an echo target outside the cluster (an --internal Docker network). With
// Endpoint-Independent Mapping off each flow is given its own NAT port of
// the VM's range, as the nodes' conntrack tables show; with it on (from a
// new source port) every flow leaves from one NAT port of that range.
// Then with dynamic port allocation (minPortsPerVm 32, maxPortsPerVm 128)
// the VM is given 32 ports; 40 flows from distinct source ports nearly
// exhaust them, so its ports are doubled to 64 and getNatMappingInfo says
// so; once the flows have timed out they are halved back to 32.
func TestCloudNATEndpointIndependentMappingAndDynamicPorts2140(t *testing.T) {
	h := New(t)
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the target's network is connected to the owned cluster's nodes")
	}
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	tg := startNATTarget(ctx, t, cluster)
	const region, zone = "us-east4", "us-east4-a"
	p := h.Project()
	v := newVPCVMs(ctx, t, h, zone)
	sfx := strconv.Itoa(int(time.Now().UnixNano() % 1e6))
	vm := "ct-eim-" + sfx
	v.make(map[string]*computepb.NetworkInterface{vm: {Network: proto.String("global/networks/default")}})
	routers, err := computeapi.NewRoutersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { routers.Close() })
	router := "ct-eim-" + sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(router), Network: proto.String("global/networks/default"),
		Nats: []*computepb.RouterNat{{Name: proto.String("nat"), NatIpAllocateOption: proto.String("AUTO_ONLY"),
			SourceSubnetworkIpRangesToNat: proto.String("ALL_SUBNETWORKS_ALL_IP_RANGES"), MinPortsPerVm: proto.Int32(64),
			EnableEndpointIndependentMapping: proto.Bool(false)}}}}))
	t.Cleanup(func() {
		if op, err := routers.Delete(context.Background(), &computepb.DeleteRouterRequest{Project: p, Region: region, Router: router}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	v.eventually("routed to the NAT gateway", vm, "ip -4 route show 0.0.0.0/1", "dev vpc0")
	v.eventually("through Cloud NAT", vm, "nc -w 5 "+tg.ip+" 8080 </dev/null; echo rc=$?", "nat-target")

	mapping := func() (string, int, int, int) {
		t.Helper()
		it := routers.GetNatMappingInfo(ctx, &computepb.GetNatMappingInfoRoutersRequest{Project: p, Region: region, Router: router})
		for {
			m, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.GetInstanceName() != vm || len(m.GetInterfaceNatMappings()) == 0 {
				continue
			}
			im := m.GetInterfaceNatMappings()[0]
			if len(im.GetNatIpPortRanges()) != 1 {
				t.Fatalf("mapping: %v", m)
			}
			ip, ports, _ := strings.Cut(im.GetNatIpPortRanges()[0], ":")
			a, b, _ := strings.Cut(ports, "-")
			return ip, atoi(a), atoi(b), int(im.GetNumTotalNatPorts())
		}
		t.Fatalf("no mapping for %s", vm)
		return "", 0, 0, 0
	}
	nodes, _ := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+cluster, "--format", "{{.Names}}").Output()
	// natPorts are the NAT source ports of the nodes' UDP flows from
	// natIP to the target's port dport.
	natPorts := func(natIP string, dport int) map[int]bool {
		out := map[int]bool{}
		for _, node := range strings.Fields(string(nodes)) {
			ct, _ := exec.CommandContext(ctx, "docker", "exec", node, "cat", "/proc/net/nf_conntrack").Output()
			for _, l := range strings.Split(string(ct), "\n") {
				if !strings.Contains(l, " udp ") || !strings.Contains(l, " src="+natIP+" dst="+tg.ip+" ") || !strings.Contains(l, fmt.Sprintf(" dport=%d ", dport)) {
					continue
				}
				for _, x := range strings.Fields(l) {
					if s, ok := strings.CutPrefix(x, "sport="); ok {
						out[atoi(s)] = true
						break
					}
				}
			}
		}
		return out
	}
	// flows sends one datagram from sport to each of dports, and returns
	// the NAT ports the nodes saw for them.
	flows := func(sport int, dports []int) map[int]bool {
		t.Helper()
		var cmd []string
		for _, d := range dports {
			cmd = append(cmd, fmt.Sprintf("echo x | nc -u -w 1 -p %d %s %d", sport, tg.ip, d))
		}
		natIP, _, _, _ := mapping()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			v.run(vm, strings.Join(cmd, "; ")+"; true")
			seen := map[int]bool{}
			all := true
			for _, d := range dports {
				ps := natPorts(natIP, d)
				if len(ps) == 0 {
					all = false
				}
				for p := range ps {
					seen[p] = true
				}
			}
			if all {
				return seen
			}
			if time.Now().After(deadline) {
				t.Fatalf("not every flow from %d reached the target through %s: %v", sport, natIP, seen)
			}
			time.Sleep(3 * time.Second)
		}
	}
	inRange := func(ports map[int]bool, lo, hi int) bool {
		for p := range ports {
			if p < lo || p > hi {
				return false
			}
		}
		return true
	}

	// Endpoint-Independent Mapping off: a NAT port per destination.
	_, lo, hi, _ := mapping()
	off := flows(40001, []int{9101, 9102, 9103, 9104, 9105, 9106})
	if len(off) < 2 || !inRange(off, lo, hi) {
		t.Fatalf("without Endpoint-Independent Mapping, six destinations from one source port: NAT ports %v (range %d-%d)", off, lo, hi)
	}

	patch := func(f func(n *computepb.RouterNat)) {
		t.Helper()
		r, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: router})
		if err != nil {
			t.Fatal(err)
		}
		f(r.GetNats()[0])
		wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: p, Region: region, Router: router, RouterResource: r}))
	}
	patch(func(n *computepb.RouterNat) { n.EnableEndpointIndependentMapping = proto.Bool(true) })
	if r, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: router}); err != nil || !r.GetNats()[0].GetEnableEndpointIndependentMapping() {
		t.Fatalf("the gateway: %v %v", r, err)
	}
	// The pod's table follows within a few polls.
	time.Sleep(10 * time.Second)
	on := flows(40002, []int{9201, 9202, 9203, 9204, 9205, 9206})
	if len(on) != 1 || !inRange(on, lo, hi) {
		t.Fatalf("with Endpoint-Independent Mapping, six destinations from one source port: NAT ports %v, want one of %d-%d", on, lo, hi)
	}

	// Dynamic port allocation.
	patch(func(n *computepb.RouterNat) {
		n.EnableEndpointIndependentMapping = proto.Bool(false)
		n.EnableDynamicPortAllocation = proto.Bool(true)
		n.MinPortsPerVm, n.MaxPortsPerVm = proto.Int32(32), proto.Int32(128)
	})
	if _, _, _, n := mapping(); n != 32 {
		t.Fatalf("dynamic port allocation's first ports: %d, want 32", n)
	}
	time.Sleep(10 * time.Second)
	var burst []string
	for i := 0; i < 40; i++ {
		burst = append(burst, fmt.Sprintf("(echo x | nc -u -w 1 -p %d %s 9300) &", 41000+i, tg.ip))
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		v.run(vm, strings.Join(burst, " ")+" wait")
		if _, lo, hi, n := mapping(); n == 64 && hi-lo == 63 {
			break
		}
		if time.Now().After(deadline) {
			_, lo, hi, n := mapping()
			t.Fatalf("40 flows on 32 ports: still %d ports (%d-%d), want 64", n, lo, hi)
		}
		time.Sleep(5 * time.Second)
	}
	// The flows time out (UDP's 30 s), then the ports are halved.
	deadline = time.Now().Add(4 * time.Minute)
	for {
		if _, _, _, n := mapping(); n == 32 {
			break
		}
		if time.Now().After(deadline) {
			_, _, _, n := mapping()
			t.Fatalf("idle: still %d ports, want 32", n)
		}
		time.Sleep(5 * time.Second)
	}
}
