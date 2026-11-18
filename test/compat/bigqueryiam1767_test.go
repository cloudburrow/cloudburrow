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
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestBigQueryPerProjectIAM (#1767): a caller with an access token the
// instance minted (IAM Credentials GenerateAccessToken for a service
// account) is held to BigQuery's permissions, through the official Go
// client: with no role, a query job is 403 "User does not have
// bigquery.jobs.create permission in project P"; with roles/bigquery.jobUser
// on the job's project, a query of another project's table is 403
// bigquery.tables.getData on that table; with roles/bigquery.dataViewer on
// the data's project it runs, the job the job project's. A request
// without such a token (the ADC fixture's, WithoutAuthentication) is
// served as before.
//
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryPerProjectIAM(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" || h.Endpoint(EnvMetadata) == "" {
		t.Skip("no BigQuery or metadata endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	c, home := bigqueryClient(t, h)
	ctx := h.Context()
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	other := v1ID(h, "bqiam-")
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
	oc, err := bigquery.NewClient(ctx, other, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oc.Close() })
	if err := oc.Dataset("data").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", other, err)
	}
	q := oc.Query("CREATE TABLE data.t AS SELECT 1 AS x UNION ALL SELECT 2")
	q.Location = "US"
	if j, err := q.Run(ctx); err != nil {
		t.Fatal(err)
	} else if _, err := j.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	p := newIAMPrincipal(t, h, home, "bqiam")
	tc, err := bigquery.NewClient(ctx, home, option.WithEndpoint(endpoint),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	sql := "SELECT SUM(x) FROM `" + other + ".data.t`"
	run := func(cl *bigquery.Client) (int64, error) {
		q := cl.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		if err != nil {
			return 0, err
		}
		if j.ProjectID() != home {
			t.Errorf("the job is in %s, want %s", j.ProjectID(), home)
		}
		it, err := j.Read(ctx)
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
	// until polls f until it reports done: the host gives the front a
	// policy change within seconds.
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
	denied := func(err error, perm string) (bool, string) {
		var ge *googleapi.Error
		return errors.As(err, &ge) && ge.Code == http.StatusForbidden && strings.Contains(ge.Message, perm),
			fmt.Sprintf("%v", err)
	}

	// Without a minted token: served as the instance's ADC account.
	if n, err := run(c); err != nil || n != 3 {
		t.Fatalf("without a minted token: %d, %v; want 3", n, err)
	}
	// No role: bigquery.jobs.create on the job's project.
	until("a query with no role", func() (bool, string) {
		_, err := run(tc)
		ok, got := denied(err, "User does not have bigquery.jobs.create permission in project "+home)
		return ok, got
	})
	// jobUser on the job's project: the other project's table's getData.
	p.grant("roles/bigquery.jobUser")
	until("a query with jobUser on "+home, func() (bool, string) {
		_, err := run(tc)
		return denied(err, "Permission bigquery.tables.getData denied on table "+other+":data.t")
	})
	// dataViewer on the data's project: it runs.
	change := projectRoles(t, h, other, "serviceAccount:"+p.email)
	change("roles/bigquery.dataViewer", true)
	t.Cleanup(func() { change("roles/bigquery.dataViewer", false) })
	until("a query with dataViewer on "+other, func() (bool, string) {
		n, err := run(tc)
		return err == nil && n == 3, fmt.Sprintf("%d, %v", n, err)
	})
	// datasets.insert in the other project: dataViewer does not grant it.
	err = tc.DatasetInProject(other, "made").Create(ctx, &bigquery.DatasetMetadata{Location: "US"})
	if ok, got := denied(err, "User does not have bigquery.datasets.create permission in project "+other); !ok {
		t.Errorf("datasets.insert in %s with dataViewer = %s, want 403 bigquery.datasets.create", other, got)
	}
}
