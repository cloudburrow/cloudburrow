//go:build compat

package compat

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// TestConsolePermissionsOnARunServiceAndJob (#1216), in the shards that
// serve Cloud Run: the Permissions tab on a service's page and a job's,
// through the official ServicesClient's and JobsClient's IAM methods.
func TestConsolePermissionsOnARunServiceAndJob(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := consoleAddr(t, h)
	sc := runClient(t, h)
	jc, _ := runJobClients(t, h)

	svcID := "console-perms-svc"
	svc := runParent(h) + "/services/" + svcID
	op, err := sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: svcID,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = sc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: svc}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	consolePermissionsRoundTrip(t, h, addr, "run", []string{svcID}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return sc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svc})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := sc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: svc, Policy: p})
			return err
		},
	}, "roles/run.invoker")

	jobID := "console-perms-job"
	job := runParent(h) + "/jobs/" + jobID
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: jobID, Job: shellJob("true", 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: job}) })
	if _, err := jop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	consolePermissionsRoundTrip(t, h, addr, "run-jobs", []string{jobID}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return jc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := jc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job, Policy: p})
			return err
		},
	}, "roles/run.developer")
}
