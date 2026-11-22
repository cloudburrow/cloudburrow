//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

const vpcProbeImage = "dev.local/cloudburrow-vpcprobe:compat"

// vpcVMs makes debian-12 VMs serving lbWebScript's "VM-<hostname>" on port
// 80, and runs commands on them over SSH.
type vpcVMs struct {
	t      *testing.T
	ctx    context.Context
	addr   string
	p      string
	zone   string
	insts  *computeapi.InstancesClient
	signer ssh.Signer
	key    string
}

func newVPCVMs(ctx context.Context, t *testing.T, h *Harness, zone string) *vpcVMs {
	insts, err := computeapi.NewInstancesRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	return &vpcVMs{t: t, ctx: ctx, addr: h.Endpoint(EnvCompute), p: h.Project(), zone: zone, insts: insts, signer: signer,
		key: "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"}
}

// make inserts the VMs by name, each on its network interface, at once,
// and returns their internal IPs.
func (v *vpcVMs) make(nics map[string]*computepb.NetworkInterface) map[string]string {
	t := v.t
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for name, nic := range nics {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := v.insts.Insert(v.ctx, &computepb.InsertInstanceRequest{Project: v.p, Zone: v.zone, InstanceResource: &computepb.Instance{
				Name: proto.String(name), MachineType: proto.String("zones/" + v.zone + "/machineTypes/e2-small"),
				Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
				NetworkInterfaces: []*computepb.NetworkInterface{nic},
				Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(lbWebScript)},
					{Key: proto.String("ssh-keys"), Value: proto.String(v.key)}}}}})
			if err == nil {
				err = op.Wait(v.ctx)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}()
		t.Cleanup(func() {
			if op, err := v.insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: v.p, Zone: v.zone, Instance: name}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	ips := map[string]string{}
	for name := range nics {
		i, err := v.insts.Get(v.ctx, &computepb.GetInstanceRequest{Project: v.p, Zone: v.zone, Instance: name})
		if err != nil {
			t.Fatal(err)
		}
		ips[name] = i.GetNetworkInterfaces()[0].GetNetworkIP()
	}
	return ips
}

// run runs cmd on vm over SSH, retrying while the guest boots.
func (v *vpcVMs) run(vm, cmd string) string {
	t := v.t
	t.Helper()
	sshAddr := vmSSHAddress(t, v.addr, v.p, v.zone, vm)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(v.signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
		var out string
		if err == nil {
			var s *ssh.Session
			if s, err = c.NewSession(); err == nil {
				var b []byte
				b, err = s.CombinedOutput(cmd)
				out = string(b)
			}
			c.Close()
			if err == nil {
				return out
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ssh %s %q: %v: %s", vm, cmd, err, out)
		}
		time.Sleep(2 * time.Second)
	}
}

// eventually runs cmd on vm until its output has want, for three minutes.
func (v *vpcVMs) eventually(what, vm, cmd, want string) string {
	v.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out := v.run(vm, cmd)
		if strings.Contains(out, want) {
			return out
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("%s: %s on %s gave %q, want %q; its ruleset:\n%s", what, cmd, vm, out, want, v.run(vm, "sudo nft list ruleset"))
		}
		time.Sleep(3 * time.Second)
	}
}

