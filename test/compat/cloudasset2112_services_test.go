//go:build compat

package compat

import (
	"context"
	"slices"
	"testing"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
)

// TestCloudAssetAnalyzeIamPolicyServiceResources (#2112): under a scope,
// the analysis enumerates the other services' resources that keep allow
// policies, by Cloud Asset Inventory's full resource names: a binding on a
// Pub/Sub topic and one on a Cloud Run service are found from the folder
// with no resourceSelector, a resourceSelector on the topic reads its own
// policy, and expandResources lists the project's topic and service as its
// children.
func TestCloudAssetAnalyzeIamPolicyServiceResources(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	psEndpoint := h.Endpoint(EnvPubSub)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	f := newAssetFixture(t, h, rmEndpoint, "asr-")

	t.Setenv("PUBSUB_EMULATOR_HOST", psEndpoint)
	ps, err := pubsub.NewClient(ctx, f.project)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	topicName := "projects/" + f.project + "/topics/asset-topic"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})
	t.Cleanup(bindOn(t, ps.TopicAdminClient.GetIamPolicy, ps.TopicAdminClient.SetIamPolicy, topicName,
		&iampb.Binding{Role: "roles/pubsub.publisher", Members: []string{"user:" + f.member}}))

	rc, err := run.NewServicesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewServicesClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	parent := "projects/" + f.project + "/locations/us-central1"
	svcName := parent + "/services/asset-svc"
	if _, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "asset-svc",
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: helloDigest}}}}}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: svcName}) })
	t.Cleanup(bindOn(t, rc.GetIamPolicy, rc.SetIamPolicy, svcName,
		&iampb.Binding{Role: "roles/run.invoker", Members: []string{"user:" + f.member}}))

	c := assetClient(t, h, rmEndpoint)
	topicFull, svcFull := "//pubsub.googleapis.com/"+topicName, "//run.googleapis.com/"+svcName

	// Scope-wide, no resourceSelector: both bindings are found.
	resp, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
		IdentitySelector: &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: "user:" + f.member}}})
	if err != nil {
		t.Fatalf("AnalyzeIamPolicy: %v", err)
	}
	if r := assetResult(resp, "roles/pubsub.publisher"); r == nil || r.GetAttachedResourceFullName() != topicFull {
		t.Errorf("the topic's binding = %v; want it attached to %s", r, topicFull)
	}
	if r := assetResult(resp, "roles/run.invoker"); r == nil || r.GetAttachedResourceFullName() != svcFull {
		t.Errorf("the service's binding = %v; want it attached to %s", r, svcFull)
	}

	// A resourceSelector on the topic reads the topic's own policy.
	resp, err = c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
		ResourceSelector: &assetpb.IamPolicyAnalysisQuery_ResourceSelector{FullResourceName: topicFull},
		AccessSelector:   &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: []string{"pubsub.topics.publish"}}}})
	if err != nil {
		t.Fatalf("AnalyzeIamPolicy on the topic: %v", err)
	}
	if r := assetResult(resp, "roles/pubsub.publisher"); r == nil || r.GetAttachedResourceFullName() != topicFull {
		t.Errorf("resourceSelector on the topic: binding = %v", r)
	}

	// expandResources: the project's binding reaches its topic and service.
	resp, err = c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
		AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}},
		Options:        &assetpb.IamPolicyAnalysisQuery_Options{ExpandResources: true, OutputResourceEdges: true}}})
	if err != nil {
		t.Fatalf("AnalyzeIamPolicy expandResources: %v", err)
	}
	r := assetResult(resp, "roles/viewer")
	if r == nil {
		t.Fatalf("no roles/viewer result: %v", resp.GetMainAnalysis())
	}
	var names []string
	edges := map[string]string{}
	for _, acl := range r.GetAccessControlLists() {
		for _, res := range acl.GetResources() {
			names = append(names, res.GetFullResourceName())
		}
		for _, e := range acl.GetResourceEdges() {
			edges[e.GetTargetNode()] = e.GetSourceNode()
		}
	}
	projectFull := "//cloudresourcemanager.googleapis.com/projects/" + f.project
	for _, want := range []string{topicFull, svcFull} {
		if !slices.Contains(names, want) {
			t.Errorf("expandResources lists %v; want %s among them", names, want)
		}
		if edges[want] != projectFull {
			t.Errorf("edge to %s from %q; want %s", want, edges[want], projectFull)
		}
	}
}
