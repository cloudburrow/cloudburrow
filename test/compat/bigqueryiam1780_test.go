//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
)

// TestBigQueryPerProjectIAM1780 (#1780): with a token the instance minted,
// through the official Go clients: a conditional binding on
// resource.matchTag grants on a dataset a Resource Manager tag binding
// tags, and on a table whose resourceTags carry the value, not on an
// untagged one; a custom role named only in a table's own IAM policy
// grants on that table; a routine called by a two-part name needs
// bigquery.routines.get (a one-part name is not found); a stored
// procedure's body is checked for the tables it reads; a JavaScript UDF
// needs bigquery.routines.get alone; and a caller's own job is still its
// own after a restart of BigQuery's front.
//
// covers: bigquery.jobs.query, bigquery.jobs.get, bigquery.tables.setIamPolicy, bigquery.tabledata.list
func TestBigQueryPerProjectIAM1780(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" || h.Endpoint(EnvMetadata) == "" {
		t.Skip("no BigQuery or metadata endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	_, home := bigqueryClient(t, h)
	ctx := h.Context()
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	other := v1ID(h, "bqiamt-")
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: other}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", other, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + other})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil)
	})
	ocl, err := bigquery.NewClient(ctx, other, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ocl.Close() })
	exec := func(cl *bigquery.Client, sql string) error {
		q := cl.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		if err != nil {
			return err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}

	// Tags: a project tag key env with prod, bound to the dataset tagged
	// and carried in the resourceTags of the table plain.rt.
	kc, err := resourcemanager.NewTagKeysClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	vc, err := resourcemanager.NewTagValuesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	bc, err := resourcemanager.NewTagBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	kop, err := kc.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: "projects/" + other, ShortName: "env"}})
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	vop, err := vc.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "prod"}})
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	prod, err := vop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}

	for _, d := range []string{"tagged", "plain", "src", "views"} {
		if err := ocl.Dataset(d).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
			t.Fatalf("datasets.insert %s: %v", d, err)
		}
	}
	tb := "`" + other + "."
	for _, sql := range []string{
		"CREATE TABLE tagged.t AS SELECT 3 AS x",
		"CREATE TABLE plain.t AS SELECT 4 AS x",
		"CREATE TABLE plain.rt AS SELECT 5 AS x",
		"CREATE TABLE plain.custom AS SELECT 6 AS x",
		"CREATE TABLE src.t AS SELECT 1 AS x UNION ALL SELECT 2",
		"CREATE FUNCTION src.inc(x INT64) AS (x + 1)",
		"CREATE FUNCTION views.inc(x INT64) AS (x + 1)",
		"CREATE PROCEDURE views.readsrc() BEGIN SELECT SUM(x) FROM " + tb + "src.t`; END",
		"CREATE FUNCTION views.js(x FLOAT64) RETURNS FLOAT64 LANGUAGE js AS r'''return x * 2;'''",
	} {
		if err := exec(ocl, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	bop, err := bc.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{
		Parent: "//bigquery.googleapis.com/projects/" + other + "/datasets/tagged", TagValue: prod.GetName()}})
	if err == nil {
		_, err = bop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateTagBinding on the dataset tagged: %v", err)
	}
	if _, err := ocl.Dataset("plain").Table("rt").Update(ctx, bigquery.TableMetadataToUpdate{
		ResourceTags: map[string]string{key.GetNamespacedName(): "prod"}}, ""); err != nil {
		t.Fatalf("tables.patch resourceTags: %v", err)
	}

	p := newIAMPrincipal(t, h, home, "bqiamt")
	p.grant("roles/bigquery.jobUser")
	member := "serviceAccount:" + p.email
	// dataViewer on the other project where the resource carries env=prod.
	t.Cleanup(bindOn(t, pc.GetIamPolicy, pc.SetIamPolicy, "projects/"+other, &iampb.Binding{Role: "roles/bigquery.dataViewer",
		Members: []string{member}, Condition: &expr.Expr{Title: "prod", Expression: fmt.Sprintf("resource.matchTag(%q, \"prod\")",
			key.GetNamespacedName())}}))
	// READER on views.
	md, err := ocl.Dataset("views").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ocl.Dataset("views").Update(ctx, bigquery.DatasetMetadataToUpdate{Access: append(md.Access,
		&bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: p.email})}, md.ETag); err != nil {
		t.Fatalf("datasets.patch views: %v", err)
	}
	// A custom role of the other project, named only in plain.custom's
	// own policy.
	ac, err := admin.NewIamClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	role, err := ac.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + other, RoleId: "tableReader", Role: &adminpb.Role{
		Title: "table reader", IncludedPermissions: []string{"bigquery.tables.get", "bigquery.tables.getData"}, Stage: adminpb.Role_GA}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	th := ocl.Dataset("plain").Table("custom").IAM()
	pol, err := th.Policy(ctx)
	if err != nil {
		t.Fatalf("tables.getIamPolicy: %v", err)
	}
	pol.Add(member, iam.RoleName(role.GetName()))
	if err := th.SetPolicy(ctx, pol); err != nil {
		t.Fatalf("tables.setIamPolicy with %s: %v", role.GetName(), err)
	}

	tc, err := bigquery.NewClient(ctx, home, option.WithEndpoint(endpoint),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	run := func(sql, defDataset string) (*bigquery.Job, int64, error) {
		q := tc.Query(sql)
		q.Location = "US"
		if defDataset != "" {
			q.DefaultProjectID, q.DefaultDatasetID = other, defDataset
		}
		j, err := q.Run(ctx)
		if err != nil {
			return nil, 0, err
		}
		it, err := j.Read(ctx)
		if err != nil {
			return j, 0, err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return j, 0, err
		}
		switch v := row[0].(type) {
		case int64:
			return j, v, nil
		case float64:
			return j, int64(v), nil
		}
		return j, 0, nil
	}
	sum := func(sql string) (int64, error) {
		_, n, err := run(sql, "")
		return n, err
	}
	denied := func(err error, msg string) (bool, string) {
		var ge *googleapi.Error
		return errors.As(err, &ge) && ge.Code == http.StatusForbidden && strings.Contains(ge.Message, msg), fmt.Sprintf("%v", err)
	}
	// The grants and tags reach the front within seconds.
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, err := sum("SELECT SUM(x) FROM " + tb + "tagged.t`")
		if err == nil && n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a dataset tagged env=prod under a matchTag condition: %d, %v; want 3", n, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if n, err := sum("SELECT SUM(x) FROM " + tb + "plain.rt`"); err != nil || n != 5 {
		t.Errorf("a table whose resourceTags carry env=prod: %d, %v; want 5", n, err)
	}
	_, err = sum("SELECT SUM(x) FROM " + tb + "plain.t`")
	if ok, got := denied(err, "Permission bigquery.tables.getData denied on table "+other+":plain.t"); !ok {
		t.Errorf("an untagged table = %s, want 403 bigquery.tables.getData", got)
	}
	if n, err := sum("SELECT SUM(x) FROM " + tb + "plain.custom`"); err != nil || n != 6 {
		t.Errorf("a table whose own policy grants a custom role: %d, %v; want 6", n, err)
	}

	// Routines.
	// A persistent function's name is qualified with its dataset: called
	// by one part, with a default dataset, it is not found, as on Google.
	var ge *googleapi.Error
	if _, _, err := run("SELECT inc(1)", "src"); !errors.As(err, &ge) || ge.Code != http.StatusBadRequest ||
		!strings.Contains(ge.Message, "Function not found: inc") {
		t.Errorf("a one-part call of src.inc (default dataset src) = %v, want 400 Function not found", err)
	}
	_, err = sum("CALL " + tb + "views.readsrc`()")
	if ok, got := denied(err, "Permission bigquery.tables.getData denied on table "+other+":src.t"); !ok {
		t.Errorf("a procedure reading src.t = %s, want 403 bigquery.tables.getData on %s:src.t", got, other)
	}
	if n, err := sum("SELECT " + tb + "views.js`(2.0)"); err != nil || n != 4 {
		t.Errorf("a JavaScript UDF with READER on views: %d, %v; want 4", n, err)
	}

	// A two-part name is the job's project's: a dataset of home's own.
	hcl, _ := bigqueryClient(t, h)
	hds := strings.ReplaceAll(v1ID(h, "bqiamt_"), "-", "_")
	if err := hcl.Dataset(hds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert %s: %v", hds, err)
	}
	t.Cleanup(func() { _ = hcl.Dataset(hds).DeleteWithContents(context.Background()) })
	if err := exec(hcl, "CREATE FUNCTION "+hds+".dbl(x INT64) AS (x * 2)"); err != nil {
		t.Fatal(err)
	}
	_, _, err = run("SELECT "+hds+".dbl(1)", "")
	if ok, got := denied(err, "Permission bigquery.routines.get denied on routine "+home+":"+hds+".dbl"); !ok {
		t.Errorf("a two-part call of %s.dbl = %s, want 403 bigquery.routines.get", hds, got)
	}

	// Its own job after a restart of the front.
	j, _, err := run("SELECT 1", "")
	if err != nil {
		t.Fatalf("a query: %v", err)
	}
	restartFront(t, "bigquery", "bigquery")
	deadline = time.Now().Add(60 * time.Second)
	for {
		_, err = tc.JobFromIDLocation(ctx, j.ID(), "US")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs.get of its own job after a restart of the front: %v", err)
		}
		time.Sleep(time.Second)
	}
}
