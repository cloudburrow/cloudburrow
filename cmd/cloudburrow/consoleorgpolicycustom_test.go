package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// TestConsoleOrgPolicyCustomConstraints (#1891): the Organization
// Policies page's custom constraints. Create custom constraint, read back
// by the list and through the Org Policy v2 registry; Edit; a condition
// that is not CEL and a taken ID refused; Delete.
func TestConsoleOrgPolicyCustomConstraints(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamOrgPoliciesProvider{registry: reg}
	set := map[string]any{"name": "versioned", "create": true, "displayName": "Versioned", "description": "Buckets must be versioned.",
		"resourceType": "storage.googleapis.com/Bucket", "methodTypes": []string{"CREATE", "UPDATE"},
		"condition": "resource.versioning.enabled == true", "actionType": "ALLOW"}
	if _, err := orgPolicyEditCall(t, p, "custom-set", set); err != nil {
		t.Fatalf("custom-set: %v", err)
	}
	if _, err := orgPolicyEditCall(t, p, "custom-set", set); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("a taken ID = %v", err)
	}
	out, err := orgPolicyEditCall(t, p, "custom-list", nil)
	if err != nil {
		t.Fatal(err)
	}
	list := out.(map[string]any)["constraints"].([]orgPolicyCustomView)
	if len(list) != 1 || list[0].ID != "custom.versioned" || list[0].ResourceType != "storage.googleapis.com/Bucket" ||
		strings.Join(list[0].MethodTypes, ",") != "CREATE,UPDATE" || list[0].ActionType != "ALLOW" {
		t.Fatalf("custom-list = %+v", list)
	}
	all, err := reg.CustomConstraints(resourcemanager.OrganizationName)
	if err != nil || len(all) != 1 || all[0].GetName() != resourcemanager.OrganizationName+"/customConstraints/custom.versioned" {
		t.Fatalf("the registry holds %v, %v", all, err)
	}

	set["create"], set["name"], set["actionType"], set["condition"] = false, "custom.versioned", "DENY", "resource.versioning.enabled == false"
	set["methodTypes"] = []string{"CREATE"}
	if _, err := orgPolicyEditCall(t, p, "custom-set", set); err != nil {
		t.Fatalf("custom-set (edit): %v", err)
	}
	got, err := orgPolicyEditCall(t, p, "custom-get", map[string]string{"name": "custom.versioned"})
	if err != nil {
		t.Fatal(err)
	}
	if v := got.(orgPolicyCustomView); v.ActionType != "DENY" || v.Condition != "resource.versioning.enabled == false" || len(v.MethodTypes) != 1 {
		t.Errorf("after the edit custom-get = %+v", v)
	}
	set["condition"] = "resource.name =="
	if _, err := orgPolicyEditCall(t, p, "custom-set", set); err == nil || !strings.Contains(err.Error(), "Custom constraint condition") {
		t.Errorf("a malformed condition = %v", err)
	}

	if _, err := orgPolicyEditCall(t, p, "custom-delete", map[string]string{"name": "custom.versioned"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := reg.CustomConstraints(resourcemanager.OrganizationName); len(all) != 0 {
		t.Errorf("after custom-delete the registry holds %v", all)
	}
}
