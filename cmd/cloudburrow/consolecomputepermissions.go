package main

// The Permissions panel of Compute Engine's VM instances, disks, snapshots
// and subnets (#2125), over the allow policies they keep since #2112.
//
// Google's console reaches them from the lists: "Select the checkbox next to
// each resource you want to update. For a VM instance, click Permissions. For
// other resources, make sure the info panel is visible. If it isn't, click
// Show info panel, then select the Permissions tab. Click Add principal"
// (https://docs.cloud.google.com/compute/docs/access/granting-access-to-resources,
// read 2026-11-21). The VM instances, Disks and Snapshots lists do that here,
// and each resource's page, a subnet's among them, has a Permissions tab.
//
// Every read and write is the resource's own getIamPolicy, setIamPolicy and
// testIamPermissions on the in-process Compute Engine API, the JSON calls
// the Go client and gcloud make, with the etag the page read, so a stale one
// is refused with the API's own message.

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const computeIAMDoc = compatibilityDoc + "compute-engine-vpc-networking--computegoogleapiscom-v1"

func computePolicyTarget(example string) *console.PolicyTarget {
	return &console.PolicyTarget{Link: computeIAMDoc, RoleHelp: roleHelp("Compute Engine", example), Conditions: true}
}

// computePermissionsTab is a list row's info panel Permissions tab.
func computePermissionsTab() []console.InfoTab {
	return []console.InfoTab{{ID: "permissions", Label: "Permissions", Permissions: true}}
}

// getComputePolicy is getIamPolicy on base (after projects/P/), at version
// 3 so a conditional binding is read with its condition.
func (c computeScreen) getComputePolicy(project, base string) (console.Policy, error) {
	m, err := c.call("GET", project, base+"/getIamPolicy", url.Values{"optionsRequestedPolicyVersion": {"3"}}, nil)
	if err != nil {
		return console.Policy{}, err
	}
	out := console.Policy{Etag: vpcStr(m, "etag"), Bindings: []console.Binding{}}
	for _, b := range vmMaps(m, "bindings") {
		cb := console.Binding{Role: vpcStr(b, "role"), Members: vpcStrs(b, "members")}
		if cond := vmMap(b, "condition"); len(cond) > 0 {
			cb.Conditional = &console.Condition{Title: vpcStr(cond, "title"), Description: vpcStr(cond, "description"),
				Expression: vpcStr(cond, "expression")}
			cb.Condition = cb.Conditional.Expression
			if cb.Conditional.Title != "" {
				cb.Condition = cb.Conditional.Title + ": " + cb.Condition
			}
		}
		out.Bindings = append(out.Bindings, cb)
	}
	return out, nil
}

