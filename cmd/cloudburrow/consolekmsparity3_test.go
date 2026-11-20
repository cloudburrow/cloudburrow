package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleKMSConfirmationsAreGoogles (#1851): Create key is a page of
// its own ending in Create; Rotate, Make primary version, Disable, Enable
// and Restore are confirmed with their button alone, as Google's prompts
// are; Destroy asks for the key's name and ends in Schedule Destruction.
func TestConsoleKMSConfirmationsAreGoogles(t *testing.T) {
	ctx := context.Background()
	p, _, ring := kmsConsole(t)

	byID := func(path []string) map[string]console.Action {
		out := map[string]console.Action{}
		for _, a := range p.DetailActions(ctx, "demo-proj", path) {
			out[a.ID] = a
		}
		return out
	}
	if a := byID([]string{ring})["createkey"]; !a.Page || a.Submit != "Create" {
		t.Errorf("Create key is %+v; want a page ending in Create", a)
	}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey",
		map[string]string{"cryptoKeyId": "sym", "purpose": "ENCRYPT_DECRYPT", "algorithm": "GOOGLE_SYMMETRIC_ENCRYPTION"}); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	key := byID([]string{ring, "sym"})
	for _, id := range []string{"rotate", "disableversions", "enableversions", "restoreversions"} {
		if a := key[id]; a.ConfirmStyle != console.DeleteConfirmClick || a.Confirm == "" {
			t.Errorf("%s is confirmed as %+v; want its button alone", id, a)
		}
	}
	if a := key["destroyversions"]; a.ConfirmStyle != "" || a.Submit != "Schedule Destruction" || !a.Destructive {
		t.Errorf("Destroy is %+v; want the key's name typed back, then Schedule Destruction", a)
	}

	// Version 2, not primary: its row offers Make primary version,
	// confirmed with Make primary.
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring, "sym"}, "addversion", map[string]string{}); err != nil {
		t.Fatalf("Add version: %v", err)
	}
	d, err := p.Detail(ctx, "demo-proj", []string{ring, "sym"})
	if err != nil || len(d.Sections) < 2 || d.Sections[0].Kind != console.KindProperties || d.Sections[1].ID != "versions" {
		t.Fatalf("the key's page is %+v, %v; want Overview's details, then Versions", d.Sections, err)
	}
	var mp, destroy console.Action
	for _, row := range d.Sections[1].Listing.Items {
		for _, a := range row.Actions {
			if row.Name == "2" && a.ID == "makeprimary" {
				mp = a
			}
			if a.ID == "destroy" {
				destroy = a
			}
		}
	}
	if mp.Label != "Make primary version" || mp.Submit != "Make primary" || mp.ConfirmStyle != console.DeleteConfirmClick {
		t.Errorf("Make primary version is %+v", mp)
	}
	if destroy.Submit != "Schedule Destruction" {
		t.Errorf("a version row's Destroy is %+v; want Schedule Destruction", destroy)
	}
	if !strings.Contains(consoleDetailText(d), "Symmetric encrypt/decrypt") {
		t.Error("Overview does not show the key's purpose")
	}
}

// TestConsoleKMSAlgorithmFollowsPurpose (#1851): Create key's Algorithm
// lists the chosen purpose's algorithms only, Google's symmetric one first;
// an external symmetric key still gets EXTERNAL_SYMMETRIC_ENCRYPTION.
func TestConsoleKMSAlgorithmFollowsPurpose(t *testing.T) {
	var alg console.Field
	for _, f := range kmsCreateKeyFields() {
		if f.Name == "algorithm" {
			alg = f
		}
	}
	if alg.OptionsBy != "purpose" || alg.Default != "GOOGLE_SYMMETRIC_ENCRYPTION" {
		t.Fatalf("Algorithm is %+v; want it to follow Purpose from Google's symmetric algorithm", alg)
	}
	if got := alg.OptionsFor["ASYMMETRIC_SIGN"]; !slices.Contains(got, "EC_SIGN_P256_SHA256") || slices.Contains(got, "HMAC_SHA256") ||
		slices.Contains(got, "GOOGLE_SYMMETRIC_ENCRYPTION") {
		t.Errorf("Asymmetric sign lists %v", got)
	}
	if got := alg.OptionsFor["MAC"]; !slices.Contains(got, "HMAC_SHA256") || slices.Contains(got, "EC_SIGN_P256_SHA256") {
		t.Errorf("MAC lists %v", got)
	}
	for p, opts := range alg.OptionsFor {
		for _, o := range opts {
			if !slices.Contains(alg.Options, o) {
				t.Errorf("%s's %s is not among Algorithm's options", p, o)
			}
		}
	}
	req, err := kmsCreateKeyRequest("projects/p/locations/global/keyRings/r", map[string]string{"cryptoKeyId": "ext",
		"protectionLevel": "EXTERNAL", "keyMaterial": kmsMaterialExternal, "purpose": "ENCRYPT_DECRYPT", "algorithm": "GOOGLE_SYMMETRIC_ENCRYPTION"})
	if err != nil || req.GetCryptoKey().GetVersionTemplate().GetAlgorithm() != kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED {
		t.Errorf("an external symmetric key's request is %v, %v; want the algorithm left to CreateCryptoKey", req, err)
	}
}

