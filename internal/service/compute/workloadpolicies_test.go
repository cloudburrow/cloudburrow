package compute

import "testing"

// TestManagedGroupWorkloadPolicies (#2206): a workload policy's fields,
// and the rules for a managed instance group that uses one.
func TestManagedGroupWorkloadPolicies(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	z := "zones/us-central1-a/"
	wp := func(name string, body obj) obj { return obj{"name": name, "workloadPolicy": body} }
	wantStatus(t, s, 400, "AUTO_CONNECT or PROVISION_ONLY", "POST", r+"resourcePolicies", wp("x", obj{"type": "HIGH_THROUGHPUT", "acceleratorTopologyMode": "FAST"}))
	wantStatus(t, s, 400, "such as 1x72", "POST", r+"resourcePolicies", wp("x", obj{"type": "HIGH_THROUGHPUT", "acceleratorTopology": "72"}))
	wantStatus(t, s, 400, "can't be set with acceleratorTopology", "POST", r+"resourcePolicies",
		wp("x", obj{"type": "HIGH_THROUGHPUT", "acceleratorTopology": "1x72", "maxTopologyDistance": "BLOCK"}))
	mustCall(t, s, "POST", r+"resourcePolicies", wp("ha", obj{"type": "HIGH_AVAILABILITY"}))
	mustCall(t, s, "POST", r+"resourcePolicies", wp("ht", obj{"type": "HIGH_THROUGHPUT", "acceleratorTopology": "1x72", "acceleratorTopologyMode": "PROVISION_ONLY"}))
	mustCall(t, s, "POST", "regions/europe-west1/resourcePolicies", wp("eu", obj{"type": "HIGH_THROUGHPUT", "maxTopologyDistance": "BLOCK"}))
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "spread", "groupPlacementPolicy": obj{"availabilityDomainCount": 2}})
	boot := []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}
	mustCall(t, s, "POST", "global/instanceTemplates", obj{"name": "web", "properties": obj{"machineType": "e2-small", "disks": boot}})
	mustCall(t, s, "POST", "global/instanceTemplates", obj{"name": "web2", "properties": obj{"machineType": "e2-small", "disks": boot}})
	mustCall(t, s, "POST", "global/instanceTemplates", obj{"name": "placed", "properties": obj{"machineType": "e2-small", "disks": boot,
		"resourcePolicies": []any{"spread"}}})
	mig := func(name, tpl string, size int, pol string) obj {
		b := obj{"name": name, "instanceTemplate": "global/instanceTemplates/" + tpl, "targetSize": float64(size)}
		if pol != "" {
			b["resourcePolicies"] = obj{"workloadPolicy": pol}
		}
		return b
	}
	wantStatus(t, s, 404, "", "POST", z+"instanceGroupManagers", mig("g", "web", 0, r+"resourcePolicies/none"))
	wantStatus(t, s, 400, "must be a workload policy", "POST", z+"instanceGroupManagers", mig("g", "web", 0, r+"resourcePolicies/spread"))
	wantStatus(t, s, 400, "group's region, us-central1", "POST", z+"instanceGroupManagers", mig("g", "web", 0, "regions/europe-west1/resourcePolicies/eu"))
	wantStatus(t, s, 400, "together with placement policies", "POST", z+"instanceGroupManagers", mig("g", "placed", 0, r+"resourcePolicies/ht"))
	wantStatus(t, s, 400, "A4X Max, A4X, A4", "POST", z+"instanceGroupManagers", mig("g", "web", 2, r+"resourcePolicies/ht"))
	mustCall(t, s, "POST", z+"instanceGroupManagers", mig("g", "web", 0, "https://www.googleapis.com/compute/v1/projects/p/"+r+"resourcePolicies/ht"))
	got := mustCall(t, s, "GET", z+"instanceGroupManagers/g", nil)
	if workloadPolicyOf(got) != "projects/p/"+r+"resourcePolicies/ht" {
		t.Fatalf("resourcePolicies = %v", got["resourcePolicies"])
	}
	wantStatus(t, s, 400, "A4X Max, A4X, A4", "PATCH", z+"instanceGroupManagers/g", obj{"targetSize": float64(1)})
	wantStatus(t, s, 400, "second instance template", "PATCH", z+"instanceGroupManagers/g", obj{"versions": []any{
		obj{"instanceTemplate": "global/instanceTemplates/web"}, obj{"name": "canary", "instanceTemplate": "global/instanceTemplates/web2", "targetSize": obj{"fixed": float64(1)}}}})
	wantStatus(t, s, 400, "Resource policies update without workload policy is not allowed", "PATCH", z+"instanceGroupManagers/g", obj{"resourcePolicies": obj{"workloadPolicy": ""}})
	wantStatus(t, s, 400, "is already being used by 'projects/p/zones/us-central1-a/instanceGroupManagers/g'", "DELETE", r+"resourcePolicies/ht", nil)
	mustCall(t, s, "PATCH", z+"instanceGroupManagers/g", obj{"resourcePolicies": obj{"workloadPolicy": r + "resourcePolicies/ha"}})
	if got := mustCall(t, s, "GET", z+"instanceGroupManagers/g", nil); workloadPolicyOf(got) != "projects/p/"+r+"resourcePolicies/ha" {
		t.Fatalf("after the patch: %v", got["resourcePolicies"])
	}
	mustCall(t, s, "DELETE", r+"resourcePolicies/ht", nil)

	// A group with VMs can't take or change one.
	mustCall(t, s, "POST", z+"instanceGroupManagers", mig("vms", "web", 1, ""))
	wantStatus(t, s, 400, "Workload policy update is not allowed when the MIG has instances.", "PATCH", z+"instanceGroupManagers/vms",
		obj{"resourcePolicies": obj{"workloadPolicy": r + "resourcePolicies/ha"}})

	// A regional group: only the ANY and ANY_SINGLE_ZONE shapes.
	rg := func(name, shape string) obj {
		b := mig(name, "web", 0, r+"resourcePolicies/ha")
		b["distributionPolicy"] = obj{"targetShape": shape}
		return b
	}
	wantStatus(t, s, 400, "ANY or ANY_SINGLE_ZONE", "POST", r+"instanceGroupManagers", rg("rg", "EVEN"))
	mustCall(t, s, "POST", r+"instanceGroupManagers", rg("rg", "ANY_SINGLE_ZONE"))
}

// TestInstanceTemplateByID (#2206): a template is read and deleted by its
// numeric ID too, as Terraform's provider reads it.
func TestInstanceTemplateByID(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	mustCall(t, s, "POST", "global/instanceTemplates", obj{"name": "web", "properties": obj{"machineType": "e2-small", "disks": []any{map[string]any{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}}})
	id := str(mustCall(t, s, "GET", "global/instanceTemplates/web", nil), "id")
	if got := mustCall(t, s, "GET", "global/instanceTemplates/"+id, nil); str(got, "name") != "web" {
		t.Fatalf("by ID: %v", got)
	}
	wantStatus(t, s, 404, "", "GET", "global/instanceTemplates/12345", nil)
	mustCall(t, s, "DELETE", "global/instanceTemplates/"+id, nil)
	wantStatus(t, s, 404, "", "GET", "global/instanceTemplates/web", nil)
}
