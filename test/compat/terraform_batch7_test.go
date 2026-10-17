//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's seventh batch of google provider resources, each through
// `cloudburrow terraform` with hashicorp/google and egress blocked, read
// back through an official SDK, planned clean, changed in place where the
// resource has an updatable field, planned clean again and destroyed. Each
// test asks for the endpoints of the services it uses first, so it skips in
// a shard without them.

// TestTerraformArtifactRegistryRuleAndCleanupPolicies (#1304, #1449): a
// DOCKER google_artifact_registry_repository with two cleanup_policies (a
// DELETE by tag state and age, a KEEP of the most recent versions) in dry
// run, and a google_artifact_registry_rule on it (DENY DOWNLOAD of one
// package under a CEL condition), apply; the official gRPC client reads the
// policies and the rule back; the plan is clean; a new age, keep count and
// dry-run flag on the repository and ALLOW on the rule apply in place
// (UpdateRepository and UpdateRule); the plan is clean again; destroy
// removes both. Rules and cleanup policies are stored, never enforced.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRule, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRule, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRule, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRule, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository
func TestTerraformArtifactRegistryRuleAndCleanupPolicies(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ar.Close() }()
	id := "tf-rules-" + strings.TrimPrefix(h.Project(), "cb-test-")
	repo := runParent(h) + "/repositories/" + id
	rule := repo + "/rules/deny-app"
	module := func(age string, keep int, dryRun bool, action string) string {
		return fmt.Sprintf(`
resource "google_artifact_registry_repository" "r" {
  project                = %[1]q
  location               = "us-central1"
  repository_id          = %[2]q
  format                 = "DOCKER"
  cleanup_policy_dry_run = %[5]t
  cleanup_policies {
    id     = "delete-untagged"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = %[3]q
    }
  }
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = %[4]d
    }
  }
}
resource "google_artifact_registry_rule" "deny" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.r.repository_id
  rule_id       = "deny-app"
  action        = %[6]q
  operation     = "DOWNLOAD"
  package_id    = "app"
  condition {
    title      = "outside hours"
    expression = "request.time.getHours(\"UTC\") > 20"
  }
}
`, h.Project(), id, age, keep, dryRun, action)
	}
	check := func(when string, age time.Duration, keep int32, dryRun bool, action arpb.Rule_Action) {
		t.Helper()
		r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repo})
		if err != nil {
			t.Fatalf("%s, GetRepository: %v", when, err)
		}
		del, kept := r.GetCleanupPolicies()["delete-untagged"], r.GetCleanupPolicies()["keep-recent"]
		if r.GetCleanupPolicyDryRun() != dryRun || len(r.GetCleanupPolicies()) != 2 ||
			del.GetAction() != arpb.CleanupPolicy_DELETE || del.GetCondition().GetTagState() != arpb.CleanupPolicyCondition_UNTAGGED ||
			del.GetCondition().GetOlderThan().AsDuration() != age ||
			kept.GetAction() != arpb.CleanupPolicy_KEEP || kept.GetMostRecentVersions().GetKeepCount() != keep {
			t.Errorf("%s, the repository's cleanup = dry run %t, %v; want dry run %t, DELETE UNTAGGED older than %s, KEEP %d",
				when, r.GetCleanupPolicyDryRun(), r.GetCleanupPolicies(), dryRun, age, keep)
		}
		got, err := ar.GetRule(ctx, &arpb.GetRuleRequest{Name: rule})
		if err != nil {
			t.Fatalf("%s, GetRule: %v", when, err)
		}
		if got.GetAction() != action || got.GetOperation() != arpb.Rule_DOWNLOAD || got.GetPackageId() != "app" ||
			got.GetCondition().GetExpression() != `request.time.getHours("UTC") > 20` || got.GetCondition().GetTitle() != "outside hours" {
			t.Errorf("%s, the rule = %v; want %s DOWNLOAD of app under the condition", when, got, action)
		}
	}

	m.write(module("2592000s", 5, true, "DENY"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 30*24*time.Hour, 5, true, arpb.Rule_DENY)
	m.planClean()

	m.write(module("604800s", 3, false, "ALLOW"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the changes were not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", 7*24*time.Hour, 3, false, arpb.Rule_ALLOW)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ar.GetRule(ctx, &arpb.GetRuleRequest{Name: rule}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRule after destroy: %v; want NotFound", err)
	}
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repo}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
}

