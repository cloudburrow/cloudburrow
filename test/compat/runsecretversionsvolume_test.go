//go:build compat

package compat

import (
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob
//
// TestRunJobMountsTwoVersionsInOneVolume (#1596): each Secret Manager
// version is a Kubernetes Secret of its own, so a secret volume naming two
// versions reads two Kubernetes Secrets, and is rendered as a projected
// volume. The task exits 0 only when both files hold their versions'
// payloads.
func TestRunJobMountsTwoVersionsInOneVolume(t *testing.T) {
	h := New(t)
	jc, _ := runJobClients(t, h)
	ctx := h.Context()
	const secretID = "compat-job-two-versions"
	runVolumeSecret(t, h, secretID, "first-value")
	sc := secretsClient(t, h)
	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretsParent(h) + "/secrets/" + secretID,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("second-value")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}

	id := "compat-job-two-versions"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`[ "$(cat /secrets/old)" = "first-$(echo value)" ] || { echo "no first version"; exit 1; }
[ "$(cat /secrets/new)" = "second-$(echo value)" ] || { echo "no second version"; exit 1; }`, 1, nil)
	tt := job.Template.Template
	tt.Volumes = []*runpb.Volume{{Name: "creds", VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: secretID,
		Items: []*runpb.VersionToPath{{Path: "old", Version: "1"}, {Path: "new", Version: "latest"}}}}}}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "creds", MountPath: "/secrets"}}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the task did not see both versions: %v", err)
	}
}
