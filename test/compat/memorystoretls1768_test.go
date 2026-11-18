//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/GetInstanceAuthString, google.cloud.redis.v1.CloudRedis/FailoverInstance
//
// TestMemorystoreInTransitEncryptionBefore60 (#1768), with
// cloud.google.com/go/redis/apiv1 and go-redis: REDIS_3_2, REDIS_4_0 and
// REDIS_5_0, which have no TLS of their own, are served in-transit
// encryption by an endpoint in front of the server, as Memorystore's are.
// Each instance reports SERVER_AUTHENTICATION and one server CA
// (PEM, serial, SHA-1 fingerprint, ten years) in CreateInstance and
// GetInstance; go-redis over TLS trusting that CA, with the instance's
// AUTH string (GetInstanceAuthString), reaches the old server itself
// (INFO redis_version), a plain TCP client is refused and a TLS client
// that does not trust the CA fails verification. A STANDARD_HA REDIS_5_0
// instance replicates over TLS (WAIT 1), and after FailoverInstance keeps
// its keys and replicates again.
func TestMemorystoreInTransitEncryptionBefore60(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	for _, tc := range []struct {
		version, release string
		tier             redispb.Instance_Tier
	}{
		{"REDIS_3_2", "3.2.", redispb.Instance_BASIC},
		{"REDIS_4_0", "4.0.", redispb.Instance_BASIC},
		{"REDIS_5_0", "5.0.", redispb.Instance_STANDARD_HA},
	} {
		id := fmt.Sprintf("oldtls-%s-%d", strings.ToLower(strings.ReplaceAll(tc.version, "_", "")), time.Now().UnixNano()%1e8)
		name := parent + "/instances/" + id
		op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
			Instance: &redispb.Instance{Tier: tc.tier, MemorySizeGb: 1, RedisVersion: tc.version, AuthEnabled: true,
				TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", tc.version, err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		inst, err := waitLRO(ctx, op)
		if err != nil {
			t.Fatalf("CreateInstance %s operation: %v", tc.version, err)
		}
		if inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
			t.Fatalf("%s: transit_encryption_mode %v, server_ca_certs %v", tc.version, inst.GetTransitEncryptionMode(), inst.GetServerCaCerts())
		}
		ca := inst.GetServerCaCerts()[0]
		if !strings.HasPrefix(ca.GetCert(), "-----BEGIN CERTIFICATE-----") || ca.GetSerialNumber() == "" || len(ca.GetSha1Fingerprint()) != 40 ||
			ca.GetExpireTime().AsTime().Before(time.Now().AddDate(9, 0, 0)) {
			t.Errorf("%s: the server CA reads back as %v", tc.version, ca)
		}
		got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil || got.GetServerCaCerts()[0].GetCert() != ca.GetCert() || got.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION {
			t.Errorf("%s: GetInstance: %v, %v", tc.version, got, err)
		}
		auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
		if err != nil || auth.GetAuthString() == "" {
			t.Fatalf("%s: GetInstanceAuthString: %v, %v", tc.version, auth, err)
		}
		rc := tlsRedisAt(t, inst, auth.GetAuthString())
		defer rc.Close()
		info, err := rc.Info(ctx, "server").Result()
		if err != nil || !strings.Contains(info, "redis_version:"+tc.release) {
			t.Fatalf("%s: INFO over TLS with the instance's CA: %v (want redis_version %s)", tc.version, err, tc.release)
		}
		if err := rc.Set(ctx, "k", "secret", 0).Err(); err != nil {
			t.Fatalf("%s: SET over TLS: %v", tc.version, err)
		}
		if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "secret" {
			t.Errorf("%s: GET over TLS = %q, %v", tc.version, v, err)
		}
		plain := redisAt(inst, auth.GetAuthString())
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		if err := plain.Ping(pctx).Err(); err == nil {
			t.Errorf("%s: a plain TCP client reached the instance", tc.version)
		}
		pcancel()
		plain.Close()
		untrusted := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))),
			Password: auth.GetAuthString(), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}})
		if err := untrusted.Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("%s: a TLS client without the CA: %v, want a certificate error", tc.version, err)
		}
		untrusted.Close()
		if tc.tier != redispb.Instance_STANDARD_HA {
			continue
		}
		eventuallyTrue(t, tc.version+": the replica syncs over TLS", func() bool {
			n, err := rc.Do(ctx, "WAIT", 1, 1000).Int()
			return err == nil && n >= 1
		})
		fop, err := c.FailoverInstance(ctx, &redispb.FailoverInstanceRequest{Name: name, DataProtectionMode: redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS})
		if err != nil {
			t.Fatalf("%s: FailoverInstance: %v", tc.version, err)
		}
		if inst, err = waitLRO(ctx, fop); err != nil {
			t.Fatalf("%s: FailoverInstance operation: %v", tc.version, err)
		}
		after := tlsRedisAt(t, inst, auth.GetAuthString())
		defer after.Close()
		eventuallyTrue(t, tc.version+": the new primary has the key", func() bool {
			v, err := after.Get(ctx, "k").Result()
			return err == nil && v == "secret"
		})
		if err := after.Set(ctx, "k2", "after", 0).Err(); err != nil {
			t.Fatalf("%s: SET after the failover: %v", tc.version, err)
		}
		eventuallyTrue(t, tc.version+": the old primary replicates the new one over TLS", func() bool {
			n, err := after.Do(ctx, "WAIT", 1, 1000).Int()
			return err == nil && n >= 1
		})
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestGcloudRedisInTransitEncryptionBefore60 (#1768): the real gcloud
// creates a REDIS_5_0 instance with
// `--transit-encryption-mode=server-authentication`; `describe` shows
// SERVER_AUTHENTICATION and the server CA, which a go-redis TLS
// client trusts to reach the Redis 5.0 server.
func TestGcloudRedisInTransitEncryptionBefore60(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("gcloud-oldtls-%d", time.Now().UnixNano()%1e8)
	p, region := "--project="+g.project, "--region=us-central1"
	name := "projects/" + g.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	g.must("redis", "instances", "create", id, p, region, "--size=1", "--tier=basic", "--redis-version=redis_5_0",
		"--transit-encryption-mode=server-authentication")
	if got := g.must("redis", "instances", "describe", id, p, region, "--format=value(transitEncryptionMode,redisVersion)"); strings.TrimSpace(got) != "SERVER_AUTHENTICATION\tREDIS_5_0" {
		t.Errorf("describe transitEncryptionMode, redisVersion = %q", got)
	}
	ca := g.must("redis", "instances", "describe", id, p, region, "--format=value(serverCaCerts[0].cert)")
	if !strings.Contains(ca, "BEGIN CERTIFICATE") {
		t.Fatalf("describe serverCaCerts[0].cert = %q", ca)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	inst.ServerCaCerts = []*redispb.TlsCertificate{{Cert: ca}} // the CA gcloud printed
	rc := tlsRedisAt(t, inst, "")
	defer rc.Close()
	if info, err := rc.Info(ctx, "server").Result(); err != nil || !strings.Contains(info, "redis_version:5.0.") {
		t.Errorf("INFO over TLS with the CA gcloud describes: %v", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance
//
// TestTerraformRedisInTransitEncryptionBefore60 (#1768): a
// google_redis_instance with redis_version REDIS_4_0 and
// transit_encryption_mode SERVER_AUTHENTICATION applies with the official
// provider, its server_ca_certs output is the instance's CA, a go-redis
// TLS client trusting it reaches the Redis 4.0 server, the second plan is
// empty, and destroy removes it.
func TestTerraformRedisInTransitEncryptionBefore60(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := fmt.Sprintf("tf-oldtls-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	m.write(fmt.Sprintf(`
resource "google_redis_instance" "tls" {
  project                 = %q
  region                  = "us-central1"
  name                    = %q
  tier                    = "BASIC"
  memory_size_gb          = 1
  redis_version           = "REDIS_4_0"
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
	if info, err := rc.Info(ctx, "server").Result(); err != nil || !strings.Contains(info, "redis_version:4.0.") {
		t.Errorf("INFO over TLS: %v", err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err == nil {
		t.Errorf("GetInstance after destroy found the instance")
	}
}
