package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/dockergc"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// computeVMs is Compute Engine's VM backend (#1963, the user's decision:
// option A): each instance is a privileged, systemd-booting pod in the
// instance's kind cluster, from a container image of its boot image's
// distribution (compute.PublicImages, built here from the official image's
// pinned digest), its root file system's /etc, /usr, /var, /home, /root,
// /opt and /srv on its boot disk's PersistentVolumeClaim (seeded from the
// image at first boot, so what the guest changes survives stop and
// start), each attached disk a PersistentVolumeClaim mounted at
// /mnt/disks/DEVICE_NAME (a directory, not a block device), its port 22
// forwarded to a port on this machine.
type computeVMs struct {
	cfg       config.Config
	installer *components.Installer
	bg        context.Context
	// guestBase is the in-cluster URL of the Compute port, which the guest
	// reads its metadata under; metadataHost the metadata server's
	// in-cluster host:port (GCE_METADATA_HOST).
	guestBase    func() string
	metadataHost func() string
	// opsAgentHosts are the Ops Agent's Logging and Monitoring addresses
	// (#2139, computeopsagent.go); nil or "" when not served.
	opsAgentHosts func() (logging, monitoring string)

	mu     sync.Mutex
	built  map[string]bool
	tunnel map[string]*netfwd.Forwarder
	// lbTunnels are load balancers' tunnels to instances' ports (#2153,
	// computelb.go), by instance key and port.
	lbTunnels map[string]*netfwd.Forwarder
	build     sync.Mutex
}

func newComputeVMs(cfg config.Config, bg context.Context) *computeVMs {
	return &computeVMs{cfg: cfg, bg: bg, built: map[string]bool{}, tunnel: map[string]*netfwd.Forwarder{}, lbTunnels: map[string]*netfwd.Forwarder{},
		installer: &components.Installer{Kubeconfig: cfg.KubeconfigPath(), Namespace: cfg.Cluster.Namespace, Instance: cfg.Name}}
}

func (b *computeVMs) ns() string { return b.cfg.Cluster.Namespace }

func (b *computeVMs) kubectl(ctx context.Context, args ...string) (string, error) {
	return b.installer.Kubectl(ctx, append([]string{"-n", b.ns()}, args...)...)
}

func vmDocker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(vmTail(string(out), 5)))
	}
	return string(out), nil
}

func vmTail(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// ensureImage builds the public image's container image when this machine
// has not got it, and loads it into the cluster.
func (b *computeVMs) ensureImage(ctx context.Context, img compute.PublicImage) (string, error) {
	ref := img.ImageRef()
	b.build.Lock()
	defer b.build.Unlock()
	if b.built[ref] {
		return ref, nil
	}
	if _, err := vmDocker(ctx, "image", "inspect", "--format", "{{.Id}}", ref); err != nil {
		dir, err := os.MkdirTemp("", "cloudburrow-vm-image-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		for p, c := range img.BuildContext() {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if strings.HasPrefix(p, "root/usr/lib/cloudburrow/") {
				mode = 0o755
			}
			if err := os.WriteFile(full, c, mode); err != nil {
				return "", err
			}
		}
		arch := runtime.GOARCH
		if _, err := vmDocker(ctx, "build", "--platform", "linux/"+arch, "--build-arg", "BASE="+img.Base,
			"--label", "cloudburrow.dev/owned=true", "--label", "cloudburrow.dev/vm-image="+img.Family,
			"--label", dockergc.Label(b.cfg.Name), "-t", ref, dir); err != nil {
			return "", fmt.Errorf("build the %s image: %w", img.Family, err)
		}
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", ref, "--name", b.cfg.ClusterName()).CombinedOutput(); err != nil {
		return "", fmt.Errorf("load the %s image into the cluster: %w: %s", img.Family, err, vmTail(string(out), 3))
	}
	b.built[ref] = true
	return ref, nil
}

func pvcManifest(ns, name string, sizeGb int64, kind string) string {
	if sizeGb <= 0 {
		sizeGb = 10
	}
	m := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{
			k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute", "cloudburrow.dev/compute-" + kind: name}},
		"spec": map[string]any{"accessModes": []string{"ReadWriteOnce"},
			"resources": map[string]any{"requests": map[string]string{"storage": strconv.FormatInt(sizeGb, 10) + "Gi"}}}}
	out, _ := json.Marshal(m)
	return string(out)
}

