package modelgarden

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Deploying on the instance's Kubernetes cluster (#2148), as Model Garden's
// Hugging Face deployments run on Google's serving infrastructure and as
// Marketplace apps run on the instance's cluster: a Deployment of the
// engine's container, which pulls the weights from the Hub inside the pod,
// a ClusterIP Service in front of it, and, for a gated model, a Secret
// holding the end user's Hugging Face token, which the pod reads as
// HF_TOKEN. The host reaches the Service through a supervised tunnel
// (internal/netfwd), which prediction calls go through.

// RuntimeCluster and RuntimeDocker are where a deployed model runs.
const (
	RuntimeCluster = "cluster"
	RuntimeDocker  = "docker"
)

// Workload is one deployed model's objects in the cluster.
type Workload struct {
	Name  string
	Image string
	// Command replaces the image's entrypoint when set.
	Command []string
	Args    []string
	// Files are the ConfigMap mounted at /etc/cloudburrow (a Python
	// runtime's start script, requirements and server), none when empty.
	Files  map[string]string
	Env    map[string]string
	Labels map[string]string
	// ShmSize is /dev/shm's size, for vLLM's CPU engine.
	ShmSize string
	// HealthRoute is the readiness probe's path.
	HealthRoute string
}

// Observation is a workload's state, read from its pod.
type Observation struct {
	// Phase is missing (no Deployment), pending, pulling, loading, ready or
	// failed.
	Phase   string
	Message string
	// Pulled and PullTotal are the image's compressed bytes on the node
	// while it is being pulled.
	Pulled, PullTotal int64
}

// Observation phases.
const (
	PhaseMissing = "missing"
	PhasePending = "pending"
	PhasePulling = "pulling"
	PhaseLoading = "loading"
	PhaseReady   = "ready"
	PhaseFailed  = "failed"
)

// Cluster runs deployed models' workloads.
type Cluster interface {
	// Arch is the node's architecture, as Go names it (amd64, arm64).
	Arch(ctx context.Context) (string, error)
	// Apply creates or updates the workload; a non-empty token is put in
	// the workload's Secret, and only there.
	Apply(ctx context.Context, w Workload, token string) error
	Observe(ctx context.Context, name string) (Observation, error)
	// Forward opens (or returns) the host tunnel to the workload's Service
	// and returns its host port.
	Forward(ctx context.Context, name string) (int, error)
	// Unforward closes the tunnel, leaving the workload running.
	Unforward(name string)
	// UnforwardAll closes every tunnel (the instance is stopping).
	UnforwardAll()
	// Remove deletes the workload, its Service and Secret, and its tunnel.
	Remove(ctx context.Context, name string) error
	Logs(ctx context.Context, name string, lines int) string
}

// WorkloadLabel selects one deployed model's objects.
const WorkloadLabel = "cloudburrow.dev/modelgarden"

// workloadName is a DNS-1123 label for an endpoint's workload.
func workloadName(epID string) string {
	n := "mg-" + strings.ToLower(epID)
	if len(n) > 52 {
		n = fmt.Sprintf("%s-%08x", n[:43], crc32.ChecksumIEEE([]byte(epID)))
	}
	return n
}

// secretName is a workload's token Secret.
func secretName(name string) string { return name + "-hf-token" }

// configMapName is a workload's runtime ConfigMap.
func configMapName(name string) string { return name + "-runtime" }

// cacheHostPath is where, on a node, the Python runtimes keep the packages
// they download, so a second deployment does not fetch them again.
const cacheHostPath = "/var/lib/cloudburrow/modelgarden-cache"

// KubeCluster is the instance's kind cluster, through kubectl.
type KubeCluster struct {
	Kubeconfig  string
	Namespace   string
	Instance    string
	ClusterName string // the kind cluster, whose node containers PullMeter reads
	// Kube, when set, replaces kubectl (tests).
	Kube *k8s.Runner
	// Tunnel, when set, replaces netfwd (tests): it returns the host port.
	Tunnel func(ctx context.Context, name string) (int, func(), error)
	// Meter, when set, replaces the image pull meter (tests).
	Meter func(ctx context.Context, node, image string) (images.PullProgress, error)

	mu   sync.Mutex
	fwds map[string]func()
	port map[string]int
	arch string
}

func (c *KubeCluster) kube() *k8s.Runner {
	if c.Kube != nil {
		return c.Kube
	}
	return k8s.New(c.Kubeconfig, "", c.Namespace)
}

