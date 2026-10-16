//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// Volume objects are reference-counted (#1476): a gcsfuse pod, and a
// service's PersistentVolume and claim, stay while any workload uses them
// and go with the last one. Checked with kubectl on the instance's own
// cluster, in the adapter's namespace.

// volumeKubectl runs kubectl against the instance's cluster, or skips the
// test when the harness does not name it.
func volumeKubectl(t *testing.T) func(args ...string) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set: the volume objects are checked in the instance's cluster", envKubeconfig)
	}
	return func(args ...string) string {
		raw, err := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "default"}, args...)...).Output()
		if err != nil {
			t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(raw))
	}
}

// fuseMountOf is the name of the gcsfuse pod mounting bucket, or "".
func fuseMountOf(kubectl func(...string) string, bucket string) string {
	out := kubectl("get", "pods", "-l", "cloudburrow.dev/gcsfuse", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.annotations.cloudburrow\.dev/gcs-bucket}{"\n"}{end}`)
	for _, line := range strings.Split(out, "\n") {
		if name, b, ok := strings.Cut(strings.TrimSpace(line), " "); ok && b == bucket {
			return name
		}
	}
	return ""
}

// nfsClaimOf is the name of the PersistentVolume of an NFS export, and of
// its claim, or "".
func nfsClaimOf(kubectl func(...string) string, server string) string {
	out := kubectl("get", "persistentvolumes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{.spec.nfs.server}{"\n"}{end}`)
	for _, line := range strings.Split(out, "\n") {
		if name, s, ok := strings.Cut(strings.TrimSpace(line), " "); ok && s == server {
			return name
		}
	}
	return ""
}

// volumeObjectsPresent lists which of the named objects exist.
func volumeObjectsPresent(kubectl func(...string) string, objects ...string) []string {
	var found []string
	for _, o := range objects {
		if kubectl("get", o, "--ignore-not-found", "-o", "name") != "" {
			found = append(found, o)
		}
	}
	return found
}

// awaitGone waits for objects to be removed: a claim goes once the pods
// that mounted it have stopped.
func awaitGone(t *testing.T, kubectl func(...string) string, objects ...string) {
	t.Helper()
	var left []string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if left = volumeObjectsPresent(kubectl, objects...); len(left) == 0 {
			return
		}
	}
	t.Errorf("after the last workload was deleted, %v remain", left)
}

// volumeService deploys a service that mounts volume at /mnt/v.
func volumeService(t *testing.T, ctx context.Context, h *Harness, c *run.ServicesClient, id string, volume *runpb.Volume) string {
	t.Helper()
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Volumes: []*runpb.Volume{volume},
			Containers: []*runpb.Container{{Image: helloDigest,
				VolumeMounts: []*runpb.VolumeMount{{Name: volume.GetName(), MountPath: "/mnt/v"}}}}}}})
	if err != nil {
		t.Fatalf("CreateService %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for %s: %v", id, err)
	}
	return name
}

func deleteService(t *testing.T, ctx context.Context, c *run.ServicesClient, name string) {
	t.Helper()
	op, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteService %s: %v", name, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteService %s: %v", name, err)
	}
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/DeleteService, google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob, google.cloud.run.v2.Jobs/DeleteJob
//
// TestRunCloudStorageVolumeObjectsGoWithTheirLastWorkload (#1476), with the
// official run/apiv2 client: two services mount one bucket; deleting the
// first leaves the bucket's gcsfuse pod, PersistentVolume and claim, and
// deleting the second removes all three. Two jobs mount another bucket, one
// of them run: deleting it leaves the mount pod, which the other job still
// names, and deleting that one removes it.
func TestRunCloudStorageVolumeObjectsGoWithTheirLastWorkload(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	sc := storageClient(t, h)
	kubectl := volumeKubectl(t)
	c := runClient(t, h)
	jc, _ := runJobClients(t, h)
	// Pulling gcsfuse and busybox, and two rollouts, outlast h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	bucket := fuseBucket(t, h, sc, "compat-fusegc")
	a := volumeService(t, ctx, h, c, "compat-fusegc-a", gcsVolume("v", bucket, false))
	b := volumeService(t, ctx, h, c, "compat-fusegc-b", gcsVolume("v", bucket, false))
	mount := fuseMountOf(kubectl, bucket)
	if mount == "" {
		t.Fatalf("no gcsfuse pod mounts %s", bucket)
	}
	objects := []string{"pod/" + mount, "persistentvolume/" + mount, "persistentvolumeclaim/" + mount}
	if got := volumeObjectsPresent(kubectl, objects...); len(got) != 3 {
		t.Fatalf("with two services, only %v exist", got)
	}
	deleteService(t, ctx, c, a)
	if got := volumeObjectsPresent(kubectl, objects...); len(got) != 3 {
		t.Errorf("with one service left, only %v exist; want the pod, volume and claim kept", got)
	}
	deleteService(t, ctx, c, b)
	awaitGone(t, kubectl, objects...)

	jobBucket := fuseBucket(t, h, sc, "compat-fusegc-job")
	var jobs []string
	for _, id := range []string{"compat-fusegc-j1", "compat-fusegc-j2"} {
		job := shellJob(`echo done > /mnt/data/`+id, 1, nil)
		job.Template.Template.Volumes = []*runpb.Volume{gcsVolume("data", jobBucket, false)}
		job.Template.Template.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}
		name := runParent(h) + "/jobs/" + id
		op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: name}) })
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, name)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobs[0]})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the execution failed: %v", err)
	}
	jobMount := fuseMountOf(kubectl, jobBucket)
	if jobMount == "" {
		t.Fatalf("no gcsfuse pod mounts %s", jobBucket)
	}
	for i, name := range jobs {
		op, err := jc.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name})
		if err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		if i == 0 && len(volumeObjectsPresent(kubectl, "pod/"+jobMount)) != 1 {
			t.Errorf("deleting %s removed the mount pod %s, which the other job names", name, jobMount)
		}
	}
	awaitGone(t, kubectl, "pod/"+jobMount)
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/DeleteService
//
// TestRunNFSVolumeObjectsGoWithTheirLastService (#1476): two services mount
// one NFS export (the NFS test's pinned server, in the instance's cluster)
// through one PersistentVolume and claim; deleting the first leaves both,
// and deleting the second removes both.
func TestRunNFSVolumeObjectsGoWithTheirLastService(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	kubectl := volumeKubectl(t)
	server, _ := runNFSServer(t)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	a := volumeService(t, ctx, h, c, "compat-nfsgc-a", nfsVolume("v", server, "/", false))
	b := volumeService(t, ctx, h, c, "compat-nfsgc-b", nfsVolume("v", server, "/", false))
	claim := nfsClaimOf(kubectl, server)
	if claim == "" {
		t.Fatalf("no PersistentVolume of %s:/", server)
	}
	objects := []string{"persistentvolume/" + claim, "persistentvolumeclaim/" + claim}
	if got := volumeObjectsPresent(kubectl, objects...); len(got) != 2 {
		t.Fatalf("with two services, only %v exist", got)
	}
	deleteService(t, ctx, c, a)
	if got := volumeObjectsPresent(kubectl, objects...); len(got) != 2 {
		t.Errorf("with one service left, only %v exist; want the shared export's volume and claim kept", got)
	}
	deleteService(t, ctx, c, b)
	awaitGone(t, kubectl, objects...)
}
