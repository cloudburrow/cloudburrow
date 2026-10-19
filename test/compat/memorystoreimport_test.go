//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// createBasic creates a Basic instance with AUTH and returns it, with a
// client of its server, deleting it when the test ends.
func createBasic(t *testing.T, h *Harness, c *redisadmin.CloudRedisClient, parent, id string, pc *redispb.PersistenceConfig) (*redispb.Instance, *redis.Client) {
	t.Helper()
	ctx := memorystoreWait(t)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true, PersistenceConfig: pc}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	rc := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))), Password: auth.GetAuthString()})
	t.Cleanup(func() { _ = rc.Close() })
	return inst, rc
}

func exportTo(name, uri string) *redispb.ExportInstanceRequest {
	return &redispb.ExportInstanceRequest{Name: name,
		OutputConfig: &redispb.OutputConfig{Destination: &redispb.OutputConfig_GcsDestination{GcsDestination: &redispb.GcsDestination{Uri: uri}}}}
}

func importFrom(name, uri string) *redispb.ImportInstanceRequest {
	return &redispb.ImportInstanceRequest{Name: name,
		InputConfig: &redispb.InputConfig{Source: &redispb.InputConfig_GcsSource{GcsSource: &redispb.GcsSource{Uri: uri}}}}
}

// covers: google.cloud.redis.v1.CloudRedis/ExportInstance, google.cloud.redis.v1.CloudRedis/ImportInstance
//
// TestMemorystoreExportImport (#1384), with cloud.google.com/go/redis/
// apiv1 and cloud.google.com/go/storage: ExportInstance writes the
// instance's data to a gs:// object as an RDB file (REDIS and its
// version); ImportInstance of that object into a second instance gives it
// the same keys, and an import into the first, after changes, overwrites
// them: a key set since is gone and a changed one is back. A file that is
// not an RDB fails the import's operation with INVALID_ARGUMENT and
// leaves the data as it was; a missing object is NOT_FOUND.
func TestMemorystoreExportImport(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	sc := instanceStorage(t, h)
	bh := bucket(t, h, sc)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	stamp := time.Now().UnixNano() % 1e8
	src, rc := createBasic(t, h, c, parent, fmt.Sprintf("export-%d", stamp), nil)
	if err := rc.Set(ctx, "k", "v1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rc.HSet(ctx, "h", "f", "x").Err(); err != nil {
		t.Fatal(err)
	}

	uri := "gs://" + bh.BucketName() + "/dumps/cache.rdb"
	eop, err := c.ExportInstance(ctx, exportTo(src.GetName(), uri))
	if err != nil {
		t.Fatalf("ExportInstance: %v", err)
	}
	if _, err := waitLRO(ctx, eop); err != nil {
		t.Fatalf("ExportInstance operation: %v", err)
	}
	if md, err := eop.Metadata(); err != nil || md.GetVerb() != "export" {
		t.Errorf("export metadata = %v, %v", md, err)
	}
	r, err := bh.Object("dumps/cache.rdb").NewReader(ctx)
	if err != nil {
		t.Fatalf("read the exported object: %v", err)
	}
	rdb, _ := io.ReadAll(r)
	r.Close()
	if !bytes.HasPrefix(rdb, []byte("REDIS00")) {
		t.Fatalf("the exported object starts %q, not an RDB header", rdb[:min(len(rdb), 9)])
	}

	// Into a second instance: the same keys.
	dst, rc2 := createBasic(t, h, c, parent, fmt.Sprintf("import-%d", stamp), nil)
	if err := rc2.Set(ctx, "only-here", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	iop, err := c.ImportInstance(ctx, importFrom(dst.GetName(), uri))
	if err != nil {
		t.Fatalf("ImportInstance: %v", err)
	}
	inst, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatalf("ImportInstance operation: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY {
		t.Errorf("after import the instance is %v", inst.GetState())
	}
	if v, err := rc2.Get(ctx, "k").Result(); err != nil || v != "v1" {
		t.Errorf("imported GET k = %q, %v", v, err)
	}
	if v, err := rc2.HGet(ctx, "h", "f").Result(); err != nil || v != "x" {
		t.Errorf("imported HGET h f = %q, %v", v, err)
	}
	if n, _ := rc2.Exists(ctx, "only-here").Result(); n != 0 {
		t.Error("the import kept a key the file does not have: an import overwrites")
	}

	// Back into the first, after changes: overwritten.
	rc.Set(ctx, "k", "changed", 0)
	rc.Set(ctx, "new", "1", 0)
	iop, err = c.ImportInstance(ctx, importFrom(src.GetName(), uri))
	if err != nil {
		t.Fatalf("ImportInstance: %v", err)
	}
	if _, err := waitLRO(ctx, iop); err != nil {
		t.Fatalf("ImportInstance operation: %v", err)
	}
	if v, _ := rc.Get(ctx, "k").Result(); v != "v1" {
		t.Errorf("after import GET k = %q, want v1", v)
	}
	if n, _ := rc.Exists(ctx, "new").Result(); n != 0 {
		t.Error("after import a key set since the export is still there")
	}

	w := bh.Object("not.rdb").NewWriter(ctx)
	w.Write([]byte("this is not an RDB file"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	iop, err = c.ImportInstance(ctx, importFrom(src.GetName(), "gs://"+bh.BucketName()+"/not.rdb"))
	if err != nil {
		t.Fatalf("ImportInstance of a bad file: %v", err)
	}
	if _, err := waitLRO(ctx, iop); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import of a bad file = %v, want INVALID_ARGUMENT", err)
	}
	if v, _ := rc.Get(ctx, "k").Result(); v != "v1" {
		t.Errorf("a refused import changed the data: GET k = %q", v)
	}
	if _, err := c.ImportInstance(ctx, importFrom(src.GetName(), "gs://"+bh.BucketName()+"/missing.rdb")); status.Code(err) != codes.NotFound {
		t.Errorf("import of a missing object = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance
//
// TestMemorystoreRDBPersistence (#1384): persistence_config RDB with a
// snapshot period, on create and through UpdateInstance's mask, reads back
// with rdb_next_snapshot_time aligned to the start time, and is the
// server's own: CONFIG GET save is the period with one change, without
// the append-only file, and a snapshot is taken at once (LASTSAVE). Back
// to DISABLED, the save point goes.
func TestMemorystoreRDBPersistence(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	inst, rc := createBasic(t, h, c, parent, fmt.Sprintf("rdb-%d", time.Now().UnixNano()%1e8),
		&redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB, RdbSnapshotPeriod: redispb.PersistenceConfig_SIX_HOURS})
	pc := inst.GetPersistenceConfig()
	if pc.GetPersistenceMode() != redispb.PersistenceConfig_RDB || pc.GetRdbSnapshotPeriod() != redispb.PersistenceConfig_SIX_HOURS ||
		pc.GetRdbSnapshotStartTime() == nil || !pc.GetRdbNextSnapshotTime().AsTime().After(time.Now()) {
		t.Errorf("created persistence_config = %v", pc)
	}
	if cfg, err := rc.ConfigGet(ctx, "save").Result(); err != nil || cfg["save"] != "21600 1" {
		t.Errorf("CONFIG GET save = %v, %v; want 21600 1", cfg, err)
	}

	rc.Set(ctx, "k", "v", 0)
	before, _ := rc.LastSave(ctx).Result()
	time.Sleep(1100 * time.Millisecond)
	op, err := c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistence_config"}},
		Instance: &redispb.Instance{Name: inst.GetName(), PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB,
			RdbSnapshotPeriod: redispb.PersistenceConfig_ONE_HOUR}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	got, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPersistenceConfig().GetRdbSnapshotPeriod() != redispb.PersistenceConfig_ONE_HOUR ||
		!got.GetPersistenceConfig().GetRdbSnapshotStartTime().AsTime().Equal(pc.GetRdbSnapshotStartTime().AsTime()) {
		t.Errorf("updated persistence_config = %v; want ONE_HOUR from the same start", got.GetPersistenceConfig())
	}
	cfg, _ := rc.ConfigGet(ctx, "*").Result()
	if cfg["save"] != "3600 1" || cfg["appendonly"] != "no" {
		t.Errorf("after the update save = %q, appendonly = %q", cfg["save"], cfg["appendonly"])
	}
	if after, _ := rc.LastSave(ctx).Result(); after <= before {
		t.Errorf("LASTSAVE %d not after %d: no snapshot was taken", after, before)
	}

	op, err = c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistence_config"}},
		Instance: &redispb.Instance{Name: inst.GetName(), PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_DISABLED}}})
	if err != nil {
		t.Fatalf("UpdateInstance DISABLED: %v", err)
	}
	if got, err = waitLRO(ctx, op); err != nil || got.GetPersistenceConfig().GetPersistenceMode() != redispb.PersistenceConfig_DISABLED {
		t.Errorf("DISABLED = %v, %v", got.GetPersistenceConfig(), err)
	}
	if cfg, _ := rc.ConfigGet(ctx, "save").Result(); cfg["save"] != "" {
		t.Errorf("DISABLED save = %q", cfg["save"])
	}
}