func (c *KubeCluster) labels(w Workload) map[string]string {
	l := map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.InstanceLabel: c.Instance,
		k8s.ServiceLabel: "aiplatform", WorkloadLabel: w.Name}
	for k, v := range w.Labels {
		l[k] = v
	}
	return l
}

// Arch reads the first node's architecture once.
func (c *KubeCluster) Arch(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.arch != "" {
		defer c.mu.Unlock()
		return c.arch, nil
	}
	c.mu.Unlock()
	out, err := c.kube().Do(ctx, "", "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	if err != nil {
		return "", fmt.Errorf("read the cluster node's architecture: %w", err)
	}
	arch := strings.TrimSpace(out)
	if arch == "" {
		return "", fmt.Errorf("the cluster has no node")
	}
	c.mu.Lock()
	c.arch = arch
	c.mu.Unlock()
	return arch, nil
}

// Manifests are the workload's Deployment and Service.
func (c *KubeCluster) Manifests(w Workload) (deployment, service []byte) {
	labels := c.labels(w)
	selector := map[string]string{WorkloadLabel: w.Name}
	env := []map[string]any{}
	for _, k := range sortedKeys(w.Env) {
		env = append(env, map[string]any{"name": k, "value": w.Env[k]})
	}
	// The token, when the deploy carried one, is the Secret's: optional, so
	// an ungated model's pod starts without one.
	env = append(env, map[string]any{"name": "HF_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{
		"name": secretName(w.Name), "key": "token", "optional": true}}})
	health := w.HealthRoute
	if health == "" {
		health = "/health"
	}
	mounts := []map[string]any{{"name": "data", "mountPath": "/data"}}
	volumes := []map[string]any{{"name": "data", "emptyDir": map[string]any{}}}
	if w.ShmSize != "" {
		mounts = append(mounts, map[string]any{"name": "shm", "mountPath": "/dev/shm"})
		volumes = append(volumes, map[string]any{"name": "shm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": shmQuantity(w.ShmSize)}})
	}
	if len(w.Files) > 0 {
		mounts = append(mounts, map[string]any{"name": "runtime", "mountPath": "/etc/cloudburrow", "readOnly": true},
			map[string]any{"name": "cache", "mountPath": "/cache"})
		volumes = append(volumes, map[string]any{"name": "runtime", "configMap": map[string]any{"name": configMapName(w.Name)}},
			map[string]any{"name": "cache", "hostPath": map[string]any{"path": cacheHostPath, "type": "DirectoryOrCreate"}})
	}
	container := map[string]any{
		"name": "serving", "image": w.Image, "imagePullPolicy": "IfNotPresent",
		"args": w.Args, "env": env,
		"ports":        []map[string]any{{"name": "http", "containerPort": containerPort}},
		"volumeMounts": mounts,
		"readinessProbe": map[string]any{"httpGet": map[string]any{"path": health, "port": containerPort},
			"periodSeconds": 5, "timeoutSeconds": 3, "failureThreshold": 3},
	}
	if len(w.Command) > 0 {
		container["command"] = w.Command
	}
	if w.Args == nil {
		delete(container, "args")
	}
	d := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": w.Name, "namespace": c.Namespace, "labels": labels},
		"spec": map[string]any{
			"replicas": 1, "strategy": map[string]any{"type": "Recreate"},
			"selector": map[string]any{"matchLabels": selector},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"automountServiceAccountToken": false, "enableServiceLinks": false,
					"containers": []map[string]any{container},
					"volumes":    volumes,
				},
			},
		},
	}
	s := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": w.Name, "namespace": c.Namespace, "labels": labels},
		"spec": map[string]any{"selector": selector, "ports": []map[string]any{{"name": "http", "port": containerPort,
			"targetPort": containerPort}}},
	}
	deployment, _ = json.Marshal(d)
	service, _ = json.Marshal(s)
	return deployment, service
}

// ConfigMapManifest is the workload's runtime ConfigMap, nil when it has no
// files.
func (c *KubeCluster) ConfigMapManifest(w Workload) []byte {
	if len(w.Files) == 0 {
		return nil
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": configMapName(w.Name), "namespace": c.Namespace, "labels": c.labels(w)},
		"data":     w.Files,
	})
	return b
}

// shmQuantity turns Docker's "2g" into Kubernetes' "2Gi".
func shmQuantity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for suf, q := range map[string]string{"g": "Gi", "m": "Mi"} {
		if n, ok := strings.CutSuffix(s, suf); ok {
			return n + q
		}
	}
	return s
}

