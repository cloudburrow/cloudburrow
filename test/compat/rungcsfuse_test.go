//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func gcsVolume(name, bucket string, readOnly bool, options ...string) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{
		Bucket: bucket, ReadOnly: readOnly, MountOptions: options}}}
}

// fuseObject reads an object through the official Storage client.
func fuseObject(ctx context.Context, c *storage.Client, bucket, name string) (string, error) {
	r, err := c.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	return string(raw), err
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob
//
// TestRunCloudStorageFUSEVolumes (#1474), with the official run/apiv2 and
// Storage clients: a bucket of the instance's Cloud Storage, never Google's,
// is mounted by Cloud Storage FUSE.
//
//   - a service mounts the bucket read-write, and a second time read-only
//     with mountOptions, reads an object the Storage client wrote, writes one
//     the Storage client then reads back, and is refused a write to the
//     read-only mount;
//   - a job's task writes into the same bucket through its mount;
//   - the volumes read back as set, and a mount option that would point
//     gcsfuse elsewhere is refused INVALID_ARGUMENT.
func TestRunCloudStorageFUSEVolumes(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	h.Endpoint(EnvRun)
	sc := storageClient(t, h)
	c := runClient(t, h)
	jc, _ := runJobClients(t, h)
	// Pulling busybox and gcsfuse, and two rollouts, outlast h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	bucket := fmt.Sprintf("compat-fuse-%d", time.Now().UnixNano()%1e9)
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(h.Context(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(o.Name).Delete(h.Context())
		}
		_ = sc.Bucket(bucket).Delete(h.Context())
	})
	if err := writeObject(h, sc, bucket, "dir/in.txt", "from-storage"); err != nil {
		t.Fatalf("write object: %v", err)
	}

	id := "compat-fuse-svc"
	name := runParent(h) + "/services/" + id
	script := `mkdir -p /www && echo from-service > /mnt/rw/from-service.txt &&
if echo x > /mnt/ro/refused 2>/dev/null; then ro=writable; else ro=read-only; fi &&
echo "$(cat /mnt/rw/dir/in.txt) $(cat /mnt/ro/in.txt) $ro" > /www/index.html &&
exec httpd -f -p 8080 -h /www`
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Volumes: []*runpb.Volume{gcsVolume("rw", bucket, false), gcsVolume("ro", bucket, true, "only-dir=dir", "implicit-dirs")},
			Containers: []*runpb.Container{{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{script},
				VolumeMounts: []*runpb.VolumeMount{{Name: "rw", MountPath: "/mnt/rw"}, {Name: "ro", MountPath: "/mnt/ro"}}}},
		}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	base, host := ingress(t), hostOf(t, svc.GetUri())
	var code int
	var body string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if code, body = httpGet(t, base, host, "/"); code == http.StatusOK {
			break
		}
	}
	if want := "from-storage from-storage read-only"; code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Errorf("the service served %d %q; want %q", code, body, want)
	}
	if got, err := fuseObject(ctx, sc, bucket, "from-service.txt"); err != nil || strings.TrimSpace(got) != "from-service" {
		t.Errorf("the bucket holds %q (%v); want what the service wrote through its mount", got, err)
	}
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if v := got.GetTemplate().GetVolumes(); len(v) != 2 || v[0].GetGcs().GetBucket() != bucket || v[0].GetGcs().GetReadOnly() ||
		!v[1].GetGcs().GetReadOnly() || len(v[1].GetGcs().GetMountOptions()) != 2 {
		t.Errorf("volumes read back as %v", v)
	}

	jobID := "compat-fuse-job"
	jobName := runParent(h) + "/jobs/" + jobID
	job := shellJob(`echo "$CLOUD_RUN_TASK_INDEX" > /mnt/data/from-job.txt`, 1, nil)
	job.Template.Template.Volumes = []*runpb.Volume{gcsVolume("data", bucket, false)}
	job.Template.Template.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: jobID, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: jobName}) })
	if _, err := waitLRO(ctx, jop); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := waitLRO(ctx, rop); err != nil {
		t.Fatalf("the task did not write to its Cloud Storage volume: %v", err)
	}
	if got, err := fuseObject(ctx, sc, bucket, "from-job.txt"); err != nil || strings.TrimSpace(got) != "0" {
		t.Errorf("the bucket holds %q (%v); want what the job wrote", got, err)
	}

	_, err = c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: "compat-fuse-endpoint",
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Volumes:    []*runpb.Volume{gcsVolume("v", bucket, false, "custom-endpoint=https://storage.googleapis.com:443")},
			Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "custom-endpoint") {
		t.Errorf("a mount option naming another endpoint = %v; want INVALID_ARGUMENT", err)
	}
}

