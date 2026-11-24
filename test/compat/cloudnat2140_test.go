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
	logadmin "cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// natTarget is an echo server outside the cluster, on a Docker network
// created --internal (no route off this machine), its range one that no
// private range holds, so a VM reaches it only as it would the internet;
// the kind nodes are connected to it for the test.
type natTarget struct {
	ip, network, container string
}

func startNATTarget(ctx context.Context, t *testing.T, cluster string) natTarget {
	t.Helper()
	docker := func(args ...string) (string, error) {
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}
	// An image already on this machine: the VM image, which has nc.
	imgs, err := docker("images", "--format", "{{.Repository}} {{.ID}}")
	if err != nil {
		t.Fatal(err)
	}
	img := ""
	for _, l := range strings.Split(imgs, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "cloudburrow-vm-debian-12" {
			img = f[1]
			break
		}
	}
	if img == "" {
		t.Skip("no cloudburrow-vm-debian-12 image on this machine for the echo target")
	}
	var tg natTarget
	for n := 0; n < 40 && tg.network == ""; n++ {
		// 198.18.0.0/15 (benchmarking): neither private nor in use here.
		subnet := fmt.Sprintf("198.18.%d.0/24", 100+(time.Now().Nanosecond()/1000+n)%150)
		id, err := docker("network", "create", "--internal", "--subnet", subnet, fmt.Sprintf("cb-nat2140-%d", time.Now().UnixNano()%1e9))
		if err == nil {
			tg.network, tg.ip = id, strings.TrimSuffix(subnet, "0/24")+"10"
		}
	}
	if tg.network == "" {
		t.Fatal("no free range for the internal network")
	}
	t.Cleanup(func() { _, _ = exec.Command("docker", "network", "rm", tg.network).CombinedOutput() })
	id, err := docker("run", "-d", "--rm", "--network", tg.network, "--ip", tg.ip, "--entrypoint", "sh", img, "-c",
		"while :; do echo nat-target | nc -l -p 8080 -q 1; done")
	if err != nil {
		t.Fatalf("start the echo target: %v: %s", err, id)
	}
	tg.container = id
	t.Cleanup(func() { _, _ = exec.Command("docker", "rm", "-f", tg.container).CombinedOutput() })
	nodes, err := docker("ps", "--filter", "label=io.x-k8s.kind.cluster="+cluster, "--format", "{{.Names}}")
	if err != nil || nodes == "" {
		t.Fatalf("the cluster's nodes: %v %q", err, nodes)
	}
	for _, node := range strings.Fields(nodes) {
		if out, err := docker("network", "connect", tg.network, node); err != nil {
			t.Fatalf("connect %s: %v: %s", node, err, out)
		}
		t.Cleanup(func() {
			_, _ = exec.Command("docker", "network", "disconnect", "-f", tg.network, node).CombinedOutput()
		})
	}
	return tg
}

