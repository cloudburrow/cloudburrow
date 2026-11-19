//go:build compat

package compat

import (
	"context"
	"errors"
	"testing"

	"cloud.google.com/go/bigquery"
	fsadminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orgLocationProject is a new project under a gcp.resourceLocations
// policy allowing in:us-locations, and the message Google documents for a
// europe-west1 resource in it.
func orgLocationProject(t *testing.T, h *Harness, prefix string) (string, string) {
	t.Helper()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, prefix)
	setOrgPolicy(t, h, c, "projects/"+project, "gcp.resourceLocations", opValues("in:us-locations"))
	return project, "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/" + project + "."
}

func wantFailedPrecondition(t *testing.T, what string, err error, msg string) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != msg {
		t.Errorf("%s = %v, want FAILED_PRECONDITION %q", what, err, msg)
	}
}

// covers: google.pubsub.v1.Publisher/CreateTopic, google.iam.v1.IAMPolicy/SetIamPolicy
//
// TestOrgPolicyEnforcementPubSub (#1890), with the official Pub/Sub
// client: under gcp.resourceLocations a topic whose message storage
// policy allows europe-west1 is FAILED_PRECONDITION with the documented
// message, one allowing us-east1 is created; under
// iam.allowedPolicyMemberDomains granting a user outside the customer on
// the topic is FAILED_PRECONDITION.
func TestOrgPolicyEnforcementPubSub(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvPubSub)
	project, want := orgLocationProject(t, h, "opp-")
	setOrgPolicy(t, h, orgPolicyClients(t, h)["grpc"], "projects/"+project, "iam.allowedPolicyMemberDomains", opValues("is:C0cloudburrow"))
	t.Setenv("PUBSUB_EMULATOR_HOST", endpoint)
	ctx := h.Context()
	ps, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	_, err = ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/" + project + "/topics/op-eu",
		MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"europe-west1"}}})
	wantFailedPrecondition(t, "a topic stored in europe-west1", err, want)
	topic := "projects/" + project + "/topics/op-us"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic,
		MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-east1"}}}); err != nil {
		t.Fatalf("a topic stored in us-east1: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
	})
	_, err = ps.TopicAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: topic, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/pubsub.publisher", Members: []string{"user:someone@example.com"}}}}})
	wantFailedPrecondition(t, "granting a user outside the customer", err, "One or more users named in the policy do not belong to a permitted customer.")
}

// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstance
//
// TestOrgPolicyEnforcementSpanner (#1890), with the official Spanner
// instance admin client: under gcp.resourceLocations an instance in
// regional-europe-west1 is FAILED_PRECONDITION with the documented
// message.
func TestOrgPolicyEnforcementSpanner(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envBigQueryResourceManager, EnvResourceManager)
	addr := h.Endpoint(EnvSpanner)
	project, want := orgLocationProject(t, h, "opn-")
	ctx := h.Context()
	c, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: "op-eu",
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/regional-europe-west1", DisplayName: "op", NodeCount: 1}})
	wantFailedPrecondition(t, "an instance in regional-europe-west1", err, want)
}

// covers: google.firestore.admin.v1.FirestoreAdmin/CreateDatabase
//
// TestOrgPolicyEnforcementFirestore (#1890), with the official Firestore
// admin client: under gcp.resourceLocations a database in europe-west1 is
// FAILED_PRECONDITION with the documented message.
func TestOrgPolicyEnforcementFirestore(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envFirestoreResourceManager, EnvResourceManager)
	c := firestoreAdmin(t, h)
	project, want := orgLocationProject(t, h, "opf-")
	_, err := c.CreateDatabase(h.Context(), &fsadminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: "op-eu",
		Database: &fsadminpb.Database{LocationId: "europe-west1", Type: fsadminpb.Database_FIRESTORE_NATIVE}})
	wantFailedPrecondition(t, "a database in europe-west1", err, want)
}

// covers: bigquery.datasets.insert
//
// TestOrgPolicyEnforcementBigQuery (#1890), with the official BigQuery
// client on the emulator's one project: under gcp.resourceLocations a
// dataset in europe-west1 is 412 with the documented message, one in US
// is created. The policy is removed when the test ends.
func TestOrgPolicyEnforcementBigQuery(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envBigQueryResourceManager, EnvResourceManager)
	bq, project := bigqueryClient(t, h)
	ctx := h.Context()
	setOrgPolicy(t, h, orgPolicyClients(t, h)["grpc"], "projects/"+project, "gcp.resourceLocations", opValues("in:us-locations"))
	err := bq.Dataset("op_eu").Create(ctx, &bigquery.DatasetMetadata{Location: "europe-west1"})
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 412 ||
		ge.Message != "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/"+project+"." {
		t.Errorf("a europe-west1 dataset = %v, want 412", err)
	}
	ds := bq.Dataset("op_us")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("a US dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.Delete(context.Background()) })
}
