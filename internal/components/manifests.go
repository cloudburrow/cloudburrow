// Package components installs and manages the in-cluster backends CloudBurrow
// runs: the Google Pub/Sub emulator and the Cloud Storage backend, plus the
// Knative Serving installation that executes Cloud Run workloads.
//
// Component versions come from dependencies.json (ADR-0005). Nothing here
// resolves a mutable tag at runtime.
package components

import (
	"fmt"
	"strings"
)

// Pinned component references. These duplicate dependencies.json, which
// remains the source of truth; #31 keeps them in step.
const (
	// PubSubImage is Google's Cloud SDK image carrying the Pub/Sub emulator,
	// pinned by digest. The emulator binary is not redistributable, so the
	// official image is pulled rather than vendored (docs/upstream-evaluation.md §5.1).
	PubSubImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:3294e8a543de846703594a8a89bfe009e0e9ffbdd2e495d00133f3de14cabcc0"
	// StorageImage is fake-gcs-server, BSD-2-Clause.
	StorageImage = "fsouza/fake-gcs-server:1.56.1"

	// KnativeVersion pins Serving and the networking layer together; they must
	// be upgraded as a set.
	KnativeVersion = "knative-v1.23.0"

	// PubSubPort is the emulator's gRPC port.
	PubSubPort = 8085
	// StoragePort is the storage backend's HTTP port.
	StoragePort = 4443
)

// KnativeManifests are the release YAMLs applied in order.
func KnativeManifests() []string {
	base := "https://github.com/knative/serving/releases/download/" + KnativeVersion
	net := "https://github.com/knative-extensions/net-kourier/releases/download/" + KnativeVersion
	return []string{
		base + "/serving-crds.yaml",
		base + "/serving-core.yaml",
		net + "/kourier.yaml",
	}
}

// Backend describes one in-cluster service CloudBurrow deploys.
type Backend struct {
	Name    string
	Image   string
	Port    int
	Args    []string
	Command []string
	// Persistent requests a PersistentVolumeClaim. Only meaningful for backends
	// that can actually use one.
	Persistent bool
	MountPath  string
	// ClaimName is the PVC to mount. Empty means "<Name>-data".
	ClaimName string
	// OwnsClaim marks the backend responsible for creating the PVC, so two
	// deployments sharing one claim do not both try to declare it.
	OwnsClaim bool
}

// claim returns the PVC name this backend mounts.
func (b Backend) claim() string {
	if b.ClaimName != "" {
		return b.ClaimName
	}
	return b.Name + "-data"
}

// PubSubBackend returns the Pub/Sub emulator definition.
//
// It is deliberately not persistent: the upstream audit measured Google's
// emulator losing a topic across a restart even with --data-dir, so allocating
// a volume would imply durability that does not exist.
func PubSubBackend(project string) Backend {
	return Backend{
		Name:  "pubsub",
		Image: PubSubImage,
		Port:  PubSubPort,
		Command: []string{"gcloud", "beta", "emulators", "pubsub", "start",
			"--project=" + project,
			fmt.Sprintf("--host-port=0.0.0.0:%d", PubSubPort)},
		Persistent: false,
	}
}

// StorageBackend returns the Cloud Storage backend definition.
//
// publicHost must be the address the *client* will use. The official storage
// client follows the mediaLink the backend advertises, so a value that does not
// resolve for the caller breaks downloads (docs/local-verification.md §5.2).
// StorageBackend configures the Cloud Storage backend for two different
// audiences at once.
//
// The two flags serve different callers, which is what makes this work:
//
//   - -public-host is matched against the request Host on the virtual-host
//     download path /{bucket}/{object}. The official client uses that path in
//     emulator mode (STORAGE_EMULATOR_HOST), which is how a developer on the
//     host configures it. So it names the *host* address.
//   - -external-url is what appears in the advertised mediaLink, which the
//     client follows when it is *not* in emulator mode — the configuration a
//     workload inside the cluster uses. So it names the *in-cluster* address.
//
// Pointing both at one address makes the other audience fail: with both set to
// the host, an in-cluster read follows a mediaLink of 127.0.0.1 and cannot
// reach it; with both in-cluster, a host read 404s on the vhost path. Splitting
// them is what lets a developer and a deployed workload use the same bucket.
func StorageBackend(namespace string, persistent bool, clientHost string) Backend {
	host := clientHost
	if host == "" {
		host = InClusterStorageHost(namespace)
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "http://"), "https://")
	return storageBackend("storage", namespace, persistent, host, true)
}