// covers: compute.routers.insert, compute.routers.getNatMappingInfo, compute.routers.getNatIpInfo, compute.routers.delete
//
// TestCloudNATTranslatesVMTraffic2140 (#2140), the official compute/apiv1
// clients and logadmin, as nat/docs/set-up-manage-network-address-translation
// sets a gateway up: a VM with no external IP does not reach an address
// outside its network's private ranges (an echo server on an --internal
// Docker network, no internet involved); once a Cloud Router's NAT gateway
// (AUTO_ONLY, minPortsPerVm 64, logging ALL) covers its subnet, it does,
// through the gateway's pod: the node's conntrack table shows the
// connection leaving from the NAT IP and a port of the range
// getNatMappingInfo gives the VM, getNatIpInfo has the address IN_USE, and
// the connection's compute.googleapis.com/nat_flows entry is in Cloud
// Logging.
func TestCloudNATTranslatesVMTraffic2140(t *testing.T) {
	h := New(t)
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the target's network is connected to the owned cluster's nodes")
	}
	logAddr := h.Endpoint(EnvLogging)
	if logAddr == "" {
		t.Fatal("no Cloud Logging endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	tg := startNATTarget(ctx, t, cluster)
	const region, zone = "us-east4", "us-east4-a"
	p := h.Project()
	v := newVPCVMs(ctx, t, h, zone)
	sfx := strconv.Itoa(int(time.Now().UnixNano() % 1e6))
	vm := "ct-nat-" + sfx
	ips := v.make(map[string]*computepb.NetworkInterface{vm: {Network: proto.String("global/networks/default")}})
	probe := "nc -w 5 " + tg.ip + " 8080 </dev/null; echo rc=$?"
	// Its firewall in force first: before it, the pod's own route would
	// reach the target.
	v.eventually("no external IP", vm, "sudo nft list table inet cloudburrow", "no external IP and no Cloud NAT")
	if out := v.run(vm, probe); strings.Contains(out, "nat-target") {
		t.Fatalf("a VM with neither an external IP nor Cloud NAT reached %s: %s", tg.ip, out)
	}

	routers, err := computeapi.NewRoutersRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	// Closed after the router's deletion (cleanups run last-first).
	t.Cleanup(func() { routers.Close() })
	router := "ct-nat-" + sfx
	op, err := routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(router), Network: proto.String("global/networks/default"),
		Nats: []*computepb.RouterNat{{Name: proto.String("nat"), NatIpAllocateOption: proto.String("AUTO_ONLY"),
			SourceSubnetworkIpRangesToNat: proto.String("ALL_SUBNETWORKS_ALL_IP_RANGES"), MinPortsPerVm: proto.Int32(64),
			LogConfig: &computepb.RouterNatLogConfig{Enable: proto.Bool(true), Filter: proto.String("ALL")}}}}})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := routers.Delete(context.Background(), &computepb.DeleteRouterRequest{Project: p, Region: region, Router: router}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	// The VM routes the internet to the gateway's pod once it runs; only
	// then is the target reached through it.
	v.eventually("routed to the NAT gateway", vm, "ip -4 route show 0.0.0.0/1", "dev vpc0")
	v.eventually("through Cloud NAT", vm, probe, "nat-target")

	// The VM's NAT IP and ports.
	var natIP string
	var lo, hi int
	it := routers.GetNatMappingInfo(ctx, &computepb.GetNatMappingInfoRoutersRequest{Project: p, Region: region, Router: router})
	for {
		m, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if m.GetInstanceName() != vm {
			continue
		}
		im := m.GetInterfaceNatMappings()[0]
		if im.GetSourceVirtualIp() != ips[vm] || im.GetNumTotalNatPorts() != 64 || len(im.GetNatIpPortRanges()) != 1 {
			t.Fatalf("mapping: %v", m)
		}
		ipPart, ports, _ := strings.Cut(im.GetNatIpPortRanges()[0], ":")
		a, b, _ := strings.Cut(ports, "-")
		natIP, lo, hi = ipPart, atoi(a), atoi(b)
	}
	if natIP == "" || hi-lo != 63 {
		t.Fatalf("no mapping for %s (%s:%d-%d)", vm, natIP, lo, hi)
	}
	info, err := routers.GetNatIpInfo(ctx, &computepb.GetNatIpInfoRouterRequest{Project: p, Region: region, Router: router})
	if err != nil {
		t.Fatal(err)
	}
	if ms := info.GetResult()[0].GetNatIpInfoMappings(); len(ms) == 0 || ms[0].GetNatIp() != natIP || ms[0].GetUsage() != "IN_USE" || ms[0].GetMode() != "ACTIVE" {
		t.Fatalf("getNatIpInfo: %v", info)
	}

	// The node's conntrack: the connection from the NAT IP, a port of the
	// VM's range, to the target.
	nodes, _ := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+cluster, "--format", "{{.Names}}").Output()
	found := ""
	for try := 0; try < 3 && found == ""; try++ {
		v.run(vm, probe)
		for _, node := range strings.Fields(string(nodes)) {
			ct, _ := exec.CommandContext(ctx, "docker", "exec", node, "cat", "/proc/net/nf_conntrack").Output()
			for _, l := range strings.Split(string(ct), "\n") {
				if !strings.Contains(l, " src="+natIP+" dst="+tg.ip+" ") || !strings.Contains(l, "dport=8080") {
					continue
				}
				f := strings.Fields(l)
				for _, x := range f {
					if port, ok := strings.CutPrefix(x, "sport="); ok {
						if n := atoi(port); n >= lo && n <= hi {
							found = l
						}
						break
					}
				}
			}
		}
	}
	if found == "" {
		t.Fatalf("no connection from %s:%d-%d to %s in the nodes' conntrack tables", natIP, lo, hi, tg.ip)
	}

	// The flow's log entry.
	ac, err := logadmin.NewClient(ctx, "projects/"+p, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := fmt.Sprintf(`logName="projects/%s/logs/compute.googleapis.com%%2Fnat_flows" AND resource.type="nat_gateway" AND resource.labels.gateway_name="nat" AND jsonPayload.connection.dest_ip=%q`, p, tg.ip)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		eit := ac.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())
		e, err := eit.Next()
		if err == nil {
			pl, _ := e.Payload.(*structpb.Struct)
			m := pl.AsMap()
			conn, _ := m["connection"].(map[string]any)
			ep, _ := m["endpoint"].(map[string]any)
			port, _ := conn["nat_port"].(float64)
			if m["allocation_status"] != "OK" || conn["src_ip"] != ips[vm] || conn["nat_ip"] != natIP || int(port) < lo || int(port) > hi ||
				conn["dest_port"] != 8080.0 || ep["vm_name"] != vm || e.Resource.GetLabels()["region"] != region {
				t.Fatalf("nat_flows entry: %v %v", e.Resource, m)
			}
			break
		}
		if err != iterator.Done {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no nat_flows entry for %s", filter)
		}
		time.Sleep(3 * time.Second)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