// covers: compute.forwardingRules.insert, compute.forwardingRules.get, compute.forwardingRules.delete, compute.serviceAttachments.insert, compute.serviceAttachments.delete, compute.regionBackendServices.insert, compute.regionBackendServices.delete, compute.regionHealthChecks.insert, compute.regionHealthChecks.delete, compute.instanceGroups.insert, compute.instanceGroups.addInstances
//
// TestInternalLBAndPSCEndpointCarryVMTraffic2140 (#2140, #2153), the
// official compute/apiv1 REST clients, as
// load-balancing/docs/internal/setting-up-internal and
// vpc/docs/configure-private-service-connect-services set them up: a
// producer VM serving port 80 in an unmanaged instance group, a firewall
// rule for Google's health-check ranges, an internal passthrough Network
// Load Balancer of the group, a service attachment publishing its
// forwarding rule on a PRIVATE_SERVICE_CONNECT subnet, and in a second
// network a consumer VM with an endpoint to the attachment. A client VM
// on the producer's network reaches the backend at the load balancer's
// address; the consumer VM reaches it at the endpoint's, and not at the
// load balancer's (another network); and once an egress rule denies the
// endpoint's address, the consumer VM no longer reaches it.
func TestInternalLBAndPSCEndpointCarryVMTraffic2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	igs, _ := computeapi.NewInstanceGroupsRESTClient(ctx, opts...)
	sas, _ := computeapi.NewServiceAttachmentsRESTClient(ctx, opts...)
	frs, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	defNet := "projects/" + p + "/global/networks/default"
	cons := "ct-cons-" + sfx
	consNet := "projects/" + p + "/global/networks/" + cons
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(cons), AutoCreateSubnetworks: proto.Bool(false)}}))
	t.Cleanup(func() {
		c := context.Background()
		if op, err := subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: cons}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: cons + "-no-ep"}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: cons}); err == nil {
			_ = op.Wait(c)
		}
	})
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(cons), Network: proto.String(consNet), IpCidrRange: proto.String("10.91.0.0/24")}}))
	nat := "ct-pscnat-" + sfx
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(nat), Network: proto.String(defNet), IpCidrRange: proto.String("10.92.0.0/24"),
		Purpose: proto.String("PRIVATE_SERVICE_CONNECT")}}))
	t.Cleanup(func() {
		if op, err := subs.Delete(context.Background(), &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: nat}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	hcfw := "ct-hc-" + sfx
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(hcfw), Network: proto.String(defNet), SourceRanges: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80"}}}}}))
	t.Cleanup(func() {
		if op, err := fws.Delete(context.Background(), &computepb.DeleteFirewallRequest{Project: p, Firewall: hcfw}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	vms := newVPCVMs(ctx, t, h, zone)
	web, client, consumer := "ct-ilbweb-"+sfx, "ct-ilbcli-"+sfx, "ct-pscc-"+sfx
	vms.make(map[string]*computepb.NetworkInterface{
		web:      {Network: proto.String("global/networks/default")},
		client:   {Network: proto.String("global/networks/default")},
		consumer: {Subnetwork: proto.String("regions/" + region + "/subnetworks/" + cons)},
	})
	group := "ct-ilbig-" + sfx
	wait(igs.Insert(ctx, &computepb.InsertInstanceGroupRequest{Project: p, Zone: zone, InstanceGroupResource: &computepb.InstanceGroup{
		Name: proto.String(group), Network: proto.String(defNet)}}))
	t.Cleanup(func() {
		if op, err := igs.Delete(context.Background(), &computepb.DeleteInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	wait(igs.AddInstances(ctx, &computepb.AddInstancesInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group,
		InstanceGroupsAddInstancesRequestResource: &computepb.InstanceGroupsAddInstancesRequest{
			Instances: []*computepb.InstanceReference{{Instance: proto.String("zones/" + zone + "/instances/" + web)}}}}))
	ilb := internalLB(ctx, t, opts, p, region, defNet, "ct-ilb-"+sfx, "projects/"+p+"/zones/"+zone+"/instanceGroups/"+group, 80)
	rule, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: "ct-ilb-" + sfx})
	if err != nil {
		t.Fatal(err)
	}
	att := "ct-att-" + sfx
	wait(sas.Insert(ctx, &computepb.InsertServiceAttachmentRequest{Project: p, Region: region, ServiceAttachmentResource: &computepb.ServiceAttachment{
		Name: proto.String(att), TargetService: proto.String(ilb), ConnectionPreference: proto.String("ACCEPT_AUTOMATIC"),
		NatSubnets: []string{"regions/" + region + "/subnetworks/" + nat}}}))
	t.Cleanup(func() {
		if op, err := sas.Delete(context.Background(), &computepb.DeleteServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: att}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	ep := "ct-ep-" + sfx
	wait(frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(ep), Network: proto.String(consNet), Subnetwork: proto.String("regions/" + region + "/subnetworks/" + cons),
		Target: proto.String("projects/" + p + "/regions/" + region + "/serviceAttachments/" + att)}}))
	t.Cleanup(func() {
		if op, err := frs.Delete(context.Background(), &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	e, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep})
	if err != nil || e.GetPscConnectionStatus() != "ACCEPTED" {
		t.Fatalf("endpoint = %v, %v", e, err)
	}

	curl := func(ip string) string { return "curl -s -m 5 http://" + ip + "/ || echo UNREACHABLE" }
	vms.eventually("the client reaches the internal load balancer", client, curl(rule.GetIPAddress()), "VM-"+web)
	vms.eventually("the consumer reaches the published service at its endpoint", consumer, curl(e.GetIPAddress()), "VM-"+web)
	if out := vms.run(consumer, curl(rule.GetIPAddress())); strings.Contains(out, "VM-") {
		t.Errorf("the consumer, on another network, reaches the producer's load balancer directly: %q", out)
	}
	// An egress rule denying the endpoint's address applies to it.
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(cons + "-no-ep"), Network: proto.String(consNet), Direction: proto.String("EGRESS"), Priority: proto.Int32(100),
		DestinationRanges: []string{e.GetIPAddress() + "/32"}, Denied: []*computepb.Denied{{IPProtocol: proto.String("tcp")}}}}))
	vms.eventually("an egress deny of the endpoint", consumer, curl(e.GetIPAddress()), "UNREACHABLE")
}

