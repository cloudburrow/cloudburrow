//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/googleapi"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's eighth batch of google provider resources, each through
// `cloudburrow terraform` with hashicorp/google and egress blocked, read
// back through an official SDK, planned clean, changed in place where the
// resource has an updatable field, planned clean again and destroyed. Each
// test asks for the endpoints of the services it uses first, so it skips in
// a shard without them.

// runV1PolicyMap is a run/v1 policy's bindings as role to members.
func runV1PolicyMap(p *runv1.Policy) map[string][]string {
	out := map[string][]string{}
	for _, b := range p.Bindings {
		out[b.Role] = b.Members
	}
	return out
}

// tfRunV1Service is a google_cloud_run_service (Cloud Run v1) with one
// container, an environment variable and a label on the service.
func tfRunV1Service(project, name, label, greeting string) string {
	return fmt.Sprintf(`
resource "google_cloud_run_service" "s" {
  project  = %q
  name     = %q
  location = "us-central1"
  metadata {
    labels = { env = %q }
  }
  template {
    spec {
      containers {
        image = %q
        env {
          name  = "GREETING"
          value = %q
        }
      }
    }
  }
  traffic {
    percent         = 100
    latest_revision = true
  }
}
`, project, name, label, helloDigest, greeting)
}

// TestTerraformCloudRunV1Service (#1304, #1450): a google_cloud_run_service,
// Cloud Run's v1 (Knative) resource, applies through `cloudburrow
// terraform`, which points cloud_run_custom_endpoint at
// {{location}}-run.localhost on the Run port; the provider waits for Ready.
// The official run/v1 client reads it back Ready, with its label, the
// container's image and environment and all traffic on the latest revision;
// the plan is clean; a new label and environment value apply in place
// (replaceService), which cuts a second revision; the plan is clean again;
// destroy deletes it, after which Get answers 404.
// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.replaceService, run.namespaces.services.delete
func TestTerraformCloudRunV1Service(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h) // asks for the Run endpoint first
	m := newTFModule(t)
	svc := "tf-v1-svc"
	name := "namespaces/" + h.Project() + "/services/" + svc
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete(name).Do() })
	check := func(when, label, greeting string) *runv1.Service {
		t.Helper()
		s, err := c.Namespaces.Services.Get(name).Do()
		if err != nil {
			t.Fatalf("%s, services.get: %v", when, err)
		}
		if cond := v1Cond(s.Status.Conditions, "Ready"); cond == nil || cond.Status != "True" {
			t.Errorf("%s, Ready = %+v; want True", when, cond)
		}
		if s.Metadata.Labels["env"] != label || s.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" {
			t.Errorf("%s, labels = %v; want env=%s in us-central1", when, s.Metadata.Labels, label)
		}
		cs := s.Spec.Template.Spec.Containers
		if len(cs) != 1 || cs[0].Image != helloDigest || len(cs[0].Env) != 1 || cs[0].Env[0].Name != "GREETING" || cs[0].Env[0].Value != greeting {
			t.Errorf("%s, containers = %+v; want %s with GREETING=%s", when, cs, helloDigest, greeting)
		}
		if tr := s.Spec.Traffic; len(tr) != 1 || tr[0].Percent != 100 || !tr[0].LatestRevision {
			t.Errorf("%s, traffic = %+v; want 100%% on the latest revision", when, tr)
		}
		return s
	}

	m.write(tfRunV1Service(h.Project(), svc, "a", "hello"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	first := check("after apply", "a", "hello")
	m.planClean()

	m.write(tfRunV1Service(h.Project(), svc, "b", "bonjour"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	second := check("after the change", "b", "bonjour")
	if first.Status.LatestCreatedRevisionName == second.Status.LatestCreatedRevisionName {
		t.Errorf("the template change cut no new revision: %s", second.Status.LatestCreatedRevisionName)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := c.Namespaces.Services.Get(name).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != http.StatusNotFound {
		t.Errorf("services.get after destroy: %v; want 404", err)
	}
}

// TestTerraformCloudRunV1ServiceIAM (#1304): google_cloud_run_service_iam_member,
// _binding and _policy on a google_cloud_run_service, through
// TestTerraformBigQueryIAM's phases (member and binding, none, policy,
// none), each read back with the official run/v1 client's
// projects.locations.services.getIamPolicy and planned clean; destroy
// removes the service with its policy. Stored, never enforced (ADR-0006).
// covers: run.projects.locations.services.getIamPolicy, run.projects.locations.services.setIamPolicy, run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.delete
func TestTerraformCloudRunV1ServiceIAM(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h) // asks for the Run endpoint first
	m := newTFModule(t)
	svc := "tf-v1-iam"
	name := runParent(h) + "/services/" + svc
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete("namespaces/" + h.Project() + "/services/" + svc).Do() })
	targets := []tfIAMTarget{{
		kind: "google_cloud_run_service", label: "s", policyData: true,
		ref: map[string]string{"project": "google_cloud_run_service.s.project", "location": "google_cloud_run_service.s.location",
			"service": "google_cloud_run_service.s.name"},
		read: func() map[string][]string {
			t.Helper()
			p, err := c.Projects.Locations.Services.GetIamPolicy(name).Do()
			if err != nil {
				t.Fatalf("getIamPolicy on %s: %v", name, err)
			}
			return runV1PolicyMap(p)
		},
	}}
	runTFIAMPhases(t, m, tfRunV1Service(h.Project(), svc, "iam", "hello"), [2]string{"roles/run.invoker", "roles/run.developer"}, targets)
	_, err := c.Namespaces.Services.Get("namespaces/" + h.Project() + "/services/" + svc).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != http.StatusNotFound {
		t.Errorf("services.get after destroy: %v; want 404", err)
	}
}

// TestTerraformSecretManagerSecretIAM (#1304):
// google_secret_manager_secret_iam_member, _binding and _policy on a
// google_secret_manager_secret, through TestTerraformBigQueryIAM's phases,
// each read back with the official client's GetIamPolicy and planned
// clean; destroy removes the secret. Stored, never enforced (ADR-0006).
// covers: google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret
func TestTerraformSecretManagerSecretIAM(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // asks for the Secret Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	id := "tf-secret-iam-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := "projects/" + h.Project() + "/secrets/" + id
	base := fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = %q
  replication {
    auto {}
  }
}
`, h.Project(), id)
	targets := []tfIAMTarget{{
		kind: "google_secret_manager_secret", label: "s",
		ref: map[string]string{"project": "google_secret_manager_secret.s.project", "secret_id": "google_secret_manager_secret.s.secret_id"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := sm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/secretmanager.secretAccessor", "roles/secretmanager.viewer"}, targets)
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}

// TestTerraformCloudFunctionsFunctionIAM (#1304, #1428):
// google_cloudfunctions_function_iam_member, _binding and _policy on a 1st
// gen function, through TestTerraformBigQueryIAM's phases, each read back
// with the official v1 client's GetIamPolicy and planned clean. The
// function is made by the official v1 client from a source that is not a
// zip, so it is OFFLINE (its build fails) and the test runs where no
// function can be built; a function's IAM policy does not depend on its
// build. Stored, never enforced (ADR-0006).
// covers: google.cloud.functions.v1.CloudFunctionsService/GetIamPolicy, google.cloud.functions.v1.CloudFunctionsService/SetIamPolicy
func TestTerraformCloudFunctionsFunctionIAM(t *testing.T) {
	h := New(t)
	c := functionsV1Client(t, h) // asks for the Run endpoint first
	m := newTFModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	id := "tf-fn-iam"
	name := parent + "/functions/" + id
	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("this is not a zip archive"))
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := c.DeleteFunction(dctx, &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{
		Name: name, Runtime: "go123", EntryPoint: "Hello",
		SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}},
	}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	_, _ = op.Wait(ctx) // fails with the build's error: the source is not a zip
	if f, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name}); err != nil || f.GetStatus() != fv1.CloudFunctionStatus_OFFLINE {
		t.Fatalf("GetFunction = %v, %v; want it OFFLINE", f, err)
	}
	targets := []tfIAMTarget{{
		kind: "google_cloudfunctions_function", label: "f",
		ref: map[string]string{"project": fmt.Sprintf("%q", h.Project()), "region": `"us-central1"`, "cloud_function": fmt.Sprintf("%q", id)},
		read: func() map[string][]string {
			t.Helper()
			pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, "", [2]string{"roles/cloudfunctions.invoker", "roles/cloudfunctions.developer"}, targets)
	if got := targets[0].read(); len(got) != 0 {
		t.Errorf("the function's policy after destroy = %v; want it empty", got)
	}
}

// TestTerraformBackupDRVaultAndPlanUpdates (#1304, #1462): a
// google_backup_dr_backup_vault and a google_backup_dr_backup_plan with a
// daily rule apply; the generated backupdr client reads both back; the plan
// is clean; a new description and minimum enforced retention on the vault
// (backupVaults.patch) and a new description and rule retention on the
// plan (backupPlans.patch, which makes a new revision) apply in place; both
// read back changed; the plan is clean again; destroy removes both. The
// provider replaces a plan for a new log_retention_days (ForceNew in
// hashicorp/google 8.4.0), so that stays.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.patch, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.patch, backupdr.projects.locations.backupPlans.delete, backupdr.projects.locations.operations.get
func TestTerraformBackupDRVaultAndPlanUpdates(t *testing.T) {
	h := New(t)
	_ = backupDRInstance(t) // skips without Backup and DR and a Cloud SQL service
	bdr := backupDRService(t, h)
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	vault, plan := fmt.Sprintf("tfuvault-%d", n), fmt.Sprintf("tfuplan-%d", n)
	loc := "projects/" + m.project + "/locations/us-central1"
	module := func(desc, retention string, logDays, ruleDays int) string {
		return fmt.Sprintf(`
resource "google_backup_dr_backup_vault" "v" {
  project                                    = %[1]q
  location                                   = "us-central1"
  backup_vault_id                            = %[2]q
  description                                = %[4]q
  backup_minimum_enforced_retention_duration = %[5]q
  ignore_inactive_datasources                = true
}
resource "google_backup_dr_backup_plan" "p" {
  project            = %[1]q
  location           = "us-central1"
  backup_plan_id     = %[3]q
  description        = %[4]q
  resource_type      = "sqladmin.googleapis.com/Instance"
  backup_vault       = google_backup_dr_backup_vault.v.id
  log_retention_days = %[6]d
  backup_rules {
    rule_id               = "daily"
    backup_retention_days = %[7]d
    standard_schedule {
      recurrence_type = "DAILY"
      time_zone       = "UTC"
      backup_window {
        start_hour_of_day = 1
        end_hour_of_day   = 5
      }
    }
  }
}
`, m.project, vault, plan, desc, retention, logDays, ruleDays)
	}
	check := func(when, desc, retention string, logDays, ruleDays int64) {
		t.Helper()
		v, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do()
		if err != nil {
			t.Fatalf("%s, backupVaults.get: %v", when, err)
		}
		if v.Description != desc || v.BackupMinimumEnforcedRetentionDuration != retention || v.State != "ACTIVE" {
			t.Errorf("%s, the vault = %q %q %s; want %q %q ACTIVE", when, v.Description, v.BackupMinimumEnforcedRetentionDuration, v.State, desc, retention)
		}
		p, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do()
		if err != nil {
			t.Fatalf("%s, backupPlans.get: %v", when, err)
		}
		if p.Description != desc || p.LogRetentionDays != logDays || len(p.BackupRules) != 1 ||
			p.BackupRules[0].RuleId != "daily" || p.BackupRules[0].BackupRetentionDays != ruleDays {
			t.Errorf("%s, the plan = %q, log %d days, rules %+v; want %q, %d days, daily kept %d days",
				when, p.Description, p.LogRetentionDays, p.BackupRules, desc, logDays, ruleDays)
		}
	}

	m.write(module("first", "100000s", 7, 7))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first", "100000s", 7, 7)
	m.planClean()

	m.write(module("second", "200000s", 7, 10))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the changes were not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "second", "200000s", 7, 10)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do(); err == nil {
		t.Errorf("the plan survived destroy")
	}
	if _, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do(); err == nil {
		t.Errorf("the vault survived destroy")
	}
}
