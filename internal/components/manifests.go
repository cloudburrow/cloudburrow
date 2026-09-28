// Package components installs and manages the in-cluster backends CloudBurrow
// runs: the Google Pub/Sub emulator and the Cloud Storage backend, plus the
// Knative Serving installation that executes Cloud Run workloads.
//
// Component versions come from dependencies.json (ADR-0005). Nothing here
// resolves a mutable tag at runtime.
package components

import (
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Pinned component references. These duplicate dependencies.json, which
// remains the source of truth; #31 keeps them in step.
const (
	// PubSubImage is Google's Cloud SDK image carrying the Pub/Sub emulator,
	// pinned by digest. The emulator binary is not redistributable, so the
	// official image is pulled rather than vendored (docs/upstream-evaluation.md §5.1).
	PubSubImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:3294e8a543de846703594a8a89bfe009e0e9ffbdd2e495d00133f3de14cabcc0"

	// KnativeVersion pins Serving and the networking layer together; they must
	// be upgraded as a set.
	KnativeVersion = "knative-v1.23.0"

	// PubSubPort is Pub/Sub's gRPC port on its Service, which the front
	// serves (#873).
	PubSubPort = 8085
	// PubSubEmulatorPort is where the emulator itself listens in its pod,
	// behind the front; the Service does not publish it.
	PubSubEmulatorPort = 8086
	// PubSubPushRelayPort is where the front relays the emulator's pushes,
	// on the pod's loopback, so it sees which succeed (#880).
	PubSubPushRelayPort = 8087
	// StoragePort is the builtin storage server's HTTP port.
	StoragePort = 4443
)

// Manifest is a release YAML applied by URL and pinned by its content
// hash (ADR-0005 decision 6): a tag or release asset can be replaced, so
// the bytes are checked before they reach the cluster with cluster-admin
// rights (#597).
type Manifest struct {
	Name   string
	URL    string
	SHA256 string
}

// KnativeManifests are the release YAMLs applied in order. The hashes are
// dependencies.json's knativeServing and knativeNetKourier manifests, and
// match the checksums.txt each Knative release publishes;
// TestPinsMatchTheInventory keeps the two in step.
func KnativeManifests() []Manifest {
	base := "https://github.com/knative/serving/releases/download/" + KnativeVersion
	net := "https://github.com/knative-extensions/net-kourier/releases/download/" + KnativeVersion
	return []Manifest{
		{"serving-crds.yaml", base + "/serving-crds.yaml", "b172ff4901ed50f8e4e09ff8616e54d22e264df7086ce8cb74f513a04812fe74"},
		{"serving-core.yaml", base + "/serving-core.yaml", "be3f16c9c0ac9276cc173ef04871aaeac78537f9edb116310caa02f016e9cbc2"},
		{"kourier.yaml", net + "/kourier.yaml", "cded0c3c1d7669b1aa9f7484234b454ff3940a2b54a27d5ec4825c2d4003d01d"},
	}
}

// Backend describes one in-cluster service CloudBurrow deploys.
type Backend struct {
	Name  string
	Image string
	// Port is the backend's API port, and the one its readiness is probed on.
	Port int
	// ExtraPorts are further ports the same container serves, published on
	// the Service beside Port. BigQuery is the case: REST and the gRPC
	// Storage Read API are one process on two ports.
	ExtraPorts []NamedPort
	Args       []string
	Command    []string
	// Env is the container environment, rendered in sorted order so the same
	// backend produces the same manifest on every run.
	Env map[string]string
	// Persistent requests a PersistentVolumeClaim. Only meaningful for backends
	// that can actually use one.
	Persistent bool
	MountPath  string
	// ClaimName is the PVC to mount. Empty means "<Name>-data".
	ClaimName string
	// OwnsClaim marks the backend responsible for creating the PVC, so two
	// deployments sharing one claim do not both try to declare it.
	OwnsClaim bool
	// ReadinessPath, when set, makes readiness an HTTP GET of a real
	// request on Port rather than a TCP connect (#514).
	ReadinessPath string
	// PullPolicy is the container's imagePullPolicy; empty leaves the
	// default. A locally built image must be Never.
	PullPolicy string
	// EgressTo, when not nil, adds a NetworkPolicy that denies every egress
	// except DNS and the named in-namespace apps (#514). An empty list
	// denies all but DNS. It is enforced only by a CNI that implements
	// NetworkPolicy, which kind's default does not.
	EgressTo []string
	// Front, when set, is a second container in the pod that serves Port,
	// and the backend's own container listens on Front.UpstreamPort, which
	// the Service does not publish (#873).
	Front *Front
}

// Front is a container that stands in front of a backend in its pod.
type Front struct {
	Name       string
	Image      string
	PullPolicy string
	Args       []string
	// UpstreamPort is the backend container's port, which the front
	// forwards to over the pod's loopback.
	UpstreamPort int
	// StateDir, when set, is where an emptyDir volume is mounted in the
	// front's container: it outlives a restart of that container and goes
	// with the pod, as a non-persistent backend's state does (#898).
	StateDir string
	// BackendLivenessPath, when set, is a path on the front's port that the
	// backend container's liveness probe gets: the front fails it when it
	// has seen the backend fail for good, and Kubernetes then restarts the
	// backend's container alone (#989). The probe allows the front 15
	// seconds of not answering, so that a restart of the front's own
	// container does not restart the backend's.
	BackendLivenessPath string
}

// PubSubFrontStateFile is the file the Pub/Sub front keeps its state in,
// on its emptyDir (#898).
const PubSubFrontStateFile = "/var/lib/pubsub-front/state.json"

// NamedPort is one additional port of a backend. Kubernetes requires every
// port of a multi-port Service to be named.
type NamedPort struct {
	Name string
	Port int
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
//
// frontImage is the locally built cloudburrow-storage image, whose
// `pubsub-front` enforces subscription expiration in front of the emulator
// (internal/pubsubfront, #873). It serves the Service's port, so every
// client, in the cluster or through the host tunnel, goes through it; the
// emulator moves to PubSubEmulatorPort. The emulator pushes through the
// front's relay on PubSubPushRelayPort (#880), so a push subscription's
// successful pushes keep it from expiring. The front keeps what the emulator
// does not store (updated expiration policies, its clock, activity) in a
// file on an emptyDir, so a restart of the front's container alone loses
// none of it, and a restart of the pod loses it with the emulator's state
// (#898).
func PubSubBackend(project, frontImage string) Backend {
	return Backend{
		Name:  "pubsub",
		Image: PubSubImage,
		Port:  PubSubPort,
		Command: []string{"gcloud", "beta", "emulators", "pubsub", "start",
			"--project=" + project,
			fmt.Sprintf("--host-port=0.0.0.0:%d", PubSubEmulatorPort)},
		Persistent: false,
		Front: &Front{
			Name:       "front",
			Image:      frontImage,
			PullPolicy: "Never",
			Args: []string{"pubsub-front", "--listen", fmt.Sprintf("0.0.0.0:%d", PubSubPort),
				"--upstream", fmt.Sprintf("127.0.0.1:%d", PubSubEmulatorPort),
				"--push-relay", fmt.Sprintf("127.0.0.1:%d", PubSubPushRelayPort),
				"--state-file", PubSubFrontStateFile},
			UpstreamPort: PubSubEmulatorPort,
			StateDir:     path.Dir(PubSubFrontStateFile),
		},
	}
}

// BuiltinStorageBackend is CloudBurrow's own Cloud Storage server (#514),
// one Deployment for every client. mediaLink and selfLink come from each
// request's Host, so the address a developer uses through the tunnel and
// the one a workload uses in-cluster are both answered correctly by one
// process: no second Deployment and no Host-binding workaround (#268). image is the locally built, kind-loaded image
// (internal/storageimage); it is never pulled. Readiness is a real request.
// The server may reach only the Pub/Sub emulator, for notifications, and
// DNS. In persistent mode it keeps its store on the storage PVC; in
// ephemeral mode it has no volume and starts empty, whatever claim exists
// (the #481 lesson).
//
// signingKeys are the PEM public keys, by service account email, the server
// verifies RSA signed URLs against (#577); they are passed inline, sorted by
// email so the manifest is stable across runs.
// corsOrigins are the web origins, beyond loopback, whose browser requests
// the server answers (#677).
func BuiltinStorageBackend(namespace, image string, persistent, pubsub bool, signingKeys map[string][]byte, corsOrigins []string) Backend {
	mode := "ephemeral"
	if persistent {
		mode = "persistent"
	}
	args := []string{"--listen", fmt.Sprintf("0.0.0.0:%d", StoragePort), "--allow-remote", "--mode", mode,
		// Virtual-hosted XML requests: in-cluster by Service name, and from the
		// host as <bucket>.localhost or <bucket>.storage.localhost, both of
		// which resolve to loopback (RFC 6761).
		"--host", "storage." + namespace + ".svc.cluster.local,storage.localhost,localhost"}
	if persistent {
		args = append(args, "--data-dir", "/data")
	}
	emails := make([]string, 0, len(signingKeys))
	for e := range signingKeys {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	for _, e := range emails {
		args = append(args, "--signing-key", e+"="+base64.StdEncoding.EncodeToString(signingKeys[e]))
	}
	for _, o := range corsOrigins {
		args = append(args, "--cors-allow-origin", o)
	}
	egress := []string{}
	if pubsub {
		args = append(args, "--pubsub-emulator", InClusterPubSubHost(namespace))
		egress = append(egress, "pubsub")
	}
	return Backend{
		Name:          "storage",
		Image:         image,
		Port:          StoragePort,
		Args:          args,
		Persistent:    persistent,
		MountPath:     "/data",
		ClaimName:     "storage-data",
		OwnsClaim:     true,
		ReadinessPath: "/storage/v1/b?project=_",
		PullPolicy:    "Never",
		EgressTo:      egress,
	}
}

// InClusterBuiltinStorageHost is the builtin server's in-cluster address.
func InClusterBuiltinStorageHost(namespace string) string {
	return fmt.Sprintf("storage.%s.svc.cluster.local:%d", namespace, StoragePort)
}

// InClusterPubSubHost is the Pub/Sub emulator's in-cluster address, where
// the storage server publishes notifications (#506).
func InClusterPubSubHost(namespace string) string {
	return fmt.Sprintf("pubsub.%s.svc.cluster.local:%d", namespace, PubSubPort)
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
`, b.Name, namespace, instance, b.Name, b.Name)
	// The backend's own container, the one kubectl logs and exec pick when
	// none is named, whatever stands in front of it.
	containerPort := b.Port
	if b.Front != nil {
		containerPort = b.Front.UpstreamPort
		fmt.Fprintf(&sb, "      annotations:\n        kubectl.kubernetes.io/default-container: %s\n", b.Name)
	}
	fmt.Fprintf(&sb, `    spec:
      containers:
        - name: %s
          image: %s
`, b.Name, b.Image)

	if b.PullPolicy != "" {
		fmt.Fprintf(&sb, "          imagePullPolicy: %s\n", b.PullPolicy)
	}
	if len(b.Command) > 0 {
		fmt.Fprintf(&sb, "          command: [%s]\n", quoteList(b.Command))
	}
	if len(b.Args) > 0 {
		fmt.Fprintf(&sb, "          args: [%s]\n", quoteList(b.Args))
	}
	if len(b.Env) > 0 {
		sb.WriteString("          env:\n")
		names := make([]string, 0, len(b.Env))
		for k := range b.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&sb, "            - name: %s\n              value: %q\n", k, b.Env[k])
		}
	}

	fmt.Fprintf(&sb, "          ports:\n            - containerPort: %d\n", containerPort)
	for _, p := range b.ExtraPorts {
		fmt.Fprintf(&sb, "            - containerPort: %d\n", p.Port)
	}
	if b.ReadinessPath != "" {
		fmt.Fprintf(&sb, `          readinessProbe:
            httpGet:
              path: %q
              port: %d
            initialDelaySeconds: 1
            periodSeconds: 2
`, b.ReadinessPath, containerPort)
	} else {
		fmt.Fprintf(&sb, `          readinessProbe:
            tcpSocket:
              port: %d
            initialDelaySeconds: 2
            periodSeconds: 2
`, containerPort)
	}
	if f := b.Front; f != nil && f.BackendLivenessPath != "" {
		fmt.Fprintf(&sb, `          livenessProbe:
            httpGet:
              path: %q
              port: %d
            initialDelaySeconds: 15
            periodSeconds: 5
            timeoutSeconds: 2
            failureThreshold: 3
`, f.BackendLivenessPath, b.Port)
	}
	sb.WriteString(`          resources:
            requests:
              cpu: 50m
              memory: 64Mi
`)
	if b.Persistent {
		fmt.Fprintf(&sb, "          volumeMounts:\n            - name: data\n              mountPath: %s\n", b.MountPath)
	}
	if f := b.Front; f != nil {
		fmt.Fprintf(&sb, "        - name: %s\n          image: %s\n", f.Name, f.Image)
		if f.PullPolicy != "" {
			fmt.Fprintf(&sb, "          imagePullPolicy: %s\n", f.PullPolicy)
		}
		if len(f.Args) > 0 {
			fmt.Fprintf(&sb, "          args: [%s]\n", quoteList(f.Args))
		}
		fmt.Fprintf(&sb, `          ports:
            - containerPort: %d
          readinessProbe:
            tcpSocket:
              port: %d
            initialDelaySeconds: 1
            periodSeconds: 2
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
`, b.Port, b.Port)
		if f.StateDir != "" {
			fmt.Fprintf(&sb, "          volumeMounts:\n            - name: front-state\n              mountPath: %s\n", f.StateDir)
		}
	}

	if b.Persistent || (b.Front != nil && b.Front.StateDir != "") {
		sb.WriteString("      volumes:\n")
	}
	if b.Persistent {
		fmt.Fprintf(&sb, "        - name: data\n          persistentVolumeClaim:\n            claimName: %s\n", b.claim())
	}
	if b.Front != nil && b.Front.StateDir != "" {
		sb.WriteString("        - name: front-state\n          emptyDir: {}\n")
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
	for _, p := range b.ExtraPorts {
		fmt.Fprintf(&sb, "    - name: %s\n      port: %d\n      targetPort: %d\n", p.Name, p.Port, p.Port)
	}
	if b.EgressTo != nil {
		fmt.Fprintf(&sb, `---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s-egress
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
spec:
  podSelector:
    matchLabels:
      app: %s
  policyTypes: [Egress]
  egress:
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
`, b.Name, namespace, b.Name)
		for _, app := range b.EgressTo {
			fmt.Fprintf(&sb, "    - to:\n        - podSelector:\n            matchLabels:\n              app: %s\n", app)
		}
	}

	return sb.String()
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
