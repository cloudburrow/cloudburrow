//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// TestRunWorkerPoolMountsACloudStorageVolume (#1576), with the official
// run/apiv2 WorkerPoolsClient and Storage client: a worker pool's instance
// mounts a bucket of the instance's Cloud Storage through Cloud Storage
// FUSE, as a service does (TestRunCloudStorageFUSEVolumes): it reads an
// object the Storage client wrote and writes one the Storage client reads
// back, and the volume reads back as set.
// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool
func TestRunWorkerPoolMountsACloudStorageVolume(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	sc := storageClient(t, h)
	c, err := run.NewWorkerPoolsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Pulling busybox and gcsfuse outlasts h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	bucket := fuseBucket(t, h, sc, "compat-pool-fuse")
	if err := writeObject(h, sc, bucket, "in.txt", "from-storage"); err != nil {
		t.Fatalf("write object: %v", err)
	}
	id := "compat-pool-fuse"
	name := runParent(h) + "/workerPools/" + id
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		if op, err := c.DeleteWorkerPool(dctx, &runpb.DeleteWorkerPoolRequest{Name: name}); err == nil {
			_, _ = op.Wait(dctx)
		}
	})
	one := int32(1)
	op, err := c.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: id,
		WorkerPool: &runpb.WorkerPool{
			Template: &runpb.WorkerPoolRevisionTemplate{
				Volumes: []*runpb.Volume{gcsVolume("data", bucket, false)},
				Containers: []*runpb.Container{{Image: jobImage, Command: []string{"sh", "-c"},
					Args:         []string{`echo "pool read $(cat /mnt/data/in.txt)" > /mnt/data/from-pool.txt; exec sleep 3600`},
					VolumeMounts: []*runpb.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}}}},
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &one}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the worker pool: %v", err)
	}
	var got string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if got, err = fuseObject(ctx, sc, bucket, "from-pool.txt"); err == nil {
			break
		}
	}
	if strings.TrimSpace(got) != "pool read from-storage" {
		t.Errorf("the bucket holds %q (%v); want what the pool's instance wrote through its mount", got, err)
	}
	wp, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		t.Fatalf("GetWorkerPool: %v", err)
	}
	if v := wp.GetTemplate().GetVolumes(); len(v) != 1 || v[0].GetGcs().GetBucket() != bucket || v[0].GetGcs().GetReadOnly() {
		t.Errorf("the pool's volumes read back as %v", v)
	}
}
