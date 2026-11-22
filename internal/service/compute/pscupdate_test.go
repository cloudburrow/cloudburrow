package compute

import (
	"context"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestForwardingRulePatchAndSetTarget (#2140): an endpoint for a
// published service turns global access on and off by patch under its
// fingerprint, other fields are refused (delete and create instead), an
// endpoint for Google APIs takes no global access, and setTarget accepts
// only the target an endpoint already has.
func TestForwardingRulePatchAndSetTarget(t *testing.T) {
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
	const consumer, producer, region = "pu-c", "pu-p", "us-central1"
	wait := func(op *computeapi.Operation, err error) error {
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	net := "projects/" + consumer + "/global/networks/default"
	if err := wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: producer, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String("psc-nat"), Network: proto.String("global/networks/default"), IpCidrRange: proto.String("10.0.2.0/24"),
		Purpose: proto.String("PRIVATE_SERVICE_CONNECT")}})); err != nil {
		t.Fatal(err)
	}
	att := "projects/" + producer + "/regions/" + region + "/serviceAttachments/svc"
	for _, n := range []string{"svc", "svc2"} {
		if err := wait(sas.Insert(ctx, &computepb.InsertServiceAttachmentRequest{Project: producer, Region: region, ServiceAttachmentResource: &computepb.ServiceAttachment{
			Name: proto.String(n), TargetService: proto.String("projects/" + producer + "/regions/" + region + "/forwardingRules/ilb"),
			ConnectionPreference: proto.String("ACCEPT_AUTOMATIC"), NatSubnets: []string{"regions/" + region + "/subnetworks/psc-nat"}}})); err != nil {
			t.Fatal(err)
		}
	}
	if err := wait(fr.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: consumer, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("ep"), Network: proto.String(net), Subnetwork: proto.String("regions/" + region + "/subnetworks/default"),
		Target: proto.String(att)}})); err != nil {
		t.Fatal(err)
	}
	get := func() *computepb.ForwardingRule {
		r, err := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: consumer, Region: region, ForwardingRule: "ep"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	patch := func(r *computepb.ForwardingRule) error {
		return wait(fr.Patch(ctx, &computepb.PatchForwardingRuleRequest{Project: consumer, Region: region, ForwardingRule: "ep", ForwardingRuleResource: r}))
	}
	r := get()
	if r.GetAllowPscGlobalAccess() {
		t.Fatal("global access on by default")
	}
	if err := patch(&computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true), Fingerprint: proto.String("c3RhbGU=")}); code(err) != 412 {
		t.Errorf("stale fingerprint: %v", err)
	}
	if err := patch(&computepb.ForwardingRule{Description: proto.String("new")}); code(err) != 400 {
		t.Errorf("patching the description: %v", err)
	}
	if err := patch(&computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true), Fingerprint: r.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	r2 := get()
	if !r2.GetAllowPscGlobalAccess() || r2.GetFingerprint() == r.GetFingerprint() {
		t.Fatalf("after patch = %v", r2)
	}
	// The resource sent back as read, with global access off.
	r2.AllowPscGlobalAccess = proto.Bool(false)
	if err := patch(r2); err != nil {
		t.Fatalf("patching the resource as read: %v", err)
	}
	if get().GetAllowPscGlobalAccess() {
		t.Error("global access still on")
	}
	setTarget := func(target string) error {
		return wait(fr.SetTarget(ctx, &computepb.SetTargetForwardingRuleRequest{Project: consumer, Region: region, ForwardingRule: "ep",
			TargetReferenceResource: &computepb.TargetReference{Target: proto.String(target)}}))
	}
	if err := setTarget("projects/" + producer + "/regions/" + region + "/serviceAttachments/svc2"); code(err) != 400 {
		t.Errorf("a new target: %v", err)
	}
	if err := setTarget(att); err != nil {
		t.Errorf("the same target: %v", err)
	}

	// An endpoint for Google APIs.
	if err := wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: consumer, AddressResource: &computepb.Address{
		Name: proto.String("psc-ip"), AddressType: proto.String("INTERNAL"), Purpose: proto.String("PRIVATE_SERVICE_CONNECT"),
		Address: proto.String("10.255.255.254"), Network: proto.String(net)}})); err != nil {
		t.Fatal(err)
	}
	if err := wait(gfr.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: consumer, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String("apis"), Target: proto.String("all-apis"), Network: proto.String(net), IPAddress: proto.String("global/addresses/psc-ip")}})); err != nil {
		t.Fatal(err)
	}
	if err := wait(gfr.Patch(ctx, &computepb.PatchGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "apis",
		ForwardingRuleResource: &computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true)}})); code(err) != 400 {
		t.Errorf("global access on an endpoint for Google APIs: %v", err)
	}
	if err := wait(gfr.Patch(ctx, &computepb.PatchGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "apis",
		ForwardingRuleResource: &computepb.ForwardingRule{NetworkTier: proto.String("PREMIUM")}})); err != nil {
		t.Errorf("networkTier PREMIUM: %v", err)
	}
	if err := wait(gfr.SetTarget(ctx, &computepb.SetTargetGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "apis",
		TargetReferenceResource: &computepb.TargetReference{Target: proto.String("vpc-sc")}})); code(err) != 400 {
		t.Errorf("a new bundle: %v", err)
	}
	if err := wait(gfr.SetTarget(ctx, &computepb.SetTargetGlobalForwardingRuleRequest{Project: consumer, ForwardingRule: "apis",
		TargetReferenceResource: &computepb.TargetReference{Target: proto.String("all-apis")}})); err != nil {
		t.Errorf("the same bundle: %v", err)
	}
}
