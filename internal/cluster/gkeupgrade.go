package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Upgrades of the Kubernetes Engine API's clusters (#2069).
//
// kind makes a cluster from one node image and cannot move it to another,
// so an upgrade is done the way kubeadm upgrades a cluster in place, which
// is how a GKE control plane keeps its etcd state across an upgrade:
//
//  1. The control plane: the new version's kubeadm, kubelet and kubectl
//     binaries and its control-plane images (kube-apiserver,
//     kube-controller-manager, kube-scheduler, kube-proxy, etcd, CoreDNS,
//     pause) are copied out of the newer kind node image, by a helper
//     container of that image which runs only containerd, and into the
//     control plane's node. `kubeadm upgrade apply` then rewrites the
//     static pods to the new version, with etcd's data kept, and the
//     control plane's kubelet is replaced and restarted. Nothing is
//     downloaded: every image is the pinned node image's own.
//  2. The nodes: a node pool's nodes are replaced, never changed in place,
//     as GKE replaces its VMs. A new node of the newer node image joins
//     the cluster (AddNode), an old one is cordoned and drained (the
//     eviction API, so PodDisruptionBudgets hold) and then removed. The
//     order (surge or blue-green) is the API's (internal/service/gke).

// PoolNode is one node of a node pool, with the node image it runs.
type PoolNode struct {
	Name  string
	Image string
}

// PoolNodes lists a pool's nodes, by index, with their node images.
func (g *GKE) PoolNodes(ctx context.Context, kind, pool string) ([]PoolNode, error) {
	names, err := g.poolNodes(ctx, kind, pool)
	if err != nil {
		return nil, err
	}
	out := make([]PoolNode, 0, len(names))
	for _, n := range names {
		img, err := g.runner().Run(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", n)
		if err != nil {
			return nil, fmt.Errorf("read node %s's image: %w", n, err)
		}
		out = append(out, PoolNode{Name: n, Image: strings.TrimSpace(img)})
	}
	return out, nil
}

// AddNode joins one more node of image to pool, after its highest index,
// and returns its name once it is Ready.
func (g *GKE) AddNode(ctx context.Context, kind, image, pool string) (string, error) {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return "", fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	have, err := g.poolNodes(ctx, kind, pool)
	if err != nil {
		return "", err
	}
	next := 1
	if len(have) > 0 {
		next = nodeIndex(have[len(have)-1]) + 1
	}
	name := poolNodeName(kind, pool, next)
	if err := g.join(ctx, kind, image, pool, name); err != nil {
		return "", err
	}
	if len(have) > 0 {
		if err := g.carryImages(ctx, kind, have[0], name); err != nil {
			return name, err
		}
	}
	return name, nil
}

// carryImages copies to node to the images loaded onto node from (`kind
// load`, as CloudBurrow's own fixtures and users load local images) that
// to's node image does not have. A GKE node pulls its pods' images from a
// registry; a kind node has only what was loaded onto it, so a replacement
// node gets its predecessor's, or the pods moved to it could not start.
func (g *GKE) carryImages(ctx context.Context, kind, from, to string) error {
	r := g.runner()
	list := func(node string) (map[string]bool, error) {
		out, err := r.Run(ctx, "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
		set := map[string]bool{}
		for _, ref := range strings.Fields(out) {
			set[ref] = true
		}
		return set, err
	}
	src, err := list(from)
	if err != nil {
		return fmt.Errorf("list node %s's images: %w", from, err)
	}
	dst, err := list(to)
	if err != nil {
		return fmt.Errorf("list node %s's images: %w", to, err)
	}
	var refs []string
	for ref := range src {
		if dst[ref] || strings.HasPrefix(ref, "sha256:") || strings.HasPrefix(ref, "import-") ||
			strings.HasPrefix(ref, "registry.k8s.io/") || strings.HasPrefix(ref, "docker.io/kindest/") {
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil
	}
	sort.Strings(refs)
	stage := filepath.Join(g.Dir, kind, "carry-"+to)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	tar := filepath.Join(stage, "images.tar")
	export := `set -e
case "$(uname -m)" in aarch64|arm64) a=arm64;; x86_64|amd64) a=amd64;; *) a=$(uname -m);; esac
ctr -n k8s.io images export --platform linux/$a /kind/carry.tar "$@"
echo $a`
	arch, err := r.Run(ctx, "docker", append([]string{"exec", from, "sh", "-c", export, "sh"}, refs...)...)
	defer func() {
		_, _ = r.Run(context.WithoutCancel(ctx), "docker", "exec", from, "rm", "-f", "/kind/carry.tar")
	}()
	if err != nil {
		return fmt.Errorf("export node %s's loaded images: %w", from, err)
	}
	if _, err := r.Run(ctx, "docker", "cp", from+":/kind/carry.tar", tar); err != nil {
		return err
	}
	if _, err := r.Run(ctx, "docker", "cp", tar, to+":/kind/carry.tar"); err != nil {
		return err
	}
	_, err = r.Run(ctx, "docker", "exec", to, "ctr", "-n", "k8s.io", "images", "import", "--platform", "linux/"+strings.TrimSpace(arch), "/kind/carry.tar")
	_, _ = r.Run(context.WithoutCancel(ctx), "docker", "exec", to, "rm", "-f", "/kind/carry.tar")
	if err != nil {
		return fmt.Errorf("import the loaded images into node %s: %w", to, err)
	}
	return nil
}

// CordonNode marks a node unschedulable.
func (g *GKE) CordonNode(ctx context.Context, kind, name string) error {
	_, err := g.runner().Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "cordon", name)
	return err
}

// UncordonNode marks a node schedulable again.
func (g *GKE) UncordonNode(ctx context.Context, kind, name string) error {
	_, err := g.runner().Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "uncordon", name)
	return err
}

// DrainTimeout is how long a drain respects PodDisruptionBudgets before
// the node's remaining pods are deleted: one hour, as GKE's surge upgrade
// (re-read 2026-11-20:
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pool-upgrade-strategies).
var DrainTimeout = time.Hour

