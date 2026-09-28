package components

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
)

// Every backend with a front listens on its pod's loopback alone, so that
// no other pod reaches it past the front's checks (#1114): measured, a pod
// that dialled the BigQuery emulator's gRPC port at the pod's IP crashed
// it with a request the front refuses, and every dataset was lost.
func TestEveryBackendWithAFrontListensOnLoopbackOnly(t *testing.T) {
	var cfg config.Config
	cfg.Services = config.AllServices()
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	c.SetBigQueryImage("dev.local/cloudburrow-bigquery:def")
	fronted := 0
	// The core Pub/Sub backend, and every optional one.
	for _, b := range append(c.Backends(), PubSubBackend("p", "dev.local/cloudburrow-storage:abc")) {
		if b.Front == nil {
			continue
		}
		fronted++
		all := strings.Join(append(append([]string{}, b.Command...), b.Args...), " ")
		if strings.Contains(all, "0.0.0.0") || !strings.Contains(all, "127.0.0.1") {
			t.Errorf("%s: the backend's command line %q does not bind it to 127.0.0.1 alone", b.Name, all)
		}
	}
	if fronted != 2 {
		t.Errorf("%d backends have a front, want 2 (pubsub, bigquery): check the new one binds to loopback", fronted)
	}
}

// The builtin server is one Deployment with a real HTTP readiness request,
// a locally loaded image, and egress only to Pub/Sub and DNS (#514).
func TestStorageManifestSingleDeploymentHTTPReadiness(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub}
	cfg.Mode = config.ModePersistent
	cfg.Cluster.Namespace = "cloudburrow"
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	var storage []Backend
	for _, b := range c.Backends() {
		if strings.HasPrefix(b.Name, "storage") {
			storage = append(storage, b)
		}
	}
	if len(storage) != 1 || storage[0].Name != "storage" {
		t.Fatalf("storage backends = %v; want exactly one", storage)
	}
	m := storage[0].Manifest("cloudburrow", "i")
	for _, want := range []string{
		"image: dev.local/cloudburrow-storage:abc", "imagePullPolicy: Never",
		"httpGet:", `path: "/storage/v1/b?project=_"`, "--mode\", \"persistent\"", "--data-dir\", \"/data\"",
		"--pubsub-emulator\", \"pubsub.cloudburrow.svc.cluster.local:8085\"", "claimName: storage-data",
		"kind: NetworkPolicy", "policyTypes: [Egress]", "app: pubsub", "port: 53",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "tcpSocket") || strings.Contains(m, "storage-internal") {
		t.Errorf("manifest still has the retired two-Deployment shape:\n%s", m)
	}

	cfg.Mode = config.ModeEphemeral
	eph := NewLifecycleComponent("kc", cfg, io.Discard)
	eph.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	for _, b := range eph.Backends() {
		if b.Name == "storage" {
			m := b.Manifest("cloudburrow", "i")
			if strings.Contains(m, "persistentVolumeClaim") || strings.Contains(m, "--data-dir") || !strings.Contains(m, "--mode\", \"ephemeral\"") {
				t.Errorf("ephemeral mode mounts a volume or keeps a store:\n%s", m)
			}
		}
	}
}

// The signing keys go into the storage server's arguments sorted by
// account, so the manifest, and with it the Deployment, is the same on every
// `up`; with none, no flag is passed (#577).
func TestStorageManifestPassesSigningKeysSorted(t *testing.T) {
	keys := map[string][]byte{"b@p.iam.gserviceaccount.com": []byte("B"), "a@p.iam.gserviceaccount.com": []byte("A")}
	args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, keys, nil).Args, " ")
	want := "--signing-key a@p.iam.gserviceaccount.com=QQ== --signing-key b@p.iam.gserviceaccount.com=Qg=="
	if !strings.Contains(args, want) {
		t.Errorf("args = %s; want %s", args, want)
	}
	if args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, nil, nil).Args, " "); strings.Contains(args, "--signing-key") {
		t.Errorf("no keys, yet args = %s", args)
	}
}

// The Bigtable emulator ships in the emulators image and is its container's
// command: the install that fetched dl.google.com on every start crashed a
// restart with no DNS (#611), and the guarded fallback kept afterwards would
// still have reached the network had the binary been missing (#604).
func TestBigtableRunsTheEmulatorDirectly(t *testing.T) {
	b, ok := OptionalBackend(config.ServiceBigtable, "p", false)
	if !ok {
		t.Fatal("no Bigtable backend")
	}
	want := []string{bigtableEmulator, "-host", "0.0.0.0", "-port", "8086"}
	if strings.Join(b.Command, " ") != strings.Join(want, " ") {
		t.Errorf("command = %q, want %q", b.Command, want)
	}
}

