package filestore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func zonal(region bool) *filestorepb.Instance {
	t := filestorepb.Instance_ZONAL
	if region {
		t = filestorepb.Instance_REGIONAL
	}
	return &filestorepb.Instance{Tier: t, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024}},
		Networks: []*filestorepb.NetworkConfig{{Network: "default"}}}
}

// TestCustomerManagedKeys: a key in the instance's region is taken, one in
// another region or on a basic tier refused; disabling its version
// suspends the instance (KMS_KEY_ISSUE, no export served) and enabling it
// resumes it; a backup with a key is restored only while it can be used.
func TestCustomerManagedKeys(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/proj-1/locations/us-central1", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	far, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/proj-1/locations/europe-west1", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	be := &MemoryBackend{}
	keys := cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }}
	s, err := NewServer("", Options{Backend: be, Keys: &keys, KeyCheckEvery: 20 * time.Millisecond, ReplicationEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	zone := "projects/proj-1/locations/us-central1-a"
	in := zonal(false)
	in.KmsKeyName = far.GetName() + "/cryptoKeys/k"
	_, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "far", Instance: in})
	wantCode(t, "a key in another region", err, codes.InvalidArgument)
	basic := zonal(false)
	basic.Tier, basic.KmsKeyName = filestorepb.Instance_BASIC_HDD, key.GetName()
	_, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "basic", Instance: basic})
	wantCode(t, "a key on Basic HDD", err, codes.InvalidArgument)
	in = zonal(false)
	in.KmsKeyName = key.GetName()
	op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "a", Instance: in})
	wait(t, s, op, err)
	name := zone + "/instances/a"
	sk := s.ServerKey(name)
	be.Write(sk, "vol1", "secret")

	bop, err := s.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: "projects/proj-1/locations/us-central1", BackupId: "b",
		Backup: &filestorepb.Backup{SourceInstance: name, SourceFileShare: "vol1", KmsKey: key.GetName()}})
	wait(t, s, bop, err)
	if got, _ := s.GetBackup(ctx, &filestorepb.GetBackupRequest{Name: "projects/proj-1/locations/us-central1/backups/b"}); got.GetKmsKey() != key.GetName() {
		t.Fatalf("backup kms_key = %q", got.GetKmsKey())
	}
	if be.deks["backup-"+strings.TrimPrefix(s.backups["projects/proj-1/locations/us-central1/backups/b"].Key, "backup-")] == "" {
		t.Fatal("the backup was not encrypted")
	}

	// Disabling the primary version suspends the instance.
	ver := key.GetPrimary().GetName()
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: ver, State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, name, filestorepb.Instance_SUSPENDED)
	got, _ := s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: name})
	if len(got.GetSuspensionReasons()) != 1 || got.GetSuspensionReasons()[0] != filestorepb.Instance_KMS_KEY_ISSUE {
		t.Fatalf("suspension reasons = %v", got.GetSuspensionReasons())
	}
	if len(be.SpecOf(sk).Shares) != 0 {
		t.Fatal("a suspended instance still exports its share")
	}
	// The encrypted backup cannot be restored meanwhile.
	rin := zonal(false)
	rin.FileShares[0].Source = &filestorepb.FileShareConfig_SourceBackup{SourceBackup: "projects/proj-1/locations/us-central1/backups/b"}
	rop, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "r1", Instance: rin})
	if err != nil {
		t.Fatal(err)
	}
	s.WaitDone(ctx, rop.GetName())
	if done, _ := s.Operation(rop.GetName()); done.GetError() == nil {
		t.Fatal("an encrypted backup was restored with its key disabled")
	}
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: ver, State: kmspb.CryptoKeyVersion_ENABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, name, filestorepb.Instance_READY)
	if len(be.SpecOf(sk).Shares) != 1 {
		t.Fatal("a resumed instance does not export its share")
	}
	rop, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "r2", Instance: zonal(false)})
	wait(t, s, rop, err)
	rin.FileShares[0].Source = &filestorepb.FileShareConfig_SourceBackup{SourceBackup: "projects/proj-1/locations/us-central1/backups/b"}
	rop, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "r3", Instance: rin})
	wait(t, s, rop, err)
	if got := be.Read(s.ServerKey(zone+"/instances/r3"), "vol1"); got != "secret" {
		t.Fatalf("restored data = %q", got)
	}
}

func waitState(t *testing.T, s *Server, name string, want filestorepb.Instance_State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.GetInstance(context.Background(), &filestorepb.GetInstanceRequest{Name: name}); got.GetState() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := s.GetInstance(context.Background(), &filestorepb.GetInstanceRequest{Name: name})
	t.Fatalf("%s is %v, want %v", name, got.GetState(), want)
}

