package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/datamigration"
)

// unreachableDMSBackend creates destinations and reaches no source: every
// source query is refused as a connection the source did not answer.
type unreachableDMSBackend struct {
	mu      sync.Mutex
	created []string
	deleted []string
}

func (b *unreachableDMSBackend) CreateCloudSQL(_ context.Context, t datamigration.Target, _ *dmspb.CloudSqlSettings, _ datamigration.Source) (datamigration.CloudSQLAddresses, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created = append(b.created, t.ID)
	return datamigration.CloudSQLAddresses{PublicIP: "172.18.0.9"}, nil
}

func (b *unreachableDMSBackend) CreateAlloyDB(_ context.Context, t datamigration.Target, _ *dmspb.AlloyDbSettings) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created = append(b.created, t.ID)
	return "POSTGRES_16", nil
}

func (b *unreachableDMSBackend) Delete(_ context.Context, t datamigration.Target) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, t.ID)
	return nil
}

func (*unreachableDMSBackend) QuerySource(context.Context, datamigration.Target, datamigration.Source, string, string) ([][]string, error) {
	return nil, &datamigration.ConnError{Msg: "could not connect to server: Connection refused"}
}

func (*unreachableDMSBackend) QueryTarget(context.Context, datamigration.Target, string, string) ([][]string, error) {
	return nil, nil
}

func (*unreachableDMSBackend) Migrate(context.Context, datamigration.Target, datamigration.Source, datamigration.Migration) error {
	return nil
}
func (*unreachableDMSBackend) Existing(context.Context, datamigration.Target) (string, error) {
	return "POSTGRES_16", nil
}
func (*unreachableDMSBackend) Demote(context.Context, datamigration.Target, datamigration.Source) error {
	return nil
}
func (*unreachableDMSBackend) Release(context.Context, datamigration.Source) error { return nil }
func (*unreachableDMSBackend) Pause(context.Context, datamigration.Target, bool) error { return nil }
func (*unreachableDMSBackend) Promote(context.Context, datamigration.Target) error     { return nil }
func (*unreachableDMSBackend) Reset(context.Context, datamigration.Target, datamigration.Source, []string) error {
	return nil
}

func formDefaults(fields []console.Field, over map[string]string) map[string]string {
	v := map[string]string{}
	for _, f := range fields {
		v[f.Name] = f.Default
	}
	for k, x := range over {
		v[k] = x
	}
	return v
}

