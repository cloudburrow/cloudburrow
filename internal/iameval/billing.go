package iameval

// Billing is the Cloud Billing API v1's and the Budget API v1's table
// (#2109). Each method's permission is the one its REST reference names
// (https://docs.cloud.google.com/billing/docs/reference/rest/v1 and
// https://docs.cloud.google.com/billing/docs/reference/budget/rest/v1,
// read 2026-11-21): billingAccounts.create "the billing.accounts.update
// IAM permission on the parent account", billingAccounts.projects.list
// billing.resourceAssociations.list, projects.getBillingInfo
// resourcemanager.projects.get on the project, budgets
// billing.budgets.VERB on the account. Where a page names a role and no
// permission ("a viewer of the billing account"), it is the permission of
// the method's name in "Cloud Billing access control"
// (https://docs.cloud.google.com/billing/docs/how-to/billing-access):
// billing.accounts.get, .update, .move, .getIamPolicy and .setIamPolicy.
// billingAccounts.list answers the accounts the caller may get ("the
// billing accounts that the current authenticated user has permission to
// view"). projects.updateBillingInfo is checked by the CLI
// (cmd/cloudburrow/billingiam.go): linking needs
// resourcemanager.projects.createBillingAssignment on the project and
// billing.resourceAssociations.create on the account; unlinking either
// resourcemanager.projects.deleteBillingAssignment on the project or
// billing.resourceAssociations.delete on its account. The Cloud Catalog
// (services, skus) needs no permission. A billing account keeps its own
// policy, which applies through the Source's HierarchyAncestry with its
// parent account's (a subaccount's) and its organization's above it.
// Google's wording of a denial is UNVERIFIED (the reference pages give
// none): "The caller does not have permission", its generic
// PERMISSION_DENIED message (docs re-read 2026-11-21, #2109:
// https://docs.cloud.google.com/billing/docs/reference/rest/v1).
var Billing = &Service{
	Name:    "cloudbilling.googleapis.com",
	Types:   map[string]string{"billingAccounts": "cloudbilling.googleapis.com/BillingAccount"},
	Methods: billingMethods(),
	Deny: func(permission, resource string) error {
		return grpcDenied("The caller does not have permission", permission, resource, true)
	},
}

func billingMethods() map[string]Method {
	out := methodRows("/google.cloud.billing.v1.CloudBilling/", map[string]Method{
		"GetBillingAccount":      {Permission: "billing.accounts.get", Field: "name"},
		"ListBillingAccounts":    {Permission: "billing.accounts.get", Filter: "billing_accounts"},
		"UpdateBillingAccount":   {Permission: "billing.accounts.update", Field: "name"},
		"CreateBillingAccount":   {Permission: "billing.accounts.update", Field: "parent"},
		"MoveBillingAccount":     {Permission: "billing.accounts.move", Field: "name", Also: []Also{{Field: "destination_parent", Permission: "billing.accounts.move"}}},
		"ListProjectBillingInfo": {Permission: "billing.resourceAssociations.list", Field: "name"},
		"GetProjectBillingInfo":  {Permission: "resourcemanager.projects.get", Field: "name"},
		"GetIamPolicy":           {Permission: "billing.accounts.getIamPolicy", Field: "resource"},
		"SetIamPolicy":           {Permission: "billing.accounts.setIamPolicy", Field: "resource"},
	})
	for k, v := range methodRows("/google.cloud.billing.budgets.v1.BudgetService/", map[string]Method{
		"CreateBudget": {Permission: "billing.budgets.create", Field: "parent"},
		"GetBudget":    {Permission: "billing.budgets.get", Field: "name"},
		"ListBudgets":  {Permission: "billing.budgets.list", Field: "parent"},
		"UpdateBudget": {Permission: "billing.budgets.update", Field: "budget.name"},
		"DeleteBudget": {Permission: "billing.budgets.delete", Field: "name"},
	}) {
		out[k] = v
	}
	return out
}

func init() {
	Services["google.cloud.billing.v1.CloudBilling"] = Billing
	Services["google.cloud.billing.budgets.v1.BudgetService"] = Billing
}
