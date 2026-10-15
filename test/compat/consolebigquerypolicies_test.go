//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	bq "google.golang.org/api/bigquery/v2"
)

// TestConsoleBigQueryRowAccessPolicies (#1218): a table's page lists its
// row access policies; Create row access policy makes one
// (rowAccessPolicies.insert) and, with Replace an existing policy, changes
// it (rowAccessPolicies.update); Delete policy on a row deletes it
// (rowAccessPolicies.delete), and Delete all row access policies deletes
// every one (rowAccessPolicies.batchDelete), each read back through the
// generated client. A table's and a routine's Permissions tab are the
// console's (#1231): Grant access and Remove principal through
// tables.setIamPolicy and routines.setIamPolicy, read back through the
// official clients, with the note that nothing is enforced.
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
	consolePermissionsRoundTripIn(t, h, addr, "bigquery", project, path, handleOf(tbl.IAM()), "roles/bigquery.dataViewer", "bigquery.tables.getData")

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
	consolePermissionsRoundTripIn(t, h, addr, "bigquery", project, []string{d, "routines", "plus_one"},
		routineHandle(svc, "projects/"+project+"/datasets/"+d+"/routines/plus_one"), "roles/bigquery.dataViewer", "bigquery.routines.get")
}

// routineHandle is a routine's IAM through the official generated client,
// routines.getIamPolicy and setIamPolicy: the Go client has no routine IAM
// call. The JSON etag is base64.
func routineHandle(svc *bq.Service, resource string) policyHandle {
	return policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			p, err := svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			etag, err := base64.StdEncoding.DecodeString(p.Etag)
			if err != nil {
				return nil, err
			}
			out := &iampb.Policy{Version: int32(p.Version), Etag: etag}
			for _, b := range p.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out, nil
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			in := &bq.Policy{Version: int64(p.GetVersion()), Etag: base64.StdEncoding.EncodeToString(p.GetEtag())}
			for _, b := range p.GetBindings() {
				in.Bindings = append(in.Bindings, &bq.Binding{Role: b.GetRole(), Members: b.GetMembers()})
			}
			_, err := svc.Routines.SetIamPolicy(resource, &bq.SetIamPolicyRequest{Policy: in}).Context(ctx).Do()
			return err
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			r, err := svc.Routines.TestIamPermissions(resource, &bq.TestIamPermissionsRequest{Permissions: perms}).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			return r.Permissions, nil
		},
	}
}
