//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	redisadmin "cloud.google.com/go/redis/apiv1"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestAPurgedProjectsDataGoesEverywhere (#1540) is TestAPurgedProjectsIDStartsEmpty
// for the services whose state it does not reach: where the instance serves
// them, a project gets a Firestore document, a Datastore entity, a Spanner
// instance with a database, a Bigtable instance with a table, a Cloud Run
// job and a Memorystore instance, through the official clients. Deleted and undeleted it keeps them; deleted, purged
// (POST /admin/purge) and created again under the ID, none reads back.
//
// It also writes, through the data-plane clients alone, what the emulators
// cannot list by project (#1550): a Firestore document in a database the
// Admin API never made, a Datastore entity in a named database, and a
// Bigtable row in a table under an instance the instance admin never made
// (a table needs the table admin's CreateTable). And the project's BigQuery
// service agent names the project's number, the re-created project's new
// one after the purge (#1551).
func TestAPurgedProjectsDataGoesEverywhere(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	served := func(env string) bool {
		if strings.TrimSpace(os.Getenv(env)) == "" {
			return false
		}
		h.Endpoint(env)
		return true
	}
	useFirestore, useDatastore := served(EnvFirestore), served(EnvDatastore)
	useSpanner, useBigtable := served(EnvSpanner), served(EnvBigtable)
	useRun, useMemorystore := served(EnvRun), served(EnvMemorystoreAdmin)
	useBigQuery := served(EnvBigQuery)
	if !useFirestore && !useDatastore && !useSpanner && !useBigtable && !useRun && !useMemorystore && !useBigQuery {
		t.Skip("the instance serves none of Firestore, Datastore, Spanner, Bigtable, Cloud Run, Memorystore and BigQuery")
	}

	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	id := v1ID(h, "pe-")
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
	found := func(kind string, err error) bool {
		t.Helper()
		switch status.Code(err) {
		case codes.OK:
			return true
		case codes.NotFound:
			return false
		}
		t.Fatalf("reading the %s: %v", kind, err)
		return false
	}

	var extras []purgeExtra
	if useFirestore {
		t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
		fc, err := firestore.NewClient(ctx, id)
		if err != nil {
			t.Fatalf("firestore.NewClient: %v", err)
		}
		t.Cleanup(func() { _ = fc.Close() })
		doc := fc.Doc("purged/one")
		if _, err := doc.Set(ctx, map[string]any{"v": 1}); err != nil {
			t.Fatalf("Firestore Set: %v", err)
		}
		extras = append(extras, purgeExtra{"Firestore document", func() bool {
			_, err := doc.Get(ctx)
			return found("Firestore document", err)
		}})
		// #1550: a database the Admin API never made.
		nc, err := firestore.NewClientWithDatabase(ctx, id, "never-made")
		if err != nil {
			t.Fatalf("firestore.NewClientWithDatabase: %v", err)
		}
		t.Cleanup(func() { _ = nc.Close() })
		named := nc.Doc("purged/two")
		if _, err := named.Set(ctx, map[string]any{"v": 2}); err != nil {
			t.Fatalf("Firestore Set in never-made: %v", err)
		}
		extras = append(extras, purgeExtra{"Firestore document in a database the Admin API never made", func() bool {
			_, err := named.Get(ctx)
			return found("Firestore document in never-made", err)
		}})
	}
	if useDatastore {
		dc := datastoreClient(t, h, id)
		key := datastore.NameKey("Purged", "one", nil)
		if _, err := dc.Put(ctx, key, &struct{ V int }{1}); err != nil {
			t.Fatalf("Datastore Put: %v", err)
		}
		extras = append(extras, purgeExtra{"Datastore entity", func() bool {
			var v struct{ V int }
			err := dc.Get(ctx, key, &v)
			if err == datastore.ErrNoSuchEntity {
				return false
			}
			if err != nil {
				t.Fatalf("Datastore Get: %v", err)
			}
			return true
		}})
		// #1550: a named database, in a namespace.
		nc, err := datastore.NewClientWithDatabase(ctx, id, "named-db")
		if err != nil {
			t.Fatalf("datastore.NewClientWithDatabase: %v", err)
		}
		t.Cleanup(func() { _ = nc.Close() })
		nkey := datastore.NameKey("Purged", "two", nil)
		nkey.Namespace = "ns"
		if _, err := nc.Put(ctx, nkey, &struct{ V int }{2}); err != nil {
			t.Fatalf("Datastore Put in named-db: %v", err)
		}
		extras = append(extras, purgeExtra{"Datastore entity in a named database", func() bool {
			var v struct{ V int }
			err := nc.Get(ctx, nkey, &v)
			if err == datastore.ErrNoSuchEntity {
				return false
			}
			if err != nil {
				t.Fatalf("Datastore Get in named-db: %v", err)
			}
			return true
		}})
	}
	if useSpanner {
		addr := h.Endpoint(EnvSpanner)
		spannerDatabase(t, ctx, addr, id, "purged", "purgeddb",
			`CREATE TABLE Items (Id INT64 NOT NULL) PRIMARY KEY (Id)`)
		ic, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ic.Close() })
		inst := parent + "/instances/purged"
		t.Cleanup(func() {
			_ = ic.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: inst})
		})
		extras = append(extras, purgeExtra{"Spanner instance", func() bool {
			_, err := ic.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: inst})
			return found("Spanner instance", err)
		}})
	}
	if useBigtable {
		t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
		iac, err := bigtable.NewInstanceAdminClient(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = iac.Close() })
		if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "purged", DisplayName: "purged",
			ClusterId: "purged-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
			t.Fatalf("Bigtable CreateInstance: %v", err)
		}
		t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), "purged") })
		ac, err := bigtable.NewAdminClient(ctx, id, "purged")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ac.Close() })
		if err := ac.CreateTable(ctx, "purged"); err != nil {
			t.Fatalf("Bigtable CreateTable: %v", err)
		}
		extras = append(extras, purgeExtra{"Bigtable instance", func() bool {
			_, err := iac.InstanceInfo(ctx, "purged")
			return found("Bigtable instance", err)
		}}, purgeExtra{"Bigtable table", func() bool {
			_, err := ac.TableInfo(ctx, "purged")
			return found("Bigtable table", err)
		}})
		// #1550: a table under an instance the instance admin never made,
		// with a row the data client wrote.
		gac, err := bigtable.NewAdminClient(ctx, id, "never-made")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gac.Close() })
		if err := gac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "ghost",
			Families: map[string]bigtable.GCPolicy{"f": bigtable.MaxVersionsPolicy(1)}}); err != nil {
			t.Fatalf("Bigtable CreateTable under never-made: %v", err)
		}
		t.Cleanup(func() { _ = gac.DeleteTable(context.Background(), "ghost") })
		dc, err := bigtable.NewClient(ctx, id, "never-made")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dc.Close() })
		mut := bigtable.NewMutation()
		mut.Set("f", "c", bigtable.Now(), []byte("v"))
		if err := dc.Open("ghost").Apply(ctx, "row", mut); err != nil {
			t.Fatalf("Bigtable Apply under never-made: %v", err)
		}
		extras = append(extras, purgeExtra{"Bigtable row under an instance never made", func() bool {
			row, err := dc.Open("ghost").ReadRow(ctx, "row")
			if status.Code(err) == codes.NotFound {
				return false
			}
			if err != nil {
				t.Fatalf("Bigtable ReadRow under never-made: %v", err)
			}
			return len(row) > 0
		}})
	}

	if useRun {
		jc, err := run.NewJobsClient(ctx, runClientOptions(h)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = jc.Close() })
		job := parent + "/locations/us-central1/jobs/purged"
		op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent + "/locations/us-central1", JobId: "purged",
			Job: &runpb.Job{Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{
				Containers: []*runpb.Container{{Image: "docker.io/library/busybox:1.36"}}}}}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("Run CreateJob: %v", err)
		}
		t.Cleanup(func() {
			if op, err := jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: job}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		extras = append(extras, purgeExtra{"Cloud Run job", func() bool {
			_, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: job})
			return found("Cloud Run job", err)
		}})
	}
	if useMemorystore {
		rc, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(h.Endpoint(EnvMemorystoreAdmin)),
			option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		inst := parent + "/locations/us-central1/instances/purged"
		op, err := rc.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent + "/locations/us-central1", InstanceId: "purged",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("Memorystore CreateInstance: %v", err)
		}
		t.Cleanup(func() {
			if op, err := rc.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: inst}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		extras = append(extras, purgeExtra{"Memorystore instance", func() bool {
			_, err := rc.GetInstance(ctx, &redispb.GetInstanceRequest{Name: inst})
			return found("Memorystore instance", err)
		}})
	}

	// #1551: the BigQuery service agent names the project's number.
	agentNumber := func() (agent, number string) {
		t.Helper()
		if !useBigQuery {
			return "", ""
		}
		p, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: parent})
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		number = strings.TrimPrefix(p.GetName(), "projects/")
		svc := bigqueryService(t, h)
		want := "bq-" + number + "@bigquery-encryption.iam.gserviceaccount.com"
		// The host sends the front the numbers as a project is made.
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
			got, err := svc.Projects.GetServiceAccount(id).Context(ctx).Do()
			if err != nil {
				t.Fatalf("projects.getServiceAccount: %v", err)
			}
			if agent = got.Email; agent == want || time.Now().After(deadline) {
				return agent, number
			}
		}
	}
	firstAgent, firstNumber := agentNumber()
	if want := "bq-" + firstNumber + "@bigquery-encryption.iam.gserviceaccount.com"; useBigQuery && firstAgent != want {
		t.Errorf("the BigQuery service agent = %s, want %s", firstAgent, want)
	}

	left := func() []string {
		t.Helper()
		var out []string
		for _, e := range extras {
			if e.exists() {
				out = append(out, e.kind)
			}
		}
		return out
	}
	if got := left(); len(got) != len(extras) {
		t.Fatalf("set up %v, want all %d", got, len(extras))
	}
	t.Logf("set up in %s: %v", id, left())

	// Deleted and undeleted: kept.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := pc.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("UndeleteProject: %v", err)
	}
	if got := left(); len(got) != len(extras) {
		t.Errorf("after undelete only %v read back, want all %d", got, len(extras))
	}

	// Deleted, purged and made again.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil); code != http.StatusOK {
		t.Fatalf("POST /admin/purge = %d %s", code, body)
	}
	create()
	if got := left(); len(got) != 0 {
		t.Errorf("a project created again under a purged ID reads the old project's %v", got)
	}
	if useBigQuery {
		agent, number := agentNumber()
		if want := "bq-" + number + "@bigquery-encryption.iam.gserviceaccount.com"; agent != want || number == firstNumber {
			t.Errorf("the re-created project's BigQuery service agent = %s, want %s, a number not the purged project's %s", agent, want, firstNumber)
		}
	}
}

// TestAProjectResetDropsItsBigQueryDatasets (#1540). The emulator serves the
// instance's one project, so a purged project never has a dataset; the
// project-scoped reset a purge runs is measured on that project instead:
// `POST /admin/reset?service=bigquery&project=` deletes its datasets with
// their tables, through the official client.
func TestAProjectResetDropsItsBigQueryDatasets(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := c.Dataset(strings.ReplaceAll(v1ID(h, "pr_"), "-", "_"))
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	if err := ds.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if code, body := adminReset(t, control, "service=bigquery&project="+project); code != http.StatusOK {
		t.Fatalf("reset = %d %s", code, body)
	}
	_, err := ds.Metadata(ctx)
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != http.StatusNotFound {
		t.Errorf("the dataset after the project's reset: %v, want 404", err)
	}
}