// TestTerraformCloudRunDomainMapping (#1304, #1450): a
// google_cloud_run_domain_mapping with certificate_mode NONE to a v1
// service applies through `cloudburrow terraform`, which points
// cloud_run_custom_endpoint at {{location}}-run.localhost on the Run port,
// so the provider's regional v1 URLs reach this instance with their region.
// The provider waits for Ready, which a NONE mapping becomes; the official
// run/v1 client reads it back Ready with its CNAME record, and the gateway
// routes the domain by Host header to the service. The plan is clean; a new
// label on the mapping's metadata replaces it, as the provider updates no
// argument of a domain mapping in place (the old mapping is deleted, then
// the new one created and routed); the plan is clean again; destroy deletes
// the mapping, after which Get answers 404.
// covers: run.namespaces.domainmappings.create, run.namespaces.domainmappings.get, run.namespaces.domainmappings.delete
func TestTerraformCloudRunDomainMapping(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h) // asks for the Run endpoint first
	base := ingress(t)
	m := newTFModule(t)
	svc := "tf-domain"
	runV1Service(t, c, h.Project(), svc)
	domain := "tf.cloudburrow-test.example.com"
	name := "namespaces/" + h.Project() + "/domainmappings/" + domain
	t.Cleanup(func() { _, _ = c.Namespaces.Domainmappings.Delete(name).Do() })
	module := func(label string) string {
		return fmt.Sprintf(`
resource "google_cloud_run_domain_mapping" "d" {
  project  = %q
  location = "us-central1"
  name     = %q
  metadata {
    namespace = %q
    labels    = { env = %q }
  }
  spec {
    route_name       = %q
    certificate_mode = "NONE"
  }
}
`, h.Project(), domain, h.Project(), label, svc)
	}
	check := func(when, label string) {
		t.Helper()
		dm, err := c.Namespaces.Domainmappings.Get(name).Do()
		if err != nil {
			t.Fatalf("%s, domainmappings.get: %v", when, err)
		}
		if dm.Spec.RouteName != svc || dm.Spec.CertificateMode != "NONE" || dm.Metadata.Labels["env"] != label ||
			dm.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" {
			t.Errorf("%s, the mapping = %+v %+v; want route %s, NONE, env=%s in us-central1", when, dm.Spec, dm.Metadata.Labels, svc, label)
		}
		if cond := v1Cond(dm.Status.Conditions, "Ready"); cond == nil || cond.Status != "True" {
			t.Errorf("%s, Ready = %+v; want True", when, cond)
		}
		if rr := dm.Status.ResourceRecords; len(rr) != 1 || rr[0].Type != "CNAME" || rr[0].Rrdata != "ghs.googlehosted.com." {
			t.Errorf("%s, resourceRecords = %+v; want one CNAME to ghs.googlehosted.com.", when, rr)
		}
		if code, body := httpGet(t, base, domain, "/"); code != http.StatusOK || !strings.Contains(body, "Hello") {
			t.Errorf("%s, GET / with Host %s = %d %q; want the service's answer", when, domain, code, body)
		}
	}

	m.write(module("a"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "a")
	m.planClean()

	m.write(module("b"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	// The provider has no update for a domain mapping: every argument,
	// a label among them, replaces it, the old one deleted first.
	if !strings.Contains(out, "1 added, 0 changed, 1 destroyed") {
		t.Errorf("the label change did not replace the mapping:\n%s", lastLines(out, 20))
	}
	var err error
	check("after the label change", "b")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = c.Namespaces.Domainmappings.Get(name).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != http.StatusNotFound {
		t.Errorf("domainmappings.get after destroy: %v; want 404", err)
	}
}

// TestTerraformSQLPostgres12Upgrade (#1304, #1462): a
// google_sql_database_instance on POSTGRES_12 applies; the generated
// sqladmin client reads it back RUNNABLE on POSTGRES_12, the server
// PostgreSQL 12's final release (POSTGRES_12_22), with POSTGRES_13 among
// its upgradable versions; the plan is clean; database_version POSTGRES_13
// applies in place, a major version upgrade (instances.patch); the instance
// reads back POSTGRES_13; the plan is clean again; destroy removes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.operations.get
func TestTerraformSQLPostgres12Upgrade(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-pg12-%d", time.Now().UnixNano()%1e6)
	module := func(version string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "pg" {
  project             = %q
  name                = %q
  region              = "us-central1"
  database_version    = %q
  deletion_protection = false
  settings {
    tier = "db-custom-1-3840"
  }
}
`, m.project, name, version)
	}
	t.Cleanup(func() {
		if op, err := svc.Instances.Delete(m.project, name).Do(); err == nil {
			waitSQLOp(t, svc, m.project, op)
		}
	})
	check := func(when, version, installed string) []string {
		t.Helper()
		i, err := svc.Instances.Get(m.project, name).Do()
		if err != nil {
			t.Fatalf("%s, instances.get: %v", when, err)
		}
		var upgradable []string
		for _, v := range i.UpgradableDatabaseVersions {
			upgradable = append(upgradable, v.MajorVersion)
		}
		if i.State != "RUNNABLE" || i.DatabaseVersion != version || !strings.HasPrefix(i.DatabaseInstalledVersion, installed) {
			t.Errorf("%s, the instance reads %s %s %s; want RUNNABLE %s %s", when, i.State, i.DatabaseVersion, i.DatabaseInstalledVersion, version, installed)
		}
		return upgradable
	}

	m.write(module("POSTGRES_12"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if up := check("after apply", "POSTGRES_12", "POSTGRES_12_22"); !strings.Contains(strings.Join(up, " "), "POSTGRES_13") {
		t.Errorf("upgradableDatabaseVersions = %v; want POSTGRES_13 among them", up)
	}
	m.planClean()

	m.write(module("POSTGRES_13"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the upgrade was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the upgrade", "POSTGRES_13", "POSTGRES_13_")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, http.StatusNotFound)
}
