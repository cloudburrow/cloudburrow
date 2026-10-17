//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runVolumeSecret creates a Secret Manager secret with one version.
func runVolumeSecret(t *testing.T, h *Harness, id, payload string) {
	t.Helper()
	sc := secretsClient(t, h)
	ctx := h.Context()
	name := secretsParent(h) + "/secrets/" + id
	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
}

func secretVolume(name, secret, path, version string) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: secret,
		Items: []*runpb.VersionToPath{{Path: path, Version: version}}}}}
}

func memoryVolume(name, size string) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{
		Medium: runpb.EmptyDirVolumeSource_MEMORY, SizeLimit: size}}}
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/RunJob
//
// TestRunJobMountsSecretAndMemoryVolumes (#1297): a job's Secret Manager
// volume is a file holding the version's payload, and its in-memory emptyDir
// is a writable tmpfs. The task exits 0 only when both hold, so the
// execution succeeding is the proof. The service account, volumes and mounts
// read back as set; a Cloud SQL volume naming no instance is refused.
func TestRunJobMountsSecretAndMemoryVolumes(t *testing.T) {
	h := New(t)
	jc, _ := runJobClients(t, h)
	ctx := h.Context()
	const secretID = "compat-job-volume"
	runVolumeSecret(t, h, secretID, "volume-secret-value")

	id := "compat-job-volumes"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`[ "$(cat /secrets/token)" = "volume-secret-$(echo value)" ] || { echo "no secret file"; exit 1; }
grep -q ' /scratch tmpfs ' /proc/mounts || { echo "/scratch is not in memory"; exit 1; }
echo ok > /scratch/x && [ "$(cat /scratch/x)" = ok ]`, 1, nil)
	tt := job.Template.Template
	tt.ServiceAccount = "runner@" + h.Project() + ".iam.gserviceaccount.com"
	tt.Volumes = []*runpb.Volume{secretVolume("creds", secretID, "token", "latest"), memoryVolume("scratch", "16Mi")}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "creds", MountPath: "/secrets"}, {Name: "scratch", MountPath: "/scratch"}}
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
	if gt.GetServiceAccount() != tt.ServiceAccount || len(gt.GetVolumes()) != 2 ||
		gt.GetVolumes()[1].GetEmptyDir().GetMedium() != runpb.EmptyDirVolumeSource_MEMORY ||
		len(gt.GetContainers()[0].GetVolumeMounts()) != 2 {
		t.Errorf("job template reads back as %v", gt)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the task did not see its volumes: %v", err)
	}

	sql := shellJob("true", 1, nil)
	sql.Template.Template.Volumes = []*runpb.Volume{{Name: "db", VolumeType: &runpb.Volume_CloudSqlInstance{
		CloudSqlInstance: &runpb.CloudSqlInstance{Instances: []string{h.Project() + ":us-central1:db"}}}}}
	_, err = jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: "compat-job-sql", Job: sql})
	// Cloud SQL volumes are served (#1346, runcloudsql_test.go); one naming
	// an instance that does not exist, or with Cloud SQL off, is refused.
	if c := status.Code(err); (c != codes.InvalidArgument && c != codes.FailedPrecondition) || !strings.Contains(err.Error(), "Cloud SQL") {
		t.Errorf("Cloud SQL volume naming no instance = %v; want it refused", err)
	}
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceVolumesAndServiceAccount (#1297): a service with a secret
// volume, an in-memory volume, a service account and a tcp startup probe
// becomes ready; the revision mounts the Kubernetes Secret read-only and
// never carries the payload; everything reads back as set.
func TestRunServiceVolumesAndServiceAccount(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx := h.Context()
	const secretID = "compat-svc-volume"
	runVolumeSecret(t, h, secretID, "service-volume-payload")
	id := "compat-volumes"
	name := runParent(h) + "/services/" + id
	sa := "web@" + h.Project() + ".iam.gserviceaccount.com"
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			ServiceAccount: sa,
			Volumes:        []*runpb.Volume{secretVolume("creds", secretID, "token", "1"), memoryVolume("cache", "32Mi")},
			Containers: []*runpb.Container{{Image: helloDigest,
				StartupProbe: &runpb.Probe{ProbeType: &runpb.Probe_TcpSocket{TcpSocket: &runpb.TCPSocketAction{Port: 8080}},
					TimeoutSeconds: 1, PeriodSeconds: 1, FailureThreshold: 60},
				VolumeMounts: []*runpb.VolumeMount{{Name: "creds", MountPath: "/secrets"}, {Name: "cache", MountPath: "/cache"}}}},
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
	if gt.GetServiceAccount() != sa || len(gt.GetVolumes()) != 2 || gt.GetVolumes()[0].GetSecret().GetSecret() != secretID ||
		gt.GetVolumes()[1].GetEmptyDir().GetSizeLimit() != "32Mi" || len(gt.GetContainers()[0].GetVolumeMounts()) != 2 ||
		gt.GetContainers()[0].GetStartupProbe().GetTcpSocket() == nil {
		t.Errorf("service template reads back as %v", gt)
	}
	if os.Getenv(envKubeconfig) != "" {
		if y, err := kubectlGet(t, "ksvc", id); err != nil {
			t.Error(err)
		} else if strings.Contains(y, "service-volume-payload") || !strings.Contains(y, "secretName:") ||
			!strings.Contains(y, "medium: Memory") || !strings.Contains(y, "readOnly: true") {
			t.Errorf("the Knative Service should mount the secret, not carry its payload:\n%s", y)
		}
	}
}

// covers: run.namespaces.services.create, run.namespaces.services.get
//
// TestRunV1VolumesProbesAndServiceAccount (#1297): through the official v1
// client, a tcpSocket startup probe (the shape Cloud Run defaults, with a
// short period so a fresh kubelet's random first-probe delay stays short),
// an httpGet liveness probe, a secret volume, an in-memory volume and serviceAccountName are
// accepted, the service becomes ready, and each reads back as set.
func TestRunV1VolumesProbesAndServiceAccount(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h)
	const secretID = "compat-v1-volume"
	runVolumeSecret(t, h, secretID, "v1-volume-payload")
	ns := "namespaces/" + h.Project()
	id := "v1-volumes"
	name := ns + "/services/" + id
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete(name).Do() })
	sa := "v1@" + h.Project() + ".iam.gserviceaccount.com"
	svc := &runv1.Service{ApiVersion: "serving.knative.dev/v1", Kind: "Service",
		Metadata: &runv1.ObjectMeta{Name: id, Namespace: h.Project(),
			Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
			ServiceAccountName: sa,
			Volumes: []*runv1.Volume{
				{Name: "creds", Secret: &runv1.SecretVolumeSource{SecretName: secretID, Items: []*runv1.KeyToPath{{Key: "latest", Path: "token"}}}},
				{Name: "tmp", EmptyDir: &runv1.EmptyDirVolumeSource{Medium: "Memory", SizeLimit: "16Mi"}}},
			Containers: []*runv1.Container{{Image: helloDigest,
				Ports:         []*runv1.ContainerPort{{Name: "http1", ContainerPort: 8080}},
				StartupProbe:  &runv1.Probe{TcpSocket: &runv1.TCPSocketAction{Port: 8080}, TimeoutSeconds: 1, PeriodSeconds: 1, FailureThreshold: 60},
				LivenessProbe: &runv1.Probe{HttpGet: &runv1.HTTPGetAction{Path: "/"}, PeriodSeconds: 10},
				VolumeMounts: []*runv1.VolumeMount{{Name: "creds", MountPath: "/secrets", ReadOnly: true}, {Name: "tmp", MountPath: "/tmp/mem"},
					{Name: "tmp", MountPath: "/tmp/sub", SubPath: "a/b"}}}}}}}}
	if _, err := c.Namespaces.Services.Create(ns, svc).Do(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := runV1Ready(t, c, name).Spec.Template.Spec
	if got.ServiceAccountName != sa || len(got.Volumes) != 2 || got.Volumes[0].Secret.SecretName != secretID ||
		got.Volumes[1].EmptyDir.Medium != "Memory" || len(got.Containers[0].VolumeMounts) != 3 ||
		got.Containers[0].VolumeMounts[2].SubPath != "a/b" ||
		got.Containers[0].StartupProbe == nil || got.Containers[0].StartupProbe.TcpSocket == nil ||
		got.Containers[0].LivenessProbe == nil || got.Containers[0].LivenessProbe.HttpGet == nil {
		t.Errorf("v1 template reads back as %+v", got)
	}
	// A mount's subPath (#1576) is Kubernetes' subPath on the Knative
	// Service, as on v2 (TestRunServiceStoredSettingsAndSubPath).
	if os.Getenv(envKubeconfig) != "" {
		if y, err := kubectlGet(t, "ksvc", id); err != nil {
			t.Error(err)
		} else if !strings.Contains(y, "subPath: a/b") {
			t.Errorf("the Knative Service should carry the subPath:\n%s", y)
		}
	}
}

// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.replaceService, run.namespaces.services.delete
//
// TestGcloudRunDeployImage (#1297): `gcloud run deploy --image`, through
// the configuration `cloudburrow gcloud-setup` writes. gcloud 587 asks
// Service Usage whether the API is enabled unless told not to, which
// gcloud-setup now does, and annotates the service and revision with
// run.googleapis.com/client-name, client-version and startup-cpu-boost and
// sends an empty sandboxes, all of which were refused before. It deploys,
// waits for Ready, deploys again with a changed variable (a GET and a
// replaceService), is described with the new value, and is deleted.
func TestGcloudRunDeployImage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-deploy"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	g.must("run", "deploy", id, "--image="+helloDigest, region, "--no-allow-unauthenticated",
		"--set-env-vars=TARGET=gcloud", "--quiet")
	g.must("run", "deploy", id, "--image="+helloDigest, region, "--no-allow-unauthenticated",
		"--set-env-vars=TARGET=again", "--quiet")
	got := g.must("run", "services", "describe", id, region,
		"--format=value(status.url,status.latestReadyRevisionName,spec.template.spec.containers[0].env[0].value)")
	if !strings.Contains(got, "http") || !strings.Contains(got, id+"-00002") || !strings.Contains(got, "again") {
		t.Errorf("describe = %q; want a URL, the second revision ready and TARGET=again", got)
	}
	g.must("run", "services", "delete", id, region, "--quiet")
}