// setComputePolicy is setIamPolicy on base with p and the etag it was read
// with; a conditional binding makes it a version 3 policy.
func (c computeScreen) setComputePolicy(project, base string, p console.Policy) error {
	if p.Etag == "" {
		return errors.New("etag is required: send the one the policy was read with")
	}
	version := 1
	bindings := []any{}
	for _, b := range p.Bindings {
		pb := map[string]any{"role": b.Role, "members": append([]string(nil), b.Members...)}
		switch {
		case b.Conditional != nil:
			pb["condition"] = map[string]any{"title": b.Conditional.Title, "description": b.Conditional.Description,
				"expression": b.Conditional.Expression}
			version = 3
		case b.Condition != "":
			return fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		bindings = append(bindings, pb)
	}
	_, err := c.call("POST", project, base+"/setIamPolicy", nil, map[string]any{
		"policy": map[string]any{"version": version, "etag": p.Etag, "bindings": bindings}})
	return err
}

// testComputePermissions is testIamPermissions on base.
func (c computeScreen) testComputePermissions(project, base string, perms []string) ([]string, error) {
	m, err := c.call("POST", project, base+"/testIamPermissions", nil, map[string]any{"permissions": perms})
	if err != nil {
		return nil, err
	}
	return vpcStrs(m, "permissions"), nil
}

// computeIAMBase is the resource an IAM call names for a page path, "" when
// the page has no policy.
type computeIAMBase func(path []string) string

func zonalIAMBase(coll string) computeIAMBase {
	return func(path []string) string {
		if len(path) != 2 || path[0] == "" || path[1] == "" {
			return ""
		}
		return "zones/" + path[0] + "/" + coll + "/" + path[1]
	}
}

func snapshotIAMBase(path []string) string {
	if len(path) != 1 || path[0] == "" {
		return ""
	}
	return "global/snapshots/" + path[0]
}

// subnetIAMBase is a subnet's page, network/subnets/region/name.
func subnetIAMBase(path []string) string {
	if len(path) != 4 || path[1] != "subnets" {
		return ""
	}
	return "regions/" + path[2] + "/subnetworks/" + path[3]
}

func (c computeScreen) iamBase(base computeIAMBase, path []string) (string, error) {
	if c.api() == nil {
		return "", errors.New(computeNotStarted)
	}
	b := base(path)
	if b == "" {
		return "", errors.New("this resource has no IAM policy")
	}
	return b, nil
}

func (c computeScreen) policyGet(base computeIAMBase, project string, path []string) (console.Policy, error) {
	b, err := c.iamBase(base, path)
	if err != nil {
		return console.Policy{}, err
	}
	return c.getComputePolicy(project, b)
}

func (c computeScreen) policySet(base computeIAMBase, project string, path []string, p console.Policy) error {
	b, err := c.iamBase(base, path)
	if err != nil {
		return err
	}
	return c.setComputePolicy(project, b, p)
}

func (c computeScreen) policyTest(base computeIAMBase, project string, path []string, perms []string) ([]string, error) {
	b, err := c.iamBase(base, path)
	if err != nil {
		return nil, err
	}
	return c.testComputePermissions(project, b, perms)
}

// VM instances.

var instanceIAM = zonalIAMBase("instances")

func (p computeInstancesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if instanceIAM(path) == "" {
		return nil
	}
	return computePolicyTarget("roles/compute.instanceAdmin.v1")
}

func (p computeInstancesProvider) GetPolicy(_ context.Context, project string, path []string) (console.Policy, error) {
	return p.policyGet(instanceIAM, project, path)
}

func (p computeInstancesProvider) SetPolicy(_ context.Context, project string, path []string, pol console.Policy) error {
	return p.policySet(instanceIAM, project, path, pol)
}

func (p computeInstancesProvider) TestPermissions(_ context.Context, project string, path []string, perms []string) ([]string, error) {
	return p.policyTest(instanceIAM, project, path, perms)
}

// Disks.

// diskIAM is a disk's, at [zone, name], or a regional disk's, at
// [region, name] (#2206).
func diskIAM(path []string) string {
	if len(path) != 2 || path[0] == "" || path[1] == "" {
		return ""
	}
	return diskBase(path)
}

func (p computeDisksProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if diskIAM(path) == "" {
		return nil
	}
	return computePolicyTarget("roles/compute.storageAdmin")
}

func (p computeDisksProvider) GetPolicy(_ context.Context, project string, path []string) (console.Policy, error) {
	return p.policyGet(diskIAM, project, path)
}

func (p computeDisksProvider) SetPolicy(_ context.Context, project string, path []string, pol console.Policy) error {
	return p.policySet(diskIAM, project, path, pol)
}

func (p computeDisksProvider) TestPermissions(_ context.Context, project string, path []string, perms []string) ([]string, error) {
	return p.policyTest(diskIAM, project, path, perms)
}

// Snapshots.

func (p computeSnapshotsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if snapshotIAMBase(path) == "" {
		return nil
	}
	return computePolicyTarget("roles/compute.storageAdmin")
}

func (p computeSnapshotsProvider) GetPolicy(_ context.Context, project string, path []string) (console.Policy, error) {
	return p.policyGet(snapshotIAMBase, project, path)
}

func (p computeSnapshotsProvider) SetPolicy(_ context.Context, project string, path []string, pol console.Policy) error {
	return p.policySet(snapshotIAMBase, project, path, pol)
}

func (p computeSnapshotsProvider) TestPermissions(_ context.Context, project string, path []string, perms []string) ([]string, error) {
	return p.policyTest(snapshotIAMBase, project, path, perms)
}

// Subnets: a subnet's page under its VPC network. A network keeps no
// policy of its own.

func (p computeNetworksProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if subnetIAMBase(path) == "" {
		return nil
	}
	return computePolicyTarget("roles/compute.networkUser")
}

func (p computeNetworksProvider) GetPolicy(_ context.Context, project string, path []string) (console.Policy, error) {
	return p.policyGet(subnetIAMBase, project, path)
}

func (p computeNetworksProvider) SetPolicy(_ context.Context, project string, path []string, pol console.Policy) error {
	return p.policySet(subnetIAMBase, project, path, pol)
}

func (p computeNetworksProvider) TestPermissions(_ context.Context, project string, path []string, perms []string) ([]string, error) {
	return p.policyTest(subnetIAMBase, project, path, perms)
}

var (
	_ console.PolicyEditor = computeInstancesProvider{}
	_ console.PolicyEditor = computeDisksProvider{}
	_ console.PolicyEditor = computeSnapshotsProvider{}
	_ console.PolicyEditor = computeNetworksProvider{}
)
