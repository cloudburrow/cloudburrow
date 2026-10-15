package main

// BigQuery row access policies and permissions on a table's and a routine's
// page (#1218).
//
// A table's page has a Row access policies tab: its policies from
// rowAccessPolicies.list, each with Delete policy (rowAccessPolicies.delete);
// Create row access policy on the page (rowAccessPolicies.insert, or
// rowAccessPolicies.update with Replace an existing policy), and Delete all
// row access policies (rowAccessPolicies.batchDelete of every policy listed).
// The tab says whom CloudBurrow takes the caller to be, since that decides
// which rows a query returns (docs/compatibility.md, Row-level security).
//
// A table's (a view's too) and a routine's page have the console's
// Permissions tab (#793, #1231): tables.getIamPolicy and setIamPolicy, or
// routines.getIamPolicy and setIamPolicy, which the front stores and never
// enforces (ADR-0006, as amended by #1231).
//
// A row access policy has no setIamPolicy: its IAM policy is its grantees,
// holding roles/bigquery.filteredDataViewer, and they are not unenforced:
// they decide which rows a query returns. So its Permissions are not the
// PolicyEditor tab, whose note says a binding grants nothing; its row's
// Grantees column and Create row access policy with Replace show and change
// them.
//
// All of it goes through the official generated client
// (google.golang.org/api/bigquery/v2), since cloud.google.com/go/bigquery
// has no row access policy calls.

import (
	"context"
	"fmt"
	"strings"

	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	actCreateRowPolicy      = "createrowpolicy"
	actDeleteRowPolicy      = "deleterowpolicy"
	actDeleteAllRowPolicies = "droprowpolicies"
	// bigqueryRowPoliciesSegment is the third segment of a policy row's
	// path, [dataset, table, "rowAccessPolicies", policy].
	bigqueryRowPoliciesSegment = "rowAccessPolicies"
)

