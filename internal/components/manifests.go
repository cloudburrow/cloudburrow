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

	"github.com/cloudburrow/cloudburrow/internal/frontready"
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
	// EgressPorts, with EgressTo, are TCP ports the pod may also reach at
	// any address: the cloudburrow-host Service's ports, whose endpoint is
	// the developer's machine, which no pod selector names (#1532).
	EgressPorts []int
	// Front, when set, is a second container in the pod that serves Port,
	// and the backend's own container listens on Front.UpstreamPort, which
	// the Service does not publish (#873).
	Front *Front
	// Selects, when set, is the app label the Service selects in place of
	// Name: a Cloud SQL high-availability instance's Service selects its
	// primary's server, which a failover changes (#1408).
	Selects string
	// JVM marks a backend whose process is a Java emulator (Pub/Sub,
	// Firestore, Datastore), whose heap LifecycleComponent.Backends bounds
	// with WithJVMHeap (#1603).
	JVM bool
	// MemoryRequest and MemoryLimit, when set, are the backend container's
	// memory request and limit, Kubernetes quantities such as "512Mi".
	// Empty leaves the request at 64Mi and sets no limit.
	MemoryRequest, MemoryLimit string
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
	// Extra are ports of the backend's ExtraPorts the front serves too,
	// each forwarded to its Upstream port of the backend container, which
	// the Service does not publish (#1032: BigQuery's Storage Read API).
	Extra []FrontPort
	// StateDir, when set, is where an emptyDir volume is mounted in the
	// front's container: it outlives a restart of that container and goes
	// with the pod, as a non-persistent backend's state does (#898).
	StateDir string
	// StateSizeLimit, when set, is the emptyDir's sizeLimit (#1514): the
	// kubelet evicts a pod whose emptyDir grows past it, so a front that
	// keeps data copies there cannot fill the node's disk.
	StateSizeLimit string
	// BackendLivenessPath, when set, is a path on the front's port that the
	// front fails (503) when it has seen the backend fail for good (#989).
	// Without SuperviseBackend it is the backend container's liveness
	// probe, and Kubernetes then restarts the backend's container alone;
	// the probe allows the front 15 seconds of not answering, so that a
	// restart of the front's own container does not restart the backend's.
	// With SuperviseBackend the supervisor gets it instead.
	BackendLivenessPath string
	// SuperviseBackend runs the backend's Command (its image's entrypoint,
	// which must be set) under `cloudburrow-storage supervise`
	// (internal/supervisor, #1091), copied into the backend's container
	// from the front's image by an init container: the supervisor starts
	// the process again at once when it ends or the front fails
	// BackendLivenessPath, so the container never restarts and Kubernetes'
	// growing restart back-off never applies. The backend's container then
	// has no liveness probe.
	SuperviseBackend bool
}

// Where the supervisor is put in a supervised backend's container.
const (
	supervisorDir  = "/cloudburrow-supervisor"
	supervisorPath = supervisorDir + "/cloudburrow-storage"
)

// supervised reports whether b's process runs under the supervisor.
func (b Backend) supervised() bool {
	return b.Front != nil && b.Front.SuperviseBackend && len(b.Command) > 0
}

// FrontPort is a port the front serves, Port, and the backend container's
// port it forwards to, Upstream.
type FrontPort struct {
	Port, Upstream int
}

// upstreamOf is the port the backend container listens on for p, one of
// its ExtraPorts: p, unless the front serves p.
func (b Backend) upstreamOf(p int) int {
	if b.Front != nil {
		for _, e := range b.Front.Extra {
			if e.Port == p {
				return e.Upstream
			}
		}
	}
	return p
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
			// The pod's loopback alone, so every client goes through
			// the front (#1114).
			fmt.Sprintf("--host-port=127.0.0.1:%d", PubSubEmulatorPort)},
		Persistent: false,
		JVM:        true,
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

// JVMOverheadMiB is what a Java emulator's container holds beyond its
// heap: the gcloud wrapper's Python process (about 75 MiB), and the JVM's
// metaspace, code cache, thread stacks, GC structures and Netty's buffers,
// which came to about 170 MiB beside a heap of under 60 MiB committed in the
// soak (#1603). The container's limit is the heap's maximum plus this.
const JVMOverheadMiB = 512

