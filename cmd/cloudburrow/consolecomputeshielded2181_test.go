package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleShieldedVM2181: Create an instance's Security pane opens with
// Shielded VM's three checkboxes and Google's defaults; a VM created with
// Secure Boot on shows it in the Details page's Shielded VM group; Edit
// draws the settings disabled, with the reason and unsent, while the VM
// runs, and once it is stopped changes them with
// updateShieldedInstanceConfig, refusing Integrity Monitoring without the
// vTPM as the API does.
func TestConsoleShieldedVM2181(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "sv"
	vms := computeInstancesProvider{s}
	_, fields := vms.CreateForm()
	var sec []string
	for _, f := range fields {
		if f.Section == "Security" {
			sec = append(sec, f.Name+"="+f.Default+"/"+f.Subsection)
		}
	}
	if got := strings.Join(sec, ","); !strings.HasPrefix(got, "enableSecureBoot=false/Shielded VM,enableVtpm=true/Shielded VM,enableIntegrityMonitoring=true/Shielded VM,serviceAccount=") {
		t.Fatalf("the Security pane = %s", got)
	}
	if f := vpcField(fields, "enableSecureBoot"); f.Label != "Turn on Secure Boot" || f.SubsectionNote != shieldedNote {
		t.Fatalf("Turn on Secure Boot = %+v", f)
	}
	if f := vpcField(fields, "enableIntegrityMonitoring"); f.Label != "Turn on Integrity Monitoring" || strings.Join(f.Requires, ",") != "enableVtpm" {
		t.Fatalf("Turn on Integrity Monitoring = %+v", f)
	}

	if _, err := vms.Create(ctx, project, map[string]string{"name": "sb", "zone": "us-central1-a", "machineType": "e2-small",
		"enableSecureBoot": "true", "enableVtpm": "true", "enableIntegrityMonitoring": "true"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	path := []string{"us-central1-a", "sb"}
	d, err := vms.Detail(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	if p := c2139Props(d); !strings.Contains(p, "Secure Boot=On") || !strings.Contains(p, "vTPM=On") || !strings.Contains(p, "Integrity Monitoring=On") {
		t.Fatalf("Shielded VM on the Details page: %s", p)
	}

	// Running: drawn, disabled, unsent.
	for _, n := range []string{"enableSecureBoot", "enableVtpm", "enableIntegrityMonitoring"} {
		f := vpcField(d.Edit.Fields, n)
		if f == nil || !f.Immutable || f.Section != "Shielded VM" || f.Default != "true" {
			t.Fatalf("a running VM's %s = %+v", n, f)
		}
	}
	if f := vpcField(d.Edit.Fields, "enableSecureBoot"); f.Help != shieldedRunningNote {
		t.Fatalf("the running note = %q", f.Help)
	}
	if err := vms.Edit(ctx, project, path, map[string]string{"enableSecureBoot": "false"}); err == nil {
		t.Error("a running VM's Secure Boot was turned off")
	}

	// Stopped: changed with updateShieldedInstanceConfig.
	if err := vms.ActAt(ctx, project, path, "stop", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	d, _ = vms.Detail(ctx, project, path)
	if f := vpcField(d.Edit.Fields, "enableSecureBoot"); f.Immutable || f.Help == shieldedRunningNote {
		t.Fatalf("a stopped VM's Secure Boot = %+v", f)
	}
	if err := vms.Edit(ctx, project, path, map[string]string{"enableSecureBoot": "true", "enableVtpm": "false", "enableIntegrityMonitoring": "true"}); err == nil {
		t.Error("Integrity Monitoring was kept without the vTPM")
	}
	if err := vms.Edit(ctx, project, path, map[string]string{"enableSecureBoot": "false", "enableVtpm": "true", "enableIntegrityMonitoring": "false"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	o, _ := s.call("GET", project, "zones/us-central1-a/instances/sb", nil, nil)
	cfg := vmMap(o, "shieldedInstanceConfig")
	if cfg["enableSecureBoot"] != false || cfg["enableVtpm"] != true || cfg["enableIntegrityMonitoring"] != false {
		t.Fatalf("shieldedInstanceConfig = %v", cfg)
	}
	d, _ = vms.Detail(ctx, project, path)
	if p := c2139Props(d); !strings.Contains(p, "Secure Boot=Off") || !strings.Contains(p, "Integrity Monitoring=Off") {
		t.Fatalf("the Details page after Edit: %s", p)
	}
}
