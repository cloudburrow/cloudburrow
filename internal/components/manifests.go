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
// clientHost is the host:port the storage client will use. Both -public-host
// and -external-url must name it: the download path is matched against
// -public-host, and -external-url is what appears in advertised URLs. Pointing
// either at an address the caller cannot reach makes uploads succeed and every
// download fail.
//
// Host and in-cluster callers cannot both be satisfied by one value. The
// developer's machine is the primary client, so it wins; a workload inside the
// cluster that downloads through the SDK is a known limitation (#19).
func StorageBackend(namespace string, persistent bool, clientHost string) Backend {
	if clientHost == "" {
		clientHost = fmt.Sprintf("storage.%s.svc.cluster.local:%d", namespace, StoragePort)
	}
	hostOnly := strings.TrimPrefix(strings.TrimPrefix(clientHost, "http://"), "https://")
	args := []string{
		"-scheme", "http",
		"-port", fmt.Sprint(StoragePort),
		"-public-host", hostOnly,
		"-external-url", "http://" + hostOnly,
	}
	if persistent {
		args = append(args, "-backend", "filesystem", "-filesystem-root", "/data")
	} else {
		args = append(args, "-backend", "memory")
	}
	return Backend{
		Name:       "storage",
		Image:      StorageImage,
		Port:       StoragePort,
		Args:       args,
		Persistent: persistent,
		MountPath:  "/data",
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

	if b.Persistent {
		fmt.Fprintf(&sb, `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s-data
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
`, b.Name, namespace, instance)
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
            claimName: %s-data
`, b.MountPath, b.Name)
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
