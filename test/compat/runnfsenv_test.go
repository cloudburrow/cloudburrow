//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	lroauto "cloud.google.com/go/longrunning/autogen"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nfsServerImage is the NFS server Kubernetes' own e2e volume tests run
// (kubernetes/kubernetes test/images/volume/nfs), pinned by digest: kernel
// nfsd in a privileged pod, exporting /exports read-write as the NFSv4
// root, which a client mounts as "/".
const nfsServerImage = "registry.k8s.io/e2e-test-images/volume/nfs@sha256:c3e380bb5a92095288b4dd586a250c560612afbabf53fb2a057e99fff75f038f"

// nfsNamespace holds the test's NFS server, apart from the adapter's objects.
const nfsNamespace = "compat-nfs"

// runNFSServer starts an NFS server in the instance's cluster and returns
// its address, which the node's kubelet mounts: an instance has no
// Filestore, so a Cloud Run NFS volume names any server the node reaches.
func runNFSServer(t *testing.T) (server string, kubectl func(args ...string) (string, error)) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set: the NFS server runs in the instance's cluster", envKubeconfig)
	}
	kubectl = func(args ...string) (string, error) {
		raw, err := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", nfsNamespace}, args...)...).CombinedOutput()
		if err != nil {
			return string(raw), fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, raw)
		}
		return string(raw), nil
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Pod
metadata:
  name: nfs
  namespace: %[1]s
spec:
  containers:
    - name: nfs
      image: %[2]s
      args: ["/exports"]
      securityContext:
        privileged: true
      ports:
        - containerPort: 2049
`, nfsNamespace, nfsServerImage)
	apply := exec.Command("kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if raw, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("start the NFS server: %v\n%s", err, raw)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--kubeconfig", kubeconfig, "delete", "namespace", nfsNamespace, "--wait=false").Run()
	})
	if _, err := kubectl("wait", "--for=condition=Ready", "pod/nfs", "--timeout=180s"); err != nil {
		t.Fatal(err)
	}
	ip, err := kubectl("get", "pod", "nfs", "-o", "jsonpath={.status.podIP}")
	if err != nil || ip == "" {
		t.Fatalf("NFS server address: %q, %v", ip, err)
	}
	// The server logs "NFS started" once nfsd is serving.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		if out, err := kubectl("logs", "nfs"); err == nil && strings.Contains(out, "NFS started") {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("nfsd did not start: %s %v", out, err)
		}
	}
	return ip, kubectl
}

func nfsVolume(name, server, path string, readOnly bool) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: server, Path: path, ReadOnly: readOnly}}}
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob, google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunNFSVolumesAndCloudRunEnv (#1448), with the official run/apiv2
// clients and an NFS server in the instance's cluster:
//
//   - a job's task mounts the export read-write and writes into it the
//     Cloud Run variables it sees, CLOUD_RUN_TASK_ATTEMPT 0 on its first
//     attempt among them;
//   - a service mounts the same export read-write and a second time
//     read-only, with no path (Cloud Run's default, the server's root), reads what the job wrote, writes a file the NFS server
//     then holds, is refused a write to the read-only mount, and serves its
//     K_SERVICE, K_CONFIGURATION and K_REVISION, which are the service ID
//     and the revision ID the API reports, not the Knative names;
//   - the volumes read back as set, and an NFS volume with no server is
//     refused INVALID_ARGUMENT.
func TestRunNFSVolumesAndCloudRunEnv(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	server, kubectl := runNFSServer(t)
	jc, _ := runJobClients(t, h)
	c := runClient(t, h)
	ctx := h.Context()

	jobID := "compat-nfs-job"
	jobName := runParent(h) + "/jobs/" + jobID
	job := shellJob(`echo "$CLOUD_RUN_JOB $CLOUD_RUN_EXECUTION $CLOUD_RUN_TASK_INDEX $CLOUD_RUN_TASK_ATTEMPT $CLOUD_RUN_TASK_COUNT" > /mnt/share/job-env`, 1, nil)
	job.Template.Template.Volumes = []*runpb.Volume{nfsVolume("share", server, "/", false)}
	job.Template.Template.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "share", MountPath: "/mnt/share"}}
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: jobID, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: jobName}) })
	if _, err := jop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	ex, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("the task did not write to its NFS volume: %v", err)
	}
	execID := ex.GetName()[strings.LastIndex(ex.GetName(), "/")+1:]
	if out, err := kubectl("exec", "nfs", "--", "cat", "/exports/job-env"); err != nil ||
		strings.TrimSpace(out) != jobID+" "+execID+" 0 0 1" {
		t.Errorf("the job wrote %q (%v) to the export; want %q", out, err, jobID+" "+execID+" 0 0 1")
	}

	id := "compat-nfs-svc"
	name := runParent(h) + "/services/" + id
	script := `mkdir -p /www && echo from-service > /mnt/share/from-service &&
