package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleKMSGooglesKeyPages (#1851): Create key is Google's form, with
// its words and defaults; a key's page has Overview, Versions, Permissions
// and Logs,
// Rotate makes the new version primary, Edit Rotation Period saves the
// schedule, the checked versions are disabled, destroyed and restored
// together, and Key inventory lists the key with Google's columns.
func TestConsoleKMSGooglesKeyPages(t *testing.T) {
	ctx := context.Background()
	p, c, ring := kmsConsole(t)

	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	byName := map[string]console.Field{}
	var order []string
	for _, f := range kmsCreateKeyFieldsAt(now) {
		byName[f.Name] = f
		order = append(order, f.Label)
	}
	if got, want := strings.Join(order, "|"), "Key name|Protection level|Single-tenant HSM instance|EKM connection|Key material|Purpose|Algorithm|"+
		"Trusted wrapping|Rotation period|Starting on|Labels|Duration of 'scheduled for destruction' state"; !strings.HasPrefix(got, "Key name|Protection level|Single-tenant HSM instance|EKM connection|Key material|Purpose|Algorithm|") ||
		!strings.HasSuffix(got, "Rotation period|Starting on|Labels|Duration of 'scheduled for destruction' state") {
		t.Errorf("Create key's fields are %s; want %s", got, want)
	}
	if f := byName["purpose"]; f.OptionLabels["ENCRYPT_DECRYPT"] != "Symmetric encrypt/decrypt" {
		t.Errorf("Purpose shows ENCRYPT_DECRYPT as %q", f.OptionLabels["ENCRYPT_DECRYPT"])
	}
	if f := byName["algorithm"]; f.OptionLabels["RSA_DECRYPT_OAEP_3072_SHA256"] != "3072 bit RSA - OAEP Padding - SHA256 Digest" ||
		f.OptionLabels["EC_SIGN_P256_SHA256"] != "Elliptic Curve P-256 - SHA256 Digest" {
		t.Errorf("Algorithm's words are %q, %q", f.OptionLabels["RSA_DECRYPT_OAEP_3072_SHA256"], f.OptionLabels["EC_SIGN_P256_SHA256"])
	}
	if f := byName["rotationPeriod"]; f.Default != "90d" || f.OptionLabels[""] != "Never (manual rotation)" {
		t.Errorf("Rotation period starts at %q with Never as %q", f.Default, f.OptionLabels[""])
	}
	if f := byName["nextRotationTime"]; f.Default != "2027-02-17T00:00:00Z" {
		t.Errorf("Starting on starts at %q, want one rotation period on", f.Default)
	}
	if f := byName["destroyScheduledDays"]; f.Default != "30" || f.Section != "Additional settings" {
		t.Errorf("the destruction duration is %+v, want 30 days under Additional settings", f)
	}

	// The form as it is submitted: a symmetric key with the defaults.
	values := map[string]string{"cryptoKeyId": "sym", "protectionLevel": "SOFTWARE", "keyMaterial": kmsMaterialGenerated,
		"purpose": "ENCRYPT_DECRYPT", "algorithm": "", "rotationPeriod": "90d",
		"nextRotationTime": time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339), "destroyScheduledDays": "7"}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey", values); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	key := ring + "/cryptoKeys/sym"
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetRotationPeriod().AsDuration() != 90*24*time.Hour || k.GetDestroyScheduledDuration().AsDuration() != 7*24*time.Hour ||
		k.GetPrimary() == nil {
		t.Fatalf("the key read back is %v, %v", k, err)
	}
	// An imported key has no first version; an asymmetric key gets no
	// rotation, which Cloud KMS refuses for it.
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey", map[string]string{"cryptoKeyId": "imp",
		"keyMaterial": kmsMaterialImported, "purpose": "ASYMMETRIC_SIGN", "algorithm": "EC_SIGN_P256_SHA256",
		"rotationPeriod": "90d", "nextRotationTime": values["nextRotationTime"]}); err != nil {
		t.Fatalf("Create key (imported, asymmetric): %v", err)
	}
	vs, err := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: ring + "/cryptoKeys/imp"}).Next()
	if err == nil {
		t.Errorf("an imported key was created with version %v", vs)
	}

	d, err := p.Detail(ctx, "demo-proj", []string{ring, "sym"})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.ID)
	}
	if strings.Join(tabs, ",") != "overview,versions,permissions,logs" {
		t.Errorf("a key's tabs are %v; want Overview, Versions, Permissions, Logs", tabs)
	}
	ids := kmsActionIDs(p, []string{ring, "sym"})
	for _, want := range []string{"rotate", "editrotation", "disableversions", "enableversions", "destroyversions", "restoreversions"} {
		if !slices.Contains(ids, want) {
			t.Errorf("the key's actions %v lack %s", ids, want)
		}
	}
	row := d.Sections[1].Listing.Items[0]
	if !row.Selectable || len(row.Actions) == 0 || row.Actions[len(row.Actions)-1].ID != "destroy" {
		t.Errorf("a version row is %+v; want selectable with its lifecycle menu", row)
	}

	// Rotate: version 2, primary.
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring, "sym"}, "rotate", map[string]string{}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if k, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); !strings.HasSuffix(k.GetPrimary().GetName(), "/2") {
		t.Errorf("after Rotate the primary is %s", k.GetPrimary().GetName())
	}
	// Edit Rotation Period: Never clears the schedule.
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring, "sym"}, "editrotation",
		map[string]string{"rotationPeriod": "", "nextRotationTime": values["nextRotationTime"]}); err != nil {
		t.Fatalf("Edit Rotation Period: %v", err)
	}
	if k, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); k.GetRotationPeriod() != nil || k.GetNextRotationTime() != nil {
		t.Errorf("after Never (manual rotation) the key rotates every %v from %v", k.GetRotationPeriod(), k.GetNextRotationTime())
	}
	// The checked versions, together.
	for _, step := range []struct{ action, want string }{
		{"disableversions", "DISABLED"}, {"destroyversions", "DESTROY_SCHEDULED"}, {"restoreversions", "DISABLED"}, {"enableversions", "ENABLED"},
	} {
		if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring, "sym"}, step.action, map[string]string{kmsVersionsField: "1\n2"}); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		for _, n := range []string{"1", "2"} {
			v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/" + n})
			if err != nil || v.GetState().String() != step.want {
				t.Errorf("after %s version %s is %v, %v; want %s", step.action, n, v.GetState(), err, step.want)
			}
		}
	}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring, "sym"}, "disableversions", map[string]string{kmsVersionsField: ""}); err == nil {
		t.Error("Disable with no version checked was performed")
	}

	inv, err := kmsInventoryProvider{svc: p.svc}.List(ctx, "demo-proj")
	if err != nil {
		t.Fatal(err)
	}
	var found *console.Resource
	for i := range inv.Items {
		if inv.Items[i].Name == "sym" {
			found = &inv.Items[i]
		}
	}
	if found == nil || found.Status != "Available" || found.Fields["Key ring"] != "crypto" || found.Fields["Purpose"] != "Symmetric encrypt/decrypt" ||
		found.Fields["Rotation frequency"] != "Never (manual rotation)" || !strings.HasPrefix(found.Link, "/kms/keyrings/projects%2Fdemo-proj%2F") {
		t.Errorf("Key inventory's row is %+v", found)
	}
	if strings.Join(inv.Columns, ",") != strings.Join(kmsInventoryColumns, ",") {
		t.Errorf("Key inventory's columns are %v", inv.Columns)
	}
}

// Create key ring is Google's: Key ring name, Key ring location, Create,
// then the ring's Create key (#1851).
func TestConsoleKMSCreateKeyRingIsGoogles(t *testing.T) {
	label, fields := kmsProvider{}.CreateForm()
	if label != "Create key ring" || fields[0].Label != "Key ring name" || fields[1].Label != "Key ring location" {
		t.Errorf("Create key ring is %q with %+v", label, fields)
	}
	if (kmsProvider{}).CreateSubmit() != "Create" || (kmsProvider{}).CreateThen() != "createkey" {
		t.Error("Create key ring does not end in Create and go on to Create key")
	}
}
