//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob, google.cloud.run.v2.Services/CreateService
//
// TestRunMountsASecretUnderACustomerManagedKey (#1489), through
// cloud.google.com/go/run/apiv2, cloud.google.com/go/secretmanager/apiv1 and
// cloud.google.com/go/kms/apiv1: a job's task reads a version stored under a
// customer-managed key (#1453) as a secretKeyRef environment variable and as
// a secret volume file, and exits 0 only when both hold the plaintext. With
// the key version disabled the secret cannot be accessed, so, as Cloud Run
// documents for a secret it cannot access, RunJob and a service's
// CreateService are FAILED_PRECONDITION naming the key; once it is enabled
// the job runs again.
func TestRunMountsASecretUnderACustomerManagedKey(t *testing.T) {
	h := New(t)
	kc := kmsClients(t, h)["grpc"]
	sc := secretsClient(t, h)
	jc, _ := runJobClients(t, h)
	rc := runClient(t, h)
	ctx := h.Context()

	key := cmekKeys(t, h, kc, "global")["global/k"]
	const secretID, payload = "compat-run-cmek", "cmek-plain-value"
	secretName := secretsParent(h) + "/secrets/" + secretID
	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: cmekAutomatic(key)}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secretName})
	})
	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	// The key version is left enabled whatever the test does.
	t.Cleanup(func() { setKeyVersionState(t, h, kc, key, kmspb.CryptoKeyVersion_ENABLED) })

	secretEnv := &runpb.EnvVar{Name: "SECRET_ENV", Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
		SecretKeyRef: &runpb.SecretKeySelector{Secret: secretID, Version: "latest"}}}}

	id := "compat-run-cmek"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`[ "$SECRET_ENV" = "cmek-plain-$(echo value)" ] || { echo "env is not the plaintext"; exit 1; }
[ "$(cat /secrets/token)" = "cmek-plain-$(echo value)" ] || { echo "file is not the plaintext"; exit 1; }`, 1, nil)
	tt := job.Template.Template
	tt.Containers[0].Env = []*runpb.EnvVar{secretEnv}
	tt.Volumes = []*runpb.Volume{secretVolume("creds", secretID, "token", "1")}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "creds", MountPath: "/secrets"}}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	run := func() error {
		rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
		if err != nil {
			return err
		}
		_, err = rop.Wait(ctx)
		return err
	}
	if err := run(); err != nil {
		t.Fatalf("the task did not read the plaintext: %v", err)
	}

	setKeyVersionState(t, h, kc, key, kmspb.CryptoKeyVersion_DISABLED)
	refused := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), key) {
			t.Errorf("%s with the key disabled = %v; want FAILED_PRECONDITION naming the key", what, err)
		}
	}
	refused("RunJob", run())
	svcID := "compat-run-cmek-svc"
	_, err = rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: svcID,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: helloDigest, Env: []*runpb.EnvVar{secretEnv}}}}}})
	t.Cleanup(func() {
		_, _ = rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: runParent(h) + "/services/" + svcID})
	})
	refused("CreateService", err)

	setKeyVersionState(t, h, kc, key, kmspb.CryptoKeyVersion_ENABLED)
	if err := run(); err != nil {
		t.Errorf("RunJob after the key is enabled again: %v", err)
	}
}
