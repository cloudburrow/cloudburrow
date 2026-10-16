//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runStoredSettings sets, on a template, the settings Google documents and
// nothing here acts on (#1346).
func runStoredSettings(h *Harness) (*runpb.VpcAccess, string) {
	return &runpb.VpcAccess{Connector: "projects/" + h.Project() + "/locations/us-central1/connectors/compat",
			Egress: runpb.VpcAccess_ALL_TRAFFIC},
		"projects/" + h.Project() + "/locations/us-central1/keyRings/compat/cryptoKeys/run"
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Revisions/GetRevision
//
// TestRunServiceStoredSettingsAndSubPath (#1346): a service with VPC access
// and its egress, an encryption key, the second-generation execution
// environment, session affinity and binary authorization is accepted and
// becomes ready, and the service and its revision return each as set. Its
// in-memory volume is mounted twice, once whole and once at a subPath: the
// Knative revision carries the subPath, and both mounts read back.
func TestRunServiceStoredSettingsAndSubPath(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	vpc, key := runStoredSettings(h)
	id := "compat-settings"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		BinaryAuthorization: &runpb.BinaryAuthorization{BinauthzMethod: &runpb.BinaryAuthorization_UseDefault{UseDefault: true}},
		Template: &runpb.RevisionTemplate{
			VpcAccess: vpc, EncryptionKey: key, SessionAffinity: true,
			ExecutionEnvironment: runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2,
			Volumes:              []*runpb.Volume{memoryVolume("cache", "16Mi")},
			Containers: []*runpb.Container{{Image: helloDigest,
				VolumeMounts: []*runpb.VolumeMount{{Name: "cache", MountPath: "/cache"},
					{Name: "cache", MountPath: "/app-cache", SubPath: "app"}}}},
		}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	gt := got.GetTemplate()
	if gt.GetVpcAccess().GetConnector() != vpc.Connector || gt.GetVpcAccess().GetEgress() != runpb.VpcAccess_ALL_TRAFFIC ||
		gt.GetEncryptionKey() != key || !gt.GetSessionAffinity() ||
		gt.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		!got.GetBinaryAuthorization().GetUseDefault() {
		t.Errorf("the service reads back as %v", got)
	}
	if m := gt.GetContainers()[0].GetVolumeMounts(); len(m) != 2 || m[1].GetSubPath() != "app" {
		t.Errorf("mounts read back as %v", m)
	}
	rc, err := run.NewRevisionsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewRevisionsClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	revName := got.GetLatestReadyRevision()
	if !strings.HasPrefix(revName, "projects/") {
		revName = name + "/revisions/" + revName
	}
	rev, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: revName})
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if rev.GetVpcAccess().GetConnector() != vpc.Connector || rev.GetEncryptionKey() != key || !rev.GetSessionAffinity() ||
		rev.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 {
		t.Errorf("the revision reads back as %v", rev)
	}
	if os.Getenv(envKubeconfig) != "" {
		if y, err := kubectlGet(t, "ksvc", id); err != nil {
			t.Error(err)
		} else if !strings.Contains(y, "subPath: app") {
			t.Errorf("the Knative Service should carry the subPath:\n%s", y)
		}
	}
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/RunJob
//
// TestRunJobSubPathAndStoredSettings (#1346): a job's in-memory volume is
// mounted whole at /data and at /sub with subPath a/b. The task writes
// through /sub and exits 0 only when the file is at /data/a/b, so the
// execution succeeding is the proof. VPC access, the encryption key, the
// execution environment and binary authorization read back as set. A
// subPath that leaves the volume is InvalidArgument; a Cloud Storage FUSE
// volume is still refused by name (NFS volumes are served since #1448:
// runnfsenv_test.go).
func TestRunJobSubPathAndStoredSettings(t *testing.T) {
	h := New(t)
	jc, _ := runJobClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	vpc, key := runStoredSettings(h)
	id := "compat-job-subpath"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`echo ok > /sub/x && [ "$(cat /data/a/b/x)" = ok ] || { echo "the subPath is not a/b of the volume"; exit 1; }`, 1, nil)
	job.BinaryAuthorization = &runpb.BinaryAuthorization{BinauthzMethod: &runpb.BinaryAuthorization_UseDefault{UseDefault: true}}
	tt := job.Template.Template
	tt.VpcAccess, tt.EncryptionKey = vpc, key
	tt.ExecutionEnvironment = runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2
	tt.Volumes = []*runpb.Volume{memoryVolume("scratch", "8Mi")}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "scratch", MountPath: "/data"},
		{Name: "scratch", MountPath: "/sub", SubPath: "a/b"}}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	gt := got.GetTemplate().GetTemplate()
	if gt.GetVpcAccess().GetEgress() != runpb.VpcAccess_ALL_TRAFFIC || gt.GetEncryptionKey() != key ||
		gt.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		!got.GetBinaryAuthorization().GetUseDefault() || gt.GetContainers()[0].GetVolumeMounts()[1].GetSubPath() != "a/b" {
		t.Errorf("the job reads back as %v", got)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the task did not see the subPath: %v", err)
	}

	bad := shellJob("true", 1, nil)
	bad.Template.Template.Volumes = []*runpb.Volume{memoryVolume("v", "")}
	bad.Template.Template.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "v", MountPath: "/v", SubPath: "../up"}}
	if _, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: "compat-job-badsub", Job: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("subPath ../up = %v; want InvalidArgument", err)
	}
	// Cloud Storage (#1474) and NFS (#1448) volumes are served since; their
	// tests are TestRunCloudStorageFUSEVolumes and TestRunNFSVolumesAndCloudRunEnv.
}

