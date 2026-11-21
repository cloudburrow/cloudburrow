//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cbredis "github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestMemorystoreCMEKFilesOnDisk (#2113), with cloud.google.com/go/redis/
// apiv1, cloud.google.com/go/kms/apiv1 and go-redis: a Memorystore for
// Redis instance with customer_managed_key and RDB persistence keeps its
// persistence files on disk only sealed with its data key, which its pod
// asks the instance's Cloud KMS to unwrap
// (https://docs.cloud.google.com/memorystore/docs/redis/cmek: with CMEK,
// "customer data that are stored in persistent storage" is encrypted with
// the key).
//
//   - A value written and snapshotted (SAVE) is in the server's own RDB
//     file, which is on a memory volume, and nowhere in the clear on the
//     instance's persistent volume: neither in the pod's view of it nor in
//     the kind node's directory that holds it.
//   - The server's pod deleted, its next pod unwraps the data key and
//     serves the value again.
//   - The key's version disabled: the instance is suspended as Memorystore
//     documents, no READY state, suspension_reasons
//     CUSTOMER_MANAGED_KEY_ISSUE, its server gone, so the data cannot be
//     read; the sealed file stays on the volume.
//   - The version enabled again: the instance resumes, READY, and serves
//     the value it had.
func TestMemorystoreCMEKFilesOnDisk(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	c, project := memorystoreAdminClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if kubeconfig == "" || cluster == "" {
		t.Skipf("%s and CLOUDBURROW_TEST_CLUSTER are required to read the server's volume", envKubeconfig)
	}
	for _, bin := range []string{"docker", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to read the server's volume", bin)
		}
	}
	kc := kmsClients(t, h)["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	key := memorystoreKey(t, h, project)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("cmekdisk-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, CustomerManagedKey: key,
			PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB,
				RdbSnapshotPeriod: redispb.PersistenceConfig_ONE_HOUR}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY || inst.GetCustomerManagedKey() != key {
		t.Fatalf("created: state %v, customer_managed_key %q", inst.GetState(), inst.GetCustomerManagedKey())
	}

	// A value an RDB file keeps as it is: 16 bytes, below the length
	// Redis compresses (LZF) a string from.
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	value := hex.EncodeToString(raw)
	rc := redisAt(inst, "")
	if err := rc.Set(ctx, "cmek-probe", value, 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	if err := rc.Save(ctx).Err(); err != nil {
		t.Fatalf("SAVE: %v", err)
	}
	_ = rc.Close()

	server := cbredis.ServerName(name)
	kubectl := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	ns, err := kubectl("get", "deployments", "-A", "--field-selector", "metadata.name="+server, "-o", "jsonpath={.items[0].metadata.namespace}")
	if err != nil || ns == "" {
		t.Fatalf("the server's Deployment %s: %q, %v", server, ns, err)
	}
	// node is the kind node that holds the volume, the server's pod's.
	nodeName, err := kubectl("-n", ns, "get", "pods", "-l", "app="+server, "-o", "jsonpath={.items[0].spec.nodeName}")
	if err != nil || nodeName == "" {
		nodeName = cluster + "-control-plane"
	}
	node := func() string { return nodeName }
	inPod := func(script string) (string, error) {
		return kubectl("-n", ns, "exec", "deploy/"+server, "-c", server, "--", "sh", "-c", script)
	}

	// cat is the bytes of the files named, read in the server's container
	// (pod) or on the kind node (node); busybox grep does not match in a
	// binary RDB file, so they are searched here.
	cat := func(where string, files string) []byte {
		t.Helper()
		var cmd *exec.Cmd
		if where == "pod" {
			cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "exec", "deploy/"+server, "-c", server, "--", "sh", "-c", "cat "+files)
		} else {
			cmd = exec.CommandContext(ctx, "docker", "exec", node(), "sh", "-c", "cat "+files)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cat %s in the %s: %v", files, where, err)
		}
		return out
	}
	plainRDB := func(b []byte) bool { return bytes.Contains(b, []byte(value)) || bytes.Contains(b, []byte("REDIS0")) }

	// The server's own file has the value; its directory is in memory.
	eventuallyTrue(t, "the snapshot has the value", func() bool { return bytes.Contains(cat("pod", "/data/dump.rdb"), []byte(value)) })
	if out, err := inPod(`awk '$2 == "/data" { print $3 }' /proc/mounts`); err != nil || out != "tmpfs" {
		t.Errorf("the server's directory /data is on %q (%v), want tmpfs", out, err)
	}
	// Its sealed copy, once the keeper has sealed the snapshot (every
	// second), is all the volume holds.
	time.Sleep(3 * time.Second)
	if out, err := inPod("ls -A /sealed"); err != nil || out != "dump.rdb.sealed" {
		t.Errorf("the volume, from the pod, holds %q (%v), want dump.rdb.sealed alone", out, err)
	}
	if b := cat("pod", "/sealed/*"); !bytes.HasPrefix(b, []byte("CBCMEK1\n")) || plainRDB(b) {
		t.Errorf("the volume's dump.rdb.sealed is not sealed: it starts %q, or has the value or an RDB header", b[:min(len(b), 16)])
	}

	// The volume as the node keeps it on its disk.
	pv, err := kubectl("-n", ns, "get", "pvc", server+"-data", "-o", "jsonpath={.spec.volumeName}")
	if err != nil || pv == "" {
		t.Fatalf("the server's claim: %q, %v", pv, err)
	}
	dir, err := kubectl("get", "pv", pv, "-o", "jsonpath={.spec.hostPath.path}{.spec.local.path}")
	if err != nil || dir == "" {
		t.Fatalf("the volume's directory on the node: %q, %v", dir, err)
	}
	onNode := func() {
		t.Helper()
		ls, err := exec.CommandContext(ctx, "docker", "exec", node(), "ls", "-A", dir).Output()
		if err != nil || strings.TrimSpace(string(ls)) != "dump.rdb.sealed" {
			t.Errorf("the node's %s holds %q (%v), want dump.rdb.sealed alone", dir, ls, err)
		}
		if b := cat("node", dir+"/*"); !bytes.HasPrefix(b, []byte("CBCMEK1\n")) || plainRDB(b) {
			t.Errorf("the node's %s/dump.rdb.sealed is not sealed", dir)
		}
	}
	onNode()

	// A restart with the key enabled: the next pod unwraps the data key
	// and opens the snapshot.
	if out, err := kubectl("-n", ns, "delete", "pod", "-l", "app="+server, "--wait=true"); err != nil {
		t.Fatalf("delete the server's pod: %v: %s", err, out)
	}
	if out, err := kubectl("-n", ns, "rollout", "status", "deploy/"+server, "--timeout=3m"); err != nil {
		t.Fatalf("the server's next pod: %v: %s", err, out)
	}
	eventuallyTrue(t, "the restarted server serves the value", func() bool {
		got, err := inPod("valkey-cli GET cmek-probe")
		return err == nil && got == value
	})

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

	// The key's version disabled: suspended, and the data unreachable.
	version(kmspb.CryptoKeyVersion_DISABLED)
	got := until("suspended", func(i *redispb.Instance) bool { return len(i.GetSuspensionReasons()) > 0 })
	if got.GetState() == redispb.Instance_READY || got.GetSuspensionReasons()[0] != redispb.Instance_CUSTOMER_MANAGED_KEY_ISSUE {
		t.Errorf("suspended: state %v, suspension_reasons %v", got.GetState(), got.GetSuspensionReasons())
	}
	eventuallyTrue(t, "the suspended instance's server is gone", func() bool {
		out, err := kubectl("-n", ns, "get", "pods", "-l", "app="+server, "-o", "name")
		return err == nil && out == ""
	})
	probe := redisAt(inst, "")
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if err := probe.Get(pctx, "cmek-probe").Err(); err == nil {
		t.Error("a suspended instance's data is still served")
	}
	pcancel()
	_ = probe.Close()
	onNode()
	if st := until("still suspended", func(*redispb.Instance) bool { return true }); st.GetState() == redispb.Instance_READY {
		t.Errorf("a suspended instance is READY")
	}

	// Enabled again: resumed, with its data.
	version(kmspb.CryptoKeyVersion_ENABLED)
	got = until("resumed", func(i *redispb.Instance) bool { return i.GetState() == redispb.Instance_READY })
	if len(got.GetSuspensionReasons()) != 0 {
		t.Errorf("resumed: suspension_reasons %v", got.GetSuspensionReasons())
	}
	rc = redisAt(got, "")
	defer rc.Close()
	eventuallyTrue(t, "the resumed instance serves the value", func() bool {
		v, err := rc.Get(ctx, "cmek-probe").Result()
		return err == nil && v == value
	})
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestMemorystoreValkeyCMEKFilesOnDisk (#2113), with cloud.google.com/go/
// memorystore/apiv1, cloud.google.com/go/kms/apiv1 and go-redis: a
// Memorystore for Valkey instance with kms_key and AOF persistence keeps
// its node's append-only file on disk only sealed with its data key
// (https://docs.cloud.google.com/memorystore/docs/valkey/about-cmek): a value
// written is in the node's own AOF, on a memory volume, and in no file of
// its persistent volume. With the key's version disabled the instance
// serves on, ACTIVE, with its persistence deactivated
// (https://docs.cloud.google.com/memorystore/docs/valkey/about-cmek):
// a write reaches no file of the volume; enabled again, an update
// re-enables persistence and the volume is written again, sealed.
func TestMemorystoreValkeyCMEKFilesOnDisk(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is required to read the node's volume", envKubeconfig)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to read the node's volume")
	}
	kc := kmsClients(t, h)["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	key := memorystoreKey(t, h, project)
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("kmsdisk-%d", time.Now().UnixNano()%1e8), func(in *memorystorepb.Instance) {
		in.KmsKey = proto.String(key)
		in.PersistenceConfig = &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_AOF,
			AofConfig: &memorystorepb.PersistenceConfig_AOFConfig{AppendFsync: memorystorepb.PersistenceConfig_AOFConfig_ALWAYS}}
	})
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	value := hex.EncodeToString(raw)
	if err := valkeyPrimary(t, inst).Set(ctx, "cmek-probe", value, 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}

	base := cbredis.ValkeyServerBase(inst.GetName())
	// The node's Deployment: named base-N, its container the same.
	var ns, pod, container string
	eventuallyTrue(t, "the node's server", func() bool {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "deployments", "-A", "-o",
			`jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}`).Output()
		if err != nil {
			return false
		}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && strings.HasPrefix(f[1], base+"-") {
				ns, pod, container = f[0], "deploy/"+f[1], f[1]
				return true
			}
		}
		return false
	})
	inPod := func(script string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "exec", pod, "-c", container,
			"--", "sh", "-c", script).Output()
		if err != nil {
			t.Fatalf("%s in the node's pod: %v", script, err)
		}
		return out
	}
	eventuallyTrue(t, "the node's AOF has the value", func() bool {
		return bytes.Contains(inPod("find /data -type f -exec cat {} +"), []byte(value))
	})
	time.Sleep(3 * time.Second)
	files := strings.Fields(string(inPod("find /sealed -type f")))
	if len(files) == 0 {
		t.Fatal("the node's volume holds no sealed file")
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".sealed") {
			t.Errorf("a file on the node's volume is not sealed: %s", f)
		}
	}
	if b := inPod("find /sealed -type f -exec cat {} +"); bytes.Contains(b, []byte(value)) || bytes.Contains(b, []byte("cmek-probe")) {
		t.Error("the node's volume has the value or its key in the clear")
	}

	versionState := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
	until := func(what string, ok func(*memorystorepb.Instance) bool) *memorystorepb.Instance {
		t.Helper()
		for {
			got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: inst.GetName()})
			if err == nil && ok(got) {
				return got
			}
			if ctx.Err() != nil {
				t.Fatalf("%s: %v, %v", what, got, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	sealedNow := func() []byte { return inPod("find /sealed -type f -name '*.sealed' | sort | xargs cat") }

	// The key's version disabled: the instance serves on, its persistence
	// deactivated, so a write reaches no file on the volume.
	versionState(kmspb.CryptoKeyVersion_DISABLED)
	got := until("the key's state reported", func(i *memorystorepb.Instance) bool {
		return i.GetEncryptionInfo().GetKmsKeyPrimaryState() == memorystorepb.EncryptionInfo_DISABLED
	})
	if got.GetState() != memorystorepb.Instance_ACTIVE {
		t.Errorf("with the key disabled: state %v, want ACTIVE", got.GetState())
	}
	time.Sleep(8 * time.Second) // the nodes' persistence off, and the keeper's own check
	before := sealedNow()
	rc := valkeyPrimary(t, got)
	if err := rc.Set(ctx, "while-disabled", value, 0).Err(); err != nil {
		t.Fatalf("SET with the key disabled: %v", err)
	}
	if v, err := rc.Get(ctx, "cmek-probe").Result(); err != nil || v != value {
		t.Errorf("GET with the key disabled = %q, %v", v, err)
	}
	time.Sleep(4 * time.Second)
	if !bytes.Equal(before, sealedNow()) {
		t.Error("the node's volume was written while the key's version was disabled")
	}
	port, _ := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "get", pod, "-o",
		"jsonpath={.spec.template.spec.containers[0].ports[0].containerPort}").Output()
	if out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "exec", pod, "-c", container,
		"--", "sh", "-c", "valkey-cli -h $(hostname -i) -p "+strings.TrimSpace(string(port))+" CONFIG GET appendonly").CombinedOutput(); err != nil || !strings.Contains(string(out), "no") {
		t.Errorf("appendonly with the key disabled: %q, %v", out, err)
	}

	// Enabled again: an update re-enables persistence, and the volume is
	// written again, still sealed.
	versionState(kmspb.CryptoKeyVersion_ENABLED)
	got = until("re-enabled", func(i *memorystorepb.Instance) bool {
		return i.GetState() == memorystorepb.Instance_ACTIVE && i.GetEncryptionInfo().GetKmsKeyPrimaryState() == memorystorepb.EncryptionInfo_ENABLED
	})
	eventuallyTrue(t, "the volume is written again", func() bool {
		now := sealedNow()
		return !bytes.Equal(before, now) && !bytes.Contains(now, []byte(value))
	})
	valkeyEventually(t, ctx, valkeyPrimary(t, got), "cmek-probe", value)
}
