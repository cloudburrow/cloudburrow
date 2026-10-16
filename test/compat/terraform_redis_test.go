//go:build compat

package compat

import (
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance, google.cloud.redis.v1.CloudRedis/GetInstanceAuthString
//
// TestTerraformRedisInstance (#1302): a google_redis_instance with AUTH
// applies with the official hashicorp/google provider through `cloudburrow
// terraform`, which sets redis_custom_endpoint to the Memorystore admin
// API's v1 path. The official Go client reads it back, go-redis reaches it
// at its host and port with the auth_string the provider read, the second
// plan is empty, a change of size, labels and the RDB persistence_config's
// snapshot period (#1384) applies in place, and after
// destroy the instance is gone.
func TestTerraformRedisInstance(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := fmt.Sprintf("tf-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	module := func(size int, env, period string) string {
		return fmt.Sprintf(`
resource "google_redis_instance" "cache" {
  project        = %q
  region         = "us-central1"
  name           = %q
  tier           = "BASIC"
  memory_size_gb = %d
  auth_enabled   = true
  display_name   = "tf cache"
  labels         = { env = %q }
  redis_configs  = { "maxmemory-policy" = "allkeys-lru" }
  persistence_config {
    persistence_mode    = "RDB"
    rdb_snapshot_period = %q
  }
}
output "auth" {
  value     = google_redis_instance.cache.auth_string
  sensitive = true
}
`, m.project, id, size, env, period)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(module(1, "dev", "TWELVE_HOURS"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after apply: %v", err)
	}
	if inst.GetMemorySizeGb() != 1 || !inst.GetAuthEnabled() || inst.GetLabels()["env"] != "dev" || inst.GetDisplayName() != "tf cache" ||
		inst.GetPersistenceConfig().GetRdbSnapshotPeriod() != redispb.PersistenceConfig_TWELVE_HOURS {
		t.Errorf("after apply, the instance reads back as %v", inst)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if out := m.must("output", "-raw", "auth"); out != auth.GetAuthString() {
		t.Error("the provider's auth_string is not the instance's")
	}
	rc := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))), Password: auth.GetAuthString()})
	defer rc.Close()
	if err := rc.Set(ctx, "tf", "1", 0).Err(); err != nil {
		t.Errorf("SET at the instance's address: %v", err)
	}
	m.planClean()

	m.write(module(2, "prod", "ONE_HOUR"))
	m.apply()
	inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetMemorySizeGb() != 2 || inst.GetLabels()["env"] != "prod" ||
		inst.GetPersistenceConfig().GetRdbSnapshotPeriod() != redispb.PersistenceConfig_ONE_HOUR {
		t.Errorf("after the update, GetInstance = %v, %v", inst, err)
	}
	if v, err := rc.Get(ctx, "tf").Result(); err != nil || v != "1" {
		t.Errorf("after the in-place update GET = %q, %v; want the key kept", v, err)
	}

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance
//
// TestTerraformRedisStandardTier (#1385): a google_redis_instance of tier
// STANDARD_HA with read replicas and replica_count 2 applies with the
// official provider; the official client reads three nodes and a read
// endpoint, which the provider's read_endpoint output names, the second
// plan is empty, replica_count 3 applies in place with a fourth node, and
// destroy removes it.
func TestTerraformRedisStandardTier(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := fmt.Sprintf("tf-ha-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	module := func(replicas int) string {
		return fmt.Sprintf(`
resource "google_redis_instance" "ha" {
  project                 = %q
  region                  = "us-central1"
  name                    = %q
  tier                    = "STANDARD_HA"
  memory_size_gb          = 5
  location_id             = "us-central1-a"
  alternative_location_id = "us-central1-b"
  read_replicas_mode      = "READ_REPLICAS_ENABLED"
  replica_count           = %d
}
output "read_endpoint" {
  value = google_redis_instance.ha.read_endpoint
}
`, m.project, id, replicas)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(module(2))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after apply: %v", err)
	}
	if inst.GetTier() != redispb.Instance_STANDARD_HA || inst.GetReplicaCount() != 2 || len(inst.GetNodes()) != 3 || inst.GetReadEndpoint() == "" {
		t.Errorf("after apply, the instance reads back as %v", inst)
	}
	if out := m.must("output", "-raw", "read_endpoint"); out != inst.GetReadEndpoint() {
		t.Errorf("the provider's read_endpoint is %q, the instance's %q", out, inst.GetReadEndpoint())
	}
	m.planClean()
	m.write(module(3))
	m.apply()
	if inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil || inst.GetReplicaCount() != 3 || len(inst.GetNodes()) != 4 {
		t.Errorf("after replica_count 3, GetInstance = %v, %v", inst, err)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance
//
// TestTerraformRedisInTransitEncryption (#1413): a google_redis_instance
// with transit_encryption_mode SERVER_AUTHENTICATION applies with the
// official provider, its server_ca_certs output is the instance's CA, a
// go-redis TLS client trusting it reaches the instance, the second plan
// is empty, and destroy removes it.
func TestTerraformRedisInTransitEncryption(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := fmt.Sprintf("tf-tls-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(fmt.Sprintf(`
resource "google_redis_instance" "tls" {
  project                 = %q
  region                  = "us-central1"
  name                    = %q
  tier                    = "BASIC"
  memory_size_gb          = 1
  transit_encryption_mode = "SERVER_AUTHENTICATION"
}
output "ca" {
  value = google_redis_instance.tls.server_ca_certs[0].cert
}
`, m.project, id))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after apply: %v", err)
	}
	if inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
		t.Fatalf("after apply, the instance reads back as %v", inst)
	}
	if out := m.must("output", "-raw", "ca"); out != inst.GetServerCaCerts()[0].GetCert() {
		t.Errorf("the provider's server_ca_certs[0].cert is not the instance's CA: %q", out)
	}
	rc := tlsRedisAt(t, inst, "")
	defer rc.Close()
	if err := rc.Set(ctx, "tf", "1", 0).Err(); err != nil {
		t.Errorf("SET over TLS: %v", err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}
