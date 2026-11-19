//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam/apiv1/iampb"
)

// TestBigQueryConnectionGroupMembers1812 (#1812), with the official
// cloud.google.com/go/bigquery, bigquery/connection/apiv1,
// resourcemanager/apiv3 and cloudidentity/v1 clients: CREATE MODEL …
// REMOTE WITH CONNECTION takes bigquery.connections.use granted on the
// connection's policy to group:callers (the caller in it through a nested
// group), and the connection's service account's roles/aiplatform.user
// granted on the project to group:agents, which it is a member of; the
// nested membership's removal takes the caller's grant away. Gated like
// the remote-model tests: the front reaches these checks only when it
// serves local Vertex AI.
//
// covers: bigquery.jobs.query, bigqueryconnection.projects.locations.connections.setIamPolicy
func TestBigQueryConnectionGroupMembers1812(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	svc := cloudIdentity(t, h, runShardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager))
	ctx := h.Context()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e8)
	callers, inner := groupFixture(t, svc, "bqc-callers-"+sfx, "bqc-inner-"+sfx)
	agents, _ := groupFixture(t, svc, "bqc-agents-"+sfx, "bqc-unused-"+sfx)
	addMember(t, svc, inner.Name, strings.TrimPrefix(callerMember(project), "serviceAccount:"))
	nested := addMember(t, svc, callers.Name, inner.GroupKey.Id)

	id := strings.ReplaceAll(h.Project(), "-", "_") + "_grp"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	sa := cn.GetCloudResource().GetServiceAccountId()
	addMember(t, svc, agents.Name, sa)
	ds := mlDataset(t, h, c, map[string]string{"prompts": "SELECT 'hi' AS prompt"})
	create := func(name string) error {
		return mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` REMOTE WITH CONNECTION `%s.us.%s` OPTIONS(ENDPOINT = '%s')", ds, name, project, id, localModel))
	}
	if err := create("m"); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("no grant: %v, want 403 naming bigquery.connections.use", err)
	}
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{"group:" + callers.GroupKey.Id}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	if err := create("m"); err == nil || !strings.Contains(err.Error(), "Vertex AI user role") {
		t.Errorf("connectionUser through group:%s, no roles/aiplatform.user: %v; want the service account's refusal", callers.GroupKey.Id, err)
	}
	grantProject(t, h, pc, project, "roles/aiplatform.user", "group:"+agents.GroupKey.Id)
	if err := create("m"); err != nil {
		t.Fatalf("CREATE MODEL with both grants through groups: %v", err)
	}
	if _, err := svc.Groups.Memberships.Delete(nested.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if err := create("m2"); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("after the inner group left group:%s: %v, want 403 naming bigquery.connections.use", callers.GroupKey.Id, err)
	}
}