// jvmOptions are the JVM options WithJVMHeap passes, after -Xmx.
//
// ExitOnOutOfMemoryError: Google's Pub/Sub emulator keeps every message a
// subscription was sent until the subscription is deleted (#1603), so a
// bounded heap fills. Measured with a 256 MiB heap: after 175,000
// acknowledged 1 KB messages the emulator spent its time in full
// collections and answered no call, every Publish timing out, its pod not
// ready, for as long as it was left (6 minutes); with this option it logged
// "Terminating due to java.lang.OutOfMemoryError: Java heap space", its
// container restarted, and it served again, empty, within seconds.
const jvmOptions = "-XX:+ExitOnOutOfMemoryError"

// WithJVMHeap bounds a Java emulator's heap at heapMiB, through
// JAVA_TOOL_OPTIONS, which every JVM reads whatever starts it (the gcloud
// wrapper, the emulator's own script, or Datastore's exec of java), and
// sets its container's memory request to half the heap plus 256 MiB and
// its limit to the heap plus JVMOverheadMiB (#1603).
//
// Without a bound the JVM sized its heap at a quarter of what the kind
// node sees (24 GiB: about 6 GiB) and grew it rather than collect it, so
// the containers' memory only rose. Under a 256 MiB bound the 30-minute
// soak of docs/soak.md ran clean: Firestore's live heap held at 20-27 MiB,
// and Pub/Sub's grew by the messages its subscriptions keep (see
// jvmOptions), which a bound turns from unbounded growth into a restart.
// heapMiB 0 leaves the backend as it was, unbounded.
func (b Backend) WithJVMHeap(heapMiB int) Backend {
	if heapMiB <= 0 {
		return b
	}
	env := make(map[string]string, len(b.Env)+1)
	for k, v := range b.Env {
		env[k] = v
	}
	env["JAVA_TOOL_OPTIONS"] = fmt.Sprintf("-Xmx%dm %s", heapMiB, jvmOptions)
	b.Env = env
	b.MemoryRequest = fmt.Sprintf("%dMi", heapMiB/2+256)
	b.MemoryLimit = fmt.Sprintf("%dMi", heapMiB+JVMOverheadMiB)
	return b
}

// BuiltinStorageBackend is CloudBurrow's own Cloud Storage server (#514),
// one Deployment for every client. mediaLink and selfLink come from each
// request's Host, so the address a developer uses through the tunnel and
// the one a workload uses in-cluster are both answered correctly by one
// process: no second Deployment and no Host-binding workaround (#268). image is the locally built, kind-loaded image
// (internal/storageimage); it is never pulled. Readiness is a real request.
// The server may reach only the Pub/Sub emulator, for notifications, and
// DNS; object change notification channels are relayed by the CLI (#1290). In persistent mode it keeps its store on the storage PVC; in
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
		"--host", "storage." + namespace + ".svc.cluster.local,storage.localhost,localhost",
		// Object change notification channels are delivered by the CLI on
		// the developer's machine, which takes them through the tunnel
		// (#1290), so the pod keeps its egress to Pub/Sub and DNS alone.
		"--relay-channels"}
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

