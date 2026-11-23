//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestVMIdentityThroughALoadBalancerAndSecureWebProxy (#2160) runs two
// VMs with different service accounts and secure tags, and has each send
// real requests, from inside the VM, to an internal Application Load
// Balancer's VPC address and through a Secure Web Proxy gateway's VPC
// address. The guest relays them with the VM's identity
// (internal/vmrelay), so an authorization policy's source resources
// (iamServiceAccount, then tagValueIdSet) and the gateway's
// source.matchServiceAccount() and source.matchTag() rules allow one VM
// and deny the other. A request from this machine, which no VM sent,
// matches neither. Every backend is a server on this machine's loopback.
//
// covers: google.cloud.resourcemanager.v3.TagBindings/CreateTagBinding, google.cloud.networksecurity.v1.NetworkSecurity/CreateAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/UpdateGatewaySecurityPolicyRule, google.cloud.networkservices.v1.NetworkServices/CreateGateway
func TestVMIdentityThroughALoadBalancerAndSecureWebProxy(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvCompute) // skips without it
	rmEndpoint := h.Endpoint(envLoggingResourceManager)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("vmid-%s-%d", s, sfx) }
	webSA, batchSA := "web@"+p+".iam.gserviceaccount.com", "batch@"+p+".iam.gserviceaccount.com"
	wait := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin "+r.URL.Path)
	}))
	t.Cleanup(origin.Close)
	oHost, oPort, _ := net.SplitHostPort(origin.Listener.Addr().String())
	op, _ := strconv.Atoi(oPort)

	// The network, its subnetwork and its proxy-only subnet.
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nets.Close() })
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = subs.Close() })
	o, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(n("net")), AutoCreateSubnetworks: proto.Bool(false)}})
	wait("the network", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: n("net")})
	})
	netRel := "projects/" + p + "/global/networks/" + n("net")
	subRel := "projects/" + p + "/regions/" + region + "/subnetworks/" + n("sub")
	for _, sn := range []*computepb.Subnetwork{
		{Name: proto.String(n("sub")), IpCidrRange: proto.String("10.81.0.0/24"), Network: proto.String(netRel)},
		{Name: proto.String(n("proxy")), IpCidrRange: proto.String("10.82.0.0/23"), Network: proto.String(netRel),
			Purpose: proto.String("REGIONAL_MANAGED_PROXY"), Role: proto.String("ACTIVE")},
	} {
		o, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: sn})
		wait("the subnetwork "+sn.GetName(), o, err)
		name := sn.GetName()
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: name})
		})
	}

	// The internal Application Load Balancer: a hybrid NEG to the origin,
	// a regional backend service, URL map, target HTTP proxy and rule.
	negs, _ := computeapi.NewNetworkEndpointGroupsRESTClient(ctx, opts...)
	rhcs, _ := computeapi.NewRegionHealthChecksRESTClient(ctx, opts...)
	rbss, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	rums, _ := computeapi.NewRegionUrlMapsRESTClient(ctx, opts...)
	rthps, _ := computeapi.NewRegionTargetHttpProxiesRESTClient(ctx, opts...)
	frs, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	o, err = negs.Insert(ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: p, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(n("neg")),
			NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"), Network: proto.String(netRel), DefaultPort: proto.Int32(int32(op))}})
	wait("the hybrid NEG", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: p, Zone: zone, NetworkEndpointGroup: n("neg")})
	})
	o, err = negs.AttachNetworkEndpoints(ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: p, Zone: zone,
		NetworkEndpointGroup: n("neg"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String(oHost), Port: proto.Int32(int32(op))}}}})
	wait("the origin's endpoint", o, err)
	o, err = rhcs.Insert(ctx, &computepb.InsertRegionHealthCheckRequest{Project: p, Region: region, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(n("hc")), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		HttpHealthCheck: &computepb.HTTPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT"), RequestPath: proto.String("/healthz")}}})
	wait("the health check", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rhcs.Delete(c, &computepb.DeleteRegionHealthCheckRequest{Project: p, Region: region, HealthCheck: n("hc")})
	})
	o, err = rbss.Insert(ctx, &computepb.InsertRegionBackendServiceRequest{Project: p, Region: region, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("bs")), Protocol: proto.String("HTTP"), LoadBalancingScheme: proto.String("INTERNAL_MANAGED"),
		HealthChecks: []string{"regions/" + region + "/healthChecks/" + n("hc")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + n("neg")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}}}})
	wait("the backend service", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rbss.Delete(c, &computepb.DeleteRegionBackendServiceRequest{Project: p, Region: region, BackendService: n("bs")})
	})
	o, err = rums.Insert(ctx, &computepb.InsertRegionUrlMapRequest{Project: p, Region: region, UrlMapResource: &computepb.UrlMap{
		Name: proto.String(n("map")), DefaultService: proto.String("regions/" + region + "/backendServices/" + n("bs"))}})
	wait("the URL map", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rums.Delete(c, &computepb.DeleteRegionUrlMapRequest{Project: p, Region: region, UrlMap: n("map")})
	})
	o, err = rthps.Insert(ctx, &computepb.InsertRegionTargetHttpProxyRequest{Project: p, Region: region, TargetHttpProxyResource: &computepb.TargetHttpProxy{
		Name: proto.String(n("proxy")), UrlMap: proto.String("regions/" + region + "/urlMaps/" + n("map"))}})
	wait("the target HTTP proxy", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rthps.Delete(c, &computepb.DeleteRegionTargetHttpProxyRequest{Project: p, Region: region, TargetHttpProxy: n("proxy")})
	})
	o, err = frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(n("rule")), LoadBalancingScheme: proto.String("INTERNAL_MANAGED"), IPProtocol: proto.String("TCP"), PortRange: proto.String("80"),
		Network: proto.String(netRel), Subnetwork: proto.String(subRel), Target: proto.String("regions/" + region + "/targetHttpProxies/" + n("proxy"))}})
	wait("the forwarding rule", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return frs.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: n("rule")})
	})
	rule, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: n("rule")})
	if err != nil {
		t.Fatal(err)
	}
	vip := rule.GetIPAddress()
	if !strings.HasPrefix(vip, "10.81.0.") {
		t.Fatalf("the rule's address %q is not from its subnetwork", vip)
	}

	// The secure tag: an organization tag key and its value.
	keys, err := resourcemanager.NewTagKeysClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close() })
	values, _ := resourcemanager.NewTagValuesClient(ctx, rmOptionsAt(rmEndpoint)...)
	t.Cleanup(func() { _ = values.Close() })
	bindings, _ := resourcemanager.NewTagBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	t.Cleanup(func() { _ = bindings.Close() })
	kop, err := keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: n("tier")}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vop, err := values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "frontend"}})
	if err != nil {
		t.Fatal(err)
	}
	tag, err := vop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tagID, _ := strconv.ParseInt(strings.TrimPrefix(tag.GetName(), "tagValues/"), 10, 64)

	// The two VMs: web (service account web@, tagged) and batch.
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = insts.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	vms := map[string]string{n("web"): webSA, n("batch"): batchSA}
	var wg sync.WaitGroup
	for name, sa := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
				Name:        proto.String(name),
				MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
				Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
				NetworkInterfaces: []*computepb.NetworkInterface{{Subnetwork: proto.String(subRel)}},
				ServiceAccounts:   []*computepb.ServiceAccount{{Email: proto.String(sa), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
				Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
			}})
			if err == nil {
				err = o.Wait(ctx)
			}
			if err != nil {
				t.Errorf("the instance %s: %v", name, err)
			}
		}()
	}
	t.Cleanup(func() {
		var wg sync.WaitGroup
		for name := range vms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if o, err := insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: name}); err == nil {
					_ = o.Wait(context.Background())
				}
			}()
		}
		wg.Wait()
	})
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	web, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: n("web")})
	if err != nil {
		t.Fatal(err)
	}
	bop, err := bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{
		Parent: "//compute.googleapis.com/projects/" + p + "/zones/" + zone + "/instances/" + strconv.FormatUint(web.GetId(), 10), TagValue: tag.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := bop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if o, err := bindings.DeleteTagBinding(context.Background(), &rmpb.DeleteTagBindingRequest{Name: binding.GetName()}); err == nil {
			_ = o.Wait(context.Background())
		}
		if o, err := values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: tag.GetName()}); err == nil {
			_, _ = o.Wait(context.Background())
		}
		if o, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = o.Wait(context.Background())
		}
	})

	run := func(vm, cmd string) string {
		t.Helper()
		sshAddr := vmSSHAddress(t, addr, p, zone, vm)
		var out string
		deadline := time.Now().Add(3 * time.Minute)
		for {
			c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				s, err2 := c.NewSession()
				if err2 == nil {
					b, err3 := s.CombinedOutput(cmd)
					out, err = string(b), err3
				} else {
					err = err2
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
	// status is the HTTP status a VM's curl gets ("000" when it cannot
	// connect).
	status := func(vm, args string) string {
		out := run(vm, "curl -s -o /dev/null -m 10 -w '%{http_code}' "+args+" || true")
		return strings.TrimSpace(out)
	}
	expect := func(what, args string, want map[string]string) {
		t.Helper()
		got := map[string]string{}
		same := func() bool {
			for vm, w := range want {
				got[vm] = status(vm, args)
				if got[vm] != w {
					return false
				}
			}
			return true
		}
		deadline := time.Now().Add(3 * time.Minute)
		for !same() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
		}
		if !same() {
			t.Fatalf("%s: got %v, want %v; web's relay: %s", what, got, want,
				run(n("web"), "sudo nft list chain inet cloudburrow relay; ss -ltn | grep ':470'; sudo journalctl -u cloudburrow-vpc --no-pager | tail -5; "+
					"sudo ls -l /run/cloudburrow-vpc; ps -eo args | grep [s]ocat; curl -sv -m 5 "+args+" 2>&1 | tail -15; "+
					"sudo cat /run/cloudburrow-vpc/relay-47000.hdr | od -c | head -3; echo test | curl -sv -m 5 http://127.0.0.1:47000/ 2>&1 | tail -5"))
		}
	}
	lbURL := "http://" + vip + "/hello"
	expect("both VMs reach the internal load balancer", lbURL, map[string]string{n("web"): "200", n("batch"): "200"})

	// The authorization policy: only web@'s VM.
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	loc := "projects/" + p + "/locations/" + region
	ruleURL := "https://www.googleapis.com/compute/v1/projects/" + p + "/regions/" + region + "/forwardingRules/" + n("rule")
	from := func(res *networksecuritypb.AuthzPolicy_AuthzRule_RequestResource) []*networksecuritypb.AuthzPolicy_AuthzRule {
		return []*networksecuritypb.AuthzPolicy_AuthzRule{{From: &networksecuritypb.AuthzPolicy_AuthzRule_From{
			Sources: []*networksecuritypb.AuthzPolicy_AuthzRule_From_RequestSource{{Resources: []*networksecuritypb.AuthzPolicy_AuthzRule_RequestResource{res}}}}}}
	}
	aop, err := ns.CreateAuthzPolicy(ctx, &networksecuritypb.CreateAuthzPolicyRequest{Parent: loc, AuthzPolicyId: n("authz"), AuthzPolicy: &networksecuritypb.AuthzPolicy{
		Target: &networksecuritypb.AuthzPolicy_Target{LoadBalancingScheme: networksecuritypb.AuthzPolicy_INTERNAL_MANAGED, Resources: []string{ruleURL}},
		Action: networksecuritypb.AuthzPolicy_ALLOW,
		HttpRules: from(&networksecuritypb.AuthzPolicy_AuthzRule_RequestResource{IamServiceAccount: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch{
			MatchPattern: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch_Exact{Exact: webSA}}})}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := aop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if o, err := ns.DeleteAuthzPolicy(context.Background(), &networksecuritypb.DeleteAuthzPolicyRequest{Name: pol.GetName()}); err == nil {
			_ = o.Wait(context.Background())
		}
	})
	expect("the service account policy", lbURL, map[string]string{n("web"): "200", n("batch"): "403"})
	// The same rule's listener reached from this machine: no VM's
	// identity, so the ALLOW policy matches nothing.
	if hostAddr := lbHostAddress(t, addr, "projects/"+p+"/regions/"+region+"/forwardingRules/"+n("rule")); hostAddr != "" {
		r, err := http.Get("http://" + hostAddr + "/hello")
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode != http.StatusForbidden {
				t.Errorf("a request from this machine: %d, want 403", r.StatusCode)
			}
		}
	}
	uop, err := ns.UpdateAuthzPolicy(ctx, &networksecuritypb.UpdateAuthzPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"http_rules"}},
		AuthzPolicy: &networksecuritypb.AuthzPolicy{Name: pol.GetName(), HttpRules: from(&networksecuritypb.AuthzPolicy_AuthzRule_RequestResource{
			TagValueIdSet: &networksecuritypb.AuthzPolicy_AuthzRule_RequestResource_TagValueIdSet{Ids: []int64{tagID}}})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	expect("the secure tag policy", lbURL, map[string]string{n("web"): "200", n("batch"): "403"})

	// Secure Web Proxy on the same network: a gateway security policy
	// whose one rule allows web@'s VM, then the tagged VM.
	nsv, err := networkservices.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsv.Close() })
	gpop, err := ns.CreateGatewaySecurityPolicy(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRequest{Parent: loc, GatewaySecurityPolicyId: n("gsp"),
		GatewaySecurityPolicy: &networksecuritypb.GatewaySecurityPolicy{Description: "VM identity"}})
	if err != nil {
		t.Fatal(err)
	}
	gsp, err := gpop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if o, err := ns.DeleteGatewaySecurityPolicy(context.Background(), &networksecuritypb.DeleteGatewaySecurityPolicyRequest{Name: gsp.GetName()}); err == nil {
			_ = o.Wait(context.Background())
		}
	})
	rop, err := ns.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: gsp.GetName(),
		GatewaySecurityPolicyRuleId: "allow-web", GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: 1,
			Profile:        &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: networksecuritypb.GatewaySecurityPolicyRule_ALLOW},
			SessionMatcher: "source.matchServiceAccount('" + webSA + "')"}})
	if err != nil {
		t.Fatal(err)
	}
	gr, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if o, err := ns.DeleteGatewaySecurityPolicyRule(context.Background(), &networksecuritypb.DeleteGatewaySecurityPolicyRuleRequest{Name: gr.GetName()}); err == nil {
			_ = o.Wait(context.Background())
		}
	})
	gop, err := nsv.CreateGateway(ctx, &networkservicespb.CreateGatewayRequest{Parent: loc, GatewayId: n("swp"),
		Gateway: &networkservicespb.Gateway{Type: networkservicespb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{443},
			GatewaySecurityPolicy: gsp.GetName(), Network: netRel, Subnetwork: subRel, RoutingMode: networkservicespb.Gateway_EXPLICIT_ROUTING_MODE}})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if o, err := nsv.DeleteGateway(context.Background(), &networkservicespb.DeleteGatewayRequest{Name: gw.GetName()}); err == nil {
			_ = o.Wait(context.Background())
		}
	})
	if len(gw.GetAddresses()) != 1 {
		t.Fatalf("the gateway's addresses: %v", gw.GetAddresses())
	}
	proxyArgs := "-x http://" + gw.GetAddresses()[0] + ":443 " + origin.URL + "/through"
	expect("the matchServiceAccount rule", proxyArgs, map[string]string{n("web"): "200", n("batch"): "403"})
	if out := run(n("web"), "curl -s -m 10 "+proxyArgs); out != "origin /through" {
		t.Errorf("web's request through the proxy: %q", out)
	}
	ruop, err := ns.UpdateGatewaySecurityPolicyRule(ctx, &networksecuritypb.UpdateGatewaySecurityPolicyRuleRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"session_matcher"}},
		GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Name: gr.GetName(),
			SessionMatcher: "source.matchTag('" + tag.GetName() + "')"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ruop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	expect("the matchTag rule", proxyArgs, map[string]string{n("web"): "200", n("batch"): "403"})
}

// lbHostAddress is a forwarding rule's listener on this machine
// (cloudburrowAddress), "" when its JSON names none.
func lbHostAddress(t *testing.T, addr, rel string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/compute/v1/" + rel)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	const k = `"cloudburrowAddress":"`
	s := string(b)
	i := strings.Index(s, k)
	if i < 0 {
		return ""
	}
	s = s[i+len(k):]
	return s[:strings.Index(s, `"`)]
}