// Apply writes the Secret (when there is a token; one written earlier is
// kept, so a restart's re-apply keeps a gated model's token), the
// Deployment and the Service, server-side, so kubectl keeps no last-applied copy of the
// Secret's data in an annotation.
func (c *KubeCluster) Apply(ctx context.Context, w Workload, token string) error {
	kube := c.kube()
	opts := k8s.ApplyOptions{ServerSide: true, ForceConflicts: true, FieldManager: "cloudburrow-modelgarden"}
	if token != "" {
		secret, _ := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":   map[string]any{"name": secretName(w.Name), "namespace": c.Namespace, "labels": c.labels(w)},
			"stringData": map[string]string{"token": token},
		})
		if err := kube.Apply(ctx, string(secret), opts); err != nil {
			// kubectl's words, with the token taken out should they quote it.
			return fmt.Errorf("create the Hugging Face token Secret: %s", scrub(err.Error(), token))
		}
	}
	if cm := c.ConfigMapManifest(w); cm != nil {
		if err := kube.Apply(ctx, string(cm), opts); err != nil {
			return fmt.Errorf("create the serving runtime ConfigMap: %w", err)
		}
	}
	d, s := c.Manifests(w)
	if err := kube.Apply(ctx, string(d), opts); err != nil {
		return fmt.Errorf("create the serving Deployment: %w", err)
	}
	if err := kube.Apply(ctx, string(s), opts); err != nil {
		return fmt.Errorf("create the serving Service: %w", err)
	}
	return nil
}

func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name              string `json:"name"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type, Status, Reason, Message string
			} `json:"conditions"`
			ContainerStatuses []struct {
				Ready        bool  `json:"ready"`
				RestartCount int   `json:"restartCount"`
				State        state `json:"state"`
				LastState    state `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type state struct {
	Waiting *struct {
		Reason, Message string
	} `json:"waiting"`
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason   string `json:"reason"`
		Message  string `json:"message"`
		ExitCode int    `json:"exitCode"`
	} `json:"terminated"`
}

// Observe reads the workload's pod.
func (c *KubeCluster) Observe(ctx context.Context, name string) (Observation, error) {
	kube := c.kube()
	if _, err := kube.Get(ctx, "deployment", name, "name"); err != nil {
		if k8s.IsNotFound(err) {
			return Observation{Phase: PhaseMissing, Message: "The serving Deployment does not exist"}, nil
		}
		return Observation{}, err
	}
	out, err := kube.List(ctx, "pods", WorkloadLabel+"="+name, "json")
	if err != nil {
		return Observation{}, err
	}
	var pods podList
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return Observation{}, fmt.Errorf("read the serving pod: %w", err)
	}
	for _, p := range pods.Items {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		return c.observePod(ctx, name, p.Spec.NodeName, p.Status.Phase, p.Status.Conditions, p.Status.ContainerStatuses), nil
	}
	return Observation{Phase: PhasePending, Message: "Waiting for the serving pod to be created"}, nil
}

