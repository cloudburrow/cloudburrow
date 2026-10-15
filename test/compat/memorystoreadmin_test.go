//go:build compat

package compat

import (
	"os"
	"strconv"
	"strings"
	"testing"

	redisadmin "cloud.google.com/go/redis/apiv1"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// EnvMemorystoreAdmin is the Memorystore admin API endpoint (#1302), and
// EnvMemorystoreProject the project its one instance is in.
const (
	EnvMemorystoreAdmin   = "CLOUDBURROW_TEST_MEMORYSTORE_ADMIN"
	EnvMemorystoreProject = "CLOUDBURROW_TEST_MEMORYSTORE_PROJECT"
)

// covers: google.cloud.redis.v1.CloudRedis/ListInstances, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/GetInstanceAuthString
//
// TestMemorystoreAdminDescribesTheServer (#1302), with cloud.google.com/go/
// redis/apiv1: the one instance is listed and described as the Valkey
// server `--services memorystore` runs — its host and port are the RESP
// endpoint, which a Redis client then reaches, READY, REDIS_7_2 and 1 GB —
// AUTH is off, and it cannot be deleted.
//
// unverified: google.cloud.redis.v1.CloudRedis/GetInstanceAuthString FAILED_PRECONDITION: an instance with AUTH disabled
// unverified: google.cloud.redis.v1.CloudRedis/DeleteInstance FAILED_PRECONDITION: the instance `up` runs (CloudBurrow's own)
func TestMemorystoreAdminDescribesTheServer(t *testing.T) {
	h := New(t)
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	ctx := h.Context()
	c, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(h.Endpoint(EnvMemorystoreAdmin)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var listed []*redispb.Instance
	it := c.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"})
	for {
		inst, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		listed = append(listed, inst)
	}
	name := "projects/" + project + "/locations/us-central1/instances/memorystore"
	found := false
	for _, l := range listed {
		found = found || l.GetName() == name
	}
	if !found {
		t.Fatalf("ListInstances listed %d instances, without the server `up` runs, %s", len(listed), name)
	}

	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY || inst.GetRedisVersion() != "REDIS_7_2" ||
		inst.GetMemorySizeGb() != 1 || inst.GetTier() != redispb.Instance_BASIC || inst.GetAuthEnabled() {
		t.Errorf("GetInstance = %v", inst)
	}
	// The host and port are the server's: a Redis client at that address
	// reaches the server the RESP endpoint does.
	resp := h.Endpoint(EnvMemorystore)
	if got := inst.GetHost() + ":" + strconv.Itoa(int(inst.GetPort())); got != resp {
		t.Errorf("host:port = %s, want the RESP endpoint %s", got, resp)
	}
	rc := redisClient(t, h)
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Errorf("PING at the described address: %v", err)
	}

	// Another project has no instance.
	other := c.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + h.Project() + "/locations/us-central1"})
	if _, err := other.Next(); err != iterator.Done {
		t.Errorf("ListInstances in another project = %v, want none", err)
	}
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: "projects/" + project + "/locations/us-central1/instances/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance(unknown) = %v, want NOT_FOUND", err)
	}
	if _, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetInstanceAuthString with AUTH off = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := c.DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance(the server up runs) = %v, want FAILED_PRECONDITION", err)
	}
}
