//go:build compat

package compat

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestConsoleBigQueryRowAccessPolicies (#1218): a table's page lists its
// row access policies; Create row access policy makes one
// (rowAccessPolicies.insert) and, with Replace an existing policy, changes
// it (rowAccessPolicies.update); Delete policy on a row deletes it
// (rowAccessPolicies.delete), and Delete all row access policies deletes
// every one (rowAccessPolicies.batchDelete), each read back through the
// generated client. A table's and a routine's Permissions tab show the
// API's refusal, which names ADR-0006.
func TestConsoleBigQueryRowAccessPolicies(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds, tbl := seedOrders(t, h, c)
	d, tb := ds.DatasetID, tbl.TableID
	path := []string{d, tb}
	page := func(name ...string) consoleDetail {
		var p consoleDetail
		consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": name}.Encode(), "", &p)
		return p
	}
	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		if code, out := consoleAct(t, addr, "bigquery", project, path, action, values); code != http.StatusOK {
			t.Fatalf("console %s = %d: %s", action, code, out)
		}
	}

	act(path, "createrowpolicy", map[string]string{"policyId": "eu", "filter": "region = 'eu'", "grantees": "allUsers\nuser:ada@example.com"})
	got, err := svc.RowAccessPolicies.Get(project, d, tb, "eu").Context(ctx).Do()
	if err != nil || got.FilterPredicate != "region = 'eu'" || strings.Join(got.Grantees, ",") != "allUsers,user:ada@example.com" {
		t.Fatalf("rowAccessPolicies.get after Create = %+v, %v", got, err)
	}
	if code, out := consoleAct(t, addr, "bigquery", project, path, "createrowpolicy", map[string]string{"policyId": "eu", "filter": "TRUE"}); code == http.StatusOK || !strings.Contains(out, "Already Exists") {
		t.Errorf("Create of an existing policy = %d: %s, want the API's refusal", code, out)
	}
	act(path, "createrowpolicy", map[string]string{"policyId": "eu", "filter": "region = 'us'", "grantees": "allUsers", "replace": "true"})
	if got, err := svc.RowAccessPolicies.Get(project, d, tb, "eu").Context(ctx).Do(); err != nil || got.FilterPredicate != "region = 'us'" {
		t.Errorf("rowAccessPolicies.get after Replace = %+v, %v", got, err)
	}

	sec := page(path...).section(t, "rowaccesspolicies")
	if sec.Unavailable != "" || len(sec.Items) != 1 || sec.Items[0].Name != "eu" || sec.Items[0].Fields["Filter"] != "region = 'us'" {
		t.Errorf("the Row access policies tab = %+v", sec)
	}
	if perm := page(path...).section(t, "permissions"); !strings.Contains(perm.Unavailable, "bigquery.tables.getIamPolicy") || !strings.Contains(perm.Unavailable, "ADR-0006") {
		t.Errorf("the table's Permissions tab = %+v, want the API's 501 naming ADR-0006", perm)
	}

	act([]string{d, tb, "rowAccessPolicies", "eu"}, "deleterowpolicy", nil)
	if list, err := svc.RowAccessPolicies.List(project, d, tb).Context(ctx).Do(); err != nil || len(list.RowAccessPolicies) != 0 {
		t.Errorf("after Delete policy, list = %+v, %v", list, err)
	}
	act(path, "createrowpolicy", map[string]string{"policyId": "a", "filter": "TRUE", "grantees": "allUsers"})
	act(path, "createrowpolicy", map[string]string{"policyId": "b", "filter": "FALSE"})
	act(path, "droprowpolicies", nil)
	if list, err := svc.RowAccessPolicies.List(project, d, tb).Context(ctx).Do(); err != nil || len(list.RowAccessPolicies) != 0 {
		t.Errorf("after Delete all row access policies, list = %+v, %v", list, err)
	}

	if _, err := c.Query("CREATE FUNCTION `" + d + ".plus_one`(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	if perm := page(d, "routines", "plus_one").section(t, "permissions"); !strings.Contains(perm.Unavailable, "bigquery.routines.getIamPolicy") {
		t.Errorf("the routine's Permissions tab = %+v, want the API's 501", perm)
	}
}