func (c *KubeCluster) observePod(ctx context.Context, name, node, phase string, conds []struct{ Type, Status, Reason, Message string },
	cs []struct {
		Ready        bool  `json:"ready"`
		RestartCount int   `json:"restartCount"`
		State        state `json:"state"`
		LastState    state `json:"lastState"`
	}) Observation {
	if len(cs) == 0 {
		for _, cd := range conds {
			if cd.Type == "PodScheduled" && cd.Status == "False" {
				return Observation{Phase: PhasePending, Message: "Waiting for the cluster to schedule the serving pod: " + cd.Message}
			}
		}
		return Observation{Phase: PhasePending, Message: "Waiting for the serving pod to start"}
	}
	s := cs[0]
	switch {
	case s.Ready:
		return Observation{Phase: PhaseReady, Message: "Ready"}
	case s.State.Waiting != nil:
		w := s.State.Waiting
		switch w.Reason {
		case "ContainerCreating", "PodInitializing":
			o := Observation{Phase: PhasePulling, Message: "Pulling the serving container image"}
			if img := c.imageOf(ctx, name); img != "" && node != "" {
				if p, err := c.meter(ctx, node, img); err == nil && p.Total > 0 {
					o.Pulled, o.PullTotal = p.Fetched, p.Total
				}
			}
			return o
		case "ErrImagePull", "ImagePullBackOff":
			return Observation{Phase: PhasePulling, Message: "Pulling the serving container image: " + w.Message}
		case "CrashLoopBackOff":
			msg := "The serving container exited"
			if t := s.LastState.Terminated; t != nil {
				msg = fmt.Sprintf("The serving container exited (%s, code %d)", t.Reason, t.ExitCode)
			}
			return Observation{Phase: PhaseFailed, Message: msg + ": " + c.Logs(ctx, name, 12)}
		case "CreateContainerConfigError", "InvalidImageName", "CreateContainerError":
			return Observation{Phase: PhaseFailed, Message: w.Reason + ": " + w.Message}
		}
		return Observation{Phase: PhasePending, Message: w.Reason}
	case s.State.Terminated != nil:
		t := s.State.Terminated
		return Observation{Phase: PhaseFailed, Message: fmt.Sprintf("The serving container exited (%s, code %d): %s",
			t.Reason, t.ExitCode, c.Logs(ctx, name, 12))}
	case s.State.Running != nil:
		msg := "Downloading the weights from Hugging Face and loading the model"
		if last := lastLine(c.Logs(ctx, name, 1)); last != "" {
			msg += ": " + last
		}
		return Observation{Phase: PhaseLoading, Message: msg}
	}
	return Observation{Phase: PhasePending, Message: phase}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func lastLine(s string) string {
	s = strings.TrimSpace(ansi.ReplaceAllString(s, ""))
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

func (c *KubeCluster) imageOf(ctx context.Context, name string) string {
	out, err := c.kube().Get(ctx, "deployment", name, "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (c *KubeCluster) meter(ctx context.Context, node, image string) (images.PullProgress, error) {
	if c.Meter != nil {
		return c.Meter(ctx, node, image)
	}
	// PullMeter reads repository@sha256:<hex>: the tag is dropped.
	ref := image
	if at := strings.Index(ref, "@"); at > 0 {
		repo := ref[:at]
		if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
			repo = repo[:colon]
		}
		ref = repo + ref[at:]
	}
	m := &images.PullMeter{Loader: &images.Loader{ClusterName: c.ClusterName, Runner: images.ExecRunner{}}, Ref: ref}
	return m.Read(ctx, node)
}

// Logs returns the serving container's last lines.
func (c *KubeCluster) Logs(ctx context.Context, name string, lines int) string {
	out, err := c.kube().Do(ctx, "", "logs", "deployment/"+name, "-c", "serving", "--tail="+strconv.Itoa(lines))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(ansi.ReplaceAllString(out, ""))
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// Forward opens the workload's tunnel once.
func (c *KubeCluster) Forward(ctx context.Context, name string) (int, error) {
	c.mu.Lock()
	if p, ok := c.port[name]; ok {
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	port, stop, err := c.tunnel(ctx, name)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fwds == nil {
		c.fwds, c.port = map[string]func(){}, map[string]int{}
	}
	if p, ok := c.port[name]; ok {
		stop()
		return p, nil
	}
	c.fwds[name], c.port[name] = stop, port
	return port, nil
}

func (c *KubeCluster) tunnel(ctx context.Context, name string) (int, func(), error) {
	if c.Tunnel != nil {
		return c.Tunnel(ctx, name)
	}
	f := netfwd.New(netfwd.Target{Name: name, Namespace: c.Namespace, ServicePort: containerPort, Label: "modelgarden-" + name},
		c.Kubeconfig, "127.0.0.1")
	if err := f.Start(ctx); err != nil {
		return 0, nil, fmt.Errorf("open the tunnel to the serving Service: %w", err)
	}
	_, p, _ := net.SplitHostPort(f.HostAddr())
	port, err := strconv.Atoi(p)
	if err != nil {
		_ = f.Stop(context.Background())
		return 0, nil, fmt.Errorf("the tunnel to %s has no port", name)
	}
	return port, func() { _ = f.Stop(context.Background()) }, nil
}

// Unforward closes a tunnel.
func (c *KubeCluster) Unforward(name string) {
	c.mu.Lock()
	stop := c.fwds[name]
	delete(c.fwds, name)
	delete(c.port, name)
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// UnforwardAll closes every tunnel.
func (c *KubeCluster) UnforwardAll() {
	c.mu.Lock()
	names := make([]string, 0, len(c.fwds))
	for n := range c.fwds {
		names = append(names, n)
	}
	c.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		c.Unforward(n)
	}
}

// Remove deletes the workload's objects and its tunnel.
func (c *KubeCluster) Remove(ctx context.Context, name string) error {
	c.Unforward(name)
	kube := c.kube()
	for _, o := range [][2]string{{"deployment", name}, {"service", name}, {"secret", secretName(name)},
		{"configmap", configMapName(name)}} {
		if err := kube.Delete(ctx, o[0], o[1], true); err != nil {
			return fmt.Errorf("delete %s %s: %w", o[0], o[1], err)
		}
	}
	return nil
}
