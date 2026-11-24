//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	filev1beta1 "google.golang.org/api/file/v1beta1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	betapb "github.com/cloudburrow/cloudburrow/internal/service/filestore/filestorev1beta1pb"
)

// fsBeta is Google's v1beta1 REST client (the one GKE's Filestore CSI
// driver provisions multishares with) and a v1beta1 gRPC client, at the
// local endpoint.
func fsBeta(t *testing.T, h *Harness) (*filev1beta1.Service, betapb.CloudFilestoreManagerClient) {
	t.Helper()
	addr := h.Endpoint(EnvFilestore)
	svc, err := filev1beta1.NewService(context.Background(), option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return svc, betapb.NewCloudFilestoreManagerClient(conn)
}

// betaWait polls a v1beta1 operation until it is done.
func betaWait(t *testing.T, svc *filev1beta1.Service, op *filev1beta1.Operation, err error) error {
	t.Helper()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for !op.Done {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not finish", op.Name)
		}
		time.Sleep(time.Second)
		if op, err = svc.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
			return err
		}
	}
	if op.Error != nil {
		return errors.New(op.Error.Message)
	}
	return nil
}

func fsHTTPCode(err error) int {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// fsNode is the instance's cluster node, a client of every instance; the
// test skips without CLOUDBURROW_TEST_CLUSTER.
func fsNode(t *testing.T) string {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is required to mount shares from the cluster's node")
	}
	return cluster + "-control-plane"
}

// fsLabel is the label value of the instance's NFS servers: its name,
// the cluster's without its "cloudburrow-" prefix.
func fsLabel(t *testing.T) string {
	return strings.TrimSuffix(strings.TrimPrefix(fsNode(t), "cloudburrow-"), "-control-plane")
}

// nodeSh runs script in the node.
func nodeSh(t *testing.T, node, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return dockerSh(ctx, node, script)
}

