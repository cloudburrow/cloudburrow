package compute

import (
	"context"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestPrivateServiceConnectThroughTheGoClient (#2140): an endpoint for
// Google APIs (all-apis) on a PRIVATE_SERVICE_CONNECT address, which turns
// IN_USE and back; the bundle, name and address rules; a producer's service
// attachment with a PSC NAT subnet; a consumer endpoint ACCEPTED under
// ACCEPT_AUTOMATIC, PENDING under ACCEPT_MANUAL until the accept list names
// its project, listed in connectedEndpoints, CLOSED once the attachment
// goes.
func TestPrivateServiceConnectThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	_, opts := newTestServer(t, Options{})
	gaddrs, err := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	gfr, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	fr, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	sas, _ := computeapi.NewServiceAttachmentsRESTClient(ctx, opts...)
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	const consumer, producer, region = "psc-c", "psc-p", "us-central1"
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	net := "projects/" + consumer + "/global/networks/default"
	if err := wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: consumer, AddressResource: &computepb.Address{
		Name: proto.String("psc-ip"), AddressType: proto.String("INTERNAL"), Purpose: proto.String("PRIVATE_SERVICE_CONNECT"),
		Address: proto.String("10.255.255.254"), Network: proto.String(net)}})); err != nil {
		t.Fatal(err)
	}
	endpoint := func(name, target, ip string) error {
		return wait(gfr.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: consumer, ForwardingRuleResource: &computepb.ForwardingRule{
			Name: proto.String(name), Target: proto.String(target), Network: proto.String(net), IPAddress: proto.String(ip)}}))
	}
	if err := endpoint("apis", "storage-api", "global/addresses/psc-ip"); code(err) != 501 {
		t.Fatalf("an unknown bundle: %v", err)
	}
	if err := endpoint("my-apis", "all-apis", "global/addresses/psc-ip"); code(err) != 400 {
		t.Fatalf("a hyphen in an endpoint for Google APIs: %v", err)
	}
	if err := endpoint("allapis", "all-apis", "10.255.255.254"); err != nil {
		t.Fatal(err)
	}
	r, err := gfr.Get(ctx, &computepb.GetGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "allapis"})
	if err != nil || r.GetPscConnectionStatus() != "ACCEPTED" || r.GetIPAddress() != "10.255.255.254" || r.GetTarget() != "all-apis" || r.GetPscConnectionId() == 0 {
		t.Fatalf("endpoint = %v, %v", r, err)
	}
	a, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: consumer, Address: "psc-ip"})
	if a.GetStatus() != "IN_USE" || len(a.GetUsers()) != 1 || !strings.HasSuffix(a.GetUsers()[0], "/global/forwardingRules/allapis") {
		t.Fatalf("address = %v", a)
	}
	if err := endpoint("vpcsc", "vpc-sc", "global/addresses/psc-ip"); code(err) != 400 {
		t.Fatalf("an address in use: %v", err)
	}
	if err := wait(gfr.Delete(ctx, &computepb.DeleteGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "allapis"})); err != nil {
		t.Fatal(err)
	}
	if a, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: consumer, Address: "psc-ip"}); a.GetStatus() != "RESERVED" {
		t.Fatalf("address after delete = %v", a.GetStatus())
	}

	// The producer: a PSC NAT subnet and an attachment.
	if err := wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: producer, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String("plain"), Network: proto.String("global/networks/default"), IpCidrRange: proto.String("10.0.1.0/24")}})); err != nil {
		t.Fatal(err)
	}
	if err := wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: producer, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String("psc-nat"), Network: proto.String("global/networks/default"), IpCidrRange: proto.String("10.0.2.0/24"),
		Purpose: proto.String("PRIVATE_SERVICE_CONNECT")}})); err != nil {
		t.Fatal(err)
	}
	attach := func(name, pref, nat string) error {
		return wait(sas.Insert(ctx, &computepb.InsertServiceAttachmentRequest{Project: producer, Region: region, ServiceAttachmentResource: &computepb.ServiceAttachment{
			Name: proto.String(name), TargetService: proto.String("projects/" + producer + "/regions/" + region + "/forwardingRules/ilb"),
			ConnectionPreference: proto.String(pref), NatSubnets: []string{"regions/" + region + "/subnetworks/" + nat}}}))
	}
	if err := attach("bad", "ACCEPT_AUTOMATIC", "plain"); code(err) != 400 {
		t.Fatalf("a NAT subnet without PSC purpose: %v", err)
	}
	if err := attach("auto", "ACCEPT_AUTOMATIC", "psc-nat"); err != nil {
		t.Fatal(err)
	}
	if err := attach("manual", "ACCEPT_MANUAL", "psc-nat"); err != nil {
		t.Fatal(err)
	}
	consume := func(name, att string) error {
		return wait(fr.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: consumer, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
			Name: proto.String(name), Network: proto.String(net), Subnetwork: proto.String("regions/" + region + "/subnetworks/default"),
			Target: proto.String("projects/" + producer + "/regions/" + region + "/serviceAttachments/" + att)}}))
	}
	if err := consume("to-auto", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := consume("to-manual", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := consume("to-none", "nothing"); code(err) != 404 {
		t.Fatalf("an endpoint to no attachment: %v", err)
	}
	status := func(name string) string {
		r, err := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: consumer, Region: region, ForwardingRule: name})
		if err != nil {
			t.Fatal(err)
		}
		return r.GetPscConnectionStatus()
	}
	if status("to-auto") != "ACCEPTED" || status("to-manual") != "PENDING" {
		t.Fatalf("statuses %s %s", status("to-auto"), status("to-manual"))
	}
	if r, _ := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: consumer, Region: region, ForwardingRule: "to-auto"}); !strings.HasPrefix(r.GetIPAddress(), "10.128.0.") {
		t.Fatalf("endpoint IP = %s", r.GetIPAddress())
	}
	m, _ := sas.Get(ctx, &computepb.GetServiceAttachmentRequest{Project: producer, Region: region, ServiceAttachment: "manual"})
	if len(m.GetConnectedEndpoints()) != 1 || m.GetConnectedEndpoints()[0].GetStatus() != "PENDING" {
		t.Fatalf("connected endpoints = %v", m.GetConnectedEndpoints())
	}
	if err := wait(sas.Patch(ctx, &computepb.PatchServiceAttachmentRequest{Project: producer, Region: region, ServiceAttachment: "manual",
		ServiceAttachmentResource: &computepb.ServiceAttachment{Fingerprint: m.Fingerprint,
			ConsumerAcceptLists: []*computepb.ServiceAttachmentConsumerProjectLimit{{ProjectIdOrNum: proto.String(consumer), ConnectionLimit: proto.Uint32(10)}}}})); err != nil {
		t.Fatal(err)
	}
	if status("to-manual") != "ACCEPTED" {
		t.Fatalf("after the accept list: %s", status("to-manual"))
	}
	var names []string
	it := fr.AggregatedList(ctx, &computepb.AggregatedListForwardingRulesRequest{Project: consumer})
	for {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Value.GetForwardingRules() {
			names = append(names, r.GetName())
		}
	}
	if len(names) != 2 {
		t.Fatalf("aggregated = %v", names)
	}
	if err := wait(subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: producer, Region: region, Subnetwork: "psc-nat"})); code(err) != 400 {
		t.Fatalf("delete the NAT subnet in use: %v", err)
	}
	if err := wait(sas.Delete(ctx, &computepb.DeleteServiceAttachmentRequest{Project: producer, Region: region, ServiceAttachment: "auto"})); err != nil {
		t.Fatal(err)
	}
	if status("to-auto") != "CLOSED" {
		t.Fatalf("after the attachment is deleted: %s", status("to-auto"))
	}
}
