package filestorecsi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestGiBRoundsUp(t *testing.T) {
	for q, want := range map[string]int64{"1Ti": 1024, "100Gi": 100, "1G": 1, "1500Mi": 2, "2.5Ti": 2560} {
		if got, err := GiB(q); err != nil || got != want {
			t.Errorf("GiB(%q) = %d, %v; want %d", q, got, err, want)
		}
	}
	if _, err := GiB("lots"); err == nil {
		t.Error("a bad quantity was accepted")
	}
}

func TestMountArgs(t *testing.T) {
	got, err := MountArgs(map[string]string{"ip": "10.0.0.2", "volume": "vol1", "protocol": "NFS_V4_1"}, "/t", true)
	if err != nil || strings.Join(got, " ") != "mount -t nfs -o vers=4.1,ro 10.0.0.2:/vol1 /t" {
		t.Fatalf("MountArgs = %v, %v", got, err)
	}
	got, _ = MountArgs(map[string]string{"ip": "10.0.0.2", "volume": "vol1"}, "/t", false)
	if strings.Join(got, " ") != "mount -t nfs -o vers=3 10.0.0.2:/vol1 /t" {
		t.Fatalf("MountArgs = %v", got)
	}
	if _, err := MountArgs(map[string]string{"ip": "x;y", "volume": "vol1"}, "/t", false); err == nil {
		t.Fatal("a bad address was accepted")
	}
}

func TestManifestInstallsGKEsStorageClasses(t *testing.T) {
	m := Manifest("default")
	for _, want := range []string{"kind: CSIDriver", "attachRequired: false", "name: standard-rwx", "name: premium-rwx", "name: zonal-rwx",
		"name: enterprise-rwx", "provisioner: " + Driver, "volumeBindingMode: WaitForFirstConsumer"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q", want)
		}
	}
}

// TestNodePluginServesCSIAndRegistration: the kubelet's calls, encoded as
// the generated clients encode them, reach the plugin.
func TestNodePluginServesCSIAndRegistration(t *testing.T) {
	dir, err := os.MkdirTemp("", "csi")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var mounts []string
	n := &Node{NodeID: "node-1", Mount: func(_ context.Context, args ...string) error {
		mounts = append(mounts, strings.Join(args, " "))
		return nil
	}, Mounted: func(p string) bool { return len(mounts) == 1 && strings.HasSuffix(mounts[0], p) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.Serve(ctx, filepath.Join(dir, "p"), filepath.Join(dir, "r")) }()
	sock := filepath.Join(dir, "r", Driver+"-reg.sock")
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	call := func(path, method string, req []byte) msg {
		t.Helper()
		cc, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		var out []byte
		if req == nil {
			req = []byte{}
		}
		if err := cc.Invoke(context.Background(), method, &req, &out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		m, err := decode(out)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	info := call(sock, "/pluginregistration.Registration/GetInfo", nil)
	if info.get(1) != "CSIPlugin" || info.get(2) != Driver || !strings.HasSuffix(info.get(3), "csi.sock") || info.get(4) != "1.0.0" {
		t.Fatalf("GetInfo = %+v", info)
	}
	csi := filepath.Join(dir, "p", "csi.sock")
	if got := call(csi, "/csi.v1.Identity/GetPluginInfo", nil); got.get(1) != Driver {
		t.Fatalf("GetPluginInfo = %+v", got)
	}
	if got := call(csi, "/csi.v1.Node/NodeGetInfo", nil); got.get(1) != "node-1" {
		t.Fatalf("NodeGetInfo = %+v", got)
	}
	target := filepath.Join(dir, "target")
	entry := func(k, v string) []byte { return enc(nil).str(1, k).str(2, v) }
	req := enc(nil).str(1, "modeInstance/us-central1-a/pvc-1/vol1").str(4, target).
		message(8, entry("ip", "10.0.0.2")).message(8, entry("volume", "vol1"))
	req = protowire.AppendTag(req, 6, protowire.VarintType)
	req = protowire.AppendVarint(req, 0)
	call(csi, "/csi.v1.Node/NodePublishVolume", req)
	if len(mounts) != 1 || mounts[0] != "mount -t nfs -o vers=3 10.0.0.2:/vol1 "+target {
		t.Fatalf("mounts = %v", mounts)
	}
	call(csi, "/csi.v1.Node/NodeUnpublishVolume", enc(nil).str(1, "v").str(2, target))
	if len(mounts) != 2 || mounts[1] != "umount "+target {
		t.Fatalf("mounts = %v", mounts)
	}
}
