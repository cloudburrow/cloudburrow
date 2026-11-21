package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kmsinventory"
)

// TestConsoleKMSUsageTracking (#2053): a key's Usage tracking tab draws the
// Inventory API's summary and protected resources, with the same-project
// notice for a project in no organization.
func TestConsoleKMSUsageTracking(t *testing.T) {
	svc := startedKMS(t, kmsConfig(t, "kms"), nil)
	ctx := context.Background()
	api := svc.API()
	ring, err := api.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/demo-proj/locations/us", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := api.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	svc.inventory.Sources = func() []kmsinventory.Source {
		return []kmsinventory.Source{func(_ context.Context, project string) ([]kmsinventory.Resource, error) {
			return []kmsinventory.Resource{{Name: "//storage.googleapis.com/b", ProjectID: project, CloudProduct: "storage",
				ResourceType: "storage.googleapis.com/Bucket", Location: "us", Keys: []string{key.GetName()},
				Versions: []string{key.GetName() + "/cryptoKeyVersions/1"}, Labels: map[string]string{"team": "a"}}}, nil
		}}
	}
	d, err := kmsProvider{svc: svc}.Detail(ctx, "demo-proj", []string{ring.GetName(), "k"})
	if err != nil {
		t.Fatal(err)
	}
	var usage *console.Section
	for i := range d.Sections {
		if d.Sections[i].ID == "usage" {
			usage = &d.Sections[i]
		}
	}
	if usage == nil || usage.Label != "Usage tracking" {
		t.Fatalf("no Usage tracking tab in %v", d.Sections)
	}
	l := usage.Listing
	if l.Unavailable != "" || len(l.Items) != 1 || l.Items[0].Name != "//storage.googleapis.com/b" ||
		l.Items[0].Fields["Crypto key version"] != key.GetName()+"/cryptoKeyVersions/1" || l.Items[0].Fields["Labels"] != "team: a" {
		t.Errorf("Usage tracking rows = %+v", l)
	}
	if len(l.Summary) != 1 || l.Summary[0].Properties[0].Value != "1" || !strings.Contains(usage.Note, "limited to the selected project") {
		t.Errorf("summary %+v, note %q", l.Summary, usage.Note)
	}
}

// TestConsoleKMSControls (#2053): KMS Controls sets and clears a
// project's default Key Access Justifications policy through Cloud KMS's
// own JSON method.
func TestConsoleKMSControls(t *testing.T) {
	svc := startedKMS(t, kmsConfig(t, "kms"), nil)
	ctx := context.Background()
	p := kmsConfigProvider{svc: svc}
	l, err := p.List(ctx, "demo-proj")
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "projects/demo-proj/kajPolicyConfig" {
		t.Fatalf("List = %+v, %v", l, err)
	}
	path := []string{"projects/demo-proj/kajPolicyConfig"}
	if err := p.Edit(ctx, "demo-proj", path, map[string]string{"policy": "allow", "reason_CUSTOMER_INITIATED_ACCESS": "true"}); err != nil {
		t.Fatal(err)
	}
	var c kajConfigBody
	if err := p.call(ctx, http.MethodGet, "/v1/projects/demo-proj/kajPolicyConfig", nil, &c); err != nil || c.Policy == nil ||
		strings.Join(c.Policy.Reasons, ",") != "CUSTOMER_INITIATED_ACCESS" {
		t.Fatalf("after Edit the config is %+v, %v", c, err)
	}
	d, _ := p.Detail(ctx, "demo-proj", path)
	if got := d.Sections[0].Groups[0].Properties[1].Value; got != "Allow specific reason codes: Customer-initiated access" {
		t.Errorf("the page shows %q", got)
	}
	if err := p.Edit(ctx, "demo-proj", path, map[string]string{"policy": "allow"}); err == nil {
		t.Error("Allow specific reason codes with none checked was accepted")
	}
	if err := p.Edit(ctx, "demo-proj", path, map[string]string{"policy": "none"}); err != nil {
		t.Fatal(err)
	}
	c = kajConfigBody{}
	if err := p.call(ctx, http.MethodGet, "/v1/projects/demo-proj/kajPolicyConfig", nil, &c); err != nil || c.Policy != nil {
		t.Errorf("after No default policy the config is %+v, %v", c, err)
	}
	if err := p.Edit(ctx, "demo-proj", []string{"projects/other/kajPolicyConfig"}, map[string]string{"policy": "none"}); err == nil {
		t.Error("another project's configuration was edited")
	}
}