// strategy is the Deployment's update strategy. A persistent backend's one
// pod has its volume ReadWriteOnce, and its emulator may save only when it
// exits (Datastore, #1307, whose next emulator waits on a lock the old one
// holds until then): a rolling update would start the new pod while the old
// one still runs, and keep the old one until the new one is ready, which it
// never is. Recreate ends the old pod first. Main run 38783742220: after
// stop/up a changed Datastore spec rolled out and waited 5m on the old pod.
func (b Backend) strategy() string {
	if !b.Persistent {
		return ""
	}
	return "  strategy:\n    type: Recreate\n"
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
%s  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
        cloudburrow.dev/owned: "true"
`, b.Name, namespace, instance, b.strategy(), b.Name, b.Name)
	// The backend's own container, the one kubectl logs and exec pick when
	// none is named, whatever stands in front of it.
	containerPort := b.Port
	if b.Front != nil {
		containerPort = b.Front.UpstreamPort
		fmt.Fprintf(&sb, "      annotations:\n        kubectl.kubernetes.io/default-container: %s\n", b.Name)
	}
	sb.WriteString("    spec:\n")
	if b.supervised() {
		f := b.Front
		fmt.Fprintf(&sb, "      initContainers:\n        - name: supervisor\n          image: %s\n", f.Image)
		if f.PullPolicy != "" {
			fmt.Fprintf(&sb, "          imagePullPolicy: %s\n", f.PullPolicy)
		}
		fmt.Fprintf(&sb, `          args: [%s]
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
          volumeMounts:
            - name: supervisor
              mountPath: %s
`, quoteList([]string{"install-self", supervisorPath}), supervisorDir)
	}
	fmt.Fprintf(&sb, `      containers:
        - name: %s
          image: %s
`, b.Name, b.Image)

	if b.PullPolicy != "" {
		fmt.Fprintf(&sb, "          imagePullPolicy: %s\n", b.PullPolicy)
	}
	if b.supervised() {
		sup := []string{supervisorPath, "supervise"}
		if b.Front.BackendLivenessPath != "" {
			sup = append(sup, "--liveness", fmt.Sprintf("http://127.0.0.1:%d%s", b.Port, b.Front.BackendLivenessPath))
		}
		fmt.Fprintf(&sb, "          command: [%s]\n", quoteList(append(append(sup, "--"), b.Command...)))
	} else if len(b.Command) > 0 {
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
		fmt.Fprintf(&sb, "            - containerPort: %d\n", b.upstreamOf(p.Port))
	}
	if b.Front != nil {
		// The backend listens on the pod's loopback alone (#1114), which
		// a probe, sent to the pod's IP, cannot reach: the front answers
		// for it (internal/frontready).
		fmt.Fprintf(&sb, `          readinessProbe:
            httpGet:
              path: %q
              port: %d
            initialDelaySeconds: 2
            periodSeconds: 2
`, frontready.Path, b.Port)
	} else if b.ReadinessPath != "" {
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
	if f := b.Front; f != nil && f.BackendLivenessPath != "" && !b.supervised() {
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
	request := b.MemoryRequest
	if request == "" {
		request = "64Mi"
	}
	fmt.Fprintf(&sb, "          resources:\n            requests:\n              cpu: 50m\n              memory: %s\n", request)
	if b.MemoryLimit != "" {
		fmt.Fprintf(&sb, "            limits:\n              memory: %s\n", b.MemoryLimit)
	}
	// The data volume is the backend's alone: its mount is written before
	// the front's container starts, so it can never land under the front
	// (#915, TestPersistentBackendWithFrontMountsEachVolumeInItsOwnContainer).
	if b.Persistent || b.supervised() {
		sb.WriteString("          volumeMounts:\n")
	}
	if b.Persistent {
		fmt.Fprintf(&sb, "            - name: data\n              mountPath: %s\n", b.MountPath)
	}
	if b.supervised() {
		fmt.Fprintf(&sb, "            - name: supervisor\n              mountPath: %s\n              readOnly: true\n", supervisorDir)
	}
	if f := b.Front; f != nil {
		fmt.Fprintf(&sb, "        - name: %s\n          image: %s\n", f.Name, f.Image)
		if f.PullPolicy != "" {
			fmt.Fprintf(&sb, "          imagePullPolicy: %s\n", f.PullPolicy)
		}
		if len(f.Args) > 0 {
			fmt.Fprintf(&sb, "          args: [%s]\n", quoteList(f.Args))
		}
		fmt.Fprintf(&sb, "          ports:\n            - containerPort: %d\n", b.Port)
		for _, e := range f.Extra {
			fmt.Fprintf(&sb, "            - containerPort: %d\n", e.Port)
		}
		fmt.Fprintf(&sb, `          readinessProbe:
            tcpSocket:
              port: %d
            initialDelaySeconds: 1
            periodSeconds: 2
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
`, b.Port)
		if f.StateDir != "" {
			fmt.Fprintf(&sb, "          volumeMounts:\n            - name: front-state\n              mountPath: %s\n", f.StateDir)
		}
	}

	if b.Persistent || (b.Front != nil && b.Front.StateDir != "") || b.supervised() {
		sb.WriteString("      volumes:\n")
	}
	if b.Persistent {
		fmt.Fprintf(&sb, "        - name: data\n          persistentVolumeClaim:\n            claimName: %s\n", b.claim())
	}
	if b.Front != nil && b.Front.StateDir != "" {
		if b.Front.StateSizeLimit != "" {
			fmt.Fprintf(&sb, "        - name: front-state\n          emptyDir:\n            sizeLimit: %s\n", b.Front.StateSizeLimit)
		} else {
			sb.WriteString("        - name: front-state\n          emptyDir: {}\n")
		}
	}
	if b.supervised() {
		sb.WriteString("        - name: supervisor\n          emptyDir: {}\n")
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
`, b.Name, namespace, b.selects(), b.Port, b.Port)
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
		for _, p := range b.EgressPorts {
			fmt.Fprintf(&sb, "    - ports:\n        - protocol: TCP\n          port: %d\n", p)
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

func (b Backend) selects() string {
	if b.Selects != "" {
		return b.Selects
	}
	return b.Name
}
