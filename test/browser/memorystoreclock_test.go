//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	"cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMemorystoreReleaseAndClockThroughThePage (#1919), in the memorystore
// shard. Google's Memorystore console has no release or clock control, so
// CloudBurrow's are list actions labelled as its extensions. On the
// Memorystore for Redis list page: Release an update (CloudBurrow
// extension) for europe-west4 lists a Basic instance there, whose
// maintenance_schedule GetInstance then reads (an hour, a week on by the
// Memorystore clock, with no window); Advance Memorystore clock (CloudBurrow extension) by 8d,
// confirmed, answers with the clock's new time and applies the
// maintenance: the instance is READY with no maintenance_schedule.
func TestMemorystoreReleaseAndClockThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the maintenance is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx := context.Background()
	c, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	const region = "europe-west4"
	id := fmt.Sprintf("browser-rel-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/" + region + "/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + region, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/memorystore?project=" + project)
	rows := p.listAction("release-update", map[string]string{"region": region})
	if !strings.Contains(rows, name) {
		t.Errorf("Release an update answered %q; want %s among the resources", rows, name)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetMaintenanceSchedule() == nil {
		t.Fatalf("after Release an update: %v, %v", inst, err)
	}
	if s := inst.GetMaintenanceSchedule(); s.GetEndTime().AsTime().Sub(s.GetStartTime().AsTime()) != time.Hour {
		t.Errorf("maintenance_schedule %v: not an hour long", s)
	}
	rows = p.listAction("advance-memorystore-clock", map[string]string{"duration": "8d"})
	if !strings.Contains(rows, "Memorystore clock") {
		t.Errorf("Advance Memorystore clock answered %q", rows)
	}
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
		inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err == nil && inst.GetState() == redispb.Instance_READY && inst.GetMaintenanceSchedule() == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("after Advance Memorystore clock: %v, %v", inst, err)
		}
	}
}