// dockerSh runs script in container until ctx ends.
func dockerSh(ctx context.Context, container, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", script)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// fsStep runs one step of a test with its own deadline. A step that
// overruns it fails the test at once with the state of the NFS servers
// serving address and of the node's NFS mounts, so a share that stops
// answering is seen as such, never as a hung test.
func fsStep(t *testing.T, address, step string, limit time.Duration, f func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	err := f(ctx)
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("%s did not finish in %v (%v); state:\n%s", step, limit, err, fsDump(t, address))
	}
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

// fsDump is the state of the instance's NFS servers and of the node's
// NFS mounts and blocked processes, each read with its own deadline.
func fsDump(t *testing.T, address string) string {
	run := func(container, script string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := dockerSh(ctx, container, script)
		if err != nil {
			out += "\n(" + err.Error() + ")"
		}
		return out
	}
	var b strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ps, _ := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label=io.cloudburrow.filestore="+fsLabel(t), "--format", "{{.Names}} {{.Status}}").Output()
	b.WriteString("NFS servers:\n" + string(ps))
	for _, line := range strings.Split(strings.TrimSpace(string(ps)), "\n") {
		n, _, _ := strings.Cut(line, " ")
		if n == "" || !strings.Contains(line, " Up ") {
			continue
		}
		lctx, lcancel := context.WithTimeout(context.Background(), 20*time.Second)
		logs, _ := exec.CommandContext(lctx, "docker", "logs", "--tail", "15", n).CombinedOutput()
		lcancel()
		b.WriteString("--- " + n + " logs:\n" + string(logs))
		b.WriteString("--- " + n + " state:\n" + run(n, `dmsetup info -c 2>&1; losetup -a 2>&1; grep " /export" /proc/mounts; ps -eo pid,stat,wchan:30,args | grep -v " ps -eo"`) + "\n")
	}
	b.WriteString("--- node (" + address + "):\n" + run(fsNode(t), `grep " nfs" /proc/mounts; iptables -t nat -S CLOUDBURROW-FILESTORE | grep -F "`+address+`"; ps -eo pid,stat,wchan:30,args | awk '$2 ~ /D/'`))
	return b.String()
}

// fsServerOf is the NFS server container serving address: the one at the
// Docker address the node's DNAT rule sends address to.
func fsServerOf(t *testing.T, h *Harness, address string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		rules, _ := nodeSh(t, fsNode(t), "iptables -t nat -S CLOUDBURROW-FILESTORE")
		for _, line := range strings.Split(rules, "\n") {
			if strings.Contains(line, "-d "+address+"/32 ") {
				ip := line[strings.LastIndex(line, " ")+1:]
				out, _ := exec.Command("docker", "ps", "--filter", "label=io.cloudburrow.filestore="+fsLabel(t), "--format", "{{.Names}}").Output()
				for _, n := range strings.Fields(string(out)) {
					got, _ := exec.Command("docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}`, n).Output()
					if strings.TrimSpace(string(got)) == ip && !strings.HasSuffix(n, "-deleted") {
						return n
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no NFS server serves %s:\n%s", address, rules)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestFilestoreMultishareInstancesAndShares (#2194): Google's v1beta1 REST
// client (the GKE CSI driver's) and a v1beta1 gRPC client make an
// Enterprise multishare instance and its shares, as
// https://docs.cloud.google.com/filestore/docs/multishares documents them:
// the tier, the instance's 1 TiB to 10 TiB in 256 GiB steps, shares of 10
// GiB to 10 TiB / max_share_count, their sum within the instance, no
// snapshots; each share is a filesystem of its own size, exported as
// /MOUNT_NAME.
// covers: google.cloud.filestore.v1beta1.CloudFilestoreManager/CreateInstance, google.cloud.filestore.v1beta1.CloudFilestoreManager/GetInstance, google.cloud.filestore.v1beta1.CloudFilestoreManager/ListInstances, google.cloud.filestore.v1beta1.CloudFilestoreManager/UpdateInstance, google.cloud.filestore.v1beta1.CloudFilestoreManager/DeleteInstance, google.cloud.filestore.v1beta1.CloudFilestoreManager/CreateShare, google.cloud.filestore.v1beta1.CloudFilestoreManager/GetShare, google.cloud.filestore.v1beta1.CloudFilestoreManager/ListShares, google.cloud.filestore.v1beta1.CloudFilestoreManager/UpdateShare, google.cloud.filestore.v1beta1.CloudFilestoreManager/DeleteShare
func TestFilestoreMultishareInstancesAndShares(t *testing.T) {
	h := New(t)
	svc, gc := fsBeta(t, h)
	fg, _ := filestoreClients(t, h)
	ctx := fsCtx(t)
	region := "projects/" + h.Project() + "/locations/us-central1"
	inst := svc.Projects.Locations.Instances

	// Refusals: a multishare instance is Enterprise, 1 TiB to 10 TiB in
	// 256 GiB steps.
	_, err := inst.Create(region, &filev1beta1.Instance{Tier: "ZONAL", MultiShareEnabled: true, CapacityGb: 1024,
		Networks: []*filev1beta1.NetworkConfig{{Network: "default"}}}).InstanceId("ct-ms-bad").Do()
	if fsHTTPCode(err) != http.StatusBadRequest {
		t.Errorf("multishare ZONAL: %v, want 400", err)
	}
	_, err = inst.Create(region, &filev1beta1.Instance{Tier: "ENTERPRISE", MultiShareEnabled: true, CapacityGb: 1000,
		Networks: []*filev1beta1.NetworkConfig{{Network: "default"}}}).InstanceId("ct-ms-bad").Do()
	if fsHTTPCode(err) != http.StatusBadRequest {
		t.Errorf("multishare of 1000 GiB: %v, want 400", err)
	}

	name := region + "/instances/ct-ms"
	op, err := inst.Create(region, &filev1beta1.Instance{Tier: "ENTERPRISE", MultiShareEnabled: true, CapacityGb: 1024, MaxShareCount: 10,
		Networks: []*filev1beta1.NetworkConfig{{Network: "default"}}}).InstanceId("ct-ms").Do()
	if err := betaWait(t, svc, op, err); err != nil {
		t.Fatalf("Create multishare instance: %v", err)
	}
	t.Cleanup(func() {
		op, err := inst.Delete(name).Force(true).Do()
		_ = betaWait(t, svc, op, err)
	})
	got, err := inst.Get(name).Do()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.MultiShareEnabled || got.CapacityGb != 1024 || got.MaxShareCount != 10 || got.MaxCapacityGb != 10240 || got.CapacityStepSizeGb != 256 || got.State != "READY" {
		t.Fatalf("instance = %+v", got)
	}
	if pl := got.PerformanceLimits; pl == nil || pl.MaxReadIops != 12000 || pl.MaxWriteIops != 4000 || pl.MaxReadThroughputBps != 120<<20 || pl.MaxWriteThroughputBps != 100<<20 {
		t.Errorf("performance limits = %+v, want Enterprise's 1 TiB row", pl)
	}

	shares := inst.Shares
	mk := func(id, mount string, gb int64) error {
		op, err := shares.Create(name, &filev1beta1.Share{MountName: mount, CapacityGb: gb}).ShareId(id).Do()
		return betaWait(t, svc, op, err)
	}
	if err := mk("s1", "s1", 100); err != nil {
		t.Fatalf("CreateShare s1: %v", err)
	}
	if err := mk("s2", "share_two", 200); err != nil {
		t.Fatalf("CreateShare s2: %v", err)
	}
	// Shares are 10 GiB to 1 TiB here, and fit in the instance.
	if err := mk("s3", "s3", 2000); fsHTTPCode(err) != http.StatusBadRequest {
		t.Errorf("a share of 2000 GiB: %v, want 400", err)
	}
	if err := mk("s3", "s3", 5); fsHTTPCode(err) != http.StatusBadRequest {
		t.Errorf("a share of 5 GiB: %v, want 400", err)
	}
	if err := mk("s3", "s3", 900); fsHTTPCode(err) != http.StatusBadRequest {
		t.Errorf("a share past the instance's capacity: %v, want 400 FAILED_PRECONDITION", err)
	}
	l, err := shares.List(name).Do()
	if err != nil || len(l.Shares) != 2 {
		t.Fatalf("ListShares = %v, %v", l, err)
	}
	op, err = shares.Patch(name+"/shares/s1", &filev1beta1.Share{CapacityGb: 300}).UpdateMask("capacity_gb").Do()
	if err := betaWait(t, svc, op, err); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	sh, err := shares.Get(name + "/shares/s1").Do()
	if err != nil || sh.CapacityGb != 300 || sh.State != "READY" || sh.MountName != "s1" {
		t.Fatalf("GetShare = %+v, %v", sh, err)
	}

	// Over gRPC too.
	bi, err := gc.GetInstance(ctx, &betapb.GetInstanceRequest{Name: name})
	if err != nil || !bi.GetMultiShareEnabled() {
		t.Fatalf("gRPC GetInstance = %v, %v", bi, err)
	}
	gl, err := gc.ListShares(ctx, &betapb.ListSharesRequest{Parent: name})
	if err != nil || len(gl.GetShares()) != 2 {
		t.Fatalf("gRPC ListShares = %v, %v", gl, err)
	}

	// Each share is a filesystem of its own capacity on the server.
	server := fsServerOf(t, h, got.Networks[0].IpAddresses[0])
	out, err := exec.Command("docker", "exec", server, "sh", "-c", "df -B1G --output=size,target /export/fs/s1 /export/fs/share_two | tail -n 2").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "/export/fs/s1") || !strings.Contains(string(out), "/export/fs/share_two") {
		t.Fatalf("share filesystems: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		size, _ := strconv.Atoi(f[0])
		want := map[string]int{"/export/fs/s1": 300, "/export/fs/share_two": 200}[f[1]]
		if size < want-5 || size > want {
			t.Errorf("%s is %d GiB, want about %d", f[1], size, want)
		}
	}
	// Both shares mount from a client, and a full share refuses writes.
	ip := got.Networks[0].IpAddresses[0]
	script := `set -e; mkdir -p /mnt/ct-ms1 /mnt/ct-ms2; mount -t nfs -o vers=3,nolock ` + ip + `:/s1 /mnt/ct-ms1; mount -t nfs -o vers=3,nolock ` + ip + `:/share_two /mnt/ct-ms2;
echo one > /mnt/ct-ms1/f; echo two > /mnt/ct-ms2/f; cat /mnt/ct-ms1/f /mnt/ct-ms2/f; umount /mnt/ct-ms1 /mnt/ct-ms2`
	if out, err := nodeSh(t, fsNode(t), script); err != nil || out != "one\ntwo" {
		t.Fatalf("mount the shares: %v\n%s", err, out)
	}

	// Snapshots are not supported for multishares.
	if _, err := fg.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: name, SnapshotId: "ct-ms-snap", Snapshot: &filestorepb.Snapshot{}}); err == nil {
		t.Errorf("CreateSnapshot of a multishare instance succeeded")
	}

	// The instance grows; then a share goes, with its filesystem.
	op, err = inst.Patch(name, &filev1beta1.Instance{CapacityGb: 1280}).UpdateMask("capacity_gb").Do()
	if err := betaWait(t, svc, op, err); err != nil {
		t.Fatalf("UpdateInstance capacity_gb: %v", err)
	}
	op, err = shares.Delete(name + "/shares/s2").Do()
	if err := betaWait(t, svc, op, err); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	if out, _ := exec.Command("docker", "exec", server, "ls", "/export/disks").CombinedOutput(); strings.Contains(string(out), "share_two") {
		t.Errorf("share_two's filesystem is left: %s", out)
	}
	l2, err := inst.List(region).Do()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	for _, in := range l2.Instances {
		if in.Name == name && (in.CapacityGb != 1280 || !in.MultiShareEnabled) {
			t.Errorf("listed instance = %+v", in)
		}
	}
}

// TestFilestorePerformanceLimitsAreEnforced (#2194): performance_limits
// answer each tier's documented limits
// (https://docs.cloud.google.com/filestore/docs/performance), and the
// server enforces them: a Zonal instance with custom performance of 2,000
// IOPS (16,384,000 bytes a second written, 49,152,000 read, 600 write
// IOPS) is measured from a client through NFS, then again after its
// performance_config is raised.
// covers: google.cloud.filestore.v1.CloudFilestoreManager/UpdateInstance
func TestFilestorePerformanceLimitsAreEnforced(t *testing.T) {
	h := New(t)
	fg, _ := filestoreClients(t, h)
	ctx := fsCtx(t)
	zone := "projects/" + h.Project() + "/locations/us-central1-a"

	basic := fsInstance(t, fg, zone, "ct-fs-perf-hdd", &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD,
		FileShares: fsShare("vol1", 1024), Networks: fsNetwork})
	if pl := basic.GetPerformanceLimits(); pl.GetMaxReadIops() != 600 || pl.GetMaxWriteIops() != 1000 || pl.GetMaxReadThroughputBps() != 100<<20 || pl.GetMaxWriteThroughputBps() != 100<<20 {
		t.Errorf("Basic HDD 1 TiB limits = %v", pl)
	}
	in := fsInstance(t, fg, zone, "ct-fs-perf", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL,
		FileShares: fsShare("vol1", 1024), Networks: fsNetwork,
		PerformanceConfig: &filestorepb.Instance_PerformanceConfig{Mode: &filestorepb.Instance_PerformanceConfig_FixedIops{FixedIops: &filestorepb.Instance_FixedIOPS{MaxIops: 2000}}}})
	pl := in.GetPerformanceLimits()
	if pl.GetMaxIops() != 2000 || pl.GetMaxReadIops() != 2000 || pl.GetMaxWriteIops() != 600 || pl.GetMaxReadThroughputBps() != 49152000 || pl.GetMaxWriteThroughputBps() != 16384000 {
		t.Fatalf("custom 2000 IOPS limits = %v", pl)
	}
	ip := in.GetNetworks()[0].GetIpAddresses()[0]
	// measure prints the seconds a 48 MiB write (flushed), a 48 MiB read
	// (from a fresh mount, so from the server) and 1,200 4 KiB direct
	// writes take.
	measure := func() (write, read, iops float64) {
		t.Helper()
		script := `set -e; d=/mnt/ct-perf; mkdir -p $d; mount -t nfs -o vers=3,nolock ` + ip + `:/vol1 $d
s=$(date +%s%N); dd if=/dev/zero of=$d/big bs=1M count=48 conv=fsync 2>/dev/null; e=$(date +%s%N); echo $(( (e - s) / 1000000 ))
umount $d; mount -t nfs -o vers=3,nolock ` + ip + `:/vol1 $d
s=$(date +%s%N); dd if=$d/big of=/dev/null bs=1M 2>/dev/null; e=$(date +%s%N); echo $(( (e - s) / 1000000 ))
s=$(date +%s%N); dd if=/dev/zero of=$d/small bs=4k count=1200 oflag=direct 2>/dev/null; e=$(date +%s%N); echo $(( (e - s) / 1000000 ))
rm -f $d/big $d/small; umount $d`
		out, err := nodeSh(t, fsNode(t), script)
		if err != nil {
			t.Fatalf("measure: %v\n%s", err, out)
		}
		f := strings.Fields(out)
		if len(f) != 3 {
			t.Fatalf("measure: %q", out)
		}
		ms := func(s string) float64 { v, _ := strconv.Atoi(s); return float64(v) / 1000 }
		return ms(f[0]), ms(f[1]), ms(f[2])
	}
	w, r, io := measure()
	t.Logf("at 2,000 IOPS: 48 MiB written in %.2fs (%.1f MB/s), read in %.2fs (%.1f MB/s); 1,200 4 KiB writes in %.2fs (%.0f IOPS)",
		w, 48*1.048576/w, r, 48*1.048576/r, io, 1200/io)
	// 48 MiB at 16.4 MB/s is 3.07s; at 49.2 MB/s, 1.02s; 1,200 writes at
	// 600 IOPS, 2s. A margin is left for the limiter's burst.
	if w < 2.5 {
		t.Errorf("48 MiB was written in %.2fs: faster than max_write_throughput_bps 16384000", w)
	}
	if r < 0.8 {
		t.Errorf("48 MiB was read in %.2fs: faster than max_read_throughput_bps 49152000", r)
	}
	if io < 1.6 {
		t.Errorf("1,200 writes took %.2fs: more than max_write_iops 600", io)
	}

	// Raised to 20,000 IOPS, the same work is much faster.
	op, err := fg.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{Instance: &filestorepb.Instance{Name: in.GetName(),
		PerformanceConfig: &filestorepb.Instance_PerformanceConfig{Mode: &filestorepb.Instance_PerformanceConfig_FixedIops{FixedIops: &filestorepb.Instance_FixedIOPS{MaxIops: 20000}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"performance_config"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	up, err := op.Wait(ctx)
	if err != nil || up.GetPerformanceLimits().GetMaxWriteThroughputBps() != 163840000 {
		t.Fatalf("UpdateInstance = %v, %v", up.GetPerformanceLimits(), err)
	}
	w2, r2, io2 := measure()
	t.Logf("at 20,000 IOPS: written in %.2fs, read in %.2fs, 1,200 writes in %.2fs", w2, r2, io2)
	if w2 > w/2 {
		t.Errorf("at 20,000 IOPS the write took %.2fs, at 2,000 %.2fs: the limit did not rise", w2, w)
	}
}

// TestFilestoreCMEKEncryptsTheShareAtRest (#2194): an instance with a
// customer-managed key keeps its share encrypted at rest (dm-crypt with a
// data key Cloud KMS wraps): what a client writes is not in the share's
// file in plain text. Disabling the key's version suspends the instance
// and closes the share, whose data no client can then read, as
// https://docs.cloud.google.com/filestore/docs/cmek documents; enabling it
// again serves the same data.
// covers: google.cloud.filestore.v1.CloudFilestoreManager/GetInstance
func TestFilestoreCMEKEncryptsTheShareAtRest(t *testing.T) {
	h := New(t)
	fg, _ := filestoreClients(t, h)
	kc := kmsClients(t, h)["grpc"]
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	sfx := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-central1", KeyRingId: "fs-rest-" + sfx})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "fs-key",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	in := fsInstance(t, fg, zone, "ct-fs-atrest", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL,
		FileShares: fsShare("vol1", 1024), Networks: fsNetwork, KmsKeyName: key.GetName()})
	ip := in.GetNetworks()[0].GetIpAddresses()[0]
	server := fsServerOf(t, h, ip)
	node := fsNode(t)
	marker := "cloudburrow-at-rest-" + sfx
	// Each mount is soft, with a short timeout, so a share that stops
	// answering fails its step instead of holding it.
	mount := `d=/mnt/ct-atrest; mkdir -p $d; mount -t nfs -o vers=3,nolock,soft,timeo=50,retrans=2,retry=0 ` + ip + `:/vol1 $d`
	fsStep(t, ip, "write through the share", 2*time.Minute, func(ctx context.Context) error {
		out, err := dockerSh(ctx, node, `set -e; `+mount+`; for i in $(seq 1 2000); do echo `+marker+`; done > $d/secret.txt; umount $d`)
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})
	// The share is a dm-crypt mapping, and its file holds no plain text:
	// the block where the share keeps secret.txt holds the marker read
	// through the mapping and not in the share's file. Only that block is
	// read; the file is a sparse 1 TiB, which a scan of it would take
	// hours to read. sync -f flushes the share's filesystem alone: a bare
	// sync waits on every filesystem of the Docker VM, other clients' NFS
	// mounts too.
	fsStep(t, ip, "read the share's file at rest", time.Minute, func(ctx context.Context) error {
		out, err := dockerSh(ctx, server, `set -e; sync -f /export/fs/vol1
dmsetup table | grep -c " crypt aes-xts-plain64 "
dev=/dev/mapper/$(dmsetup ls --target crypt | awk '{print $1}')
bs=$(dumpe2fs -h "$dev" 2>/dev/null | awk -F: '/^Block size/ {print $2 + 0}')
b=$(debugfs -R "blocks /data/secret.txt" "$dev" 2>/dev/null | awk '{print $1}')
dd if="$dev" bs="$bs" skip="$b" count=1 2>/dev/null | grep -c -a `+marker+` || true
dd if=/export/disks/vol1.img bs="$bs" skip="$b" count=1 2>/dev/null | grep -c -a `+marker+` || true`)
		if f := strings.Fields(out); err != nil || len(f) != 3 || f[0] != "1" || f[1] == "0" || f[2] != "0" {
			return fmt.Errorf("%v: %q (want one crypt mapping, the marker through it, none in the file)", err, out)
		}
		return nil
	})

	ver := key.GetPrimary().GetName()
	setVersion := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		fsStep(t, ip, "set the key version "+st.String(), time.Minute, func(ctx context.Context) error {
			_, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: ver, State: st},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
			return err
		})
	}
	waitFor := func(want filestorepb.Instance_State) {
		t.Helper()
		var got *filestorepb.Instance
		fsStep(t, ip, "wait for "+want.String(), 3*time.Minute, func(ctx context.Context) error {
			for {
				var err error
				got, err = fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: in.GetName()})
				if err == nil && got.GetState() == want {
					return nil
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("instance is %v (%v), want %v", got.GetState(), err, want)
				case <-time.After(2 * time.Second):
				}
			}
		})
	}
	setVersion(kmspb.CryptoKeyVersion_DISABLED)
	waitFor(filestorepb.Instance_SUSPENDED)
	server = fsServerOf(t, h, ip)
	fsStep(t, ip, "check the suspended server", time.Minute, func(ctx context.Context) error {
		out, _ := dockerSh(ctx, server, `dmsetup table 2>/dev/null | grep -c " crypt " || true; grep -c " /export/fs/" /proc/mounts || true`)
		if f := strings.Fields(out); len(f) != 2 || f[0] != "0" || f[1] != "0" {
			t.Errorf("suspended, the share is still open: %q", out)
		}
		return nil
	})
	fsStep(t, ip, "mount the suspended share", time.Minute, func(ctx context.Context) error {
		if out, err := dockerSh(ctx, node, `timeout 20 sh -c '`+mount+`'`); err == nil {
			_, _ = dockerSh(ctx, node, "umount -f /mnt/ct-atrest")
			t.Errorf("suspended, the share still mounts: %s", out)
		}
		return nil
	})
	setVersion(kmspb.CryptoKeyVersion_ENABLED)
	waitFor(filestorepb.Instance_READY)
	fsStep(t, ip, "read after the key is enabled again", 2*time.Minute, func(ctx context.Context) error {
		out, err := dockerSh(ctx, node, `set -e; `+mount+`; head -n 1 $d/secret.txt; umount $d`)
		if err != nil || out != marker {
			return fmt.Errorf("%v: %q", err, out)
		}
		return nil
	})
}

// TestGKEFilestoreMultishareClass (#2194): GKE's enterprise-multishare-rwx
// class, as https://docs.cloud.google.com/filestore/docs/multishares
// documents it: two claims become two shares of one Enterprise multishare
// instance (seen with Google's v1beta1 client), pods mount and write each,
// growing a claim grows its share, and deleting the claims deletes the
// shares, then the instance with its last share.
func TestGKEFilestoreMultishareClass(t *testing.T) {
	h := New(t)
	svc, _ := fsBeta(t, h)
	cg, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	cname := zone + "/clusters/ct-gke-fs-ms"
	op, err := cg.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: zone, Cluster: &containerpb.Cluster{Name: "ct-gke-fs-ms", InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cg.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: cname}); err == nil {
			gkeWait(t, cg, project, "us-central1-a", op)
		}
	})
	gkeWait(t, cg, project, "us-central1-a", op)
	c, err := cg.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kc := gkeKubeconfig(t, c)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := kubectlRun(kc, "", time.Minute, "get", "storageclass", "enterprise-multishare-rwx", "-o", "jsonpath={.provisioner} {.parameters.multishare} {.parameters.tier}")
		if out == "filestore.csi.storage.gke.io true enterprise" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("enterprise-multishare-rwx was not installed: %q", out)
		}
		time.Sleep(2 * time.Second)
	}
	var manifest string
	for _, claim := range []string{"ct-ms-a", "ct-ms-b"} {
		manifest += `---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ` + claim + `}
spec: {accessModes: [ReadWriteMany], storageClassName: enterprise-multishare-rwx, resources: {requests: {storage: 100Gi}}}
---
apiVersion: v1
kind: Pod
metadata: {name: ` + claim + `}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh", "-c", "sleep 100000"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: ` + claim + `}
`
	}
	if out, err := kubectlRun(kc, manifest, time.Minute, "apply", "-f", "-"); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
	instances := map[string]bool{}
	for _, claim := range []string{"ct-ms-a", "ct-ms-b"} {
		waitPodRunning(t, kc, claim)
		if out, err := kubectlRun(kc, "", time.Minute, "exec", claim, "--", "sh", "-c", "echo "+claim+" > /data/f && cat /data/f"); err != nil || out != claim {
			t.Fatalf("pod %s write and read: %v: %s", claim, err, out)
		}
		pv, _ := kubectlRun(kc, "", time.Minute, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
		handle, _ := kubectlRun(kc, "", time.Minute, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeHandle}")
		p := strings.Split(handle, "/")
		if len(p) != 6 || p[0] != "modeMultishare" || p[1] != "enterprise-multishare-rwx" || p[2] != project || p[3] != "us-central1" || p[5] != pv {
			t.Fatalf("PV %s handle = %q", pv, handle)
		}
		name := "projects/" + project + "/locations/us-central1/instances/" + p[4]
		instances[name] = true
		sh, err := svc.Projects.Locations.Instances.Shares.Get(name + "/shares/" + pv).Do()
		if err != nil || sh.CapacityGb != 100 {
			t.Fatalf("share of %s = %+v, %v", claim, sh, err)
		}
	}
	if len(instances) != 1 {
		t.Fatalf("the two claims are on %d instances, want one: %v", len(instances), instances)
	}
	var inst string
	for n := range instances {
		inst = n
	}
	in, err := svc.Projects.Locations.Instances.Get(inst).Do()
	if err != nil || !in.MultiShareEnabled || in.Tier != "ENTERPRISE" || in.MaxShareCount != 10 {
		t.Fatalf("instance = %+v, %v", in, err)
	}
	// Growing a claim grows its share.
	if out, err := kubectlRun(kc, "", time.Minute, "patch", "pvc", "ct-ms-a", "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"200Gi"}}}}`); err != nil {
		t.Fatalf("expand: %v\n%s", err, out)
	}
	deadline = time.Now().Add(2 * time.Minute)
	for {
		got, _ := kubectlRun(kc, "", time.Minute, "get", "pvc", "ct-ms-a", "-o", "jsonpath={.status.capacity.storage}")
		if got == "200Gi" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ct-ms-a's capacity is %q, want 200Gi", got)
		}
		time.Sleep(2 * time.Second)
	}
	// Deleting the claims deletes the shares, then the instance.
	if out, err := kubectlRun(kc, "", 3*time.Minute, "delete", "pod", "ct-ms-a", "ct-ms-b", "--wait=true"); err != nil {
		t.Fatalf("delete pods: %v\n%s", err, out)
	}
	if out, err := kubectlRun(kc, "", 3*time.Minute, "delete", "pvc", "ct-ms-a", "ct-ms-b", "--wait=true"); err != nil {
		t.Fatalf("delete claims: %v\n%s", err, out)
	}
	deadline = time.Now().Add(4 * time.Minute)
	for {
		_, err := svc.Projects.Locations.Instances.Get(inst).Do()
		if fsHTTPCode(err) == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the multishare instance %s is left after its last share: %v", inst, err)
		}
		time.Sleep(3 * time.Second)
	}
}