// TestReplicationAndPromotion: a STANDBY replica of a regional instance is
// read-only, holds the active share's data and is synced; promotion makes
// both independent and the replica writable.
func TestReplicationAndPromotion(t *testing.T) {
	ctx := context.Background()
	be := &MemoryBackend{}
	s, err := NewServer("", Options{Backend: be, ReplicationEvery: 20 * time.Millisecond, KeyCheckEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r1, r2 := "projects/proj-1/locations/us-central1", "projects/proj-1/locations/us-east1"
	op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: r1, InstanceId: "a", Instance: zonal(true)})
	wait(t, s, op, err)
	a := r1 + "/instances/a"
	be.Write(s.ServerKey(a), "vol1", "one")
	bad := zonal(true)
	bad.Replication = &filestorepb.Replication{Role: filestorepb.Replication_ACTIVE, Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: a}}}
	_, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: r2, InstanceId: "x", Instance: bad})
	wantCode(t, "a replica made ACTIVE", err, codes.InvalidArgument)
	rep := zonal(true)
	rep.Replication = &filestorepb.Replication{Role: filestorepb.Replication_STANDBY, Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: a}}}
	op, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: r2, InstanceId: "b", Instance: rep})
	wait(t, s, op, err)
	b := r2 + "/instances/b"
	if got := be.Read(s.ServerKey(b), "vol1"); got != "one" {
		t.Fatalf("replica data = %q", got)
	}
	if !be.SpecOf(s.ServerKey(b)).ReadOnly {
		t.Fatal("the replica is writable")
	}
	ga, _ := s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: a})
	if ga.GetReplication().GetRole() != filestorepb.Replication_ACTIVE || peerOf(ga) != b || ga.GetReplication().GetReplicas()[0].GetState() != filestorepb.ReplicaConfig_READY {
		t.Fatalf("active replication = %v", ga.GetReplication())
	}
	// Changes reach the replica.
	be.Write(s.ServerKey(a), "vol1", "two")
	deadline := time.Now().Add(5 * time.Second)
	for be.Read(s.ServerKey(b), "vol1") != "two" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := be.Read(s.ServerKey(b), "vol1"); got != "two" {
		t.Fatalf("replica data after sync = %q", got)
	}
	_, err = s.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: a})
	wantCode(t, "deleting an instance with a replica", err, codes.FailedPrecondition)
	_, err = s.PromoteReplica(ctx, &filestorepb.PromoteReplicaRequest{Name: a})
	wantCode(t, "PromoteReplica on the active instance without peer_instance", err, codes.InvalidArgument)
	var pop interface{ GetName() string }
	for i := 0; i < 100; i++ {
		o, err := s.PromoteReplica(ctx, &filestorepb.PromoteReplicaRequest{Name: a, PeerInstance: b})
		if err == nil {
			pop = o
			wait(t, s, o, nil)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pop == nil {
		t.Fatal("PromoteReplica never ran")
	}
	gb, _ := s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: b})
	ga, _ = s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: a})
	if gb.GetReplication() != nil || ga.GetReplication() != nil || be.SpecOf(s.ServerKey(b)).ReadOnly {
		t.Fatalf("after promotion: a %v, b %v, read-only %v", ga.GetReplication(), gb.GetReplication(), be.SpecOf(s.ServerKey(b)).ReadOnly)
	}
}

type fakeBinder struct{ bound map[string]map[string]string }

func (f *fakeBinder) BindTags(res string, tags map[string]string, validateOnly bool) error {
	for k := range tags {
		if !strings.Contains(k, "/") {
			return fmt.Errorf("tag key %q does not exist", k)
		}
	}
	if !validateOnly {
		f.bound[res] = tags
	}
	return nil
}

func (f *fakeBinder) UnbindTags(res string) error { delete(f.bound, res); return nil }

// TestTagsAreBoundNotStored: tags become bindings on the full resource
// name, are not returned, and go with the instance.
func TestTagsAreBoundNotStored(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBinder{bound: map[string]map[string]string{}}
	s, err := NewServer("", Options{Backend: &MemoryBackend{}, Tags: func() TagBinder { return fb }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	zone := "projects/proj-1/locations/us-central1-a"
	in := zonal(false)
	in.Tags = map[string]string{"proj-1/env": "prod"}
	op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "a", Instance: in})
	wait(t, s, op, err)
	res := "//file.googleapis.com/" + zone + "/instances/a"
	if fb.bound[res]["proj-1/env"] != "prod" {
		t.Fatalf("bindings = %v", fb.bound)
	}
	if got, _ := s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/a"}); len(got.GetTags()) != 0 {
		t.Fatalf("tags were returned: %v", got.GetTags())
	}
	in = zonal(false)
	in.Tags = map[string]string{"nope": "x"}
	_, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "b", Instance: in})
	if err == nil {
		t.Fatal("an unknown tag key was accepted")
	}
	op, err = s.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: zone + "/instances/a"})
	wait(t, s, op, err)
	if _, ok := fb.bound[res]; ok {
		t.Fatal("the deleted instance's bindings remain")
	}
}