// copyPVC copies one claim's files into another with a pod of the Debian
// image (cp -a), and waits for it.
func (b *computeVMs) copyPVC(ctx context.Context, src, dst string) error {
	ref, err := b.ensureImage(ctx, compute.PublicImages[0])
	if err != nil {
		return err
	}
	name := "copy-" + dst
	if len(name) > 63 {
		name = name[:63]
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(), "labels": map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute"}},
		"spec": map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false,
			"containers": []any{map[string]any{"name": "copy", "image": ref, "imagePullPolicy": "Never",
				"command":      []string{"/bin/sh", "-c", "cp -a /src/. /dst/ && sync"},
				"volumeMounts": []any{map[string]any{"name": "src", "mountPath": "/src", "readOnly": true}, map[string]any{"name": "dst", "mountPath": "/dst"}}}},
			"volumes": []any{map[string]any{"name": "src", "persistentVolumeClaim": map[string]any{"claimName": src}},
				map[string]any{"name": "dst", "persistentVolumeClaim": map[string]any{"claimName": dst}}}}}
	m, _ := json.Marshal(pod)
	_, _ = b.kubectl(ctx, "delete", "pod", name, "--ignore-not-found", "--wait=true")
	if err := b.installer.Apply(ctx, string(m)); err != nil {
		return err
	}
	defer func() {
		_, _ = b.kubectl(context.Background(), "delete", "pod", name, "--ignore-not-found", "--wait=false")
	}()
	if _, err := b.kubectl(ctx, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+name, "--timeout=10m"); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}

func (b *computeVMs) CreateDisk(ctx context.Context, d compute.DiskSpec) error {
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), d.Key, d.SizeGb, "disk")); err != nil {
		return err
	}
	if d.SourceSnapshot != "" {
		return b.copyPVC(ctx, d.SourceSnapshot, d.Key)
	}
	return nil
}

func (b *computeVMs) DeleteDisk(ctx context.Context, d compute.DiskSpec) error {
	_, err := b.kubectl(ctx, "delete", "pvc", d.Key, "--ignore-not-found", "--wait=true", "--timeout=2m")
	return err
}

// ResizeDisk records the new size on the claim's request where the storage
// class allows expansion; kind's local-path class neither enforces sizes nor
// expands, so the disk's directory already has the machine's free space.
func (b *computeVMs) ResizeDisk(ctx context.Context, d compute.DiskSpec) error {
	return nil
}

func (b *computeVMs) Snapshot(ctx context.Context, d compute.DiskSpec, key string) error {
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), key, d.SizeGb, "snapshot")); err != nil {
		return err
	}
	return b.copyPVC(ctx, d.Key, key)
}

func (b *computeVMs) DeleteSnapshot(ctx context.Context, key string) error {
	_, err := b.kubectl(ctx, "delete", "pvc", key, "--ignore-not-found", "--wait=true", "--timeout=2m")
	return err
}

// rootDirs are the guest directories kept on the boot disk.
var rootDirs = []string{"etc", "home", "opt", "root", "srv", "usr", "var"}

