//go:build compat

package compat

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kubectlRun runs kubectl with kc and answers its combined output.
func kubectlRun(kc string, stdin string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kc}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// waitPodRunning waits for pod name to be Running.
func waitPodRunning(t *testing.T, kc, name string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		phase, _ := kubectlRun(kc, "", time.Minute, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		if phase == "Running" {
			return
		}
		if phase == "Failed" || phase == "Succeeded" || time.Now().After(deadline) {
			desc, _ := kubectlRun(kc, "", time.Minute, "describe", "pod", name)
			t.Fatalf("pod %s is %q:\n%s", name, phase, desc)
		}
		time.Sleep(2 * time.Second)
	}
}

// sharePod is a pod that mounts in's share at /mnt/share by Kubernetes'
// NFS volume and runs until deleted.
func sharePod(name string, in *filestorepb.Instance) string {
	return `apiVersion: v1
kind: Pod
metadata: {name: ` + name + `}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh", "-c", "sleep 100000"]
    volumeMounts: [{name: share, mountPath: /mnt/share}]
  volumes:
  - name: share
    nfs: {server: "` + in.GetNetworks()[0].GetIpAddresses()[0] + `", path: "/` + in.GetFileShares()[0].GetName() + `"}
`
}

// TestGKEFilestoreDeleteTheInstanceBeforeThePodAndTheCluster (#2192): an
// instance serves at the second address of its reserved_ip_range, which a
// Kubernetes Engine cluster's pods mount; the instance is then deleted
// while the pods still mount it. As on Google, the pods' reads fail with a
// stale file handle, and the pods, then the cluster, are still deleted.
// Both protocols are checked: NFSv4.1 (Zonal) and NFSv3 (Basic HDD).
func TestGKEFilestoreDeleteTheInstanceBeforeThePodAndTheCluster(t *testing.T) {
	h := New(t)
	fg, _ := filestoreClients(t, h)
	cg, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := fsCtx(t)
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"

	zonal := fsInstance(t, fg, zone, "ct-fs-gone4", &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1,
		FileShares: fsShare("vol1", 1024), Networks: []*filestorepb.NetworkConfig{{Network: "default", ReservedIpRange: "10.123.4.0/26"}}})
	basic := fsInstance(t, fg, zone, "ct-fs-gone3", &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD,
		FileShares: fsShare("share1", 1024), Networks: []*filestorepb.NetworkConfig{{Network: "default", ReservedIpRange: "10.123.5.8/29"}}})
	if got := zonal.GetNetworks()[0].GetIpAddresses(); len(got) != 1 || got[0] != "10.123.4.2" {
		t.Fatalf("Zonal instance's ip_addresses = %v, want [10.123.4.2], in its reserved_ip_range 10.123.4.0/26", got)
	}
	if got := basic.GetNetworks()[0].GetIpAddresses(); len(got) != 1 || got[0] != "10.123.5.10" {
		t.Fatalf("Basic HDD instance's ip_addresses = %v, want [10.123.5.10], in its reserved_ip_range 10.123.5.8/29", got)
	}
	// Another instance's range may not overlap it.
	_, err := fg.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "ct-fs-overlap", Instance: &filestorepb.Instance{
		Tier: filestorepb.Instance_BASIC_HDD, FileShares: fsShare("share1", 1024),
		Networks: []*filestorepb.NetworkConfig{{Network: "default", ReservedIpRange: "10.123.4.8/29"}}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "another Filestore instance") {
		t.Fatalf("CreateInstance with an overlapping reserved_ip_range = %v, want INVALID_ARGUMENT", err)
	}

	cname := zone + "/clusters/ct-gke-fs-gone"
	op, err := cg.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: zone, Cluster: &containerpb.Cluster{Name: "ct-gke-fs-gone", InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	clusterDeleted := false
	t.Cleanup(func() {
		if clusterDeleted {
			return
		}
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

	pods := map[string]*filestorepb.Instance{"ct-fs-gone-v4": zonal, "ct-fs-gone-v3": basic}
	for name, in := range pods {
		if out, err := kubectlRun(kc, sharePod(name, in), time.Minute, "apply", "-f", "-"); err != nil {
			t.Fatalf("kubectl apply: %v\n%s", err, out)
		}
	}
	for name := range pods {
		waitPodRunning(t, kc, name)
		out, err := kubectlRun(kc, "", time.Minute, "exec", name, "--", "sh", "-c", "echo kept > /mnt/share/f.txt && cat /mnt/share/f.txt && grep ' /mnt/share ' /proc/mounts")
		if err != nil || !strings.HasPrefix(out, "kept") {
			t.Fatalf("pod %s write and read: %v: %s", name, err, out)
		}
		if ip := pods[name].GetNetworks()[0].GetIpAddresses()[0]; !strings.Contains(out, ip+":/") {
			t.Fatalf("pod %s does not mount the instance's address %s: %s", name, ip, out)
		}
	}

	// The instances go first, while the pods still mount them.
	for _, in := range []*filestorepb.Instance{zonal, basic} {
		dop, err := fg.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: in.GetName()})
		if err != nil {
			t.Fatalf("DeleteInstance: %v", err)
		}
		if err := dop.Wait(ctx); err != nil {
			t.Fatalf("DeleteInstance: %v", err)
		}
	}
	// A read now fails: the handle is stale.
	for name := range pods {
		out, err := kubectlRun(kc, "", 3*time.Minute, "exec", name, "--", "sh", "-c", "cat /mnt/share/f.txt; ls /mnt/share")
		if err == nil || !strings.Contains(strings.ToLower(out), "stale file handle") {
			t.Errorf("pod %s read a deleted instance's share: %v: %q, want a stale file handle", name, err, out)
		}
	}
	// The pods and the cluster are still deleted.
	for name := range pods {
		if out, err := kubectlRun(kc, "", 3*time.Minute, "delete", "pod", name, "--wait=true", "--timeout=2m"); err != nil {
			t.Fatalf("kubectl delete pod %s: %v\n%s", name, err, out)
		}
	}
	dop, err := cg.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if got := gkeWaitLong(t, cg, project, "us-central1-a", dop, 4*time.Minute); got.GetError() != nil {
		t.Fatalf("DeleteCluster: %v", got.GetError())
	}
	clusterDeleted = true
	if _, err := cg.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cname}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetCluster after delete = %v, want NOT_FOUND", err)
	}
}
