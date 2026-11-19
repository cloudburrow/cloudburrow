package kms

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	cloudkms "google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Default Key Access Justifications policies (#1577), with Google's
// discovery client: set on an organization, a folder and a project, read
// back as set, resolved nearest first, and given to keys created without a
// policy, not to existing ones nor to keys created with one.
func TestKAJPolicyConfigDefaults(t *testing.T) {
	srv := NewServer(store.NewMemory())
	srv.UseHierarchy(fakeHierarchy{})
	hs := httptest.NewServer(NewRESTHandler(srv))
	t.Cleanup(hs.Close)
	ctx := context.Background()
	c, err := cloudkms.NewService(ctx, option.WithEndpoint(hs.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	policy := func(reasons ...string) *cloudkms.KeyAccessJustificationsPolicyConfig {
		return &cloudkms.KeyAccessJustificationsPolicyConfig{
			DefaultKeyAccessJustificationPolicy: &cloudkms.KeyAccessJustificationsPolicy{AllowedAccessReasons: reasons}}
	}
	const mask = "defaultKeyAccessJustificationPolicy"
	got, err := c.Projects.GetKajPolicyConfig("projects/demo-project/kajPolicyConfig").Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "projects/demo-project/kajPolicyConfig" || got.DefaultKeyAccessJustificationPolicy != nil || !got.DefaultPolicyAvailable {
		t.Errorf("an unset config = %+v", got)
	}
	eff, err := c.Projects.ShowEffectiveKeyAccessJustificationsPolicyConfig("projects/demo-project").Do()
	if err != nil {
		t.Fatal(err)
	}
	if eff.EffectiveKajPolicy.DefaultKeyAccessJustificationPolicy != nil {
		t.Errorf("effective with none set = %+v", eff.EffectiveKajPolicy)
	}

	// A key ring and a key made before any default.
	if _, err := c.Projects.Locations.KeyRings.Create("projects/demo-project/locations/global", &cloudkms.KeyRing{}).KeyRingId("r").Do(); err != nil {
		t.Fatal(err)
	}
	ring := "projects/demo-project/locations/global/keyRings/r"
	mk := func(id string, p *cloudkms.KeyAccessJustificationsPolicy) *cloudkms.CryptoKey {
		t.Helper()
		k, err := c.Projects.Locations.KeyRings.CryptoKeys.Create(ring, &cloudkms.CryptoKey{Purpose: "ENCRYPT_DECRYPT",
			KeyAccessJustificationsPolicy: p}).CryptoKeyId(id).Do()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	reasonsOf := func(k *cloudkms.CryptoKey) []string {
		if k.KeyAccessJustificationsPolicy == nil {
			return nil
		}
		return k.KeyAccessJustificationsPolicy.AllowedAccessReasons
	}
	before := mk("before", nil)

	org, err := c.Organizations.UpdateKajPolicyConfig("organizations/9/kajPolicyConfig", policy("CUSTOMER_INITIATED_ACCESS")).UpdateMask(mask).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(org.DefaultKeyAccessJustificationPolicy.AllowedAccessReasons, []string{"CUSTOMER_INITIATED_ACCESS"}) {
		t.Errorf("organization config = %+v", org.DefaultKeyAccessJustificationPolicy)
	}
	if k := mk("from-org", nil); !slices.Equal(reasonsOf(k), []string{"CUSTOMER_INITIATED_ACCESS"}) {
		t.Errorf("a key under the organization's default has %v", reasonsOf(k))
	}
	if _, err := c.Folders.UpdateKajPolicyConfig("folders/1/kajPolicyConfig", policy("GOOGLE_INITIATED_SERVICE")).UpdateMask(mask).Do(); err != nil {
		t.Fatal(err)
	}
	if k := mk("from-folder", nil); !slices.Equal(reasonsOf(k), []string{"GOOGLE_INITIATED_SERVICE"}) {
		t.Errorf("a key under the folder's default has %v", reasonsOf(k))
	}
	// The default applies: GOOGLE_INITIATED_SERVICE alone refuses a call
	// of one's own.
	if _, err := c.Projects.Locations.KeyRings.CryptoKeys.Encrypt(ring+"/cryptoKeys/from-folder", &cloudkms.EncryptRequest{Plaintext: "aGk="}).Do(); err == nil {
		t.Error("encrypt under a default without CUSTOMER_INITIATED_ACCESS succeeded")
	}
	// An empty project policy is not active: the folder's still applies.
	if _, err := c.Projects.UpdateKajPolicyConfig("projects/demo-project/kajPolicyConfig", policy()).UpdateMask(mask).Do(); err != nil {
		t.Fatal(err)
	}
	if eff, err = c.Projects.ShowEffectiveKeyAccessJustificationsPolicyConfig("projects/demo-project").Do(); err != nil {
		t.Fatal(err)
	}
	if eff.EffectiveKajPolicy.Name != "folders/1/kajPolicyConfig" {
		t.Errorf("effective = %s, want folders/1's", eff.EffectiveKajPolicy.Name)
	}
	if _, err := c.Projects.UpdateKajPolicyConfig("projects/demo-project/kajPolicyConfig",
		policy("CUSTOMER_INITIATED_ACCESS", "MODIFIED_CUSTOMER_INITIATED_ACCESS")).UpdateMask(mask).Do(); err != nil {
		t.Fatal(err)
	}
	if eff, err = c.Projects.ShowEffectiveKeyAccessJustificationsPolicyConfig("projects/demo-project").Do(); err != nil {
		t.Fatal(err)
	}
	if eff.EffectiveKajPolicy.Name != "projects/demo-project/kajPolicyConfig" {
		t.Errorf("effective = %s, want the project's", eff.EffectiveKajPolicy.Name)
	}
	if k := mk("from-project", nil); len(reasonsOf(k)) != 2 {
		t.Errorf("a key under the project's default has %v", reasonsOf(k))
	}
	if k := mk("own", &cloudkms.KeyAccessJustificationsPolicy{AllowedAccessReasons: []string{"CUSTOMER_AUTHORIZED_WORKFLOW_SERVICING"}}); !slices.Equal(reasonsOf(k), []string{"CUSTOMER_AUTHORIZED_WORKFLOW_SERVICING"}) {
		t.Errorf("a key with its own policy has %v", reasonsOf(k))
	}
	again, err := c.Projects.Locations.KeyRings.CryptoKeys.Get(before.Name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if reasonsOf(again) != nil {
		t.Errorf("an existing key got %v", reasonsOf(again))
	}

	var gerr *googleapi.Error
	if _, err := c.Projects.UpdateKajPolicyConfig("projects/demo-project/kajPolicyConfig", policy("NOT_A_REASON")).UpdateMask(mask).Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("an unknown reason = %v, want 400", err)
	}
	if _, err := c.Folders.GetKajPolicyConfig("folders/77/kajPolicyConfig").Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
		t.Errorf("a missing folder = %v, want 404", err)
	}
	if _, err := c.Projects.ShowEffectiveKeyAccessJustificationsEnrollmentConfig("projects/demo-project").Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusNotImplemented {
		t.Errorf("the enrollment config = %v, want 501", err)
	}
}