func (b *computeVMs) podManifest(vm compute.VMSpec, ref string) string {
	seed := "if [ ! -e /disk/.cloudburrow-seeded ]; then for d in " + strings.Join(rootDirs, " ") +
		"; do mkdir -p /disk/$d && cp -a /$d/. /disk/$d/; done; touch /disk/.cloudburrow-seeded; fi"
	var mounts []any
	for _, d := range rootDirs {
		mounts = append(mounts, map[string]any{"name": "boot", "mountPath": "/" + d, "subPath": d})
	}
	mounts = append(mounts, map[string]any{"name": "run", "mountPath": "/run"}, map[string]any{"name": "tmp", "mountPath": "/tmp"})
	volumes := []any{
		map[string]any{"name": "boot", "persistentVolumeClaim": map[string]any{"claimName": vm.BootDisk.Key}},
		map[string]any{"name": "run", "emptyDir": map[string]any{"medium": "Memory"}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
	}
	for i, d := range vm.Disks {
		vn := "disk" + strconv.Itoa(i)
		mounts = append(mounts, map[string]any{"name": vn, "mountPath": "/mnt/disks/" + d.DeviceName, "readOnly": d.ReadOnly})
		volumes = append(volumes, map[string]any{"name": vn, "persistentVolumeClaim": map[string]any{"claimName": d.Disk.Key, "readOnly": d.ReadOnly}})
	}
	guest := ""
	if b.guestBase != nil {
		if base := b.guestBase(); base != "" {
			guest = base + compute.GuestPrefix + vm.GuestToken
		}
	}
	meta := ""
	if b.metadataHost != nil {
		meta = b.metadataHost()
	}
	env := []any{
		map[string]string{"name": "container", "value": "docker"},
		map[string]string{"name": "CLOUDBURROW_GUEST_METADATA", "value": guest},
		map[string]string{"name": "CLOUDBURROW_INTERNAL_IP", "value": vm.InternalIP},
		map[string]string{"name": "GCE_METADATA_HOST", "value": meta},
		map[string]string{"name": "GOOGLE_CLOUD_PROJECT", "value": vm.Project},
	}
	env = append(env, b.opsAgentEnv()...)
	host := vm.Hostname
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	if len(host) > 63 {
		host = host[:63]
	}
	cpu := max(vm.CPUs, 1)
	mem := max(vm.MemoryMb, 512)
	labels := map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute", "cloudburrow.dev/vm": vm.Key}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": vm.Key, "namespace": b.ns(), "labels": labels,
			"annotations": map[string]string{"cloudburrow.dev/compute-instance": "projects/" + vm.Project + "/zones/" + vm.Zone + "/instances/" + vm.Name}},
		"spec": map[string]any{"hostname": host, "restartPolicy": "Always", "terminationGracePeriodSeconds": 30,
			"automountServiceAccountToken": false, "enableServiceLinks": false,
			"initContainers": []any{map[string]any{"name": "seed", "image": ref, "imagePullPolicy": "Never",
				"command":      []string{"/bin/sh", "-c", seed},
				"volumeMounts": []any{map[string]any{"name": "boot", "mountPath": "/disk"}}}},
			"containers": []any{map[string]any{"name": "vm", "image": ref, "imagePullPolicy": "Never", "tty": true, "stdin": true,
				"securityContext": map[string]any{"privileged": true}, "env": env, "volumeMounts": mounts,
				"resources": map[string]any{"limits": map[string]string{"cpu": strconv.Itoa(cpu), "memory": strconv.Itoa(mem) + "Mi"},
					"requests": map[string]string{"cpu": "50m", "memory": "64Mi"}}}},
			"volumes": volumes}}
	svc := map[string]any{"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": vm.Key, "namespace": b.ns(), "labels": labels},
		"spec": map[string]any{"selector": map[string]string{"cloudburrow.dev/vm": vm.Key},
			"ports": []any{map[string]any{"name": "ssh", "port": 22, "targetPort": 22}}}}
	p, _ := json.Marshal(pod)
	s, _ := json.Marshal(svc)
	return string(p) + "\n---\n" + string(s)
}

func (b *computeVMs) Start(ctx context.Context, vm compute.VMSpec) error {
	if vm.Image.Name == "" {
		return fmt.Errorf("the boot disk of %s has no image to boot", vm.Name)
	}
	ref, err := b.ensureImage(ctx, vm.Image)
	if err != nil {
		return err
	}
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), vm.BootDisk.Key, vm.BootDisk.SizeGb, "disk")); err != nil {
		return err
	}
	if err := b.installer.Apply(ctx, b.podManifest(vm, ref)); err != nil {
		return err
	}
	if _, err := b.kubectl(ctx, "wait", "--for=condition=Ready", "pod/"+vm.Key, "--timeout=10m"); err != nil {
		return fmt.Errorf("boot %s: %w", vm.Name, err)
	}
	f := netfwd.New(netfwd.Target{Name: vm.Key, Namespace: b.ns(), ServicePort: 22, Label: "ssh-" + vm.Key},
		b.cfg.KubeconfigPath(), b.cfg.BindAddress)
	if err := f.Start(b.bg); err != nil {
		return fmt.Errorf("forward %s's SSH port: %w", vm.Name, err)
	}
	b.mu.Lock()
	old := b.tunnel[vm.Key]
	b.tunnel[vm.Key] = f
	b.mu.Unlock()
	if old != nil {
		_ = old.Stop(context.Background())
	}
	return nil
}

