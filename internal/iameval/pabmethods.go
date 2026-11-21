package iameval

import "strings"

// PrincipalAccessBoundary is IAM v3's table (#2057): principal access
// boundary policies need iam.principalaccessboundarypolicies.* on the
// policy (its organization's ancestry applies), or on the organization for
// a list or a create; a policy binding needs
// resourcemanager.{projects,folders,organizations}.{create,update,delete,search}PolicyBinding
// on its parent, with iam.principalaccessboundarypolicies.bind on the
// policy it binds, and iam.policybindings.get and .list to read
// (Google's roles: Principal Access Boundary Admin and User, Project, Folder
// and Organization IAM Admin, in the IAM roles index read 2026-11-20). A
// workforce pool's bindings are checked on the organization (UNVERIFIED:
// Google lists iam.googleapis.com/workforcePools.createPolicyBinding too,
// re-read 2026-11-20).
var PrincipalAccessBoundary = &Service{
	Name: "iam.googleapis.com",
	Types: map[string]string{
		"principalAccessBoundaryPolicies": "iam.googleapis.com/PrincipalAccessBoundaryPolicy",
		"policyBindings":                  "iam.googleapis.com/PolicyBinding",
	},
	Deny: iamDeny,
}

// policyBindingPermission is verb's PolicyBinding permission on a binding
// or its parent's hierarchy resource.
func policyBindingPermission(verb string) func(string) string {
	return func(resource string) string {
		kind, _, _ := strings.Cut(resource, "/")
		switch kind {
		case "folders", "organizations":
		default:
			kind = "projects"
		}
		return "resourcemanager." + kind + "." + verb
	}
}

func init() {
	const pab = "iam.principalaccessboundarypolicies."
	m := methodRows("/google.iam.v3.PrincipalAccessBoundaryPolicies/", map[string]Method{
		"CreatePrincipalAccessBoundaryPolicy":         {Permission: pab + "create", Field: "parent"},
		"GetPrincipalAccessBoundaryPolicy":            {Permission: pab + "get", Field: "name"},
		"UpdatePrincipalAccessBoundaryPolicy":         {Permission: pab + "update", Field: "principal_access_boundary_policy.name"},
		"DeletePrincipalAccessBoundaryPolicy":         {Permission: pab + "delete", Field: "name"},
		"ListPrincipalAccessBoundaryPolicies":         {Permission: pab + "list", Field: "parent"},
		"SearchPrincipalAccessBoundaryPolicyBindings": {Permission: pab + "searchPolicyBindings", Field: "name"},
	})
	for k, v := range methodRows("/google.iam.v3.PolicyBindings/", map[string]Method{
		"CreatePolicyBinding": {PermissionFor: policyBindingPermission("createPolicyBinding"), Field: "parent",
			Also: []Also{{Field: "policy_binding.policy", Permission: pab + "bind"}}},
		"GetPolicyBinding":           {Permission: "iam.policybindings.get", Field: "name"},
		"UpdatePolicyBinding":        {PermissionFor: policyBindingPermission("updatePolicyBinding"), Field: "policy_binding.name"},
		"DeletePolicyBinding":        {PermissionFor: policyBindingPermission("deletePolicyBinding"), Field: "name"},
		"ListPolicyBindings":         {Permission: "iam.policybindings.list", Field: "parent"},
		"SearchTargetPolicyBindings": {PermissionFor: policyBindingPermission("searchPolicyBindings"), Field: "parent"},
	}) {
		m[k] = v
	}
	PrincipalAccessBoundary.Methods = m
	Services["google.iam.v3.PrincipalAccessBoundaryPolicies"] = PrincipalAccessBoundary
	Services["google.iam.v3.PolicyBindings"] = PrincipalAccessBoundary
}