// loadVPCProbe compiles testdata/vpcprobe, builds its image (FROM
// scratch: nothing is pulled) and loads it into the instance's cluster.
func loadVPCProbe(ctx context.Context, t *testing.T) {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}
	root := moduleRoot(t)
	dir := root + "/testdata/vpcprobe"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/vpcprobe", "./testdata/vpcprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the VPC probe: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/vpcprobe") })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", vpcProbeImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the VPC probe image: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", vpcProbeImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the VPC probe: %v\n%s", err, b)
	}
}

// covers: google.cloud.vpcaccess.v1.VpcAccessService/CreateConnector, google.cloud.vpcaccess.v1.VpcAccessService/DeleteConnector, google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/DeleteService
//
// TestRunReachesVMsThroughConnectorsAndDirectVPCEgress2140 (#2140), the
// official vpcaccess/apiv1, run/apiv2 and compute/apiv1 clients, as
// vpc/docs/configure-serverless-vpc-access and
// run/docs/configuring/vpc-direct-vpc set them up: a VM on the default
// network serving port 80 at its internal IP; a connector on the network;
// Cloud Run services of the probe image with the connector, with Direct
// VPC egress on the default subnet, and with neither. The first two reach
// the VM at its internal IP, from the connector's range and from an
// address of the subnet; the third does not, as on Google, where a
// service without VPC egress has no route to a network's internal
// addresses.
func TestRunReachesVMsThroughConnectorsAndDirectVPCEgress2140(t *testing.T) {
	h := New(t)
	va := vpcAccessClient(t, h)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	loadVPCProbe(ctx, t)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	parent := "projects/" + p + "/locations/" + region
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e5)

	vms := newVPCVMs(ctx, t, h, zone)
	web := "ct-runweb-" + sfx
	ip := vms.make(map[string]*computepb.NetworkInterface{web: {Network: proto.String("global/networks/default")}})[web]
	vms.eventually("the VM serves", web, "curl -s -m 5 http://127.0.0.1/", "VM-"+web)

	connID := "ct-rc-" + sfx
	op, err := va.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: connID,
		Connector: &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.231.0/28"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := va.DeleteConnector(context.Background(), &vpcaccesspb.DeleteConnectorRequest{Name: parent + "/connectors/" + connID}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	base := ingress(t)
	deploy := func(id string, v *runpb.VpcAccess) string {
		t.Helper()
		name := parent + "/services/" + id
		op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{VpcAccess: v, Scaling: &runpb.RevisionScaling{MinInstanceCount: 1},
				Containers: []*runpb.Container{{Image: vpcProbeImage}}}}})
		if err != nil {
			t.Fatalf("CreateService(%s): %v", id, err)
		}
		t.Cleanup(func() { _, _ = rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
		svc, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", id, err)
		}
		return hostOf(t, svc.GetUri())
	}
	probe := func(host string) (int, string) {
		return httpGet(t, base, host, "/?"+url.Values{"target": {ip + ":80"}}.Encode())
	}
	reaches := func(what, host string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			code, body := probe(host)
			if code == http.StatusOK && strings.Contains(body, "VM-"+web) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d %s", what, code, body)
			}
			time.Sleep(3 * time.Second)
		}
	}
	reaches("through the connector", deploy("ct-rc-conn-"+sfx, &runpb.VpcAccess{Connector: connID}))
	reaches("through Direct VPC egress", deploy("ct-rc-direct-"+sfx, &runpb.VpcAccess{Egress: runpb.VpcAccess_PRIVATE_RANGES_ONLY,
		NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Network: "default", Subnetwork: "default"}}}))
	if code, body := probe(deploy("ct-rc-none-"+sfx, nil)); code == http.StatusOK && strings.Contains(body, "VM-") {
		t.Errorf("a service without VPC egress reaches the VM's internal IP: %s", body)
	}
}