// TestConsoleRunVolumesAndServiceAccount (#1297), in the run shard: a
// service deployed on the console with a service account, a file from
// Secret Manager and an in-memory volume is read back by the official
// ServicesClient with all three, and its edit form is prefilled with them;
// an edit that empties the in-memory volumes removes only those. A job
// created on the console with the same fields runs a task that reads the
// file and writes to the tmpfs.
func TestConsoleRunVolumesAndServiceAccount(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	jc, _ := runJobClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	const secretID = "console-volume-secret"
	runVolumeSecret(t, h, secretID, "console-volume-payload")
	sa := "console@" + project + ".iam.gserviceaccount.com"
	files := fmt.Sprintf(`{"/secrets/token":%q}`, secretID+":1")

	id := "console-volumes"
	name := runParent(h) + "/services/" + id
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = c.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	if code, body := consoleCreate(t, addr, "run", project, map[string]string{
		"name": id, "image": helloDigest, "serviceAccount": sa, "secretFiles": files, "memoryVolumes": `{"/cache":"32Mi"}`,
	}); code != http.StatusOK {
		t.Fatalf("console deploy = %d: %s", code, body)
	}
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if svc.GetTemplate().GetServiceAccount() != sa || len(svc.GetTemplate().GetVolumes()) != 2 {
		t.Errorf("the console's service reads %v", svc.GetTemplate())
	}
	_, values, _ := consoleRunEditForm(t, addr, project, id)
	if values["serviceAccount"] != sa || values["secretFiles"] != files || values["memoryVolumes"] != `{"/cache":"32Mi"}` {
		t.Errorf("the edit form is prefilled with %q, %q, %q", values["serviceAccount"], values["secretFiles"], values["memoryVolumes"])
	}
	values["memoryVolumes"] = ""
	if code, body := consoleEdit(t, addr, "run", project, []string{id}, values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	svc, err = c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if v := svc.GetTemplate().GetVolumes(); len(v) != 1 || v[0].GetSecret().GetSecret() != secretID {
		t.Errorf("after emptying the in-memory volumes, the service has %v", v)
	}

	jobID := "console-job-volumes"
	jobName := runParent(h) + "/jobs/" + jobID
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: jobName}) })
	if code, body := consoleCreate(t, addr, "run-jobs", project, map[string]string{
		"name": jobID, "image": jobImage, "command": "sh -c",
		"args":           `'[ "$(cat /secrets/token)" = "console-volume-$(echo payload)" ] && grep -q " /scratch tmpfs " /proc/mounts && touch /scratch/x'`,
		"serviceAccount": sa, "secretFiles": files, "memoryVolumes": `{"/scratch":"8Mi"}`, "taskCount": "1", "maxRetries": "0",
	}); code != http.StatusOK {
		t.Fatalf("console create job = %d: %s", code, body)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the console's job did not see its volumes: %v", err)
	}
}
