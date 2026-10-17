//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/backupdr/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestAPurgedProjectsSQLBackupAndRegistryStartEmpty (#1541, #1542, #1543),
// TestAPurgedProjectsIDStartsEmpty's sibling for the services it could not
// reach. Through the official clients, a project gets, where the instance
// serves them:
//   - Cloud SQL: a created instance with a database, a user, a backup and a
//     client certificate, and a client certificate on the server `up` runs;
//   - Backup and DR: a vault, a plan, an association of that instance with
//     a backup, and a trial;
//   - Artifact Registry: a generic repository with a file, a rule, and the
//     project's settings and VPC-SC config.
//
// Deleted and purged (POST /admin/purge) and created again under the ID,
// none of it reads back: the instance's server and volume are gone from
// the cluster, an instance, a vault and a repository made again under the
// same names start empty.
func TestAPurgedProjectsSQLBackupAndRegistryStartEmpty(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	// Resource Manager as the sql shard's SQL_RESOURCEMANAGER (#1568), so
	// the Resource Manager suite stays in storage.
	shardEndpoint(h, envSQLResourceManager, EnvResourceManager)
	served := func(env string) bool {
		if strings.TrimSpace(os.Getenv(env)) == "" {
			return false
		}
		h.Endpoint(env)
		return true
	}
	usePG, useMySQL := served(EnvCloudSQL), served(EnvMySQL)
	useSQL := (usePG || useMySQL) && served(EnvSQLAdmin)
	useDR := useSQL && served(EnvBackupDR)
	useAR := served(EnvRun)
	if !useSQL && !useAR {
		t.Skip("needs --services cloudsql or cloudsql-mysql, or run")
	}
	ctx := h.Context()
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	id := v1ID(h, "pq-")
	parent := "projects/" + id
	create := func() {
		t.Helper()
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
		if err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	create()
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: parent})
	})
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)

	// Cloud SQL.
	var sql *sqladmin.Service
	inst := "purged-" + suffix
	version, upInstance := "POSTGRES_17", "postgres"
	if !usePG {
		version, upInstance = "MYSQL_8_4", "mysql"
	}
	newInstance := func() {
		t.Helper()
		op, err := sql.Instances.Insert(id, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: version,
			Region: "us-central1", RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
		if err != nil {
			t.Fatalf("instances.insert: %v", err)
		}
		waitSQLOp(t, sql, id, op)
	}
	if useSQL {
		sql = sqlAdminService(t, h)
		newInstance()
		t.Cleanup(func() {
			if op, err := sql.Instances.Delete(id, inst).Do(); err == nil {
				waitSQLOp(t, sql, id, op)
			}
		})
		op, err := sql.Databases.Insert(id, inst, &sqladmin.Database{Name: "purgeddb"}).Do()
		if err != nil {
			t.Fatalf("databases.insert: %v", err)
		}
		waitSQLOp(t, sql, id, op)
		op, err = sql.Users.Insert(id, inst, &sqladmin.User{Name: "purgeduser", Password: "Purged-pass1"}).Do()
		if err != nil {
			t.Fatalf("users.insert: %v", err)
		}
		waitSQLOp(t, sql, id, op)
		op, err = sql.BackupRuns.Insert(id, inst, &sqladmin.BackupRun{}).Do()
		if err != nil {
			t.Fatalf("backupRuns.insert: %v", err)
		}
		waitSQLOp(t, sql, id, op)
		for _, in := range []string{inst, upInstance} {
			if _, err := sql.SslCerts.Insert(id, in, &sqladmin.SslCertsInsertRequest{CommonName: "purged"}).Do(); err != nil {
				t.Fatalf("sslCerts.insert on %s: %v", in, err)
			}
		}
	}

	// Backup and DR, of that instance.
	var bdr *backupdr.Service
	var wait func(*backupdr.Operation, error) *backupdr.Operation
	loc := parent + "/locations/us-central1"
	vault := loc + "/backupVaults/purged"
	plan := loc + "/backupPlans/purged"
	assoc := loc + "/backupPlanAssociations/purged"
	trial := parent + "/locations/us/trial"
	newVault := func() {
		t.Helper()
		wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{
			BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId("purged").Do())
	}
	if useDR {
		bdr = backupDRService(t, h)
		wait = backupDRWaiter(t, bdr)
		newVault()
		t.Cleanup(func() {
			l := bdr.Projects.Locations
			_, _ = l.BackupPlanAssociations.Delete(assoc).Do()
			_, _ = l.BackupPlans.Delete(plan).Do()
			_, _ = l.BackupVaults.Delete(vault).Force(true).Do()
		})
		wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vault,
			ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("purged").Do())
		wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{
			BackupPlan: plan, Resource: parent + "/instances/" + inst,
			ResourceType: "sqladmin.googleapis.com/Instance"}).BackupPlanAssociationId("purged").Do())
		wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assoc,
			&backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
		if _, err := bdr.Projects.Locations.Trial.Subscribe(parent+"/locations/us", &backupdr.SubscribeTrialRequest{}).Do(); err != nil {
			t.Fatalf("trial.subscribe: %v", err)
		}
	}

	// Artifact Registry.
	arLoc := parent + "/locations/us-central1"
	repo := arLoc + "/repositories/purged"
	settings := parent + "/projectSettings"
	vpcsc := arLoc + "/vpcscConfig"
	newRepo := func() {
		t.Helper()
		ar, ctx := arClient(t, h)
		op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: arLoc, RepositoryId: "purged",
			Repository: &arpb.Repository{Format: arpb.Repository_GENERIC}})
		if err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if useAR {
		ar, actx := arClient(t, h)
		newRepo()
		t.Cleanup(func() {
			if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repo}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		if code, b := genericUpload(t, h, repo, "tool", "1.0.0", "tool.tar.gz", []byte("purged")); code != http.StatusOK {
			t.Fatalf("upload = %d %s", code, b)
		}
		if _, err := ar.CreateRule(actx, &arpb.CreateRuleRequest{Parent: repo, RuleId: "deny", Rule: &arpb.Rule{
			Action: arpb.Rule_DENY, Operation: arpb.Rule_DOWNLOAD, PackageId: "tool"}}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
		if _, err := ar.UpdateProjectSettings(actx, &arpb.UpdateProjectSettingsRequest{ProjectSettings: &arpb.ProjectSettings{
			Name: settings, PullPercent: 25}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pull_percent"}}}); err != nil {
			t.Fatalf("UpdateProjectSettings: %v", err)
		}
		if _, err := ar.UpdateVPCSCConfig(actx, &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{
			Name: vpcsc, VpcscPolicy: arpb.VPCSCConfig_ALLOW}}); err != nil {
			t.Fatalf("UpdateVPCSCConfig: %v", err)
		}
	}

	// What reads back: every piece that is there. With the instance, the
	// vault and the repository made again, what is under them.
	left := func(again bool) []string {
		t.Helper()
		var out []string
		if useSQL {
			if _, err := sql.Instances.Get(id, inst).Do(); err == nil && !again {
				out = append(out, "Cloud SQL instance")
			} else if err != nil && !strings.Contains(err.Error(), "404") {
				t.Fatalf("instances.get: %v", err)
			}
			if dbs, err := sql.Databases.List(id, inst).Do(); err == nil {
				for _, d := range dbs.Items {
					if d.Name == "purgeddb" {
						out = append(out, "database")
					}
				}
			}
			if us, err := sql.Users.List(id, inst).Do(); err == nil {
				for _, u := range us.Items {
					if u.Name == "purgeduser" {
						out = append(out, "user")
					}
				}
			}
			if bs, err := sql.Backups.ListBackups(parent).Do(); err != nil {
				t.Fatalf("backups.list: %v", err)
			} else if len(bs.Backups) > 0 {
				out = append(out, "backup")
			}
			for _, in := range []string{inst, upInstance} {
				if cs, err := sql.SslCerts.List(id, in).Do(); err == nil {
					for _, c := range cs.Items {
						if c.CommonName == "purged" {
							out = append(out, "client certificate on "+in)
						}
					}
				}
			}
			if !again {
				if ops, err := sql.Operations.List(id).Do(); err != nil {
					t.Fatalf("operations.list: %v", err)
				} else if len(ops.Items) > 0 {
					out = append(out, "Cloud SQL operations")
				}
			}
		}
		if useDR {
			l := bdr.Projects.Locations
			if _, err := l.BackupVaults.Get(vault).Do(); err == nil && !again {
				out = append(out, "backup vault")
			}
			if ds, err := l.BackupVaults.DataSources.List(vault).Do(); err == nil && len(ds.DataSources) > 0 {
				out = append(out, "data source")
			}
			if _, err := l.BackupPlans.Get(plan).Do(); err == nil {
				out = append(out, "backup plan")
			}
			if _, err := l.BackupPlanAssociations.Get(assoc).Do(); err == nil {
				out = append(out, "backup plan association")
			}
			if tr, err := l.GetTrial(trial).Do(); err != nil {
				t.Fatalf("getTrial: %v", err)
			} else if tr.State != "ELIGIBLE" {
				out = append(out, "trial")
			}
			if !again {
				if ops, err := l.Operations.List(loc).Do(); err != nil {
					t.Fatalf("operations.list: %v", err)
				} else if len(ops.Operations) > 0 {
					out = append(out, "Backup and DR operations")
				}
			}
		}
		if useAR {
			ar, actx := arClient(t, h)
			if _, err := ar.GetRepository(actx, &arpb.GetRepositoryRequest{Name: repo}); err == nil && !again {
				out = append(out, "repository")
			} else if err != nil && status.Code(err) != codes.NotFound {
				t.Fatalf("GetRepository: %v", err)
			}
			if ps, err := ar.ListPackages(actx, &arpb.ListPackagesRequest{Parent: repo}).Next(); err == nil && ps != nil {
				out = append(out, "package")
			}
			if f, err := ar.ListFiles(actx, &arpb.ListFilesRequest{Parent: repo}).Next(); err == nil && f != nil {
				out = append(out, "file")
			}
			if r, err := ar.ListRules(actx, &arpb.ListRulesRequest{Parent: repo}).Next(); err == nil && r != nil {
				out = append(out, "rule")
			}
			if s, err := ar.GetProjectSettings(actx, &arpb.GetProjectSettingsRequest{Name: settings}); err != nil {
				t.Fatalf("GetProjectSettings: %v", err)
			} else if s.GetPullPercent() != 0 {
				out = append(out, "project settings")
			}
			if c, err := ar.GetVPCSCConfig(actx, &arpb.GetVPCSCConfigRequest{Name: vpcsc}); err != nil {
				t.Fatalf("GetVPCSCConfig: %v", err)
			} else if c.GetVpcscPolicy() == arpb.VPCSCConfig_ALLOW {
				out = append(out, "VPC-SC config")
			}
		}
		return out
	}
	want := 0
	if useSQL {
		want += 7
	}
	if useDR {
		want += 6
	}
	if useAR {
		want += 6
	}
	if got := left(false); len(got) != want {
		t.Fatalf("set up %v, want %d pieces of state", got, want)
	}

	// Deleted and purged, and made again under the ID.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil); code != http.StatusOK {
		t.Fatalf("POST /admin/purge = %d %s", code, body)
	}
	create()
	if got := left(false); len(got) != 0 {
		t.Errorf("a project created again under a purged ID reads the old project's %v", got)
	}
	if kc := os.Getenv(envKubeconfig); useSQL && kc != "" {
		out, err := exec.Command("kubectl", "--kubeconfig", kc, "-n", "cloudburrow", "get",
			"deployment/sqlinst-"+inst, "persistentvolumeclaim/sqlinst-"+inst+"-data", "--ignore-not-found", "-o", "name").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("the purged instance's server and volume: %v %s", err, out)
		}
	}

	// The same names, made again, start empty.
	if useSQL {
		newInstance()
	}
	if useDR {
		newVault()
	}
	if useAR {
		newRepo()
	}
	if got := left(true); len(got) != 0 {
		t.Errorf("made again under the old names, they read the old project's %v", got)
	}
}
