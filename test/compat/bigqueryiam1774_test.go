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
	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
)

// TestBigQueryPerProjectIAMRest (#1774): with a token the instance minted,
// through the official Go clients, projects.list names the projects the
// caller holds a role on, and BigQuery checks routines
// (bigquery.routines.get, list), models (bigquery.models.getMetadata), row
// access policies (bigquery.rowAccessPolicies.list), a table's own IAM
// policy methods (bigquery.tables.getIamPolicy, setIamPolicy); a table's
// own IAM policy grants on that table; a conditional binding grants where
// its condition holds (resource.name); a role on the project's folder and
// on the organization is inherited; and jobs follow the creator rule: a
// caller gets, lists and cancels its own jobs with roles/bigquery.jobUser,
// another's job is 403 bigquery.jobs.get, listing all users' jobs 403
// bigquery.jobs.listAll, until roles/bigquery.resourceViewer (on the
// organization) grants them, and cancelling another's job stays 403
// bigquery.jobs.update.
//
// covers: bigquery.projects.list, bigquery.jobs.get, bigquery.jobs.list, bigquery.jobs.cancel, bigquery.routines.get, bigquery.routines.list, bigquery.models.get, bigquery.rowAccessPolicies.list, bigquery.tables.getIamPolicy, bigquery.tables.setIamPolicy
func TestBigQueryPerProjectIAMRest(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" || h.Endpoint(EnvMetadata) == "" {
		t.Skip("no BigQuery or metadata endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	_, home := bigqueryClient(t, h)
	ctx := h.Context()
	fc, oc, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))

	// The other project is made in a folder of the organization.
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001",
		DisplayName: folderLabel(h, "bqiam-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	other := v1ID(h, "bqiamr-")
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: other, Parent: folder.GetName()}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", other, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + other})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil)
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
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
	for _, d := range []string{"data", "cond"} {
		if err := ocl.Dataset(d).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
			t.Fatalf("datasets.insert %s in %s: %v", d, other, err)
		}
	}
	for _, sql := range []string{
		"CREATE TABLE data.t AS SELECT 1 AS x UNION ALL SELECT 2",
		"CREATE TABLE data.t2 AS SELECT 5 AS x",
		"CREATE TABLE cond.c AS SELECT 7 AS x",
		"CREATE OR REPLACE FUNCTION data.f(x INT64) AS (x + 1)",
	} {
		if err := exec(ocl, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	p := newIAMPrincipal(t, h, home, "bqiamr")
	p.grant("roles/bigquery.jobUser")
	tok := option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"}))
	tc, err := bigquery.NewClient(ctx, home, option.WithEndpoint(endpoint), tok)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	svc, err := bq.NewService(ctx, option.WithEndpoint(strings.TrimRight(endpoint, "/")+"/bigquery/v2/"), tok)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(table string) (int64, error) {
		q := tc.Query("SELECT SUM(x) FROM `" + table + "`")
		q.Location = "US"
		it, err := q.Read(ctx)
		if err != nil {
			return 0, err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return 0, err
		}
		n, _ := row[0].(int64)
		return n, nil
	}
	until := func(what string, f func() (bool, string)) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			ok, got := f()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s", what, got)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	denied := func(err error, msg string) (bool, string) {
		var ge *googleapi.Error
		return errors.As(err, &ge) && ge.Code == http.StatusForbidden && strings.Contains(ge.Message, msg), fmt.Sprintf("%v", err)
	}
	// projects.list names the projects the caller holds a role on.
	until("projects.list with jobUser on "+home, func() (bool, string) {
		l, err := svc.Projects.List().Context(ctx).Do()
		if err != nil {
			return false, err.Error()
		}
		var ids []string
		for _, p := range l.Projects {
			ids = append(ids, p.ProjectReference.ProjectId)
		}
		return len(ids) == 1 && ids[0] == home, fmt.Sprint(ids)
	})
	ds := tc.DatasetInProject(other, "data")
	routineGet := func() error { _, err := ds.Routine("f").Metadata(ctx); return err }
	tableIAM := func() error { _, err := ds.Table("t").IAM().Policy(ctx); return err }

	// jobUser on the home project alone: every read of the other's is 403.
	until("a query with jobUser on "+home, func() (bool, string) {
		_, err := sum(other + ".data.t")
		return denied(err, "Permission bigquery.tables.getData denied on table "+other+":data.t")
	})
	for _, c := range []struct {
		what, msg string
		call      func() error
	}{
		{"routines.get", "Access Denied: Routine " + other + ":data.f: Permission bigquery.routines.get denied on routine " + other + ":data.f",
			routineGet},
		{"routines.list", "Permission bigquery.routines.list denied on dataset " + other + ":data", func() error {
			_, err := ds.Routines(ctx).Next()
			return err
		}},
		{"models.get", "Permission bigquery.models.getMetadata denied on model " + other + ":data.m", func() error {
			_, err := ds.Model("m").Metadata(ctx)
			return err
		}},
		{"rowAccessPolicies.list", "Permission bigquery.rowAccessPolicies.list denied on table " + other + ":data.t", func() error {
			_, err := svc.RowAccessPolicies.List(other, "data", "t").Context(ctx).Do()
			return err
		}},
		{"tables.getIamPolicy", "Permission bigquery.tables.getIamPolicy denied on table " + other + ":data.t", tableIAM},
	} {
		if ok, got := denied(c.call(), c.msg); !ok {
			t.Errorf("%s with jobUser on %s = %s, want 403 %q", c.what, home, got, c.msg)
		}
	}

	// A table's own IAM policy grants on that table only.
	h2 := ocl.Dataset("data").Table("t2").IAM()
	pol, err := h2.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("serviceAccount:"+p.email, "roles/bigquery.dataViewer")
	if err := h2.SetPolicy(ctx, pol); err != nil {
		t.Fatalf("tables.setIamPolicy: %v", err)
	}
	if n, err := sum(other + ".data.t2"); err != nil || n != 5 {
		t.Errorf("a query of data.t2, whose policy grants dataViewer: %d, %v; want 5", n, err)
	}
	if _, err := sum(other + ".data.t"); err == nil {
		t.Errorf("a query of data.t, whose policy does not, ran")
	}

	// A conditional binding on the other project: the cond dataset alone.
	removeCond := bindOn(t, pc.GetIamPolicy, pc.SetIamPolicy, "projects/"+other, &iampb.Binding{Role: "roles/bigquery.dataViewer",
		Members: []string{"serviceAccount:" + p.email}, Condition: &expr.Expr{Title: "cond only",
			Expression: `resource.name.startsWith("projects/` + other + `/datasets/cond/")`}})
	until("a query of cond.c under a condition naming it", func() (bool, string) {
		n, err := sum(other + ".cond.c")
		return err == nil && n == 7, fmt.Sprintf("%d, %v", n, err)
	})
	if ok, got := denied(func() error { _, err := sum(other + ".data.t"); return err }(), "bigquery.tables.getData"); !ok {
		t.Errorf("a query of data.t under a condition naming cond = %s, want 403", got)
	}
	removeCond()

	// dataViewer on the project's folder is inherited.
	removeFolder := bindOn(t, fc.GetIamPolicy, fc.SetIamPolicy, folder.GetName(), &iampb.Binding{Role: "roles/bigquery.dataViewer",
		Members: []string{"serviceAccount:" + p.email}})
	until("a query with dataViewer on "+folder.GetName(), func() (bool, string) {
		n, err := sum(other + ".data.t")
		return err == nil && n == 3, fmt.Sprintf("%d, %v", n, err)
	})
	if err := routineGet(); err != nil {
		t.Errorf("routines.get with dataViewer on the folder: %v", err)
	}
	if err := tableIAM(); err != nil {
		t.Errorf("tables.getIamPolicy with dataViewer on the folder: %v", err)
	}
	if ok, got := denied(ds.Table("t").IAM().SetPolicy(ctx, &iam.Policy{InternalProto: &iampb.Policy{}}), "Permission bigquery.tables.setIamPolicy denied on table "+other+":data.t"); !ok {
		t.Errorf("tables.setIamPolicy with dataViewer = %s, want 403", got)
	}
	if ok, got := denied(func() error {
		_, err := svc.RowAccessPolicies.List(other, "data", "t").Context(ctx).Do()
		return err
	}(), "bigquery.rowAccessPolicies.list"); !ok {
		t.Errorf("rowAccessPolicies.list with dataViewer = %s, want 403", got)
	}
	removeFolder()

	// Jobs, in the other project (whose ancestry is the folder and the
	// organization): the caller's own, and the instance account's.
	changeOther := projectRoles(t, h, other, "serviceAccount:"+p.email)
	changeOther("roles/bigquery.jobUser", true)
	t.Cleanup(func() { changeOther("roles/bigquery.jobUser", false) })
	jc, err := bigquery.NewClient(ctx, other, option.WithEndpoint(endpoint), tok)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	run := func(cl *bigquery.Client) *bigquery.Job {
		t.Helper()
		q := cl.Query("SELECT 1")
		q.Location = "US"
		j, err := q.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		return j
	}
	var mine *bigquery.Job
	until("a job with jobUser on "+other, func() (bool, string) {
		q := jc.Query("SELECT 1")
		q.Location = "US"
		j, err := q.Run(ctx)
		if err == nil {
			_, err = j.Wait(ctx)
		}
		mine = j
		return err == nil, fmt.Sprintf("%v", err)
	})
	theirs := run(ocl)
	if _, err := jc.JobFromIDLocation(ctx, mine.ID(), "US"); err != nil {
		t.Errorf("jobs.get of its own job: %v", err)
	}
	getTheirs := func() (*bigquery.Job, error) { return jc.JobFromIDLocation(ctx, theirs.ID(), "US") }
	if _, err := getTheirs(); err != nil {
		if ok, got := denied(err, "User does not have bigquery.jobs.get permission for job "+other+":US."+theirs.ID()); !ok {
			t.Errorf("jobs.get of another's job = %s, want 403 bigquery.jobs.get", got)
		}
	} else {
		t.Errorf("jobs.get of another's job with jobUser succeeded")
	}
	listed := func(all bool) (map[string]bool, error) {
		it := jc.Jobs(ctx)
		it.AllUsers = all
		out := map[string]bool{}
		for {
			j, err := it.Next()
			if err == iterator.Done {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out[j.ID()] = true
		}
	}
	if got, err := listed(false); err != nil || !got[mine.ID()] || got[theirs.ID()] {
		t.Errorf("jobs.list with jobUser: mine %v, theirs %v, %v; want only its own", got[mine.ID()], got[theirs.ID()], err)
	}
	if _, err := listed(true); err == nil {
		t.Errorf("jobs.list allUsers with jobUser succeeded")
	} else if ok, got := denied(err, "User does not have bigquery.jobs.listAll permission in project "+other); !ok {
		t.Errorf("jobs.list allUsers with jobUser = %s, want 403 bigquery.jobs.listAll", got)
	}
	if err := mine.Cancel(ctx); err != nil {
		t.Errorf("jobs.cancel of its own job: %v", err)
	}
	// resourceViewer on the organization: everyone's jobs, read only.
	removeOrg := bindOn(t, oc.GetIamPolicy, oc.SetIamPolicy, "organizations/100000000001", &iampb.Binding{
		Role: "roles/bigquery.resourceViewer", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(removeOrg)
	var j *bigquery.Job
	until("jobs.get of another's job with resourceViewer on the organization", func() (bool, string) {
		j, err = getTheirs()
		return err == nil, fmt.Sprintf("%v", err)
	})
	if got, err := listed(true); err != nil || !got[mine.ID()] || !got[theirs.ID()] {
		t.Errorf("jobs.list allUsers with resourceViewer: mine %v, theirs %v, %v; want both", got[mine.ID()], got[theirs.ID()], err)
	}
	if ok, got := denied(j.Cancel(ctx), "User does not have bigquery.jobs.update permission for job"); !ok {
		t.Errorf("jobs.cancel of another's job with resourceViewer = %s, want 403 bigquery.jobs.update", got)
	}
}

// bindOn adds b to resource's IAM policy through get and set (a Resource
// Manager client's GetIamPolicy and SetIamPolicy), and returns what
// removes it again.
func bindOn[O any](t *testing.T, get func(context.Context, *iampb.GetIamPolicyRequest, ...O) (*iampb.Policy, error),
	set func(context.Context, *iampb.SetIamPolicyRequest, ...O) (*iampb.Policy, error), resource string, b *iampb.Binding) func() {
	t.Helper()
	change := func(add bool) error {
		ctx := context.Background()
		pol, err := get(ctx, &iampb.GetIamPolicyRequest{Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			return err
		}
		var keep []*iampb.Binding
		for _, x := range pol.GetBindings() {
			if x.GetRole() == b.GetRole() && x.GetCondition().GetExpression() == b.GetCondition().GetExpression() &&
				len(x.GetMembers()) == len(b.GetMembers()) && strings.Join(x.GetMembers(), ",") == strings.Join(b.GetMembers(), ",") {
				continue
			}
			keep = append(keep, x)
		}
		if add {
			keep = append(keep, b)
		}
		pol.Bindings = keep
		pol.Version = 3
		_, err = set(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
		return err
	}
	if err := change(true); err != nil {
		t.Fatalf("SetIamPolicy %s: %v", resource, err)
	}
	done := false
	return func() {
		if done {
			return
		}
		done = true
		if err := change(false); err != nil {
			t.Errorf("SetIamPolicy %s (removing %s): %v", resource, b.GetRole(), err)
		}
	}
}