// installAtStart matches a command that fetches software when a container
// starts: a package manager, the Cloud SDK's component installer, or a
// download tool.
var installAtStart = regexp.MustCompile(`(?i)\b(components\s+install|components\s+update|apt-get|apt\s+install|apk\s+add|yum|dnf|microdnf|pip3?\s+install|npm\s+(install|ci)|go\s+install|curl|wget)\b`)

// No backend installs anything when its container starts (#604): every
// component is in its digest-pinned image, so a pod start needs no network,
// and `prefetch` plus `up --offline` can stand behind that. Every service,
// in both modes, through Backends() as up calls it.
func TestNoBackendInstallsAtContainerStart(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeEphemeral, config.ModePersistent} {
		var cfg config.Config
		cfg.Services = config.KnownServices()
		cfg.Mode = mode
		cfg.Cluster.Namespace = "cloudburrow"
		c := NewLifecycleComponent("kubeconfig", cfg, io.Discard)
		c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:test")
		backends := c.Backends()
		if len(backends) == 0 {
			t.Fatal("no backends")
		}
		for _, b := range backends {
			for _, part := range [][]string{b.Command, b.Args} {
				if m := installAtStart.FindString(strings.Join(part, " ")); m != "" {
					t.Errorf("%s (%s): container start runs %q: %q", b.Name, mode, m, part)
				}
			}
		}
	}
}

// The BigQuery pod runs the validating front beside the emulator (#902):
// the front, from the locally built storage image, serves the Service's
// REST port; the emulator's REST port is the pod's other one, which the
// Service does not publish; and so does the Storage Read port (#1032).
// kubectl picks the emulator when no container is named.
func TestBigQueryManifestPutsTheFrontOnTheServicePort(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServiceBigQuery}
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	c.SetBigQueryImage("dev.local/cloudburrow-bigquery:def")
	var m string
	for _, b := range c.Backends() {
		if b.Name == "bigquery" {
			m = b.Manifest("cloudburrow", "i")
		}
	}
	if m == "" {
		t.Fatal("no bigquery backend")
	}
	for _, want := range []string{
		"kubectl.kubernetes.io/default-container: bigquery",
		// The emulator listens on the pod's loopback alone (#1114).
		`"--host=127.0.0.1", "--port=9051", "--grpc-port=9061"`,
		"- name: front\n          image: dev.local/cloudburrow-storage:abc\n          imagePullPolicy: Never\n",
		`args: ["bigquery-front", "--listen", "0.0.0.0:9050", "--upstream", "127.0.0.1:9051", ` +
			`"--storage-read-listen", "0.0.0.0:9060", "--storage-read-upstream", "127.0.0.1:9061", ` +
			`"--state-dir", "/var/lib/bigquery-front", "--storage", "http://storage.`,
		// The Storage Write streams are kept on an emptyDir only the
		// front mounts (#1115).
		"          volumeMounts:\n            - name: front-state\n              mountPath: /var/lib/bigquery-front\n",
		"        - name: front-state\n          emptyDir: {}\n",
		// The front serves the Storage Read port too (#1032); the
		// emulator's is the pod's own.
		"          ports:\n            - containerPort: 9051\n            - containerPort: 9061\n",
		"          ports:\n            - containerPort: 9050\n            - containerPort: 9060\n",
		"  selector:\n    app: bigquery\n",
		"- name: api\n      port: 9050\n      targetPort: 9050\n",
		"- name: storage-read\n      port: 9060\n      targetPort: 9060\n",
		// gs:// loads read the instance's Cloud Storage (#919).
		"- name: STORAGE_EMULATOR_HOST\n              value: \"http://storage.",
		".svc.cluster.local:4443\"\n",
		// and so does the front, for a CSV load's gs:// URIs (#944).
		`.svc.cluster.local:4443"]`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "port: 9051\n      targetPort") || strings.Contains(m, "port: 9061\n      targetPort") {
		t.Errorf("the Service publishes the emulator's own REST port:\n%s", m)
	}
	// The emulator's container is probed through the front, which
	// dials its ports on loopback (#1114); the front's on its own port.
	if !strings.Contains(m, "readinessProbe:\n            httpGet:\n              path: \""+frontready.Path+"\"\n              port: 9050\n") ||
		!strings.Contains(m, "readinessProbe:\n            tcpSocket:\n              port: 9050\n") || strings.Count(m, "readinessProbe:") != 2 {
		t.Errorf("want the emulator's readiness asked of the front, and the front's on 9050:\n%s", m)
	}
	// The emulator's process runs under the supervisor, which restarts it
	// at once when it ends or when the front fails the engine's liveness
	// path (#989), so no container of the pod has a liveness probe and
	// Kubernetes' restart back-off never applies (#1091). The supervisor
	// is copied into the emulator's container from the front's image.
	if strings.Contains(m, "livenessProbe") {
		t.Errorf("a container has a liveness probe:\n%s", m)
	}
	for _, want := range []string{
		"    spec:\n      initContainers:\n        - name: supervisor\n          image: dev.local/cloudburrow-storage:abc\n" +
			"          imagePullPolicy: Never\n          args: [\"install-self\", \"/cloudburrow-supervisor/cloudburrow-storage\"]\n",
		"        - name: bigquery\n          image: dev.local/cloudburrow-bigquery:def\n          imagePullPolicy: Never\n" +
			`          command: ["/cloudburrow-supervisor/cloudburrow-storage", "supervise", "--liveness", ` +
			`"http://127.0.0.1:9050` + BigQueryEngineLivenessPath + `", "--", "` + BigQueryEntrypoint + `"]` + "\n" +
			`          args: ["--project=`,
		"            - name: supervisor\n              mountPath: /cloudburrow-supervisor\n              readOnly: true\n",
		"        - name: supervisor\n          emptyDir: {}\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
}