// DrainNode cordons and drains a node through the eviction API, which
// respects PodDisruptionBudgets, for up to DrainTimeout; the pods still
// there are then deleted, as GKE then evicts them forcibly.
func (g *GKE) DrainNode(ctx context.Context, kind, name string) error {
	r := g.runner()
	kc := g.kubeconfig(kind)
	args := []string{"--kubeconfig", kc, "drain", name, "--ignore-daemonsets", "--delete-emptydir-data", "--force"}
	if _, err := r.Run(ctx, "kubectl", append(args, "--timeout="+DrainTimeout.String())...); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return err
	}
	_, err := r.Run(ctx, "kubectl", append(args, "--disable-eviction", "--timeout=5m")...)
	return err
}

// ForceDrainNode cordons and drains a node without the eviction API:
// PodDisruptionBudgets are not respected.
func (g *GKE) ForceDrainNode(ctx context.Context, kind, name string) error {
	_, err := g.runner().Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "drain", name, "--ignore-daemonsets",
		"--delete-emptydir-data", "--force", "--disable-eviction", "--timeout=5m")
	return err
}

// RemoveNode deletes a node from the cluster and removes its container.
func (g *GKE) RemoveNode(ctx context.Context, kind, name string) error {
	if !strings.HasPrefix(name, kind+"-") {
		return fmt.Errorf("%w: node %q is not cluster %q's", ErrNotOwned, name, kind)
	}
	r := g.runner()
	_, _ = r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "delete", "node", name, "--ignore-not-found")
	if _, err := r.Run(ctx, "docker", "rm", "-f", "-v", name); err != nil {
		return fmt.Errorf("remove node %s: %w", name, err)
	}
	return nil
}

// kubeVersion is a kind node image's Kubernetes version:
// kindest/node:v1.36.4@sha256:... is v1.36.4.
func kubeVersion(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	return ref[strings.LastIndex(ref, ":")+1:]
}

// controlPlaneImages are the images kubeadm runs a control plane and its
// add-ons from, as registry.k8s.io names them in a kind node image.
const controlPlaneImages = `^registry.k8s.io/(kube-(apiserver|controller-manager|scheduler|proxy)|etcd|coredns/coredns|pause):`

