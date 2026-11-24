//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestPrivateServiceConnectThroughTheGoClient2140 (#2140), the official
// compute/apiv1 GlobalForwardingRulesRESTClient, ForwardingRulesRESTClient
// and ServiceAttachmentsRESTClient: an endpoint for Google APIs (all-apis)
// on a global PRIVATE_SERVICE_CONNECT address, which turns IN_USE by it and
// RESERVED again once it goes; an unknown bundle (501), a name with a
// hyphen and an address in use refused; a producer's service attachments
// (ACCEPT_AUTOMATIC and ACCEPT_MANUAL) on a PRIVATE_SERVICE_CONNECT NAT
// subnet, a NAT subnet of another purpose refused; consumer endpoints
// ACCEPTED and PENDING, listed in the attachment's connectedEndpoints, the
// manual one ACCEPTED once consumerAcceptLists names the project (patch
// under the fingerprint); setLabels; aggregatedList of both; the NAT
// subnet's deletion refused while in use, and an endpoint CLOSED once its
// attachment is deleted.
// covers: compute.globalForwardingRules.insert, compute.globalForwardingRules.get, compute.globalForwardingRules.list, compute.globalForwardingRules.delete, compute.globalForwardingRules.setLabels, compute.forwardingRules.insert, compute.forwardingRules.get, compute.forwardingRules.list, compute.forwardingRules.delete, compute.forwardingRules.aggregatedList, compute.forwardingRules.setLabels, compute.serviceAttachments.insert, compute.serviceAttachments.get, compute.serviceAttachments.list, compute.serviceAttachments.patch, compute.serviceAttachments.delete, compute.serviceAttachments.aggregatedList
func TestPrivateServiceConnectThroughTheGoClient2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	gaddrs, err := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	gfr, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	fr, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	sas, _ := computeapi.NewServiceAttachmentsRESTClient(ctx, opts...)
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	n := time.Now().UnixNano() % 1e6
	sfx := fmt.Sprint(n)
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// A network of its own, so its ranges collide with no other test's.
	vpc := "psc-" + sfx
	must(wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(vpc), AutoCreateSubnetworks: proto.Bool(false)}})))
	t.Cleanup(func() {
		_, _ = nets.Delete(context.Background(), &computepb.DeleteNetworkRequest{Project: p, Network: vpc})
	})
	netLink := "projects/" + p + "/global/networks/" + vpc
	subnet := func(name, cidr, purpose string) {
		t.Helper()
		s := &computepb.Subnetwork{Name: proto.String(name), Network: proto.String(netLink), IpCidrRange: proto.String(cidr)}
		if purpose != "" {
			s.Purpose = proto.String(purpose)
		}
		must(wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: s})))
		t.Cleanup(func() {
			_ = wait(subs.Delete(context.Background(), &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: name}))
		})
	}
	subnet("main-"+sfx, "10.30.0.0/24", "")
	subnet("nat-"+sfx, "10.30.1.0/24", "PRIVATE_SERVICE_CONNECT")

	// An endpoint for Google APIs.
	ipName := "pscip-" + sfx
	must(wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(ipName), AddressType: proto.String("INTERNAL"), Purpose: proto.String("PRIVATE_SERVICE_CONNECT"),
		Address: proto.String("10.31.0.5"), Network: proto.String(netLink)}})))
	t.Cleanup(func() {
		_ = wait(gaddrs.Delete(context.Background(), &computepb.DeleteGlobalAddressRequest{Project: p, Address: ipName}))
	})
	endpoint := func(name, target string) error {
		return wait(gfr.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: p, ForwardingRuleResource: &computepb.ForwardingRule{
			Name: proto.String(name), Target: proto.String(target), Network: proto.String(netLink), IPAddress: proto.String("global/addresses/" + ipName)}}))
	}
	apis := "apis" + sfx
	if err := endpoint("x"+sfx, "storage-api"); computeCode(err) != 501 {
		t.Fatalf("an unknown bundle: %v", err)
	}
	if err := endpoint("my-apis", "all-apis"); computeCode(err) != 400 {
		t.Fatalf("a hyphen in an endpoint for Google APIs: %v", err)
	}
	must(endpoint(apis, "all-apis"))
	r, err := gfr.Get(ctx, &computepb.GetGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis})
	if err != nil || r.GetPscConnectionStatus() != "ACCEPTED" || r.GetIPAddress() != "10.31.0.5" || r.GetTarget() != "all-apis" || r.GetPscConnectionId() == 0 {
		t.Fatalf("endpoint = %v, %v", r, err)
	}
	if a, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: ipName}); a.GetStatus() != "IN_USE" || len(a.GetUsers()) != 1 {
		t.Fatalf("address = %v", a)
	}
	if err := endpoint("vpcsc"+sfx, "vpc-sc"); computeCode(err) != 400 {
		t.Fatalf("an address in use: %v", err)
	}
	must(wait(gfr.SetLabels(ctx, &computepb.SetLabelsGlobalForwardingRuleRequest{Project: p, Resource: apis,
		GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{LabelFingerprint: r.LabelFingerprint, Labels: map[string]string{"env": "test"}}})))
	found := false
	git := gfr.List(ctx, &computepb.ListGlobalForwardingRulesRequest{Project: p})
	for {
		e, err := git.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		found = found || (e.GetName() == apis && e.GetLabels()["env"] == "test")
	}
	if !found {
		t.Fatal("globalForwardingRules.list misses the labelled endpoint")
	}
	must(wait(gfr.Delete(ctx, &computepb.DeleteGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis})))
	if a, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: ipName}); a.GetStatus() != "RESERVED" {
		t.Fatalf("address after delete = %v", a.GetStatus())
	}

	// A producer's attachments, and endpoints to them.
	attach := func(name, pref, nat string) error {
		return wait(sas.Insert(ctx, &computepb.InsertServiceAttachmentRequest{Project: p, Region: region, ServiceAttachmentResource: &computepb.ServiceAttachment{
			Name: proto.String(name), TargetService: proto.String("projects/" + p + "/regions/" + region + "/forwardingRules/ilb"),
			ConnectionPreference: proto.String(pref), NatSubnets: []string{"regions/" + region + "/subnetworks/" + nat}}}))
	}
	auto, manual := "auto-"+sfx, "manual-"+sfx
	if err := attach("bad-"+sfx, "ACCEPT_AUTOMATIC", "main-"+sfx); computeCode(err) != 400 {
		t.Fatalf("a NAT subnet without PSC purpose: %v", err)
	}
	for _, a := range []struct{ name, pref string }{{auto, "ACCEPT_AUTOMATIC"}, {manual, "ACCEPT_MANUAL"}} {
		must(attach(a.name, a.pref, "nat-"+sfx))
		t.Cleanup(func() {
			_ = wait(sas.Delete(context.Background(), &computepb.DeleteServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: a.name}))
		})
	}
	consume := func(name, att string) error {
		err := wait(fr.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
			Name: proto.String(name), Network: proto.String(netLink), Subnetwork: proto.String("regions/" + region + "/subnetworks/main-" + sfx),
			Target: proto.String("projects/" + p + "/regions/" + region + "/serviceAttachments/" + att)}}))
		if err == nil {
			t.Cleanup(func() {
				_ = wait(fr.Delete(context.Background(), &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: name}))
			})
		}
		return err
	}
	toAuto, toManual := "to-auto-"+sfx, "to-manual-"+sfx
	must(consume(toAuto, auto))
	must(consume(toManual, manual))
	if err := consume("to-none-"+sfx, "nothing-"+sfx); computeCode(err) != 404 {
		t.Fatalf("an endpoint to no attachment: %v", err)
	}
	get := func(name string) *computepb.ForwardingRule {
		r, err := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: name})
		must(err)
		return r
	}
	if a, m := get(toAuto), get(toManual); a.GetPscConnectionStatus() != "ACCEPTED" || m.GetPscConnectionStatus() != "PENDING" ||
		!strings.HasPrefix(a.GetIPAddress(), "10.30.0.") {
		t.Fatalf("endpoints %v / %v", a, m)
	}
	m, err := sas.Get(ctx, &computepb.GetServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: manual})
	if err != nil || len(m.GetConnectedEndpoints()) != 1 || m.GetConnectedEndpoints()[0].GetStatus() != "PENDING" {
		t.Fatalf("connected endpoints = %v, %v", m.GetConnectedEndpoints(), err)
	}
	must(wait(sas.Patch(ctx, &computepb.PatchServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: manual,
		ServiceAttachmentResource: &computepb.ServiceAttachment{Fingerprint: m.Fingerprint,
			ConsumerAcceptLists: []*computepb.ServiceAttachmentConsumerProjectLimit{{ProjectIdOrNum: proto.String(p), ConnectionLimit: proto.Uint32(10)}}}})))
	if get(toManual).GetPscConnectionStatus() != "ACCEPTED" {
		t.Fatalf("after the accept list: %v", get(toManual))
	}
	tm := get(toManual)
	must(wait(fr.SetLabels(ctx, &computepb.SetLabelsForwardingRuleRequest{Project: p, Region: region, Resource: toManual,
		RegionSetLabelsRequestResource: &computepb.RegionSetLabelsRequest{LabelFingerprint: tm.LabelFingerprint, Labels: map[string]string{"env": "test"}}})))
	count := 0
	it := fr.List(ctx, &computepb.ListForwardingRulesRequest{Project: p, Region: region})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		if strings.HasSuffix(e.GetName(), sfx) {
			count++
		}
	}
	agg := 0
	ait := fr.AggregatedList(ctx, &computepb.AggregatedListForwardingRulesRequest{Project: p})
	for {
		e, err := ait.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		for _, r := range e.Value.GetForwardingRules() {
			if strings.HasSuffix(r.GetName(), sfx) {
				agg++
			}
		}
	}
	if count != 2 || agg != 2 {
		t.Fatalf("list %d, aggregatedList %d endpoints", count, agg)
	}
	attachments, aggAtt := 0, 0
	sit := sas.List(ctx, &computepb.ListServiceAttachmentsRequest{Project: p, Region: region})
	for {
		e, err := sit.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		if strings.HasSuffix(e.GetName(), sfx) {
			attachments++
		}
	}
	sait := sas.AggregatedList(ctx, &computepb.AggregatedListServiceAttachmentsRequest{Project: p})
	for {
		e, err := sait.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		for _, a := range e.Value.GetServiceAttachments() {
			if strings.HasSuffix(a.GetName(), sfx) {
				aggAtt++
			}
		}
	}
	if attachments != 2 || aggAtt != 2 {
		t.Fatalf("attachments listed %d, aggregated %d", attachments, aggAtt)
	}
	if err := wait(subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: "nat-" + sfx})); computeCode(err) != 400 {
		t.Fatalf("delete the NAT subnet in use: %v", err)
	}
	must(wait(sas.Delete(ctx, &computepb.DeleteServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: auto})))
	if get(toAuto).GetPscConnectionStatus() != "CLOSED" {
		t.Fatalf("after the attachment is deleted: %v", get(toAuto))
	}
}
