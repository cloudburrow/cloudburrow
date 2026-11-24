//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestFilestoreCMEKTagsAndReplication (#2192): the official clients.
// A Zonal instance takes a Cloud KMS key of its region (one of another
// region, or on Basic HDD, is refused); disabling the key's version
// suspends it (SUSPENDED, KMS_KEY_ISSUE) and enabling it brings it back;
// a backup with a key is restored only while the key can be used. Tags
// given at creation are Resource Manager bindings on the instance, and
// not returned. A STANDBY replica of a Regional instance shows its active
// instance, and the active instance it; PromoteReplica makes them two
// independent instances.
//
// covers: google.cloud.filestore.v1.CloudFilestoreManager/PromoteReplica
func TestFilestoreCMEKTagsAndReplication(t *testing.T) {
	h := New(t)
	fg, fr := filestoreClients(t, h)
	// Resource Manager as the served shard exports it.
	shardEndpoint(h, envLoggingResourceManager, EnvResourceManager)
	kc := kmsClients(t, h)["grpc"]
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	region := "projects/" + project + "/locations/us-central1"

	// Keys.
	sfx := time.Now().Format("150405")
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: region, KeyRingId: "fs-ring-" + sfx})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "fs-key",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	far, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/europe-west1", KeyRingId: "fs-ring-" + sfx})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	farKey, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: far.GetName(), CryptoKeyId: "fs-key",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	_, err = fg.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-farkey", Instance: &filestorepb.Instance{
		Tier: filestorepb.Instance_ZONAL, FileShares: fsShare("vol1", 1024), Networks: fsNetwork, KmsKeyName: farKey.GetName()}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateInstance with a key of another region = %v, want INVALID_ARGUMENT", err)
	}
	_, err = fr.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-basickey", Instance: &filestorepb.Instance{
		Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("vol1", 1024), Networks: fsNetwork, KmsKeyName: key.GetName()}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateInstance of Basic HDD with a key = %v, want INVALID_ARGUMENT", err)
	}

	// Tags.
	tc := newTagClients(t, h)
	tk, tv := iamTag(t, h, tc, "fs")
	in := fsInstance(t, fg, zone, "ct-fs-cmek", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, FileShares: fsShare("vol1", 1024),
		Networks: fsNetwork, KmsKeyName: key.GetName(), Tags: map[string]string{tk.GetNamespacedName(): tv.GetShortName()}})
	if in.GetKmsKeyName() != key.GetName() || len(in.GetTags()) != 0 {
		t.Fatalf("instance = key %q, tags %v", in.GetKmsKeyName(), in.GetTags())
	}
	it := tc.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: "//file.googleapis.com/" + in.GetName()})
	found := false
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTagBindings: %v", err)
		}
		found = found || b.GetTagValue() == tv.GetName()
	}
	if !found {
		t.Errorf("the instance's tag binding to %s was not made", tv.GetName())
	}

	// A backup with the key.
	bop, err := fg.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: region, BackupId: "ct-fs-cmek-bk",
		Backup: &filestorepb.Backup{SourceInstance: in.GetName(), SourceFileShare: "vol1", KmsKey: key.GetName()}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	bk, err := bop.Wait(ctx)
	if err != nil || bk.GetKmsKey() != key.GetName() {
		t.Fatalf("CreateBackup = %v, %v", bk, err)
	}
	t.Cleanup(func() {
		if op, err := fg.DeleteBackup(h.Context(), &filestorepb.DeleteBackupRequest{Name: bk.GetName()}); err == nil {
			_ = op.Wait(h.Context())
		}
	})

	// Disabling the key suspends the instance.
	ver := key.GetPrimary().GetName()
	setVersion := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: ver, State: st},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion: %v", err)
		}
	}
	waitFor := func(want filestorepb.Instance_State) *filestorepb.Instance {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			got, err := fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: in.GetName()})
			if err == nil && got.GetState() == want {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("instance is %v (%v), want %v", got.GetState(), err, want)
			}
			time.Sleep(2 * time.Second)
		}
	}
	setVersion(kmspb.CryptoKeyVersion_DISABLED)
	got := waitFor(filestorepb.Instance_SUSPENDED)
	if len(got.GetSuspensionReasons()) != 1 || got.GetSuspensionReasons()[0] != filestorepb.Instance_KMS_KEY_ISSUE {
		t.Errorf("suspension reasons = %v, want [KMS_KEY_ISSUE]", got.GetSuspensionReasons())
	}
	rop, err := fg.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-cmek-r", Instance: &filestorepb.Instance{
		Tier: filestorepb.Instance_ZONAL, Networks: fsNetwork, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024,
			Source: &filestorepb.FileShareConfig_SourceBackup{SourceBackup: bk.GetName()}}}}})
	if err != nil {
		t.Fatalf("CreateInstance from the backup: %v", err)
	}
	if _, err := rop.Wait(ctx); err == nil || !strings.Contains(err.Error(), "key") {
		t.Errorf("restoring a backup whose key is disabled = %v, want a refusal naming the key", err)
	}
	if op, err := fg.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: zone + "/instances/ct-fs-cmek-r"}); err == nil {
		_ = op.Wait(ctx)
	}
	setVersion(kmspb.CryptoKeyVersion_ENABLED)
	if got = waitFor(filestorepb.Instance_READY); len(got.GetSuspensionReasons()) != 0 {
		t.Errorf("after the key is enabled, suspension reasons = %v", got.GetSuspensionReasons())
	}

	// Replication.
	active := fsInstance(t, fg, region, "ct-fs-active", &filestorepb.Instance{Tier: filestorepb.Instance_REGIONAL, FileShares: fsShare("vol1", 1024), Networks: fsNetwork})
	east := "projects/" + project + "/locations/us-east1"
	replica := fsInstance(t, fg, east, "ct-fs-replica", &filestorepb.Instance{Tier: filestorepb.Instance_REGIONAL, FileShares: fsShare("vol1", 1024), Networks: fsNetwork,
		Replication: &filestorepb.Replication{Role: filestorepb.Replication_STANDBY, Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: active.GetName()}}}})
	if replica.GetReplication().GetRole() != filestorepb.Replication_STANDBY || replica.GetReplication().GetReplicas()[0].GetPeerInstance() != active.GetName() ||
		replica.GetReplication().GetReplicas()[0].GetState() != filestorepb.ReplicaConfig_READY || replica.GetReplication().GetReplicas()[0].GetLastActiveSyncTime() == nil {
		t.Fatalf("replica's replication = %v", replica.GetReplication())
	}
	a, err := fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: active.GetName()})
	if err != nil || a.GetReplication().GetRole() != filestorepb.Replication_ACTIVE || a.GetReplication().GetReplicas()[0].GetPeerInstance() != replica.GetName() {
		t.Fatalf("active's replication = %v, %v", a.GetReplication(), err)
	}
	if _, err := fg.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: active.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance of an instance with a replica = %v, want FAILED_PRECONDITION", err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		op, err := fr.PromoteReplica(ctx, &filestorepb.PromoteReplicaRequest{Name: replica.GetName()})
		if err == nil {
			p, err := op.Wait(ctx)
			if err != nil || p.GetReplication() != nil || p.GetState() != filestorepb.Instance_READY {
				t.Fatalf("PromoteReplica = %v, %v", p, err)
			}
			break
		}
		// A sync may hold the pair for a moment.
		if status.Code(err) != codes.FailedPrecondition || time.Now().After(deadline) {
			t.Fatalf("PromoteReplica: %v", err)
		}
		time.Sleep(time.Second)
	}
	if a, err = fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: active.GetName()}); err != nil || a.GetReplication() != nil {
		t.Errorf("after promotion the active instance's replication = %v, %v", a.GetReplication(), err)
	}
}
