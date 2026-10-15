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
// A table's and a routine's page have a Permissions tab: the IAM policy
// methods of tables and routines are refused 501 (ADR-0006 stores policies
// on four other services only), so the tab shows the API's refusal in its
// own words, rather than a Grant access form that could not be carried out.
// It is not the console's PolicyEditor tab, whose note says the policy is
// stored.
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

// permissionsRefusal is a Permissions tab: the API's answer to
// getIamPolicy of the resource, which is its 501 (above). Should a policy
// ever be served, the tab says so rather than draw it unreviewed.
func permissionsRefusal(err error) console.Section {
	sec := console.Section{ID: "permissions", Label: "Permissions"}
	if err != nil {
		sec.Listing.Unavailable = bigqueryRefusal(err).Error()
	} else {
		sec.Listing.Unavailable = "BigQuery answered getIamPolicy, which this console does not draw yet."
	}
	return sec
}

// tablePermissions is a table's Permissions tab.
func (p bigqueryProvider) tablePermissions(ctx context.Context, datasetID, tableID string) console.Section {
	svc, err := p.service(ctx)
	if err != nil {
		return permissionsRefusal(err)
	}
	_, err = svc.Tables.GetIamPolicy("projects/"+p.project+"/datasets/"+datasetID+"/tables/"+tableID, &bqv2.GetIamPolicyRequest{}).Context(ctx).Do()
	return permissionsRefusal(err)
}

// routinePermissions is a routine's Permissions tab.
func (p bigqueryProvider) routinePermissions(ctx context.Context, datasetID, routineID string) console.Section {
	svc, err := p.service(ctx)
	if err != nil {
		return permissionsRefusal(err)
	}
	_, err = svc.Routines.GetIamPolicy("projects/"+p.project+"/datasets/"+datasetID+"/routines/"+routineID, &bqv2.GetIamPolicyRequest{}).Context(ctx).Do()
	return permissionsRefusal(err)
}

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