// TestConsoleRunStoredSettings (#1346), in the run shard: a service deployed
// on the console with the second-generation environment, a VPC connector
// with all-traffic egress, an encryption key and session affinity reads
// back with each through the official ServicesClient, and its edit form is
// prefilled with them; a job created on the console stores the same.
func TestConsoleRunStoredSettings(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	jc, _ := runJobClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	vpc, key := runStoredSettings(h)
	id := "console-settings"
	name := runParent(h) + "/services/" + id
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = c.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	settings := map[string]string{"executionEnvironment": "Second generation", "vpcConnector": vpc.Connector,
		"vpcEgress": "All traffic", "encryptionKey": key}
	form := map[string]string{"name": id, "image": helloDigest, "sessionAffinity": "true"}
	for k, v := range settings {
		form[k] = v
	}
	if code, body := consoleCreate(t, addr, "run", project, form); code != http.StatusOK {
		t.Fatalf("console deploy = %d: %s", code, body)
	}
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if gt := svc.GetTemplate(); gt.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		gt.GetVpcAccess().GetConnector() != vpc.Connector || gt.GetVpcAccess().GetEgress() != runpb.VpcAccess_ALL_TRAFFIC ||
		gt.GetEncryptionKey() != key || !gt.GetSessionAffinity() {
		t.Errorf("the console's service reads %v", gt)
	}
	_, values, _ := consoleRunEditForm(t, addr, project, id)
	for k, v := range settings {
		if values[k] != v {
			t.Errorf("the edit form's %s is %q, want %q", k, values[k], v)
		}
	}
	if values["sessionAffinity"] != "true" {
		t.Errorf("the edit form's sessionAffinity is %q", values["sessionAffinity"])
	}

	jobID := "console-job-settings"
	jobName := runParent(h) + "/jobs/" + jobID
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: jobName}) })
	jform := map[string]string{"name": jobID, "image": jobImage, "command": "true", "taskCount": "1", "maxRetries": "0"}
	for k, v := range settings {
		jform[k] = v
	}
	if code, body := consoleCreate(t, addr, "run-jobs", project, jform); code != http.StatusOK {
		t.Fatalf("console create job = %d: %s", code, body)
	}
	job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: jobName})
	if err != nil {
		t.Fatal(err)
	}
	if tt := job.GetTemplate().GetTemplate(); tt.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		tt.GetVpcAccess().GetEgress() != runpb.VpcAccess_ALL_TRAFFIC || tt.GetEncryptionKey() != key {
		t.Errorf("the console's job reads %v", tt)
	}
}
