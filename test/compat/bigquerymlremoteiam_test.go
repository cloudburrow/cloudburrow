//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	connection "cloud.google.com/go/bigquery/connection/apiv1"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"google.golang.org/api/option"
)

// remoteConnectionClient is the official Connection API client on the
// BigQuery endpoint, and the instance's project.
func remoteConnectionClient(t *testing.T, h *Harness) (*connection.Client, string) {
	t.Helper()
	endpoint := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		t.Fatalf("BigQuery endpoint %q: %v", endpoint, err)
	}
	cc, err := connection.NewRESTClient(h.Context(), option.WithEndpoint(u.Scheme+"://"+u.Host), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("connection.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc, project
}

// remoteProjectsClient is the official Resource Manager projects client.
func remoteProjectsClient(t *testing.T, h *Harness) *resourcemanager.ProjectsClient {
	t.Helper()
	pc, err := resourcemanager.NewProjectsClient(h.Context(), rmOptionsAt(runShardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager))...)
	if err != nil {
		t.Fatalf("resourcemanager.NewProjectsClient: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// editProjectMember adds member to (add) or removes it from role on
// project's policy, read and written back.
func editProjectMember(h *Harness, pc *resourcemanager.ProjectsClient, project, role, member string, add bool) error {
	ctx := h.Context()
	p, err := pc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		return err
	}
	var out []*iampb.Binding
	done := false
	for _, b := range p.Bindings {
		if b.Role == role && b.Condition == nil {
			var ms []string
			for _, m := range b.Members {
				if m != member {
					ms = append(ms, m)
				}
			}
			if add {
				ms, done = append(ms, member), true
			}
			b.Members = ms
		}
		if len(b.Members) > 0 {
			out = append(out, b)
		}
	}
	if add && !done {
		out = append(out, &iampb.Binding{Role: role, Members: []string{member}})
	}
	p.Bindings = out
	_, err = pc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project, Policy: p})
	return err
}

// grantProject adds member to role on project's policy, and removes it
// when the test ends.
func grantProject(t *testing.T, h *Harness, pc *resourcemanager.ProjectsClient, project, role, member string) {
	t.Helper()
	if err := editProjectMember(h, pc, project, role, member, true); err != nil {
		t.Fatalf("grant %s to %s on %s: %v", role, member, project, err)
	}
	t.Cleanup(func() {
		if err := editProjectMember(h, pc, project, role, member, false); err != nil {
			t.Errorf("revoke %s from %s on %s: %v", role, member, project, err)
		}
	})
}

// wantDefaultConnectionRefused runs sql, a CREATE MODEL … REMOTE WITH
// CONNECTION DEFAULT in a US dataset, as a caller without
// bigquery.connections.create on a project with no default connection in
// the US: BigQuery cannot make the default connection, so the statement is
// refused 403 naming the permission (default connections page: "you need
// the BigQuery Connection Admin role"; #1765 serves DEFAULT, which was 501
// before). An instance that serves no local Vertex AI to BigQuery (no
// CLOUDBURROW_TEST_LOCALAI: the emulators shard, which has no Cloud Run)
// runs no remote model at all, so there it is 501 naming that (#1760),
// before any connection is read. It skips the 403 when the project
// already has a default connection, which another test may be making.
func wantDefaultConnectionRefused(t *testing.T, h *Harness, c *bigquery.Client, sql string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(EnvLocalAI)) == "" {
		_, err := c.Query(sql).Read(h.Context())
		wantAPIError(t, sql+" with no local Vertex AI", err, http.StatusNotImplemented, "local Vertex AI")
		return
	}
	cc, project := remoteConnectionClient(t, h)
	name := "projects/" + project + "/locations/us/connections/__default_cloudresource_connection__"
	if _, err := cc.GetConnection(h.Context(), &connectionpb.GetConnectionRequest{Name: name}); err == nil {
		t.Logf("the project has a default connection in the US; not checking %s", sql)
		return
	}
	_, err := c.Query(sql).Read(h.Context())
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "bigquery.connections.create") {
		t.Errorf("%s with no default connection and no bigquery.connections.create: %v; want 403 naming it", sql, err)
	}
}

// callerMember is the identity the BigQuery front takes every caller to
// be: the instance's own service account (rowpolicies.go).
func callerMember(project string) string {
	return "serviceAccount:cloudburrow-local@" + project + ".iam.gserviceaccount.com"
}

// grantRemoteModel gives the caller bigquery.connections.use on the
// connection (its own policy, through the Connection API) and the
// connection's service account roles/aiplatform.user on the project, as
// BigQuery requires of a remote model (#1765).
func grantRemoteModel(t *testing.T, h *Harness, cc *connection.Client, project string, cn *connectionpb.Connection) {
	t.Helper()
	ctx := h.Context()
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", "serviceAccount:"+cn.GetCloudResource().GetServiceAccountId())
}