// service is the generated client against the forwarded REST port.
func (p bigqueryProvider) service(ctx context.Context) (*bqv2.Service, error) {
	svc, err := bqv2.NewService(ctx, option.WithEndpoint("http://"+p.endpoint), option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	return svc, nil
}

// bigqueryCaller is whom CloudBurrow takes every caller to be (#1218).
func bigqueryCaller(project string) string {
	return "cloudburrow-local@" + project + ".iam.gserviceaccount.com"
}

// rowPoliciesSection lists a table's row access policies.
func (p bigqueryProvider) rowPoliciesSection(ctx context.Context, datasetID, tableID string) console.Section {
	sec := console.Section{ID: "rowaccesspolicies", Label: "Row access policies"}
	out := console.Listing{Columns: []string{"Filter", "Grantees"}, NameColumn: "Policy", Noun: "row access policies"}
	svc, err := p.service(ctx)
	if err != nil {
		out.Unavailable = err.Error()
		sec.Listing = out
		return sec
	}
	pols, err := p.rowPolicies(ctx, svc, datasetID, tableID)
	if err != nil {
		out.Unavailable = "listing row access policies: " + bigqueryRefusal(err).Error()
		sec.Listing = out
		return sec
	}
	for _, pol := range pols {
		id := pol.RowAccessPolicyReference.PolicyId
		out.Items = append(out.Items, console.Resource{
			Name:    id,
			Fields:  map[string]string{"Filter": orDash(pol.FilterPredicate), "Grantees": orDash(strings.Join(pol.Grantees, ", "))},
			Actions: []console.Action{deleteRowPolicyAction()},
			ActsOn:  []string{datasetID, tableID, bigqueryRowPoliciesSegment, id},
		})
	}
	out.Total = len(out.Items)
	caller := "CloudBurrow authenticates nobody, so it takes every caller to be " + bigqueryCaller(p.project) +
		": a query of this table returns the rows of the policies granted to serviceAccount:" + bigqueryCaller(p.project) +
		", allAuthenticatedUsers or allUsers, and none when no policy grants them. Other grantees are kept but match nobody here."
	if out.Total == 0 {
		out.Note = "This table has no row access policies: every query reads all of its rows. " + caller
	} else {
		out.Note = caller + " tabledata.list (Preview), exports, copies and Storage Read of a table with policies are refused."
	}
	sec.Listing = out
	return sec
}

// rowPolicies reads every policy of a table, page by page.
func (p bigqueryProvider) rowPolicies(ctx context.Context, svc *bqv2.Service, datasetID, tableID string) ([]*bqv2.RowAccessPolicy, error) {
	var all []*bqv2.RowAccessPolicy
	err := svc.RowAccessPolicies.List(p.project, datasetID, tableID).Pages(ctx, func(r *bqv2.ListRowAccessPoliciesResponse) error {
		all = append(all, r.RowAccessPolicies...)
		return nil
	})
	return all, err
}

// PolicyOn gives a table's page and a routine's a Permissions tab.
func (bigqueryProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 2 && !isRoutinePath(path) {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "bigquery-is-a-community-emulator",
		RoleHelp: roleHelp("BigQuery", "roles/bigquery.dataViewer")}
}

// iamResource is the IAM resource name of a table's or a routine's page.
func (p bigqueryProvider) iamResource(project string, path []string) (string, bool, error) {
	if project == "" {
		project = p.project
	}
	if project != p.project {
		return "", false, fmt.Errorf("BigQuery serves the project %s only", p.project)
	}
	if isRoutinePath(path) {
		return "projects/" + project + "/datasets/" + path[0] + "/routines/" + path[2], true, nil
	}
	if len(path) != 2 {
		return "", false, fmt.Errorf("only a table or a routine has an IAM policy")
	}
	return "projects/" + project + "/datasets/" + path[0] + "/tables/" + path[1], false, nil
}

// GetPolicy is tables.getIamPolicy or routines.getIamPolicy.
func (p bigqueryProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	resource, routine, err := p.iamResource(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	svc, err := p.service(ctx)
	if err != nil {
		return console.Policy{}, err
	}
	var pol *bqv2.Policy
	if routine {
		pol, err = svc.Routines.GetIamPolicy(resource, &bqv2.GetIamPolicyRequest{}).Context(ctx).Do()
	} else {
		pol, err = svc.Tables.GetIamPolicy(resource, &bqv2.GetIamPolicyRequest{}).Context(ctx).Do()
	}
	if err != nil {
		return console.Policy{}, bigqueryRefusal(err)
	}
	out := console.Policy{Etag: pol.Etag, Bindings: []console.Binding{}}
	for _, b := range pol.Bindings {
		cond := ""
		if b.Condition != nil {
			cond = b.Condition.Expression
		}
		out.Bindings = append(out.Bindings, console.Binding{Role: b.Role, Members: append([]string(nil), b.Members...), Condition: cond})
	}
	return out, nil
}

// SetPolicy is tables.setIamPolicy or routines.setIamPolicy with the etag
// the page read, so a policy changed meanwhile is refused.
func (p bigqueryProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	resource, routine, err := p.iamResource(project, path)
	if err != nil {
		return err
	}
	out := &bqv2.Policy{Version: 1, Etag: pol.Etag}
	for _, b := range pol.Bindings {
		if b.Condition != "" {
			return fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		out.Bindings = append(out.Bindings, &bqv2.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)})
	}
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	req := &bqv2.SetIamPolicyRequest{Policy: out}
	if routine {
		_, err = svc.Routines.SetIamPolicy(resource, req).Context(ctx).Do()
	} else {
		_, err = svc.Tables.SetIamPolicy(resource, req).Context(ctx).Do()
	}
	// The API's error as it is, as Cloud Storage's tab gives it, so a
	// stale etag reads as the official client reports it.
	return err
}

var _ console.PolicyEditor = bigqueryProvider{}

// rowPolicyFields are Create row access policy's inputs.
func rowPolicyFields(project string) []console.Field {
	return []console.Field{
		{Name: "policyId", Label: "Policy ID", Type: "text", Required: true, Pattern: "[A-Za-z0-9_]{1,256}",
			Help: "Letters, digits and underscores, at most 256."},
		{Name: "filter", Label: "Filter", Type: "textarea", Required: true,
			Help: "A GoogleSQL condition on the table's columns, such as region = 'eu'. A row is returned to a grantee when it holds."},
		{Name: "grantees", Label: "Grantees", Type: "textarea",
			Help: "One per line: user:ada@example.com, group:, serviceAccount:, domain:, allAuthenticatedUsers or allUsers. " +
				"CloudBurrow takes every caller to be serviceAccount:" + bigqueryCaller(project) + ", so only that " +
				"account, allAuthenticatedUsers and allUsers match here."},
		{Name: "replace", Label: "Replace an existing policy", Type: "checkbox",
			Help: "Change the policy of this ID (rowAccessPolicies.update) rather than create one, which is refused when it exists."},
	}
}

// createRowPolicy is Create row access policy.
func (p bigqueryProvider) createRowPolicy(ctx context.Context, datasetID, tableID string, values map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(values["policyId"])
	pol := &bqv2.RowAccessPolicy{
		RowAccessPolicyReference: &bqv2.RowAccessPolicyReference{ProjectId: p.project, DatasetId: datasetID, TableId: tableID, PolicyId: id},
		FilterPredicate:          strings.TrimSpace(values["filter"]),
	}
	for _, line := range strings.FieldsFunc(values["grantees"], func(r rune) bool { return r == '\n' || r == ',' }) {
		if g := strings.TrimSpace(line); g != "" {
			pol.Grantees = append(pol.Grantees, g)
		}
	}
	if checked(values, "replace") {
		_, err = svc.RowAccessPolicies.Update(p.project, datasetID, tableID, id, pol).Context(ctx).Do()
	} else {
		_, err = svc.RowAccessPolicies.Insert(p.project, datasetID, tableID, pol).Context(ctx).Do()
	}
	return bigqueryRefusal(err)
}

// deleteRowPolicy is Delete policy on a policy's row.
func (p bigqueryProvider) deleteRowPolicy(ctx context.Context, datasetID, tableID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	return bigqueryRefusal(svc.RowAccessPolicies.Delete(p.project, datasetID, tableID, id).Context(ctx).Do())
}

// deleteAllRowPolicies is Delete all row access policies: a batchDelete
// of every policy the table has.
func (p bigqueryProvider) deleteAllRowPolicies(ctx context.Context, datasetID, tableID string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	pols, err := p.rowPolicies(ctx, svc, datasetID, tableID)
	if err != nil {
		return bigqueryRefusal(err)
	}
	if len(pols) == 0 {
		return fmt.Errorf("%s.%s has no row access policies", datasetID, tableID)
	}
	ids := make([]string, len(pols))
	for i, pol := range pols {
		ids[i] = pol.RowAccessPolicyReference.PolicyId
	}
	return bigqueryRefusal(svc.RowAccessPolicies.BatchDelete(p.project, datasetID, tableID,
		&bqv2.BatchDeleteRowAccessPoliciesRequest{PolicyIds: ids}).Context(ctx).Do())
}

func deleteRowPolicyAction() console.Action {
	return console.Action{ID: actDeleteRowPolicy, Label: "Delete policy", Destructive: true}
}

// isRowPolicyPath reports whether path is a policy's row.
func isRowPolicyPath(path []string) bool {
	return len(path) == 4 && path[2] == bigqueryRowPoliciesSegment
}
