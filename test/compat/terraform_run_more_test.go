//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// More Cloud Run resources through `cloudburrow terraform` (#1304), in the
// run shard, whose instance serves Cloud Run and Secret Manager.

// TestTerraformRunVolumesAndServiceAccount (#1304): a google_cloud_run_v2_service
// and a google_cloud_run_v2_job, each with a service_account, a Secret Manager
// secret volume (from a google_secret_manager_secret and its version) and an
// in-memory empty_dir volume mounted by volume_mounts (#1297), apply through
// `cloudburrow terraform` with egress blocked; the official gRPC clients read
// the account, the volumes and the mounts back; the plan is clean; a label
// and an environment variable change in place and keep the volumes; the plan
// is clean again; and destroy removes both.
// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/DeleteService, google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/UpdateJob, google.cloud.run.v2.Jobs/DeleteJob
func TestTerraformRunVolumesAndServiceAccount(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	svcs := runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	_ = secretsClient(t, h) // skips without CLOUDBURROW_TEST_SECRETS
	jobs, _ := runJobClients(t, h)
	ctx := h.Context()
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	svcID, jobID, secretID := "tfvolsvc-"+suffix, "tfvoljob-"+suffix, "tf-run-volume-"+suffix
	svc, job := runParent(h)+"/services/"+svcID, runParent(h)+"/jobs/"+jobID
	svcSA := "web@" + h.Project() + ".iam.gserviceaccount.com"
	jobSA := "runner@" + h.Project() + ".iam.gserviceaccount.com"

	module := func(label, value string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %[1]q
  secret_id = %[4]q
  replication {
    auto {}
  }
}
resource "google_secret_manager_secret_version" "v" {
  secret      = google_secret_manager_secret.s.id
  secret_data = "terraform-volume-payload"
}
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[5]q }
  template {
    service_account = %[7]q
    volumes {
      name = "creds"
      secret {
        secret = google_secret_manager_secret.s.secret_id
        items {
          version = "latest"
          path    = "token"
        }
      }
    }
    volumes {
      name = "cache"
      empty_dir {
        medium     = "MEMORY"
        size_limit = "32Mi"
      }
    }
    containers {
      image = %[9]q
      env {
        name  = "TARGET"
        value = %[6]q
      }
      volume_mounts {
        name       = "creds"
        mount_path = "/secrets"
      }
      volume_mounts {
        name       = "cache"
        mount_path = "/cache"
      }
    }
  }
  depends_on = [google_secret_manager_secret_version.v]
}
resource "google_cloud_run_v2_job" "j" {
  project             = %[1]q
  name                = %[3]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[5]q }
  template {
    template {
      service_account = %[8]q
      max_retries     = 1
      volumes {
        name = "creds"
        secret {
          secret = google_secret_manager_secret.s.secret_id
          items {
            version = "latest"
            path    = "token"
          }
        }
      }
      volumes {
        name = "scratch"
        empty_dir {
          medium     = "MEMORY"
          size_limit = "16Mi"
        }
      }
      containers {
        image   = %[10]q
        command = ["sh", "-c", "cat /secrets/token"]
        env {
          name  = "TARGET"
          value = %[6]q
        }
        volume_mounts {
          name       = "creds"
          mount_path = "/secrets"
        }
        volume_mounts {
          name       = "scratch"
          mount_path = "/scratch"
        }
      }
    }
  }
  depends_on = [google_secret_manager_secret_version.v]
}
`, h.Project(), svcID, jobID, secretID, label, value, svcSA, jobSA, helloDigest, jobImage)
	}
	volumesOK := func(vs []*runpb.Volume, ms []*runpb.VolumeMount, size string) bool {
		return len(vs) == 2 && vs[0].GetName() == "creds" && vs[0].GetSecret().GetSecret() == secretID &&
			len(vs[0].GetSecret().GetItems()) == 1 && vs[0].GetSecret().GetItems()[0].GetPath() == "token" &&
			vs[1].GetEmptyDir().GetMedium() == runpb.EmptyDirVolumeSource_MEMORY && vs[1].GetEmptyDir().GetSizeLimit() == size &&
			len(ms) == 2 && ms[0].GetMountPath() == "/secrets" && ms[1].GetName() == vs[1].GetName()
	}
	check := func(when, label, value string) {
		t.Helper()
		s, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: svc})
		if err != nil {
			t.Fatalf("%s, GetService: %v", when, err)
		}
		st := s.GetTemplate()
		c := st.GetContainers()[0]
		if s.GetLabels()["env"] != label || st.GetServiceAccount() != svcSA || len(c.GetEnv()) != 1 ||
			c.GetEnv()[0].GetValue() != value || !volumesOK(st.GetVolumes(), c.GetVolumeMounts(), "32Mi") {
			t.Errorf("%s, the service = labels %v, template %v", when, s.GetLabels(), st)
		}
		j, err := jobs.GetJob(ctx, &runpb.GetJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetJob: %v", when, err)
		}
		jt := j.GetTemplate().GetTemplate()
		jc := jt.GetContainers()[0]
		if j.GetLabels()["env"] != label || jt.GetServiceAccount() != jobSA || len(jc.GetEnv()) != 1 ||
			jc.GetEnv()[0].GetValue() != value || !volumesOK(jt.GetVolumes(), jc.GetVolumeMounts(), "16Mi") {
			t.Errorf("%s, the job = labels %v, template %v", when, j.GetLabels(), jt)
		}
	}

	m.write(module("local", "tf"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "local", "tf")
	m.planClean()

	m.write(module("changed", "tf2"))
	m.apply()
	check("after the change", "changed", "tf2")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: svc}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
	if _, err := jobs.GetJob(ctx, &runpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}
}

// TestTerraformRunWorkerPool (#1304): a google_cloud_run_v2_worker_pool
// (#1300) with manual_instance_count applies through `cloudburrow terraform` with
// egress blocked and becomes ready; the official WorkerPoolsClient reads its
// labels, instance count and container back; the plan is clean; a label, an
// environment variable and the instance count change in place; the plan is
// clean again; and destroy removes it. The module sets no scaling_mode: the
// provider sends it as scaling.scalingMode, which google.cloud.run.v2's
// WorkerPoolScaling (and Google's published discovery document) does not
// have, so the transcoder refuses the body as INVALID_ARGUMENT (#1354).
// Checked on 2026-10-15: WorkerPoolScaling has only manual_instance_count
// (field 6) in cloud.google.com/go/run v1.23.0 (the pinned proto), in
// googleapis master's google/cloud/run/v2/vendor_settings.proto, and in
// the run v2 discovery documents of google.golang.org/api v0.298.0
// (revision 20260904) and v0.301.0 (20260917). Google publishes no
// scaling_mode for a worker pool, so the refusal is the published surface,
// not a gap: accepting the field would invent its meaning.
// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.WorkerPools/UpdateWorkerPool, google.cloud.run.v2.WorkerPools/DeleteWorkerPool
func TestTerraformRunWorkerPool(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	pools, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	id := "tfpool-" + suffix
	name := runParent(h) + "/workerPools/" + id

	module := func(label, greeting string, count int) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_worker_pool" "p" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[3]q }
  scaling {
    manual_instance_count = %[5]d
  }
  template {
    containers {
      image   = %[6]q
      command = ["sh", "-c"]
      args    = ["echo $GREETING; exec sleep 3600"]
      env {
        name  = "GREETING"
        value = %[4]q
      }
    }
  }
}
`, h.Project(), id, label, greeting, count, jobImage)
	}
	check := func(when, label, greeting string, count int32) {
		t.Helper()
		// The provider does not wait for the pool's instances; the test does.
		waitCondition(t, "the worker pool "+when, func() (*runpb.Condition, error) {
			wp, err := pools.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
			return wp.GetTerminalCondition(), err
		})
		wp, err := pools.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetWorkerPool: %v", when, err)
		}
		c := wp.GetTemplate().GetContainers()
		if wp.GetLabels()["env"] != label || wp.GetScaling().GetManualInstanceCount() != count || len(c) != 1 ||
			c[0].GetImage() != jobImage || len(c[0].GetEnv()) != 1 || c[0].GetEnv()[0].GetValue() != greeting ||
			wp.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED {
			t.Errorf("%s, the worker pool = %v", when, wp)
		}
	}

	m.write(module("local", "one", 1))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "local", "one", 1)
	m.planClean()

	m.write(module("changed", "two", 2))
	m.apply()
	check("after the change", "changed", "two", 2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := pools.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkerPool after destroy: %v; want NotFound", err)
	}
}