// TestConsoleDataMigration (#2252): the Database Migration Service pages
// through their providers: a PostgreSQL source and a Cloud SQL destination
// profile are created and listed with their role, engine and host; the
// Create migration job form offers them as source and destination; the
// job is listed and opened, Test shows the source's refusal, Edit renames
// it, Start is offered only before it runs, Resume and Promote are not;
// Delete removes the job and the destination's profile; a private
// connection is created with a /29, a /28 refused, and deleted; the
// Conversion workspaces page says it is not served; AlloyDB's and Cloud
// SQL's pages carry Migrate only when the service runs.
func TestConsoleDataMigration(t *testing.T) {
	ctx := context.Background()
	const project = "dms-console"
	be := &unreachableDMSBackend{}
	api, err := datamigration.NewServer("", datamigration.Options{Backend: be, CloudSQL: true, AlloyDB: true,
		StaticIPs: func() []string { return []string{"172.18.0.2"} }})
	if err != nil {
		t.Fatal(err)
	}
	svc := &dataMigrationService{api: api}
	profiles, jobs, private := dmsProfilesProvider{svc: svc}, dmsJobsProvider{svc: svc}, dmsPrivateProvider{svc: svc}

	_, pf := profiles.CreateForm()
	src, err := profiles.Create(ctx, project, formDefaults(pf, map[string]string{"kind": "postgresql", "profileId": "pg-src",
		"displayName": "Orders source", "host": "10.1.2.3", "port": "5432", "username": "postgres", "password": "pw"}))
	if err != nil {
		t.Fatalf("create the source profile: %v", err)
	}
	fields := profiles.CreateFormFor(ctx, project)
	offered := false
	for _, f := range fields {
		if f.Name == "sourceId" {
			for _, o := range f.Options {
				offered = offered || o == src
			}
		}
	}
	if !offered {
		t.Error("the Cloud SQL destination form does not offer the source profile")
	}
	dst, err := profiles.Create(ctx, project, formDefaults(fields, map[string]string{"kind": "cloudsql", "profileId": "pg-dst",
		"sourceId": src, "version": "POSTGRES_16", "rootPassword": "Root-pass1"}))
	if err != nil {
		t.Fatalf("create the destination profile: %v", err)
	}
	if len(be.created) != 1 || be.created[0] != "pg-dst" {
		t.Errorf("the destination's Cloud SQL instance: %v", be.created)
	}
	l, err := profiles.List(ctx, project)
	if err != nil || len(l.Items) != 2 {
		t.Fatalf("Connection profiles = %+v, %v", l, err)
	}
	for _, r := range l.Items {
		want := map[string]string{"Role": "Source", "Hostname:Port": "10.1.2.3:5432"}
		if r.Name == dst {
			want = map[string]string{"Role": "Destination"}
		}
		for k, v := range want {
			if r.Fields[k] != v {
				t.Errorf("%s: %s = %q, want %q", r.Name, k, r.Fields[k], v)
			}
		}
		if !strings.Contains(r.Fields["Database engine"], "PostgreSQL") {
			t.Errorf("%s: engine %q", r.Name, r.Fields["Database engine"])
		}
	}

	jf := jobs.CreateFormFor(ctx, project)
	for _, f := range jf {
		if f.Name == "source" && (len(f.Options) != 1 || f.Options[0] != src) {
			t.Errorf("the job form's sources: %v", f.Options)
		}
		if f.Name == "destination" && (len(f.Options) != 1 || f.Options[0] != dst) {
			t.Errorf("the job form's destinations: %v", f.Options)
		}
	}
	job, err := jobs.Create(ctx, project, formDefaults(jf, map[string]string{"jobId": "orders", "displayName": "Orders", "source": src, "destination": dst}))
	if err != nil {
		t.Fatalf("Create migration job: %v", err)
	}
	jl, err := jobs.List(ctx, project)
	if err != nil || len(jl.Items) != 1 || jl.Items[0].Fields["Status"] == "" || jl.Items[0].Fields["Type"] == "" ||
		jl.Items[0].Fields["Source"] != "pg-src" || jl.Items[0].Fields["Destination"] != "pg-dst" {
		t.Fatalf("Migration jobs = %+v, %v", jl, err)
	}
	d, err := jobs.Detail(ctx, project, []string{job})
	if err != nil || d.Unavailable != "" || !strings.Contains(consoleDetailText(d), "172.18.0.2") {
		t.Errorf("the job's page = %+v, %v", d, err)
	}
	acts := map[string]console.Action{}
	for _, a := range jobs.DetailActions(ctx, project, []string{job}) {
		acts[a.ID] = a
	}
	if acts["start"].Unavailable != "" || acts["resume"].Unavailable == "" || acts["promote"].Unavailable == "" {
		t.Errorf("a job not started: start %q, resume %q, promote %q", acts["start"].Unavailable, acts["resume"].Unavailable, acts["promote"].Unavailable)
	}
	if err := jobs.ActAt(ctx, project, []string{job}, "verify", nil); err == nil || !strings.Contains(err.Error(), "CONNECTION_FAILURE") {
		t.Errorf("Test job against an unreachable source: %v", err)
	}
	if err := jobs.ActAt(ctx, project, []string{job}, "resume", nil); err == nil {
		t.Error("Resume was allowed on a job not started")
	}
	if err := jobs.ActAt(ctx, project, []string{job}, "edit", map[string]string{"displayName": "Orders v2"}); err != nil {
		t.Fatal(err)
	}
	if jl, _ := jobs.List(ctx, project); jl.Items[0].Fields["Migration job name"] != "Orders v2" {
		t.Errorf("after Edit: %+v", jl.Items[0].Fields)
	}
	if err := jobs.ActAt(ctx, project, []string{job}, "delete", map[string]string{"force": "on"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := profiles.List(ctx, project); len(l.Items) != 1 || l.Items[0].Name != src {
		t.Errorf("profiles after the job's delete: %+v", l.Items)
	}
	if err := profiles.ActAt(ctx, project, []string{src}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	_, cf := private.CreateForm()
	vpc := "projects/" + project + "/global/networks/default"
	if _, err := private.Create(ctx, project, formDefaults(cf, map[string]string{"id": "wide", "vpc": vpc, "subnet": "10.20.0.0/28"})); err == nil {
		t.Error("a /28 private connection was created")
	}
	pc, err := private.Create(ctx, project, formDefaults(cf, map[string]string{"id": "peer", "vpc": vpc, "subnet": "10.20.0.0/29"}))
	if err != nil {
		t.Fatal(err)
	}
	if pl, err := private.List(ctx, project); err != nil || len(pl.Items) != 1 || pl.Items[0].Fields["IP range"] != "10.20.0.0/29" {
		t.Errorf("Private connectivity = %+v, %v", pl, err)
	}
	if err := private.Delete(ctx, project, pc); err != nil {
		t.Fatal(err)
	}

	if cl, err := (dmsConversionProvider{}).List(ctx, project); err != nil || !strings.Contains(cl.Unavailable, "UNIMPLEMENTED") {
		t.Errorf("Conversion workspaces = %+v, %v", cl, err)
	}
	if a := (alloyDBProvider{migrate: true}).ListActions(ctx, project); len(a) != 1 || a[0].Href != dmsHref {
		t.Errorf("AlloyDB's Migrate: %+v", a)
	}
	if a := (cloudSQLInstancesProvider{}).ListActions(ctx, project); len(a) != 0 {
		t.Errorf("Cloud SQL without Database Migration Service offers %+v", a)
	}
}