// UpgradeControlPlane upgrades kind's control plane to image's Kubernetes
// version with `kubeadm upgrade apply`, keeping etcd's data, then its
// kubelet. image must be one minor newer at most, as kubeadm requires.
func (g *GKE) UpgradeControlPlane(ctx context.Context, kind, image string) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	version := kubeVersion(image)
	if !strings.HasPrefix(version, "v") {
		return fmt.Errorf("node image %q has no Kubernetes version tag", image)
	}
	r := g.runner()
	cp := kind + "-control-plane"
	stage := filepath.Join(g.Dir, kind, "upgrade")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	// The helper runs containerd only, to export the image's own
	// control-plane images; it is removed whatever happens.
	helper := kind + "-upgrade"
	_, _ = r.Run(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", helper)
	if _, err := r.Run(ctx, "docker", "run", "--detach", "--name", helper, "--label", "cloudburrow.dev/owned=true",
		"--privileged", "--network", "none", "--entrypoint", "containerd", image); err != nil {
		return fmt.Errorf("start the upgrade helper: %w", err)
	}
	defer func() { _, _ = r.Run(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", helper) }()
	export := `set -e
case "$(uname -m)" in aarch64|arm64) a=arm64;; x86_64|amd64) a=amd64;; *) a=$(uname -m);; esac
for i in $(seq 1 60); do ctr version >/dev/null 2>&1 && break; sleep 0.5; done
ctr -n k8s.io images export --platform linux/$a /upgrade-images.tar $(ctr -n k8s.io images ls -q | grep -E '` + controlPlaneImages + `')
echo $a`
	arch, err := r.Run(ctx, "docker", "exec", helper, "sh", "-c", export)
	if err != nil {
		return fmt.Errorf("export %s's control-plane images: %w", version, err)
	}
	arch = strings.TrimSpace(arch)
	files := map[string]string{"/upgrade-images.tar": "upgrade-images.tar", "/usr/bin/kubeadm": "kubeadm", "/usr/bin/kubelet": "kubelet", "/usr/bin/kubectl": "kubectl"}
	for src, dst := range files {
		if _, err := r.Run(ctx, "docker", "cp", helper+":"+src, filepath.Join(stage, dst)); err != nil {
			return fmt.Errorf("copy %s out of %s: %w", src, image, err)
		}
	}
	if _, err := r.Run(ctx, "docker", "cp", filepath.Join(stage, "upgrade-images.tar"), cp+":/kind/upgrade-images.tar"); err != nil {
		return fmt.Errorf("copy the control-plane images in: %w", err)
	}
	_, err = r.Run(ctx, "docker", "exec", cp, "ctr", "-n", "k8s.io", "images", "import", "--platform", "linux/"+arch, "/kind/upgrade-images.tar")
	_, _ = r.Run(context.WithoutCancel(ctx), "docker", "exec", cp, "rm", "-f", "/kind/upgrade-images.tar")
	if err != nil {
		return fmt.Errorf("import the control-plane images: %w", err)
	}
	if _, err := r.Run(ctx, "docker", "cp", filepath.Join(stage, "kubeadm"), cp+":/usr/bin/kubeadm"); err != nil {
		return fmt.Errorf("install kubeadm %s: %w", version, err)
	}
	// The version is given, so kubeadm looks nothing up online, and the
	// images are present, so it pulls none.
	if _, err := r.Run(ctx, "docker", "exec", cp, "kubeadm", "upgrade", "apply", version, "--yes"); err != nil {
		return fmt.Errorf("kubeadm upgrade apply %s: %w", version, err)
	}
	for _, b := range []string{"kubelet", "kubectl"} {
		if _, err := r.Run(ctx, "docker", "cp", filepath.Join(stage, b), cp+":/usr/bin/"+b); err != nil {
			return fmt.Errorf("install %s %s: %w", b, version, err)
		}
	}
	if _, err := r.Run(ctx, "docker", "exec", cp, "sh", "-c", "systemctl daemon-reload && systemctl restart kubelet"); err != nil {
		return fmt.Errorf("restart the control plane's kubelet: %w", err)
	}
	// The API server is back, and the control plane's node reports the
	// new version.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "get", "node", cp, "-o", "jsonpath={.status.nodeInfo.kubeletVersion}")
		if err == nil && strings.TrimSpace(out) == version {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the control plane did not come back on %s (kubelet %q, %v)", version, strings.TrimSpace(out), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	_, err = r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "wait", "--for=condition=Ready", "node/"+cp, "--timeout=180s")
	return err
}
