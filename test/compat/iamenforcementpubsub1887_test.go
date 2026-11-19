//go:build compat

package compat

import (
	"errors"
	"net/http"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	pubsubrest "google.golang.org/api/pubsub/v1"
)

// wantRESTDenied checks a REST client's err is 403 PERMISSION_DENIED with
// Pub/Sub's message.
func wantRESTDenied(t *testing.T, what string, err error) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusForbidden || ge.Message != pubsubDenied {
		t.Fatalf("%s = %v; want 403 %q", what, err, pubsubDenied)
	}
}

// covers: google.pubsub.v1.Publisher/DetachSubscription, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/ValidateSchema, google.pubsub.v1.SchemaService/DeleteSchema
//
// TestIAMEnforcementPubSubRESTAndSchemas (#1887): over the REST transport,
// with the official google.golang.org/api/pubsub/v1 client and a minted
// token, Publish without a role is 403 PERMISSION_DENIED "User not
// authorized to perform this action.", the publisher role on the topic
// lets it through, testIamPermissions answers only what is held, and the
// subscription's getIamPolicy is refused; DetachSubscription (the official
// v2 client) needs pubsub.topics.detachSubscription on the subscription's
// topic; the schema service's CreateSchema and ValidateSchema need their
// permissions on the project, GetSchema and DeleteSchema on the schema.
func TestIAMEnforcementPubSubRESTAndSchemas(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvPubSub)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iepr-")
	owner, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	topic := "projects/" + project + "/topics/rest"
	sub := "projects/" + project + "/subscriptions/rest"
	if _, err := owner.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() { _ = owner.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	if _, err := owner.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	ownerIAM := pubsubIAM(t, endpoint, "")
	p := newIAMPrincipal(t, h, project, "iepr")
	me := "serviceAccount:" + p.email
	hc := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}
	t.Cleanup(hc.CloseIdleConnections)
	rs, err := pubsubrest.NewService(ctx, option.WithEndpoint("http://"+stripScheme(endpoint)+"/"), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	publish := func() error {
		_, err := rs.Projects.Topics.Publish(topic, &pubsubrest.PublishRequest{Messages: []*pubsubrest.PubsubMessage{{Data: "bQ=="}}}).Context(ctx).Do()
		return err
	}
	wantRESTDenied(t, "REST publish without a role", publish())
	undo := bindOn(t, ownerIAM.GetIamPolicy, ownerIAM.SetIamPolicy, topic, &iampb.Binding{Role: "roles/pubsub.publisher", Members: []string{me}})
	waitGranted(t, "REST publish as a publisher", publish)
	held, err := rs.Projects.Topics.TestIamPermissions(topic, &pubsubrest.TestIamPermissionsRequest{
		Permissions: []string{"pubsub.topics.publish", "pubsub.topics.delete"}}).Context(ctx).Do()
	if err != nil || len(held.Permissions) != 1 || held.Permissions[0] != "pubsub.topics.publish" {
		t.Errorf("REST testIamPermissions = %v, %v; want only pubsub.topics.publish", held, err)
	}
	undo()
	wantRESTDenied(t, "REST publish after the revoke", publish())
	_, err = rs.Projects.Subscriptions.GetIamPolicy(sub).Context(ctx).Do()
	wantRESTDenied(t, "REST getIamPolicy on the subscription", err)
	_, err = rs.Projects.Subscriptions.Get(sub).Context(ctx).Do()
	wantRESTDenied(t, "REST subscriptions.get", err)

	// Schemas.
	sc, err := vkit.NewSchemaClient(ctx, grpcAs(stripScheme(endpoint), p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	schema := &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avroSchemaV1}
	create := func() error {
		_, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "enforced", Schema: schema})
		return err
	}
	wantPubSubDenied(t, "CreateSchema without a role", create())
	_, err = sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: "projects/" + project, Schema: schema})
	wantPubSubDenied(t, "ValidateSchema without a role", err)
	p.grant("roles/pubsub.editor")
	waitGranted(t, "CreateSchema as an editor", create)
	name := "projects/" + project + "/schemas/enforced"
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); err != nil {
		t.Errorf("GetSchema as an editor: %v", err)
	}
	// DetachSubscription, on the subscription's topic.
	as, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	p.revoke("roles/pubsub.editor")
	if err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err == nil {
		t.Fatal("DeleteSchema without a role succeeded")
	} else {
		wantPubSubDenied(t, "DeleteSchema without a role", err)
	}
	_, err = as.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sub})
	wantPubSubDenied(t, "DetachSubscription without a role", err)
	undo = bindOn(t, ownerIAM.GetIamPolicy, ownerIAM.SetIamPolicy, topic, &iampb.Binding{Role: "roles/pubsub.editor", Members: []string{me}})
	defer undo()
	waitGranted(t, "DetachSubscription as an editor on the topic", func() error {
		_, err := as.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sub})
		return err
	})
	ownerSC, err := vkit.NewSchemaClient(ctx, grpcAs(stripScheme(endpoint), "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerSC.Close() })
	if err := ownerSC.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err != nil {
		t.Errorf("DeleteSchema as the instance's account: %v", err)
	}
}