func (b *computeVMs) Stop(ctx context.Context, vm compute.VMSpec) error {
	b.mu.Lock()
	f := b.tunnel[vm.Key]
	delete(b.tunnel, vm.Key)
	b.mu.Unlock()
	if f != nil {
		_ = f.Stop(context.Background())
	}
	b.stopLBTunnels(ctx, vm)
	// A suspended guest is thawed first, so it can shut down.
	_ = b.freeze(ctx, vm, false)
	if _, err := b.kubectl(ctx, "delete", "pod,service", vm.Key, "--ignore-not-found", "--wait=true", "--timeout=2m"); err != nil {
		return err
	}
	return nil
}

// freeze freezes or thaws the guest's cgroup on its kind node: a suspended
// instance keeps its memory and processes, as on Google.
func (b *computeVMs) freeze(ctx context.Context, vm compute.VMSpec, on bool) error {
	out, err := b.kubectl(ctx, "get", "pod", vm.Key, "-o", "jsonpath={.spec.nodeName} {.status.containerStatuses[0].containerID}")
	if err != nil {
		return err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return fmt.Errorf("%s is not running", vm.Name)
	}
	id := strings.TrimPrefix(f[1], "containerd://")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		return fmt.Errorf("unexpected container ID %q", f[1])
	}
	v := "0"
	if on {
		v = "1"
	}
	script := "set -e; n=0; for d in $(find /sys/fs/cgroup -type d -name '*" + id + "*'); do echo " + v + " > $d/cgroup.freeze; n=$((n+1)); done; [ $n -gt 0 ]"
	_, err = vmDocker(ctx, "exec", f[0], "sh", "-c", script)
	return err
}

func (b *computeVMs) Suspend(ctx context.Context, vm compute.VMSpec) error {
	return b.freeze(ctx, vm, true)
}
func (b *computeVMs) Resume(ctx context.Context, vm compute.VMSpec) error {
	return b.freeze(ctx, vm, false)
}

// CPUUsage implements compute.VMCPU: the CPU seconds the VM's container
// has used, its cgroup's cpu.stat usage_usec (the container has its own
// cgroup namespace, so /sys/fs/cgroup is the VM's whole cgroup).
func (b *computeVMs) CPUUsage(ctx context.Context, vm compute.VMSpec) (float64, error) {
	out, err := b.kubectl(ctx, "exec", vm.Key, "-c", "vm", "--", "cat", "/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0, err
	}
	return cpuStatSeconds(out)
}

// cpuStatSeconds is cpu.stat's usage_usec in seconds.
func cpuStatSeconds(stat string) (float64, error) {
	for _, line := range strings.Split(stat, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "usage_usec" {
			us, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return float64(us) / 1e6, nil
		}
	}
	return 0, fmt.Errorf("no usage_usec in cpu.stat")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[MD78]|\r`)

func (b *computeVMs) SerialOutput(ctx context.Context, vm compute.VMSpec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := b.kubectl(ctx, "logs", vm.Key, "-c", "vm")
	if err != nil {
		return "", err
	}
	return ansiRE.ReplaceAllString(out, ""), nil
}

func (b *computeVMs) SSHAddress(vm compute.VMSpec) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f := b.tunnel[vm.Key]; f != nil {
		return f.HostAddr()
	}
	return ""
}

func (b *computeVMs) FreeBytes() (uint64, error) {
	dir := b.cfg.InstanceDir()
	for dir != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err == nil {
			return uint64(st.Bavail) * uint64(st.Bsize), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return 0, fmt.Errorf("cannot read the free space of %s", b.cfg.InstanceDir())
}

// stopAll stops every SSH tunnel.
func (b *computeVMs) stopAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, f := range b.tunnel {
		_ = f.Stop(context.Background())
		delete(b.tunnel, k)
	}
	for k, f := range b.lbTunnels {
		_ = f.Stop(context.Background())
		delete(b.lbTunnels, k)
	}
}
