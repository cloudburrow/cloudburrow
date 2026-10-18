//go:build browser

package browser

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// TestMemorystoreValkeyMigrationThroughThePage (#1592), in the emulators
// shard. Two CLUSTER_DISABLED instances are made with the official client,
// the first standing in for a self-managed Valkey server with a key in it.
// On the second's page, Start migration with the source's address makes it
// MIGRATING with migration_config REPLICATION_ESTABLISHED, as GetInstance
// reads, and it serves the source's key; the page shows the migration and
// its source. Finish migration makes it ACTIVE and MIGRATED, and writable.
func TestMemorystoreValkeyMigrationThroughThePage(t *testing.T) {
	needService(t, "memorystore-valkey")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: every change is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := memorystore.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sfx := time.Now().UnixNano() % 1e7
	parent := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	create := func(id string) *memorystorepb.Instance {
		t.Helper()
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
			Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
			PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: network}}}})
		if err != nil {
			t.Fatal(err)
		}
		inst, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", id, err)
		}
		return inst
	}
	src, dst := create(fmt.Sprintf("br-mig-src-%d", sfx)), create(fmt.Sprintf("br-mig-dst-%d", sfx))
	primary := func(inst *memorystorepb.Instance) string {
		t.Helper()
		conns := inst.GetPscAutoConnections()
		for _, e := range inst.GetEndpoints() {
			for _, cn := range e.GetConnections() {
				conns = append(conns, cn.GetPscAutoConnection())
			}
		}
		for _, pc := range conns {
			if pc != nil && pc.GetConnectionType() == memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY {
				return net.JoinHostPort(pc.GetIpAddress(), strconv.Itoa(int(pc.GetPort())))
			}
		}
		t.Fatalf("instance %s has no primary endpoint: %v", inst.GetName(), inst.GetEndpoints())
		return ""
	}
	srcAddr := primary(src)
	sc := redis.NewClient(&redis.Options{Addr: srcAddr})
	dc := redis.NewClient(&redis.Options{Addr: primary(dst)})
	t.Cleanup(func() { _ = sc.Close(); _ = dc.Close() })
	if err := sc.Set(ctx, "carried", "from-the-source", 0).Err(); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(srcAddr)
	dstID := lastPathSegment(dst.GetName())

	p := open(t)
	p.navigate("/memorystore-valkey?project=" + project)
	p.clickText("#view tbody a", dstID)
	p.waitFor(fmt.Sprintf(`location.pathname.endsWith(%q)`, "/"+dstID))
	p.pageAction("Start migration", map[string]string{"ip": host, "port": port}, "")
	var got *memorystorepb.Instance
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		got, err = c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: dst.GetName()})
		if err == nil && got.GetMigrationConfig().GetState() == memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Start migration, GetInstance = state %v, migration_config %v, %v; want REPLICATION_ESTABLISHED", got.GetState(), got.GetMigrationConfig(), err)
		}
	}
	if got.GetState() != memorystorepb.Instance_MIGRATING || got.GetMigrationConfig().GetSelfManagedSource().GetIpAddress() != host ||
		strconv.Itoa(int(got.GetMigrationConfig().GetSelfManagedSource().GetPort())) != port {
		t.Errorf("after Start migration, GetInstance = state %v, migration_config %v; want MIGRATING from %s", got.GetState(), got.GetMigrationConfig(), srcAddr)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(500 * time.Millisecond) {
		if v, err := dc.Get(ctx, "carried").Result(); err == nil && v == "from-the-source" {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the migrating instance's GET carried = %q, %v; want the source's value", v, err)
		}
	}

	p.navigate("/memorystore-valkey/" + dstID + "?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) &&
		[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Finish migration")`, srcAddr))
	p.clickText("#view .page-actions button", "Finish migration")
	// Finish migration is confirmed by typing the instance's name back.
	var title string
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.eval(`document.querySelector(".modal #confirm-title").textContent`, &title)
	p.confirmByName(strings.TrimSuffix(strings.TrimPrefix(title, "Finish migration "), "?"))
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		got, err = c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: dst.GetName()})
		if err == nil && got.GetState() == memorystorepb.Instance_ACTIVE && got.GetMigrationConfig().GetState() == memorystorepb.MigrationConfig_MIGRATED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Finish migration, GetInstance = state %v, migration_config %v, %v; want ACTIVE and MIGRATED", got.GetState(), got.GetMigrationConfig(), err)
		}
	}
	if err := dc.Set(ctx, "mine", "1", 0).Err(); err != nil {
		t.Errorf("SET on the instance after Finish migration: %v", err)
	}
}
