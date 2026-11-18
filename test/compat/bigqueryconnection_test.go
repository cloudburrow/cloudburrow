//go:build compat

package compat

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	connection "cloud.google.com/go/bigquery/connection/apiv1"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestBigQueryConnectionAPI (#1755): the BigQuery Connection API v1,
// served on the BigQuery front's port, through the official
// cloud.google.com/go/bigquery/connection/apiv1 REST client: create (a
// cloudResource connection is given its service account; a cloudSql
// credential is kept as hasCredential and never returned), get, list with
// paging, patch by update mask (refused without one), the IAM policy
// methods, delete, and the documented errors (409 for a second create,
// 404 after delete).
//
// covers: bigqueryconnection.projects.locations.connections.create, bigqueryconnection.projects.locations.connections.get, bigqueryconnection.projects.locations.connections.list, bigqueryconnection.projects.locations.connections.patch, bigqueryconnection.projects.locations.connections.delete, bigqueryconnection.projects.locations.connections.getIamPolicy, bigqueryconnection.projects.locations.connections.setIamPolicy, bigqueryconnection.projects.locations.connections.testIamPermissions
func TestBigQueryConnectionAPI(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		t.Fatalf("BigQuery endpoint %q: %v", endpoint, err)
	}
	ctx := h.Context()
	c, err := connection.NewRESTClient(ctx, option.WithEndpoint(u.Scheme+"://"+u.Host), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("connection.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	connectionAPIRun(t, h, c, project, strings.ReplaceAll(h.Project(), "-", "_"), func(err error) int {
		var ae *apierror.APIError
		if errors.As(err, &ae) {
			return ae.HTTPCode()
		}
		return 0
	})
}

// TestBigQueryConnectionAPIGRPC (#1766): the same calls as
// TestBigQueryConnectionAPI through the official client's gRPC transport
// (connection.NewClient), served on the BigQuery front's gRPC port (the
// Storage Read API's), with the canonical codes: ALREADY_EXISTS for a
// second create, INVALID_ARGUMENT for an update without a mask, NOT_FOUND
// after delete.
func TestBigQueryConnectionAPIGRPC(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQueryStorage)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	ctx := h.Context()
	c, err := connection.NewClient(ctx, option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("connection.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	connectionAPIRun(t, h, c, project, strings.ReplaceAll(h.Project(), "-", "_")+"_g", func(err error) int {
		switch status.Code(err) {
		case codes.OK:
			return 0
		case codes.InvalidArgument:
			return http.StatusBadRequest
		case codes.NotFound:
			return http.StatusNotFound
		case codes.AlreadyExists:
			return http.StatusConflict
		}
		return -1
	})
}

// connectionAPIRun runs the Connection API's calls through c; code is an
// error's HTTP code (a gRPC one mapped to it).
func connectionAPIRun(t *testing.T, h *Harness, c *connection.Client, project, id string, code func(error) int) {
	ctx := h.Context()
	parent := "projects/" + project + "/locations/us"

	cr, err := c.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: parent, ConnectionId: id + "_ai",
		Connection: &connectionpb.Connection{FriendlyName: "ai", Description: "remote models",
			Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if !strings.HasSuffix(cr.Name, "/locations/us/connections/"+id+"_ai") || cr.FriendlyName != "ai" || cr.CreationTime == 0 ||
		!strings.HasSuffix(cr.GetCloudResource().GetServiceAccountId(), "@gcp-sa-bigquery-condel.iam.gserviceaccount.com") {
		t.Errorf("created %+v", cr)
	}
	_, err = c.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: parent, ConnectionId: id + "_ai",
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if code(err) != http.StatusConflict {
		t.Errorf("a second create: %v", err)
	}
	sq, err := c.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: parent, ConnectionId: id + "_sql",
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudSql{CloudSql: &connectionpb.CloudSqlProperties{
			InstanceId: project + ":us-central1:db", Database: "app", Type: connectionpb.CloudSqlProperties_POSTGRES,
			Credential: &connectionpb.CloudSqlCredential{Username: "u", Password: "secret"}}}}})
	if err != nil {
		t.Fatalf("CreateConnection cloudSql: %v", err)
	}
	if !sq.HasCredential || sq.GetCloudSql().GetCredential() != nil || sq.GetCloudSql().GetDatabase() != "app" {
		t.Errorf("cloudSql connection %+v", sq)
	}

	got, err := c.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: cr.Name})
	if err != nil || got.Description != "remote models" || got.GetCloudResource().GetServiceAccountId() != cr.GetCloudResource().GetServiceAccountId() {
		t.Errorf("GetConnection: %+v, %v", got, err)
	}
	if _, err := c.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: parent + "/connections/" + id + "_ai"}); err != nil {
		t.Errorf("GetConnection by project ID: %v", err)
	}

	it := c.ListConnections(ctx, &connectionpb.ListConnectionsRequest{Parent: parent, PageSize: 1})
	seen := map[string]bool{}
	for {
		cn, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListConnections: %v", err)
		}
		seen[cn.Name] = true
	}
	if !seen[cr.Name] || !seen[sq.Name] {
		t.Errorf("ListConnections saw %v", seen)
	}

	up, err := c.UpdateConnection(ctx, &connectionpb.UpdateConnectionRequest{Name: cr.Name,
		Connection: &connectionpb.Connection{FriendlyName: "gemini"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"friendly_name"}}})
	if err != nil || up.FriendlyName != "gemini" || up.Description != "remote models" {
		t.Errorf("UpdateConnection: %+v, %v", up, err)
	}
	_, err = c.UpdateConnection(ctx, &connectionpb.UpdateConnectionRequest{Name: cr.Name, Connection: &connectionpb.Connection{FriendlyName: "x"}})
	if code(err) != http.StatusBadRequest {
		t.Errorf("UpdateConnection without a mask: %v", err)
	}

	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cr.Name})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	pol.Bindings = []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{"user:dev@example.com"}}}
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cr.Name, Policy: pol})
	if err != nil || len(set.Bindings) != 1 {
		t.Errorf("SetIamPolicy: %+v, %v", set, err)
	}
	if again, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cr.Name}); err != nil || len(again.Bindings) != 1 ||
		again.Bindings[0].Role != "roles/bigquery.connectionUser" {
		t.Errorf("GetIamPolicy after set: %+v, %v", again, err)
	}
	perms, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: cr.Name, Permissions: []string{"bigquery.connections.use"}})
	if err != nil || len(perms.Permissions) != 1 {
		t.Errorf("TestIamPermissions: %+v, %v", perms, err)
	}

	for _, n := range []string{cr.Name, sq.Name} {
		if err := c.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: n}); err != nil {
			t.Errorf("DeleteConnection %s: %v", n, err)
		}
	}
	if _, err := c.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: cr.Name}); code(err) != http.StatusNotFound {
		t.Errorf("GetConnection after delete: %v", err)
	}
}
