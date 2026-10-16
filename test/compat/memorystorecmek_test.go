//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// memorystoreKey is a fresh ENCRYPT_DECRYPT CryptoKey in us-central1 of
// project, made with cloud.google.com/go/kms/apiv1.
func memorystoreKey(t *testing.T, h *Harness, project string) string {
	t.Helper()
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-central1",
		KeyRingId: fmt.Sprintf("redis-%d", time.Now().UnixNano()%1e9)})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "redis",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	return key.GetName()
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestMemorystoreCustomerManagedKey (#1463), with cloud.google.com/go/
// redis/apiv1, cloud.google.com/go/kms/apiv1 and go-redis: an instance
// created with customer_managed_key, a key in its region, reports the key
// and serves. With the key's version disabled it is suspended: no READY
// state, suspension_reasons CUSTOMER_MANAGED_KEY_ISSUE, and its server
// unreachable; with the version enabled again it resumes on its own and
// serves with its AUTH string. A key in another region is
// INVALID_ARGUMENT.
func TestMemorystoreCustomerManagedKey(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	kc := kmsClients(t, h)["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	key := memorystoreKey(t, h, project)
	parent := "projects/" + project + "/locations/us-central1"

	if _, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/europe-west1", InstanceId: "cmek-elsewhere",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, CustomerManagedKey: key}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key in another region = %v, want INVALID_ARGUMENT", err)
	}

	id := fmt.Sprintf("cmek-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true, CustomerManagedKey: key}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	inst, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetCustomerManagedKey() != key || len(inst.GetSuspensionReasons()) != 0 || inst.GetState() != redispb.Instance_READY {
		t.Fatalf("created: customer_managed_key %q, state %v, suspension_reasons %v", inst.GetCustomerManagedKey(), inst.GetState(), inst.GetSuspensionReasons())
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	rc := redisAt(inst, auth.GetAuthString())
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	_ = rc.Close()

	version := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
	until := func(what string, ok func(*redispb.Instance) bool) *redispb.Instance {
		t.Helper()
		for {
			got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
			if err == nil && ok(got) {
				return got
			}
			if ctx.Err() != nil {
				t.Fatalf("%s: %v, %v", what, got, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	version(kmspb.CryptoKeyVersion_DISABLED)
	got := until("suspended", func(i *redispb.Instance) bool { return len(i.GetSuspensionReasons()) > 0 })
	if got.GetState() == redispb.Instance_READY || got.GetSuspensionReasons()[0] != redispb.Instance_CUSTOMER_MANAGED_KEY_ISSUE ||
		!strings.Contains(got.GetStatusMessage(), "suspended") {
		t.Errorf("suspended: state %v, suspension_reasons %v, status_message %q", got.GetState(), got.GetSuspensionReasons(), got.GetStatusMessage())
	}
	deadline := time.Now().Add(time.Minute)
	for {
		probe := redisAt(inst, auth.GetAuthString())
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		err := probe.Ping(pctx).Err()
		pcancel()
		_ = probe.Close()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a suspended instance's server still answers")
		}
		time.Sleep(time.Second)
	}

	version(kmspb.CryptoKeyVersion_ENABLED)
	got = until("resumed", func(i *redispb.Instance) bool { return i.GetState() == redispb.Instance_READY })
	if len(got.GetSuspensionReasons()) != 0 || got.GetCustomerManagedKey() != key {
		t.Errorf("resumed: suspension_reasons %v, customer_managed_key %q", got.GetSuspensionReasons(), got.GetCustomerManagedKey())
	}
	rc = redisAt(got, auth.GetAuthString())
	defer rc.Close()
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Errorf("PING after resuming: %v", err)
	}
}
