//go:build compat

package compat

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	filestore "cloud.google.com/go/filestore/apiv1"
	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvFilestore points at the local Filestore endpoint (#2192), gRPC and
// JSON on one port.
const EnvFilestore = "CLOUDBURROW_TEST_FILESTORE"

// filestoreClients are the official client over gRPC and over REST, both
// at the local endpoint. Filestore has no emulator variable, so the
// endpoint and credentials are explicit.
func filestoreClients(t *testing.T, h *Harness) (g, r *filestore.CloudFilestoreManagerClient) {
	t.Helper()
	addr := h.Endpoint(EnvFilestore)
	ctx := context.Background()
	g, err := filestore.NewCloudFilestoreManagerClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err = filestore.NewCloudFilestoreManagerRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return g, r
}

func fsCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// fsInstance makes an instance and waits for it.
func fsInstance(t *testing.T, c *filestore.CloudFilestoreManagerClient, parent, id string, in *filestorepb.Instance) *filestorepb.Instance {
	t.Helper()
	ctx := fsCtx(t)
	op, err := c.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
	if err != nil {
		t.Fatalf("CreateInstance %s: %v", id, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &filestorepb.DeleteInstanceRequest{Name: parent + "/instances/" + id, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance %s: %v", id, err)
	}
	return got
}

func fsShare(name string, gb int64) []*filestorepb.FileShareConfig {
	return []*filestorepb.FileShareConfig{{Name: name, CapacityGb: gb}}
}

var fsNetwork = []*filestorepb.NetworkConfig{{Network: "default"}}

// TestFilestoreInstancesSnapshotsAndBackups (#2192): the official client,
// over gRPC and REST, against the documented tiers and limits: a zonal
// NFSv4.1 instance is made, read, listed, resized within its range and
// relabelled; a snapshot is taken and listed; a backup is made, read and
// restored to a new instance; the instance is reverted to its snapshot; a
// Basic HDD instance is made and restored in place from its backup; and
// the refusals Google documents (capacity ranges and steps, NFSv4.1 and
// snapshots on basic tiers, the one share, restores across tiers, export
// option limits, deletion protection, an instance with snapshots) are
// checked on the way.
// covers: google.cloud.filestore.v1.CloudFilestoreManager/CreateInstance, google.cloud.filestore.v1.CloudFilestoreManager/GetInstance, google.cloud.filestore.v1.CloudFilestoreManager/ListInstances, google.cloud.filestore.v1.CloudFilestoreManager/UpdateInstance, google.cloud.filestore.v1.CloudFilestoreManager/DeleteInstance, google.cloud.filestore.v1.CloudFilestoreManager/RestoreInstance, google.cloud.filestore.v1.CloudFilestoreManager/RevertInstance, google.cloud.filestore.v1.CloudFilestoreManager/CreateSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/GetSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/ListSnapshots, google.cloud.filestore.v1.CloudFilestoreManager/UpdateSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/DeleteSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/CreateBackup, google.cloud.filestore.v1.CloudFilestoreManager/GetBackup, google.cloud.filestore.v1.CloudFilestoreManager/ListBackups, google.cloud.filestore.v1.CloudFilestoreManager/UpdateBackup, google.cloud.filestore.v1.CloudFilestoreManager/DeleteBackup
func TestFilestoreInstancesSnapshotsAndBackups(t *testing.T) {
	h := New(t)
	g, r := filestoreClients(t, h)
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	region := "projects/" + project + "/locations/us-central1"

	// The refusals, before anything is made.
	for _, c := range []struct {
		what   string
		parent string
		in     *filestorepb.Instance
	}{
		{"Basic HDD below 1 TiB", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("vol1", 512), Networks: fsNetwork}},
		{"Basic SSD below 2.5 TiB", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_SSD, FileShares: fsShare("vol1", 1024), Networks: fsNetwork}},
		{"Zonal off its 256 GiB step", zone, &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, FileShares: fsShare("vol1", 1100), Networks: fsNetwork}},
		{"NFSv4.1 on Basic HDD", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, Protocol: filestorepb.Instance_NFS_V4_1, FileShares: fsShare("vol1", 1024), Networks: fsNetwork}},
		{"a Basic share name over 16 characters", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("a_very_long_share_name", 1024), Networks: fsNetwork}},
		{"two shares", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, FileShares: append(fsShare("a", 1024), fsShare("b", 1024)...), Networks: fsNetwork}},
		{"a Regional instance in a zone", zone, &filestorepb.Instance{Tier: filestorepb.Instance_REGIONAL, FileShares: fsShare("vol1", 1024), Networks: fsNetwork}},
		{"a /28 for a Basic tier", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("vol1", 1024),
			Networks: []*filestorepb.NetworkConfig{{Network: "default", ReservedIpRange: "10.0.0.0/28"}}}},
		{"anon_uid without ROOT_SQUASH", zone, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, Networks: fsNetwork,
			FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024, NfsExportOptions: []*filestorepb.NfsExportOptions{{IpRanges: []string{"10.0.0.0/8"}, AnonUid: 1000}}}}}},
	} {
		_, err := g.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: c.parent, InstanceId: "ct-fs-bad", Instance: c.in})
		wantPrivateCACode(t, c.what, err, codes.InvalidArgument)
	}

	zonal := fsInstance(t, g, zone, "ct-fs-zonal", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1,
		Description: "compat", Labels: map[string]string{"team": "ct"}, FileShares: fsShare("vol1", 1024), Networks: fsNetwork})
	if zonal.GetState() != filestorepb.Instance_READY || len(zonal.GetNetworks()[0].GetIpAddresses()) != 1 ||
		!strings.HasSuffix(zonal.GetNetworks()[0].GetReservedIpRange(), "/26") || zonal.GetNetworks()[0].GetConnectMode() != filestorepb.NetworkConfig_DIRECT_PEERING {
		t.Fatalf("zonal instance = %v", zonal)
	}
	_, err := g.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-zonal",
		Instance: &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, FileShares: fsShare("vol1", 1024), Networks: fsNetwork}})
	wantPrivateCACode(t, "a duplicate", err, codes.AlreadyExists)

	// Over REST: read, list across locations.
	got, err := r.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zonal.GetName()})
	if err != nil || got.GetTier() != filestorepb.Instance_ZONAL || got.GetProtocol() != filestorepb.Instance_NFS_V4_1 || got.GetLabels()["team"] != "ct" {
		t.Fatalf("GetInstance over REST = %v, %v", got, err)
	}
	found := false
	it := r.ListInstances(ctx, &filestorepb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"})
	for {
		in, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		found = found || in.GetName() == zonal.GetName()
	}
	if !found {
		t.Error("ListInstances over REST does not list the zonal instance")
	}

	// Resize within the range and relabel; across the range is refused.
	up, err := r.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"file_shares", "labels"}},
		Instance: &filestorepb.Instance{Name: zonal.GetName(), FileShares: fsShare("vol1", 1280), Labels: map[string]string{"team": "ct2"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if got, err := up.Wait(ctx); err != nil || got.GetFileShares()[0].GetCapacityGb() != 1280 || got.GetLabels()["team"] != "ct2" {
		t.Fatalf("UpdateInstance = %v, %v", got, err)
	}
	_, err = g.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"file_shares"}},
		Instance: &filestorepb.Instance{Name: zonal.GetName(), FileShares: fsShare("vol1", 10240)}})
	wantPrivateCACode(t, "a resize out of the instance's range", err, codes.InvalidArgument)

	// A snapshot, listed and relabelled.
	sop, err := g.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: zonal.GetName(), SnapshotId: "ct-snap",
		Snapshot: &filestorepb.Snapshot{Description: "first"}})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, err := sop.Wait(ctx)
	if err != nil || snap.GetState() != filestorepb.Snapshot_READY {
		t.Fatalf("CreateSnapshot = %v, %v", snap, err)
	}
	sit := r.ListSnapshots(ctx, &filestorepb.ListSnapshotsRequest{Parent: zonal.GetName()})
	if s, err := sit.Next(); err != nil || s.GetName() != snap.GetName() {
		t.Fatalf("ListSnapshots = %v, %v", s, err)
	}
	usop, err := g.UpdateSnapshot(ctx, &filestorepb.UpdateSnapshotRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		Snapshot: &filestorepb.Snapshot{Name: snap.GetName(), Description: "renamed"}})
	if err != nil {
		t.Fatalf("UpdateSnapshot: %v", err)
	}
	if s, err := usop.Wait(ctx); err != nil || s.GetDescription() != "renamed" {
		t.Fatalf("UpdateSnapshot = %v, %v", s, err)
	}

	// A backup, read over REST, and an instance restored from it.
	bop, err := g.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: region, BackupId: "ct-fs-backup",
		Backup: &filestorepb.Backup{SourceInstance: zonal.GetName(), SourceFileShare: "vol1"}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	backup, err := bop.Wait(ctx)
	if err != nil || backup.GetState() != filestorepb.Backup_READY || backup.GetSourceInstanceTier() != filestorepb.Instance_ZONAL ||
		backup.GetCapacityGb() != 1280 || backup.GetFileSystemProtocol() != filestorepb.Instance_NFS_V4_1 {
		t.Fatalf("CreateBackup = %v, %v", backup, err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteBackup(context.Background(), &filestorepb.DeleteBackupRequest{Name: backup.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if b, err := r.GetBackup(ctx, &filestorepb.GetBackupRequest{Name: backup.GetName()}); err != nil || b.GetSourceInstance() != zonal.GetName() {
		t.Fatalf("GetBackup over REST = %v, %v", b, err)
	}
	ubop, err := r.UpdateBackup(ctx, &filestorepb.UpdateBackupRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		Backup: &filestorepb.Backup{Name: backup.GetName(), Labels: map[string]string{"k": "v"}}})
	if err != nil {
		t.Fatalf("UpdateBackup: %v", err)
	}
	if b, err := ubop.Wait(ctx); err != nil || b.GetLabels()["k"] != "v" {
		t.Fatalf("UpdateBackup = %v, %v", b, err)
	}
	bit := g.ListBackups(ctx, &filestorepb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-"})
	if b, err := bit.Next(); err != nil || b.GetName() != backup.GetName() {
		t.Fatalf("ListBackups = %v, %v", b, err)
	}
	_, err = g.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-small",
		Instance: &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1, Networks: fsNetwork,
			FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024, Source: &filestorepb.FileShareConfig_SourceBackup{SourceBackup: backup.GetName()}}}}})
	wantPrivateCACode(t, "a restore below the backup's capacity", err, codes.InvalidArgument)
	restored := fsInstance(t, g, "projects/"+project+"/locations/us-central1-b", "ct-fs-restored", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL,
		Protocol: filestorepb.Instance_NFS_V4_1, Networks: fsNetwork,
		FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1280, Source: &filestorepb.FileShareConfig_SourceBackup{SourceBackup: backup.GetName()}}}})
	if restored.GetFileShares()[0].GetSourceBackup() != backup.GetName() {
		t.Errorf("restored instance's share = %v", restored.GetFileShares())
	}
	_, err = g.RestoreInstance(ctx, &filestorepb.RestoreInstanceRequest{Name: restored.GetName(), FileShare: "vol1",
		Source: &filestorepb.RestoreInstanceRequest_SourceBackup{SourceBackup: backup.GetName()}})
	wantPrivateCACode(t, "a zonal backup restored in place", err, codes.InvalidArgument)

	// Revert to the snapshot.
	rop, err := g.RevertInstance(ctx, &filestorepb.RevertInstanceRequest{Name: zonal.GetName(), TargetSnapshotId: "ct-snap"})
	if err != nil {
		t.Fatalf("RevertInstance: %v", err)
	}
	if got, err := rop.Wait(ctx); err != nil || got.GetState() != filestorepb.Instance_READY {
		t.Fatalf("RevertInstance = %v, %v", got, err)
	}

	// An instance with a snapshot is deleted only with force.
	_, err = g.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: zonal.GetName()})
	wantPrivateCACode(t, "an instance with snapshots", err, codes.FailedPrecondition)
	dsop, err := r.DeleteSnapshot(ctx, &filestorepb.DeleteSnapshotRequest{Name: snap.GetName()})
	if err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if err := dsop.Wait(ctx); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := g.GetSnapshot(ctx, &filestorepb.GetSnapshotRequest{Name: snap.GetName()}); err == nil {
		t.Error("the deleted snapshot is still there")
	}

	// Basic HDD: NFSv3, no snapshots, restored in place from its backup,
	// grown but never shrunk; deletion protection holds.
	basic := fsInstance(t, g, zone, "ct-fs-basic", &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("share1", 1024),
		Networks: fsNetwork, DeletionProtectionEnabled: true, DeletionProtectionReason: "compat"})
	if basic.GetProtocol() != filestorepb.Instance_NFS_V3 || !strings.HasSuffix(basic.GetNetworks()[0].GetReservedIpRange(), "/29") {
		t.Errorf("basic instance = %v", basic)
	}
	_, err = g.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: basic.GetName(), SnapshotId: "s"})
	wantPrivateCACode(t, "a snapshot of a Basic instance", err, codes.InvalidArgument)
	_, err = g.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"file_shares"}},
		Instance: &filestorepb.Instance{Name: basic.GetName(), FileShares: fsShare("share1", 1023)}})
	wantPrivateCACode(t, "a Basic instance shrunk", err, codes.InvalidArgument)
	_, err = g.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: basic.GetName()})
	wantPrivateCACode(t, "an instance with deletion protection", err, codes.FailedPrecondition)
	bbop, err := g.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: region, BackupId: "ct-fs-basic-backup",
		Backup: &filestorepb.Backup{SourceInstance: basic.GetName(), SourceFileShare: "share1"}})
	if err != nil {
		t.Fatalf("CreateBackup of the basic instance: %v", err)
	}
	bb, err := bbop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateBackup of the basic instance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteBackup(context.Background(), &filestorepb.DeleteBackupRequest{Name: bb.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rsop, err := r.RestoreInstance(ctx, &filestorepb.RestoreInstanceRequest{Name: basic.GetName(), FileShare: "share1",
		Source: &filestorepb.RestoreInstanceRequest_SourceBackup{SourceBackup: bb.GetName()}})
	if err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	if got, err := rsop.Wait(ctx); err != nil || got.GetFileShares()[0].GetSourceBackup() != bb.GetName() {
		t.Fatalf("RestoreInstance = %v, %v", got, err)
	}
	_, err = g.RestoreInstance(ctx, &filestorepb.RestoreInstanceRequest{Name: basic.GetName(), FileShare: "share1",
		Source: &filestorepb.RestoreInstanceRequest_SourceBackup{SourceBackup: backup.GetName()}})
	wantPrivateCACode(t, "a zonal backup restored to a Basic instance", err, codes.InvalidArgument)
	uop, err := g.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection_enabled"}},
		Instance: &filestorepb.Instance{Name: basic.GetName()}})
	if err != nil {
		t.Fatalf("disable deletion protection: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("disable deletion protection: %v", err)
	}

	// Operations and locations.
	oit := g.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: zone})
	n := 0
	for {
		_, err := oit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		n++
	}
	if n == 0 {
		t.Error("ListOperations lists no operation")
	}
	if loc, err := r.GetLocation(ctx, &locationpb.GetLocationRequest{Name: zone}); err != nil || loc.GetLocationId() != "us-central1-a" {
		t.Errorf("GetLocation = %v, %v", loc, err)
	}
	lit := g.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project})
	if l, err := lit.Next(); err != nil || l.GetLocationId() == "" {
		t.Errorf("ListLocations = %v, %v", l, err)
	}
}

