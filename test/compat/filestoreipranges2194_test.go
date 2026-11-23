//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// TestFilestoreExportIPRangesMatchVMAndPodAddresses (#2194): an export's
// ip_ranges name clients by their VPC addresses, as
// https://docs.cloud.google.com/filestore/docs/access-control documents
// them. A Compute Engine VM's NFS connection reaches the server from the
// VM's internal IP, and a Kubernetes Engine pod's from the pod's address;
// the instance's ip_ranges hold those two /32s, and the cluster node,
// outside them, is refused the mount.
func TestFilestoreExportIPRangesMatchVMAndPodAddresses(t *testing.T) {
	h := New(t)
	node := fsNode(t)
	fg, _ := filestoreClients(t, h)
	cg, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm := "ct-fsvm-" + sfx

	// The VM, on the default network.
	insts, err := computeapi.NewInstancesRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	op, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vm),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
	}})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("insert VM: %v", err)
	}
	got, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vm})
	if err != nil {
		t.Fatal(err)
	}
	vmIP := got.GetNetworkInterfaces()[0].GetNetworkIP()

	// The pod, in a Kubernetes Engine cluster.
	czone := "projects/" + p + "/locations/" + zone
	cname := czone + "/clusters/ct-fs-ipr"
	cop, err := cg.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: czone, Cluster: &containerpb.Cluster{Name: "ct-fs-ipr", InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cg.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: cname}); err == nil {
			gkeWait(t, cg, p, zone, op)
		}
	})
	gkeWait(t, cg, p, zone, cop)
	c, err := cg.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kc := gkeKubeconfig(t, c)
	if out, err := kubectlRun(kc, `apiVersion: v1
kind: Pod
metadata: {name: ct-ipr}
spec:
  terminationGracePeriodSeconds: 1
  containers: [{name: c, image: "busybox:1.36", command: ["sh", "-c", "sleep 100000"]}]
`, time.Minute, "apply", "-f", "-"); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
	waitPodRunning(t, kc, "ct-ipr")
	podIP, _ := kubectlRun(kc, "", time.Minute, "get", "pod", "ct-ipr", "-o", "jsonpath={.status.podIP}")
	if podIP == "" || podIP == vmIP {
		t.Fatalf("pod IP %q (VM %q)", podIP, vmIP)
	}

	// The instance exports vol1 to those two addresses only.
	in := fsInstance(t, fg, "projects/"+p+"/locations/"+zone, "ct-fs-ipr-"+sfx, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD,
		Networks: []*filestorepb.NetworkConfig{{Network: "default"}},
		FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024, NfsExportOptions: []*filestorepb.NfsExportOptions{{
			IpRanges: []string{vmIP, podIP + "/32"}, AccessMode: filestorepb.NfsExportOptions_READ_WRITE, SquashMode: filestorepb.NfsExportOptions_NO_ROOT_SQUASH}}}}})
	ip := in.GetNetworks()[0].GetIpAddresses()[0]
	server := fsServerOf(t, h, ip)

	// peers are the addresses the server's NFS port is connected from.
	peers := func() string {
		out, _ := exec.Command("docker", "exec", server, "ss", "-tnH", "state", "established", "( sport = :2049 )").CombinedOutput()
		return string(out)
	}
	allConns := func() string {
		out, _ := exec.Command("docker", "exec", server, "ss", "-tn").CombinedOutput()
		return string(out)
	}
	holds := func(addr string) bool {
		for _, l := range strings.Split(peers(), "\n") {
			if f := strings.Fields(l); len(f) >= 4 && strings.HasPrefix(f[len(f)-1], addr+":") {
				return true
			}
		}
		return false
	}
	// runVM runs cmd on the VM, its output once it ends.
	runVM := func(cmd string) (string, error) {
		sshAddr := vmSSHAddress(t, h.Endpoint(EnvCompute), p, zone, vm)
		deadline := time.Now().Add(3 * time.Minute)
		for {
			cl, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				defer cl.Close()
				s, err := cl.NewSession()
				if err != nil {
					return "", err
				}
				out, err := s.CombinedOutput(cmd)
				return string(out), err
			}
			if time.Now().After(deadline) {
				return "", err
			}
			time.Sleep(2 * time.Second)
		}
	}
	// Each client holds a connection to the NFS port open for a minute.
	hold := "timeout 60 sh -c 'sleep 60 | nc " + ip + " 2049'; echo nc=$?"
	vmOut, podOut := make(chan string, 1), make(chan string, 1)
	go func() {
		out, err := runVM(hold)
		vmOut <- fmt.Sprintf("%s %v", out, err)
	}()
	go func() {
		out, err := kubectlRun(kc, "", 2*time.Minute, "exec", "ct-ipr", "--", "sh", "-c", hold)
		podOut <- fmt.Sprintf("%s %v", out, err)
	}()
	deadline := time.Now().Add(time.Minute)
	for !holds(vmIP) || !holds(podIP) {
		if time.Now().After(deadline) {
			t.Fatalf("the server's NFS connections are from\n%s\nwant the VM's internal IP %s and the pod's address %s (VM: %s; pod: %s; all connections: %s)", peers(), vmIP, podIP, <-vmOut, <-podOut, allConns())
		}
		time.Sleep(2 * time.Second)
	}
	// The node is outside the ranges: refused.
	if out, err := nodeSh(t, node, `mkdir -p /mnt/ct-ipr; timeout 20 mount -t nfs -o vers=3,nolock,soft,retry=0 `+ip+`:/vol1 /mnt/ct-ipr`); err == nil {
		nodeSh(t, node, "umount /mnt/ct-ipr")
		t.Fatalf("the node, outside ip_ranges %s and %s, mounted vol1: %s", vmIP, podIP, out)
	}
}
