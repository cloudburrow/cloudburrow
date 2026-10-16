//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tlsRedisAt is a go-redis client of inst over TLS, trusting its
// server_ca_certs, as Google's in-transit encryption guide connects one.
func tlsRedisAt(t *testing.T, inst *redispb.Instance, password string) *redis.Client {
	t.Helper()
	pool := x509.NewCertPool()
	for _, c := range inst.GetServerCaCerts() {
		pool.AppendCertsFromPEM([]byte(c.GetCert()))
	}
	return redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))), Password: password,
		TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/ListInstances
//
// TestMemorystoreInTransitEncryption (#1413), with cloud.google.com/go/
// redis/apiv1 and go-redis: an instance created with
// transit_encryption_mode SERVER_AUTHENTICATION reports it, with one
// server CA in server_ca_certs (PEM, serial, SHA-1 fingerprint, ten
// years), in GetInstance and ListInstances. go-redis over TLS trusting
// that CA reaches it with its AUTH string; a plain TCP client is refused
// and a TLS client that does not trust the CA fails verification. A
// STANDARD_HA instance with it replicates over TLS (WAIT 1). The
// versions with no maintained image are UNIMPLEMENTED.
func TestMemorystoreInTransitEncryption(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	for _, tier := range []redispb.Instance_Tier{redispb.Instance_BASIC, redispb.Instance_STANDARD_HA} {
		id := fmt.Sprintf("tls-%d-%d", tier, time.Now().UnixNano()%1e8)
		name := parent + "/instances/" + id
		op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
			Instance: &redispb.Instance{Tier: tier, MemorySizeGb: 1, AuthEnabled: true, TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
		if err != nil {
			t.Fatalf("CreateInstance %v: %v", tier, err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		inst, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateInstance %v operation: %v", tier, err)
		}
		if inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
			t.Fatalf("%v: transit_encryption_mode %v, server_ca_certs %v", tier, inst.GetTransitEncryptionMode(), inst.GetServerCaCerts())
		}
		ca := inst.GetServerCaCerts()[0]
		if !strings.HasPrefix(ca.GetCert(), "-----BEGIN CERTIFICATE-----") || ca.GetSerialNumber() == "" || len(ca.GetSha1Fingerprint()) != 40 ||
			ca.GetExpireTime().AsTime().Before(time.Now().AddDate(9, 0, 0)) {
			t.Errorf("%v: the server CA reads back as %v", tier, ca)
		}
		got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil || got.GetServerCaCerts()[0].GetCert() != ca.GetCert() {
			t.Errorf("GetInstance: %v, %v", got, err)
		}
		auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		rc := tlsRedisAt(t, inst, auth.GetAuthString())
		defer rc.Close()
		if err := rc.Set(ctx, "k", "secret", 0).Err(); err != nil {
			t.Fatalf("%v: SET over TLS with the instance's CA: %v", tier, err)
		}
		if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "secret" {
			t.Errorf("%v: GET over TLS = %q, %v", tier, v, err)
		}
		plain := redisAt(inst, auth.GetAuthString())
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		if err := plain.Ping(pctx).Err(); err == nil {
			t.Errorf("%v: a plain TCP client reached the instance", tier)
		}
		pcancel()
		plain.Close()
		untrusted := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))),
			Password: auth.GetAuthString(), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}})
		if err := untrusted.Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("%v: a TLS client without the CA: %v, want a certificate error", tier, err)
		}
		untrusted.Close()
		if tier == redispb.Instance_STANDARD_HA {
			eventuallyTrue(t, "the replica syncs over TLS", func() bool {
				n, err := rc.Do(ctx, "WAIT", 1, 1000).Int()
				return err == nil && n >= 1
			})
		}
	}
	listed := 0
	for it := c.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent}); ; {
		inst, err := it.Next()
		if err != nil {
			break
		}
		if strings.Contains(inst.GetName(), "/instances/tls-") {
			listed++
			if inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
				t.Errorf("ListInstances: %s reads back as %v", inst.GetName(), inst.GetTransitEncryptionMode())
			}
		}
	}
	if listed != 2 {
		t.Errorf("ListInstances lists %d instances with in-transit encryption, want 2", listed)
	}
	for _, v := range []string{"REDIS_3_2", "REDIS_4_0", "REDIS_5_0"} {
		_, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "eol",
			Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, RedisVersion: v}})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "end of life") {
			t.Errorf("CreateInstance %s: %v, want UNIMPLEMENTED, end of life", v, err)
		}
	}
}