if echo x > /mnt/ro/refused 2>/dev/null; then ro=writable; else ro=read-only; fi &&
echo "$K_SERVICE $K_CONFIGURATION $K_REVISION $ro $(cat /mnt/ro/job-env)" > /www/index.html &&
exec httpd -f -p 8080 -h /www`
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Volumes: []*runpb.Volume{nfsVolume("share", server, "/", false), nfsVolume("ro", server, "", true)},
			Containers: []*runpb.Container{{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{script},
				VolumeMounts: []*runpb.VolumeMount{{Name: "share", MountPath: "/mnt/share"}, {Name: "ro", MountPath: "/mnt/ro"}}}},
		}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	rev := svc.GetLatestReadyRevision()[strings.LastIndex(svc.GetLatestReadyRevision(), "/")+1:]
	if !strings.HasPrefix(rev, id+"-00001-") {
		t.Errorf("latest ready revision = %q; want Cloud Run's form %s-00001-xxx", rev, id)
	}
	want := fmt.Sprintf("%s %s %s read-only %s %s 0 0 1", id, id, rev, jobID, execID)
	base, host := ingress(t), hostOf(t, svc.GetUri())
	var code int
	var body string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if code, body = httpGet(t, base, host, "/"); code == http.StatusOK {
			break
		}
	}
	if code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Errorf("the service served %d %q; want %q", code, body, want)
	}
	if out, err := kubectl("exec", "nfs", "--", "cat", "/exports/from-service"); err != nil || strings.TrimSpace(out) != "from-service" {
		t.Errorf("the export holds %q (%v); want what the service wrote", out, err)
	}
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if v := got.GetTemplate().GetVolumes(); len(v) != 2 || v[0].GetNfs().GetServer() != server ||
		v[0].GetNfs().GetPath() != "/" || !v[1].GetNfs().GetReadOnly() {
		t.Errorf("volumes read back as %v", v)
	}

	_, err = c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: "compat-nfs-noserver",
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Volumes:    []*runpb.Volume{nfsVolume("share", "", "/exports", false)},
			Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "nfs.server") {
		t.Errorf("an NFS volume with no server = %v; want INVALID_ARGUMENT", err)
	}
}

// TestRunCancelOperationAsGoogle (#1448): Cloud Run's Operations mixin
// (run_v2.yaml) binds get, list, wait and delete, and no cancel, so the
// official run clients have no CancelOperation. Through Google's
// longrunning OperationsClient on the Cloud Run port it is UNIMPLEMENTED,
// as google.longrunning documents for a server without the method, and the
// operation it named is left as it was; REST's :cancel is a path Google does
// not bind, 404. It names no covers: line, as CancelOperation is not a
// method of Cloud Run's surface.
func TestRunCancelOperationAsGoogle(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	jc, _ := runJobClients(t, h)
	oc, err := lroauto.NewOperationsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("NewOperationsClient: %v", err)
	}
	t.Cleanup(func() { _ = oc.Close() })
	id := "compat-cancel-op"
	name := runParent(h) + "/jobs/" + id
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: shellJob("true", 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if err := oc.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name()}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CancelOperation = %v; want UNIMPLEMENTED", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Errorf("the operation after a refused cancel: %v", err)
	}
	resp, err := http.Post("http://"+h.Endpoint(EnvRun)+"/v2/"+op.Name()+":cancel", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST :cancel = %d; want 404, a path Cloud Run does not bind", resp.StatusCode)
	}
}
