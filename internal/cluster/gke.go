package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GKE makes the local clusters behind the Kubernetes Engine API (#1913):
// each cluster is a kind cluster of its own, made the way the instance's is,
// and each node pool node a kind node of it.
//
// kind makes a cluster's nodes only when it creates the cluster, so the
// control plane is made by `kind create cluster` and every pool node is
// added afterwards the way kind adds its own workers: a container of the
// same node image, on kind's network, with kind's labels (so `kind delete
// cluster` removes it with the rest), which then runs `kubeadm join` with a
// fresh bootstrap token. That one path makes a cluster's first nodes,
// a new node pool's and a resized pool's.
type GKE struct {
	// Dir holds each cluster's kubeconfig and kind configuration, in a
	// directory named for the cluster.
	Dir string
	// Runner executes kind, docker and kubectl; nil uses the real one.
	Runner Runner
}

// poolLabel marks the node containers of a node pool.
const poolLabel = "dev.cloudburrow.gke-nodepool"

func (g *GKE) runner() Runner {
	if g.Runner != nil {
		return g.Runner
	}
	return execRunner{}
}

func (g *GKE) kubeconfig(kind string) string { return filepath.Join(g.Dir, kind, "kubeconfig") }

func (g *GKE) cluster(kind, image string) (*Cluster, error) {
	if image == "" {
		image = "unused:tag"
	}
	return New(Options{Name: kind, NodeImage: image, Kubeconfig: g.kubeconfig(kind), Runner: g.Runner})
}

// GKEAccess is a cluster's API endpoint and certificates, base64 PEM as the
// Kubernetes Engine API returns them.
type GKEAccess struct {
	Endpoint, CACert, ClientCert, ClientKey string
}

// Create makes kind's control plane. A Standard cluster's takes no
// workloads, as a GKE control plane runs none: the taint kind removes from
// a cluster of one node is put back.
func (g *GKE) Create(ctx context.Context, kind, image string, standard bool) (GKEAccess, error) {
	c, err := g.cluster(kind, image)
	if err != nil {
		return GKEAccess{}, err
	}
	// A second API server port, published unused, is the address a
	// rotation moves the endpoint to (gkerotation.go).
	cfg, err := WriteConfig(filepath.Join(g.Dir, kind), []PortMapping{{HostPort: 0, NodePort: spareAPIPort}})
	if err != nil {
		return GKEAccess{}, err
	}
	if _, err := c.Ensure(ctx, cfg); err != nil {
		return GKEAccess{}, err
	}
	if err := c.WaitReady(ctx, 2*time.Minute); err != nil {
		return GKEAccess{}, err
	}
	r := g.runner()
	if standard {
		if _, err := r.Run(ctx, "kubectl", "--kubeconfig", c.KubeconfigPath(), "taint", "node", kind+"-control-plane",
			"node-role.kubernetes.io/control-plane=:NoSchedule", "--overwrite"); err != nil {
			return GKEAccess{}, fmt.Errorf("taint the control plane: %w", err)
		}
	}
	out, err := r.Run(ctx, "kubectl", "--kubeconfig", c.KubeconfigPath(), "config", "view", "--raw", "-o", "json")
	if err != nil {
		return GKEAccess{}, fmt.Errorf("read the kubeconfig: %w", err)
	}
	return parseAccess(out)
}

func parseAccess(kubeconfigJSON string) (GKEAccess, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Cert string `json:"client-certificate-data"`
				Key  string `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(kubeconfigJSON), &kc); err != nil {
		return GKEAccess{}, fmt.Errorf("decode the kubeconfig: %w", err)
	}
	if len(kc.Clusters) != 1 || len(kc.Users) != 1 {
		return GKEAccess{}, errors.New("the kubeconfig does not have one cluster and one user")
	}
	u, err := url.Parse(kc.Clusters[0].Cluster.Server)
	if err != nil || u.Host == "" {
		return GKEAccess{}, fmt.Errorf("the kubeconfig's server %q is not a URL", kc.Clusters[0].Cluster.Server)
	}
	return GKEAccess{Endpoint: u.Host, CACert: kc.Clusters[0].Cluster.CA,
		ClientCert: kc.Users[0].User.Cert, ClientKey: kc.Users[0].User.Key}, nil
}

// poolNodeName is a pool's i-th node (from 1): the cluster's name, a hash
// of the pool's and the index, short enough for a host name.
func poolNodeName(kind, pool string, i int) string {
	sum := sha256.Sum256([]byte(pool))
	return kind + "-" + hex.EncodeToString(sum[:])[:4] + "-" + strconv.Itoa(i)
}

// poolNodes lists a pool's node containers, by index.
func (g *GKE) poolNodes(ctx context.Context, kind, pool string) ([]string, error) {
	out, err := g.runner().Run(ctx, "docker", "ps", "-a", "--filter", "label=io.x-k8s.kind.cluster="+kind,
		"--filter", "label="+poolLabel+"="+pool, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return nodeIndex(names[i]) < nodeIndex(names[j]) })
	return names, nil
}

func nodeIndex(name string) int {
	i := strings.LastIndex(name, "-")
	n, _ := strconv.Atoi(name[i+1:])
	return n
}

// Resize makes pool have n nodes: it adds nodes after the highest index and
// removes the highest first.
func (g *GKE) Resize(ctx context.Context, kind, image, pool string, n int) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	have, err := g.poolNodes(ctx, kind, pool)
	if err != nil {
		return err
	}
	r := g.runner()
	kc := g.kubeconfig(kind)
	for len(have) > n {
		name := have[len(have)-1]
		// Drain is Google's on a resize down; the pods go elsewhere.
		_, _ = r.Run(ctx, "kubectl", "--kubeconfig", kc, "drain", name, "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout=60s")
		_, _ = r.Run(ctx, "kubectl", "--kubeconfig", kc, "delete", "node", name, "--ignore-not-found")
		if _, err := r.Run(ctx, "docker", "rm", "-f", "-v", name); err != nil {
			return fmt.Errorf("remove node %s: %w", name, err)
		}
		have = have[:len(have)-1]
	}
	next := 1
	if len(have) > 0 {
		next = nodeIndex(have[len(have)-1]) + 1
	}
	for i := len(have); i < n; i++ {
		if err := g.join(ctx, kind, image, pool, poolNodeName(kind, pool, next)); err != nil {
			return err
		}
		next++
	}
	return nil
}

// join adds one node to kind's cluster, as kind adds a worker.
func (g *GKE) join(ctx context.Context, kind, image, pool, name string) error {
	r := g.runner()
	if _, err := r.Run(ctx, "docker", "run", "--name", name, "--hostname", name,
		"--label", "io.x-k8s.kind.cluster="+kind, "--label", "io.x-k8s.kind.role=worker", "--label", poolLabel+"="+pool,
		"--label", "cloudburrow.dev/owned=true",
		"--privileged", "--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--security-opt", "label=disable",
		"--tmpfs", "/tmp", "--tmpfs", "/run", "--volume", "/var", "--volume", "/lib/modules:/lib/modules:ro",
		"-e", "KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER", "--cgroupns=private", "--restart=on-failure:1",
		"--net", "kind", "--detach", "--tty", image); err != nil {
		return fmt.Errorf("start node %s: %w", name, err)
	}
	fail := func(err error) error {
		_, _ = r.Run(context.WithoutCancel(ctx), "docker", "rm", "-f", "-v", name)
		return err
	}
	ip, err := r.Run(ctx, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
	if err != nil {
		return fail(fmt.Errorf("read node %s's address: %w", name, err))
	}
	ip = strings.TrimSpace(ip)
	deadline := time.Now().Add(time.Minute)
	for {
		out, _ := r.Run(ctx, "docker", "exec", name, "systemctl", "is-system-running")
		if s := strings.TrimSpace(out); s == "running" || s == "degraded" {
			break
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("node %s's systemd did not start", name))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	token, err := r.Run(ctx, "docker", "exec", kind+"-control-plane", "kubeadm", "token", "create", "--ttl", "15m")
	if err != nil {
		return fail(fmt.Errorf("make a bootstrap token: %w", err))
	}
	conf := joinConfig(kind, name, ip, strings.TrimSpace(token), pool+g.zoneLabels(kind, pool, name))
	if _, err := r.Run(ctx, "docker", "exec", name, "sh", "-c", `printf '%s' "$1" > /kind/kubeadm.conf`, "sh", conf); err != nil {
		return fail(fmt.Errorf("write node %s's kubeadm configuration: %w", name, err))
	}
	if _, err := r.Run(ctx, "docker", "exec", name, "kubeadm", "join", "--config", "/kind/kubeadm.conf", "--skip-phases=preflight"); err != nil {
		// Why the kubelet did not come up is in its journal, which goes
		// with the node container: kept in the error.
		journal, _ := r.Run(context.WithoutCancel(ctx), "docker", "exec", name, "journalctl", "-u", "kubelet", "--no-pager", "-n", "20")
		if j := strings.TrimSpace(journal); j != "" {
			err = fmt.Errorf("%w; the kubelet's last journal lines:\n%s", err, j)
		}
		return fail(fmt.Errorf("join node %s: %w", name, err))
	}
	// The kubelet registers the node a moment after the join returns, and
	// kubectl wait fails at once on a node not there yet.
	for registered := time.Now().Add(time.Minute); time.Now().Before(registered); {
		if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "get", "node", name); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "wait", "--for=condition=Ready", "node/"+name, "--timeout=180s"); err != nil {
		return fmt.Errorf("node %s did not become Ready: %w", name, err)
	}
	return nil
}

// joinConfig is kind's JoinConfiguration and KubeletConfiguration for a
// worker (kind v0.33's), with the pool's GKE node label, and the
// control plane's imagePullCredentialsVerificationPolicy (kindconfig.go).
func joinConfig(kind, name, ip, token, pool string) string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: ` + kind + `-control-plane:6443
    token: ` + token + `
    unsafeSkipCAVerification: true
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
  kubeletExtraArgs:
  - name: node-ip
    value: ` + ip + `
  - name: provider-id
    value: kind://docker/` + kind + `/` + name + `
  - name: node-labels
    value: cloud.google.com/gke-nodepool=` + pool + `
skipPhases:
- preflight
---
apiVersion: kubelet.config.k8s.io/v1beta1
cgroupDriver: systemd
cgroupRoot: /kubelet
evictionHard:
  imagefs.available: 0%
  nodefs.available: 0%
  nodefs.inodesFree: 0%
failSwapOn: false
imageGCHighThresholdPercent: 100
imagePullCredentialsVerificationPolicy: NeverVerify
kind: KubeletConfiguration
`
}

// Delete removes the kind cluster, every node it added with it, and the
// cluster's directory. An absent cluster is already deleted.
func (g *GKE) Delete(ctx context.Context, kind string) error {
	c, err := g.cluster(kind, "")
	if err != nil {
		return err
	}
	if err := c.Delete(ctx); err != nil && !errors.Is(err, ErrAbsent) {
		return err
	}
	return os.RemoveAll(filepath.Join(g.Dir, kind))
}

// FreeBytes is the free space of the file system Dir is on.
func (g *GKE) FreeBytes() (uint64, error) {
	dir := g.Dir
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
	return 0, fmt.Errorf("cannot read the free space of %s", g.Dir)
}