// TestGKEFilestoreMountFromAPod (#2192): a Kubernetes Engine cluster's pod
// mounts a Filestore instance's share at the address the API returns, as
// on Google, writes and reads a file; a snapshot keeps it and a revert
// brings it back; a backup restored to a new instance holds it, read from
// another pod. Basic HDD's NFSv3 share is mounted and written too.
func TestGKEFilestoreMountFromAPod(t *testing.T) {
	h := New(t)
	fg, _ := filestoreClients(t, h)
	cg, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	region := "projects/" + project + "/locations/us-central1"

	// The cluster.
	cparent := zone
	cname := cparent + "/clusters/ct-gke-fs"
	op, err := cg.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: cparent, Cluster: &containerpb.Cluster{Name: "ct-gke-fs", InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cg.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: cname}); err == nil {
			gkeWait(t, cg, project, "us-central1-a", op)
		}
	})

	zonal := fsInstance(t, fg, zone, "ct-fs-pod", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1,
		FileShares: fsShare("vol1", 1024), Networks: fsNetwork})
	basic := fsInstance(t, fg, zone, "ct-fs-pod3", &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD,
		FileShares: fsShare("share1", 1024), Networks: fsNetwork})

	gkeWait(t, cg, project, "us-central1-a", op)
	c, err := cg.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kc := gkeKubeconfig(t, c)

	ip := func(in *filestorepb.Instance) string { return in.GetNetworks()[0].GetIpAddresses()[0] }
	pod := 0
	// run runs script in a pod with the share mounted at /mnt/share by
	// Kubernetes' NFS volume, and answers its output.
	run := func(in *filestorepb.Instance, script string) string {
		t.Helper()
		pod++
		name := "ct-fs-" + strings.ToLower(strings.ReplaceAll(lastSeg(in.GetName()), "_", "-")) + "-" + string(rune('a'+pod))
		manifest := `apiVersion: v1
kind: Pod
metadata: {name: ` + name + `}
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh", "-c", ` + quoteYAML(script) + `]
    volumeMounts: [{name: share, mountPath: /mnt/share}]
  volumes:
  - name: share
    nfs: {server: "` + ip(in) + `", path: "/` + in.GetFileShares()[0].GetName() + `"}
`
		apply := exec.Command("kubectl", "--kubeconfig", kc, "apply", "-f", "-")
		apply.Stdin = strings.NewReader(manifest)
		if out, err := apply.CombinedOutput(); err != nil {
			t.Fatalf("kubectl apply: %v\n%s", err, out)
		}
		deadline := time.Now().Add(4 * time.Minute)
		for {
			out, _ := exec.Command("kubectl", "--kubeconfig", kc, "get", "pod", name, "-o", "jsonpath={.status.phase}").Output()
			phase := string(out)
			if phase == "Succeeded" {
				break
			}
			if phase == "Failed" || time.Now().After(deadline) {
				desc, _ := exec.Command("kubectl", "--kubeconfig", kc, "describe", "pod", name).CombinedOutput()
				logs, _ := exec.Command("kubectl", "--kubeconfig", kc, "logs", name).CombinedOutput()
				t.Fatalf("pod %s is %q:\n%s\n%s", name, phase, desc, logs)
			}
			time.Sleep(2 * time.Second)
		}
		out, err := exec.Command("kubectl", "--kubeconfig", kc, "logs", name).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl logs: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Write and read over NFSv4.1, and over NFSv3 on Basic HDD.
	if got := run(zonal, "echo first > /mnt/share/data.txt && cat /mnt/share/data.txt"); got != "first" {
		t.Fatalf("NFSv4.1 write and read: %q", got)
	}
	if got := run(basic, "echo v3 > /mnt/share/v3.txt && cat /mnt/share/v3.txt && grep ' /mnt/share ' /proc/mounts"); !strings.HasPrefix(got, "v3") || !strings.Contains(got, "vers=3") {
		t.Fatalf("NFSv3 write and read: %q", got)
	}

	// A snapshot, then a change, then a backup of the change.
	sop, err := fg.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: zonal.GetName(), SnapshotId: "ct-pod-snap", Snapshot: &filestorepb.Snapshot{}})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if got := run(zonal, "echo second > /mnt/share/data.txt && cat /mnt/share/data.txt"); got != "second" {
		t.Fatalf("rewrite: %q", got)
	}
	bop, err := fg.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: region, BackupId: "ct-pod-backup",
		Backup: &filestorepb.Backup{SourceInstance: zonal.GetName(), SourceFileShare: "vol1"}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	backup, err := bop.Wait(ctx)
	if err != nil || backup.GetStorageBytes() == 0 {
		t.Fatalf("CreateBackup = %v, %v", backup, err)
	}
	t.Cleanup(func() {
		if op, err := fg.DeleteBackup(context.Background(), &filestorepb.DeleteBackupRequest{Name: backup.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	// The revert brings the snapshot's file back.
	rop, err := fg.RevertInstance(ctx, &filestorepb.RevertInstanceRequest{Name: zonal.GetName(), TargetSnapshotId: "ct-pod-snap"})
	if err != nil {
		t.Fatalf("RevertInstance: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("RevertInstance: %v", err)
	}
	if got := run(zonal, "cat /mnt/share/data.txt"); got != "first" {
		t.Fatalf("after revert: %q, want first", got)
	}

	// The backup restored to a new instance holds the backed-up file.
	restored := fsInstance(t, fg, zone, "ct-fs-pod-restored", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1,
		Networks: fsNetwork, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024,
			Source: &filestorepb.FileShareConfig_SourceBackup{SourceBackup: backup.GetName()}}}})
	if got := run(restored, "cat /mnt/share/data.txt"); got != "second" {
		t.Fatalf("restored instance: %q, want second", got)
	}
}

func lastSeg(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func quoteYAML(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