// TestBigQueryMLRemoteConnectionPermissions (#1765): through the official
// cloud.google.com/go/bigquery, bigquery/connection/apiv1 and
// resourcemanager/apiv3 clients, CREATE MODEL … REMOTE WITH CONNECTION
// needs bigquery.connections.use on the connection (403 Access Denied
// naming the permission without it), granted on the connection's policy
// or on the project's, and the connection's service account the Vertex
// AI User role on the project (400 naming the account without it);
// ML.GENERATE_TEXT is refused the same way, before any row is sent, when
// the grant is gone. A role without the permission grants nothing. No
// model is needed: every refusal comes before a call.
//
// covers: bigquery.jobs.query, bigqueryconnection.projects.locations.connections.setIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy
func TestBigQueryMLRemoteConnectionPermissions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_perm"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	sa := cn.GetCloudResource().GetServiceAccountId()
	if !strings.HasSuffix(sa, "@gcp-sa-bigquery-condel.iam.gserviceaccount.com") {
		t.Fatalf("serviceAccountId %q", sa)
	}
	ds := mlDataset(t, h, c, map[string]string{"prompts": "SELECT 'hi' AS prompt"})
	create := func(name string) error {
		return mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` REMOTE WITH CONNECTION `%s.us.%s` OPTIONS(ENDPOINT = '%s')", ds, name, project, id, localModel))
	}
	if err := create("m"); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Access Denied") ||
		!strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("no grant: %v", err)
	}
	// A role without the permission grants nothing.
	grantProject(t, h, pc, project, "roles/bigquery.dataViewer", callerMember(project))
	if err := create("m"); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("roles/bigquery.dataViewer: %v", err)
	}
	// roles/bigquery.connectionUser on the connection: now the service
	// account's role is missing.
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if err := create("m"); err == nil || !strings.Contains(err.Error(), sa) || !strings.Contains(err.Error(), "Vertex AI user role") {
		t.Errorf("no roles/aiplatform.user: %v", err)
	}
	grantProject(t, h, pc, project, "roles/aiplatform.user", "serviceAccount:"+sa)
	if err := create("m"); err != nil {
		t.Fatalf("CREATE MODEL with both grants: %v", err)
	}
	// The grant on the project's policy instead of the connection's.
	p, err := cc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cn.Name})
	if err != nil {
		t.Fatal(err)
	}
	p.Bindings = nil
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: p}); err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.m`, TABLE `%s.prompts`)", ds, ds)
	if err := mlRun(h, c, q); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("ML.GENERATE_TEXT with the grant gone: %v", err)
	}
	grantProject(t, h, pc, project, "roles/bigquery.connectionUser", callerMember(project))
	if err := create("m2"); err != nil {
		t.Errorf("CREATE MODEL granted on the project: %v", err)
	}
}

// TestBigQueryMLRemoteDefaultConnection (#1765): CREATE MODEL … REMOTE
// WITH CONNECTION DEFAULT, through the official cloud.google.com/go/bigquery,
// bigquery/connection/apiv1 and resourcemanager/apiv3 clients, as the
// default connections page documents it: with no default connection, a
// caller without bigquery.connections.create is refused 403 naming it; a
// caller with BigQuery Connection Admin and Project IAM Admin has
// BigQuery make __default_cloudresource_connection__ (CLOUD_RESOURCE, in
// the dataset's region) and grant its service account roles/aiplatform.user
// on the project, and the model answers ML.GENERATE_TEXT on the local
// runtime.
//
// covers: bigquery.jobs.query, bigqueryconnection.projects.locations.connections.get, google.cloud.resourcemanager.v3.Projects/GetIamPolicy
func TestBigQueryMLRemoteDefaultConnection(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	ctx := h.Context()
	name := "projects/" + project + "/locations/us/connections/__default_cloudresource_connection__"
	if _, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: name}); err == nil {
		t.Skip("the project already has a default connection in the US; this test makes it")
	}
	ds := mlDataset(t, h, c, map[string]string{"prompts": "SELECT 'What is the capital of France? Answer with one word.' AS prompt"})
	create := func() error {
		return mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.d` REMOTE WITH CONNECTION DEFAULT OPTIONS(ENDPOINT = '%s')", ds, localModel))
	}
	if err := create(); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "bigquery.connections.create") {
		t.Fatalf("no permission to make the default connection: %v", err)
	}
	grantProject(t, h, pc, project, "roles/bigquery.connectionAdmin", callerMember(project))
	grantProject(t, h, pc, project, "roles/resourcemanager.projectIamAdmin", callerMember(project))
	if err := create(); err != nil {
		t.Fatalf("CREATE MODEL REMOTE WITH CONNECTION DEFAULT: %v", err)
	}
	cn, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: name})
	if err != nil {
		t.Fatalf("the default connection: %v", err)
	}
	sa := "serviceAccount:" + cn.GetCloudResource().GetServiceAccountId()
	t.Cleanup(func() {
		_ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name})
		// What BigQuery granted goes with the connection.
		if err := editProjectMember(h, pc, project, "roles/aiplatform.user", sa, false); err != nil {
			t.Errorf("revoke roles/aiplatform.user: %v", err)
		}
	})
	p, err := pc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	granted := false
	for _, b := range p.Bindings {
		for _, m := range b.Members {
			granted = granted || b.Role == "roles/aiplatform.user" && m == sa
		}
	}
	if !granted {
		t.Errorf("the default connection's service account was not granted roles/aiplatform.user: %v", p.Bindings)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.d`, TABLE `%s.prompts`, "+
		"STRUCT(TRUE AS flatten_json_output))", ds, ds))
	if len(rows) != 1 || rows[0]["ml_generate_text_status"] != "" || rows[0]["ml_generate_text_llm_result"] == nil {
		t.Errorf("ML.GENERATE_TEXT of the DEFAULT model: %v", rows)
	}
	t.Logf("model output (recorded, not asserted): %v", rows)
}