// The Pub/Sub pod runs the front beside the emulator (#873): the front,
// from the locally built image, serves the Service's port, the emulator
// listens on the pod's other port, which the Service does not publish, and
// kubectl picks the emulator when no container is named. The front keeps
// its state on an emptyDir only it mounts.
func TestPubSubManifestPutsTheFrontOnTheServicePort(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServicePubSub}
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	var m string
	for _, b := range c.Backends() {
		if b.Name == "pubsub" {
			m = b.Manifest("cloudburrow", "i")
		}
	}
	if m == "" {
		t.Fatal("no pubsub backend")
	}
	for _, want := range []string{
		"kubectl.kubernetes.io/default-container: pubsub",
		// The emulator listens on the pod's loopback alone (#1114).
		`"--host-port=127.0.0.1:8086"`,
		"- name: front\n          image: dev.local/cloudburrow-storage:abc\n          imagePullPolicy: Never\n",
		`args: ["pubsub-front", "--listen", "0.0.0.0:8085", "--upstream", "127.0.0.1:8086", "--push-relay", "127.0.0.1:8087", ` +
			`"--state-file", "/var/lib/pubsub-front/state.json"]`,
		"port: 8085\n      targetPort: 8085\n",
		// The front's state is on an emptyDir (#898): it outlives the
		// front's container and goes with the pod.
		"              memory: 16Mi\n          volumeMounts:\n            - name: front-state\n              mountPath: /var/lib/pubsub-front\n" +
			"      volumes:\n        - name: front-state\n          emptyDir: {}\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "port: 8086\n      targetPort") {
		t.Errorf("the Service publishes the emulator's own port:\n%s", m)
	}
	if strings.Count(m, "volumeMounts:") != 1 || strings.Contains(m, "persistentVolumeClaim") {
		t.Errorf("want one volume mount, the front's, and no claim:\n%s", m)
	}
	// The emulator's container is probed through the front, which dials
	// it on loopback (#1114); the front's on its own port.
	if !strings.Contains(m, "readinessProbe:\n            httpGet:\n              path: \""+frontready.Path+"\"\n              port: 8085\n") ||
		!strings.Contains(m, "readinessProbe:\n            tcpSocket:\n              port: 8085\n") || strings.Count(m, "readinessProbe:") != 2 {
		t.Errorf("want the emulator's readiness asked of the front, and the front's on 8085:\n%s", m)
	}
}

// The BigQuery emulator runs from the image `up` builds from the patched
// build the CLI embeds (#1061), never pulled.
func TestBigQueryManifestRunsTheLocallyBuiltEmulator(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServiceBigQuery}
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	c.SetBigQueryImage("dev.local/cloudburrow-bigquery:def")
	var m string
	for _, b := range c.Backends() {
		if b.Name == "bigquery" {
			m = b.Manifest("cloudburrow", "i")
		}
	}
	if m == "" {
		t.Fatal("no bigquery backend")
	}
	for _, want := range []string{
		"- name: bigquery\n          image: dev.local/cloudburrow-bigquery:def\n          imagePullPolicy: Never\n",
		"- name: front\n          image: dev.local/cloudburrow-storage:abc\n          imagePullPolicy: Never\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "goccy") {
		t.Errorf("the manifest names upstream's image:\n%s", m)
	}
}