// TestConsoleKMSInventoryHidesEKMColumn (#1851): Key inventory's EKM via
// VPC connection is off until turned on, as Google's is.
func TestConsoleKMSInventoryHidesEKMColumn(t *testing.T) {
	p, _, _ := kmsConsole(t)
	inv, err := kmsInventoryProvider{svc: p.svc}.List(context.Background(), "demo-proj")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(inv.HiddenColumns, ",") != "EKM via VPC connection" || !slices.Contains(inv.Columns, "EKM via VPC connection") {
		t.Errorf("Key inventory hides %v of %v", inv.HiddenColumns, inv.Columns)
	}
}

// TestConsoleKMSAutokeyConfigurationSource (#1851): Configure starts with
// Google's Configuration source, Configure for project or folder, or
// Inherit, which clears the configuration so the parent's applies.
func TestConsoleKMSAutokeyConfigurationSource(t *testing.T) {
	ctx := context.Background()
	kp, _ := kmsEditFixture(t)
	p := kmsAutokeyProvider{svc: kp.svc}
	admin, err := kmsapi.NewAutokeyAdminClient(ctx, clientOpts(kp.svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const project = "autokey-source"
	cfgName := "projects/" + project + "/autokeyConfig"
	fields := func() map[string]console.Field {
		out := map[string]console.Field{}
		for _, a := range p.DetailActions(ctx, project, []string{cfgName}) {
			if a.ID == "editconfig" {
				if a.Submit != "Submit" {
					t.Errorf("Configure ends in %q; want Submit", a.Submit)
				}
				for _, f := range a.Fields {
					out[f.Name] = f
				}
			}
		}
		return out
	}
	f := fields()
	src := f["configurationSource"]
	if src.Label != "Configuration source" || src.OptionLabels[kmsAutokeySourceConfigure] != "Configure for project" ||
		src.OptionLabels[kmsAutokeySourceInherit] != "Inherit" || src.Default != kmsAutokeySourceInherit {
		t.Errorf("Configuration source is %+v; want Configure for project or Inherit, Inherit while unconfigured", src)
	}
	if m := f["keyProjectResolutionMode"]; m.OptionLabels["DISABLED"] != "Disable autokey for project" ||
		m.ShowWhen != "configurationSource="+kmsAutokeySourceConfigure {
		t.Errorf("Configuration is %+v", m)
	}
	if err := p.ActAt(ctx, project, []string{cfgName}, "editconfig", map[string]string{
		"configurationSource": kmsAutokeySourceConfigure, "keyProjectResolutionMode": "RESOURCE_PROJECT"}); err != nil {
		t.Fatalf("Configure for project: %v", err)
	}
	if got, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName}); err != nil ||
		got.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_RESOURCE_PROJECT {
		t.Fatalf("GetAutokeyConfig = %v, %v", got, err)
	}
	if f := fields(); f["configurationSource"].Default != kmsAutokeySourceConfigure {
		t.Errorf("a configured project's source starts at %q", f["configurationSource"].Default)
	}
	if err := p.ActAt(ctx, project, []string{cfgName}, "editconfig", map[string]string{"configurationSource": kmsAutokeySourceInherit}); err != nil {
		t.Fatalf("Inherit: %v", err)
	}
	got, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName})
	if err != nil || got.GetKeyProject() != "" || got.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_KEY_PROJECT_RESOLUTION_MODE_UNSPECIFIED {
		t.Errorf("after Inherit GetAutokeyConfig = %v, %v; want it cleared", got, err)
	}
}