// InClusterStorageHost is the address workloads inside the cluster use.
func InClusterStorageHost(namespace string) string {
	return fmt.Sprintf("storage-internal.%s.svc.cluster.local:%d", namespace, StoragePort)
}

// StorageInternalBackend serves the same objects to clients inside the cluster.
//
// A second deployment is needed because -public-host is a single process-wide
// value, and the official client's read path /{bucket}/{object} is matched
// against it. One process can therefore serve reads to exactly one audience:
// point it at the host and an in-cluster read 404s; point it in-cluster and the
// developer's reads 404 instead. Verified both ways.
//
// Both deployments mount the same PersistentVolumeClaim, so they serve the same
// objects. The cluster is single-node, so a ReadWriteOnce claim can be mounted
// by both. They are separate processes over one filesystem, which is acceptable
// for a development emulator but is not a concurrency guarantee.
func StorageInternalBackend(namespace string, persistent bool) Backend {
	return storageBackend("storage-internal", namespace, persistent,
		InClusterStorageHost(namespace), false)
}

func storageBackend(name, namespace string, persistent bool, publicHost string, ownsClaim bool) Backend {
	args := []string{
		"-scheme", "http",
		"-port", fmt.Sprint(StoragePort),
		"-public-host", publicHost,
		"-external-url", "http://" + publicHost,
	}
	if persistent {
		args = append(args, "-backend", "filesystem", "-filesystem-root", "/data")
	} else {
		args = append(args, "-backend", "memory")
	}
	return Backend{
		Name:       name,
		Image:      StorageImage,
		Port:       StoragePort,
		Args:       args,
		Persistent: persistent,
		MountPath:  "/data",
		ClaimName:  "storage-data",
		OwnsClaim:  ownsClaim,
	}
}

// NamespaceManifest returns the managed namespace, labelled so that reset can
// verify ownership before deleting it.
func NamespaceManifest(namespace, instance string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
`, namespace, instance)
}

// Manifest renders the Deployment, Service and optional PVC for a backend.
func (b Backend) Manifest(namespace, instance string) string {
	var sb strings.Builder

	if b.Persistent && (b.OwnsClaim || b.ClaimName == "") {
		fmt.Fprintf(&sb, `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
`, b.claim(), namespace, instance)
	}

	fmt.Fprintf(&sb, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
        cloudburrow.dev/owned: "true"
    spec:
      containers:
        - name: %s
          image: %s
`, b.Name, namespace, instance, b.Name, b.Name, b.Name, b.Image)

	if len(b.Command) > 0 {
		fmt.Fprintf(&sb, "          command: [%s]\n", quoteList(b.Command))
	}
	if len(b.Args) > 0 {
		fmt.Fprintf(&sb, "          args: [%s]\n", quoteList(b.Args))
	}

	fmt.Fprintf(&sb, `          ports:
            - containerPort: %d
          readinessProbe:
            tcpSocket:
              port: %d
            initialDelaySeconds: 2
            periodSeconds: 2
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
`, b.Port, b.Port)

	if b.Persistent {
		fmt.Fprintf(&sb, `          volumeMounts:
            - name: data
              mountPath: %s
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %s
`, b.MountPath, b.claim())
	}

	fmt.Fprintf(&sb, `---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
spec:
  selector:
    app: %s
  ports:
    - name: api
      port: %d
      targetPort: %d
`, b.Name, namespace, b.Name, b.Port, b.Port)

	return sb.String()
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