// fuseBucket creates a bucket of the instance's Cloud Storage for a test,
// emptied and deleted when it ends.
func fuseBucket(t *testing.T, h *Harness, sc *storage.Client, prefix string) string {
	t.Helper()
	bucket := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e9)
	if err := sc.Bucket(bucket).Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(h.Context(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(o.Name).Delete(h.Context())
		}
		_ = sc.Bucket(bucket).Delete(h.Context())
	})
	return bucket
}

// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.replaceService, run.namespaces.services.delete
//
// TestGcloudRunDeployCloudStorageVolume (#1474): `gcloud run deploy
// --add-volume type=cloud-storage` with mount-options and
// `--add-volume-mount`, through the configuration `cloudburrow gcloud-setup`
// writes, with egress to Google refused, mounts a bucket of the instance's
// Cloud Storage; the service becomes ready, and describe reads the volume
// back.
func TestGcloudRunDeployCloudStorageVolume(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	h.Endpoint(EnvRun)
	sc := storageClient(t, h)
	bucket := fuseBucket(t, h, sc, "gcloud-fuse")
	g := newGcloudSession(t, h)
	id := "gcloud-fuse"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	g.must("run", "deploy", id, "--image="+helloDigest, region, "--no-allow-unauthenticated",
		"--add-volume=name=data,type=cloud-storage,bucket="+bucket+",readonly=true,mount-options=implicit-dirs",
		"--add-volume-mount=volume=data,mount-path=/mnt/data", "--quiet")
	got := g.must("run", "services", "describe", id, region,
		"--format=value(status.latestReadyRevisionName,spec.template.spec.volumes[0].csi.volumeAttributes.bucketName,"+
			"spec.template.spec.volumes[0].csi.readOnly,spec.template.spec.containers[0].volumeMounts[0].mountPath)")
	if !strings.Contains(got, id+"-00001") || !strings.Contains(got, bucket) || !strings.Contains(got, "/mnt/data") ||
		!strings.Contains(strings.ToLower(got), "true") {
		t.Errorf("describe = %q; want the first revision ready, the bucket read-only and the mount path", got)
	}
	g.must("run", "services", "delete", id, region, "--quiet")
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/DeleteService
//
// TestTerraformRunCloudStorageVolume (#1474): a google_cloud_run_v2_service
// with a volumes block of gcs { bucket, read_only, mount_options } and its
// volume_mounts applies through `cloudburrow terraform` with egress blocked
// and becomes ready; the official client reads the volume back; the plan is
// clean; destroy removes it.
func TestTerraformRunCloudStorageVolume(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	svcs := runClient(t, h)
	sc := storageClient(t, h)
	m := newTFModule(t)
	bucket := fuseBucket(t, h, sc, "tf-fuse")
	ctx := h.Context()
	id := "tffuse-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id
	m.write(fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  template {
    volumes {
      name = "data"
      gcs {
        bucket        = %[3]q
        read_only     = true
        mount_options = ["implicit-dirs"]
      }
    }
    containers {
      image = %[4]q
      volume_mounts {
        name       = "data"
        mount_path = "/mnt/data"
      }
    }
  }
}
`, h.Project(), id, bucket, helloDigest))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	s, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if v := s.GetTemplate().GetVolumes(); len(v) != 1 || v[0].GetGcs().GetBucket() != bucket || !v[0].GetGcs().GetReadOnly() ||
		len(v[0].GetGcs().GetMountOptions()) != 1 {
		t.Errorf("the service's template = %v", s.GetTemplate())
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
}
