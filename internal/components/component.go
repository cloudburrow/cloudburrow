package components

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// LifecycleComponent installs backends and, when Cloud Run is enabled, Knative.
// It adapts to lifecycle.Component so the coordinator owns its ordering.
type LifecycleComponent struct {
	installer *Installer
	services  []config.Service
	mode      config.Mode
	timeout   time.Duration
	out       io.Writer
	// project is the instance's default project. Only BigQuery needs it; see
	// Backends.
	project string
	// mysql is the instance's generated MySQL credentials, for Cloud SQL for
	// MySQL.
	mysql MySQLCredentials
	// storageImage is the builtin storage server's locally built image
	// (#514), the only Cloud Storage backend (#519). The BigQuery pod runs
	// its validating front (#902) and the Pub/Sub pod its front (#873) from
	// the same image.
	storageImage string
	// bigQueryImage is the BigQuery emulator's locally built image
	// (#1061), from the patched build this CLI embeds.
	bigQueryImage string
	// signingKeys are the public keys the storage server verifies RSA
	// signed URLs against, by service account email (#577).
	signingKeys map[string][]byte
	// corsOrigins are the origins, beyond loopback, whose browser requests
	// the storage server answers (#677).
	corsOrigins []string
	// resourceManager is Resource Manager's in-cluster address
	// (host:port), which BigQuery's front reads a project's ancestry
	// from (#1392), and the storage server a project's and a folder's
	// parents (#1532), or "".
	resourceManager string
	// knative is the Knative install running in the background, nil
	// before it starts; knativeJoined says KnativeReady waits for it
	// rather than Start (#1588).
	knative       *knativeRun
	knativeJoined bool
	// emulatorHeapMiB bounds the Java emulators' heaps and sizes their
	// containers' memory (#1603); 0 bounds neither.
	emulatorHeapMiB int
	// earlierStorage migrates what an earlier release's Cloud Storage
	// server left on the volume (#1606), or nil.
	earlierStorage EarlierStorage
	// restarted and serviceNetwork come from the cluster (#1610):
	// whether this up started stopped nodes, and a wait for their
	// Service network. Nil when unset, as in prefetch.
	restarted      func() bool
	serviceNetwork func(context.Context) error
	// bigQueryHeapMiB is the BigQuery emulator's GOMEMLIMIT (#1618); 0
	// sets none.
	bigQueryHeapMiB int
}

// SetNodeRestart gives Knative's warm path the cluster's word on whether
// this up started stopped nodes, and a wait for their Service network
// (#1610).
func (c *LifecycleComponent) SetNodeRestart(restarted func() bool, serviceNetwork func(context.Context) error) {
	c.restarted, c.serviceNetwork = restarted, serviceNetwork
}

// EarlierStorage moves the buckets a Cloud Storage server from before the
// builtin one (#519) left on a persistent instance's volume into the
// builtin server (#1606). Export runs before the backends are applied, while
// the earlier server still runs, and reports whether there is anything for
// Import, which runs once they are ready.
type EarlierStorage interface {
	Export(ctx context.Context) (bool, error)
	Import(ctx context.Context) error
}

// SetEarlierStorage records the migration of an earlier release's Cloud
// Storage (#1606). It runs only in persistent mode, with storage enabled.
func (c *LifecycleComponent) SetEarlierStorage(m EarlierStorage) { c.earlierStorage = m }

// SetResourceManager records Resource Manager's in-cluster address, for
// BigQuery's front (#1392) and the storage server (#1532).
func (c *LifecycleComponent) SetResourceManager(addr string) { c.resourceManager = addr }

// SetStorageSigningKeys records the PEM public keys, by service account
// email, the builtin storage server accepts RSA signed URLs from.
func (c *LifecycleComponent) SetStorageSigningKeys(keys map[string][]byte) { c.signingKeys = keys }

// SetBuiltinStorageImage records the locally built image of the builtin
// Cloud Storage server, which `up` builds and loads before installing. The
// Pub/Sub front runs from it too (#873).
func (c *LifecycleComponent) SetBuiltinStorageImage(ref string) { c.storageImage = ref }

// SetBigQueryImage records the locally built image of the BigQuery emulator
// (#1061), which `up` builds and loads before installing.
func (c *LifecycleComponent) SetBigQueryImage(ref string) { c.bigQueryImage = ref }

// SetMySQLCredentials supplies the passwords the MySQL backend is started
// with.
func (c *LifecycleComponent) SetMySQLCredentials(m MySQLCredentials) { c.mysql = m }

// NewLifecycleComponent builds the installer component for a configuration.
func NewLifecycleComponent(kubeconfig string, cfg config.Config, out io.Writer) *LifecycleComponent {
	return &LifecycleComponent{
		installer: &Installer{
			Kubeconfig: kubeconfig,
			Namespace:  cfg.Cluster.Namespace,
			Instance:   cfg.Name,
			Out:        out,
		},
		services:    cfg.EnabledServices(),
		corsOrigins: cfg.Storage.CORSAllowOrigins,
		project:     cfg.DefaultProject(),
		mode:        cfg.Mode,
		timeout:     time.Duration(cfg.ReadyTimeout),
		out:         out,

		emulatorHeapMiB: cfg.EmulatorHeapMiB,
		bigQueryHeapMiB: cfg.BigQueryHeapMiB,
	}
}

func (c *LifecycleComponent) Name() string { return "components" }

// Backends returns the backend definitions for the enabled services.
//
// Cloud Tasks has no backend here: it has no upstream and is implemented by
// CloudBurrow (#15/#16), so nothing is deployed for it yet.
func (c *LifecycleComponent) Backends() []Backend {
	persistent := c.mode == config.ModePersistent
	var out []Backend
	for _, s := range c.services {
		switch s {
		case config.ServicePubSub:
			out = append(out, PubSubBackend("cloudburrow", c.storageImage))
		default:
			// The Google emulators serve any project, so the name they are
			// started with is only a default. BigQuery's serves the one it
			// is started with and no other (#277), so it must be the
			// instance's, the project every client is told to use.
			project := "cloudburrow"
			if s == config.ServiceBigQuery {
				project = c.project
			}
			if s == config.ServiceCloudSQLMySQL {
				out = append(out, CloudSQLMySQLBackend(persistent, c.mysql))
				continue
			}
			if b, ok := OptionalBackend(s, project, persistent); ok {
				// Bigtable's front runs from the storage image (#1206).
				// So do the call fronts of Spanner, Firestore and
				// Datastore (#1288).
				if s == config.ServiceBigtable || s == config.ServiceSpanner ||
					s == config.ServiceFirestore || s == config.ServiceDatastore {
					b.Front.Image = c.storageImage
				}
				// Firestore's exports and imports go through the
				// instance's Cloud Storage (#1500).
				if s == config.ServiceFirestore && c.enabled(config.ServiceStorage) {
					b.Front.Args = append(append([]string{}, b.Front.Args...), "--storage", "http://"+InClusterBuiltinStorageHost(c.installer.Namespace))
				}
				// BigQuery's front runs from the storage image (#902).
				if s == config.ServiceBigQuery {
					b.Image = c.bigQueryImage
					b.Front.Image = c.storageImage
					// A load or extract with gs:// URIs reads or
					// writes the instance's Cloud Storage (#919). The
					// emulator dials the host STORAGE_EMULATOR_HOST
					// names; without it, it dialled
					// storage.googleapis.com (measured). With Cloud
					// Storage not enabled, the name does not resolve
					// and the job fails, still offline.
					b.Env = map[string]string{"STORAGE_EMULATOR_HOST": "http://" + InClusterBuiltinStorageHost(c.installer.Namespace)}
					// The front reads a CSV load's objects itself, from
					// the same Cloud Storage (#944), and looks an extract's
					// bucket up there (#939): the emulator creates a missing one.
					b.Front.Args = append(append([]string{}, b.Front.Args...), "--storage", "http://"+InClusterBuiltinStorageHost(c.installer.Namespace))
					// INFORMATION_SCHEMA.JOBS_BY_FOLDER and
					// JOBS_BY_ORGANIZATION read the project's ancestry
					// (#1392).
					if c.resourceManager != "" {
						b.Front.Args = append(b.Front.Args, "--resourcemanager", "http://"+c.resourceManager)
					}
				}
				out = append(out, b)
			}
		case config.ServiceStorage:
			// One Deployment serves the host and the cluster (#514).
			b := BuiltinStorageBackend(c.installer.Namespace, c.storageImage, persistent, c.enabled(config.ServicePubSub), c.signingKeys, c.corsOrigins)
			// Storage Intelligence inheritance walks the projects and
			// folders of Resource Manager, through cloudburrow-host (#1532).
			if c.resourceManager != "" {
				b.Args = append(b.Args, "--resourcemanager", "http://"+c.resourceManager)
				if _, p, err := net.SplitHostPort(c.resourceManager); err == nil {
					if port, err := strconv.Atoi(p); err == nil {
						b.EgressPorts = append(b.EgressPorts, port)
					}
				}
			}
			out = append(out, b)
		}
	}
	for i := range out {
		if out[i].JVM {
			out[i] = out[i].WithJVMHeap(c.emulatorHeapMiB)
		}
		// Every Go process in these pods gets a GOMEMLIMIT (#1618): the
		// fronts, the storage server and the BigQuery emulator.
		out[i] = out[i].withFrontGoMemLimit()
		switch out[i].Name {
		case "storage":
			out[i] = out[i].WithGoMemLimit(StorageGoMemLimitMiB, "64Mi")
		case "bigquery":
			out[i] = out[i].WithGoMemLimit(c.bigQueryHeapMiB, fmt.Sprintf("%dMi", c.bigQueryHeapMiB/2))
		}
	}
	return out
}

func (c *LifecycleComponent) enabled(s config.Service) bool {
	for _, e := range c.services {
		if e == s {
			return true
		}
	}
	return false
}

// NeedsKnative reports whether Cloud Run is enabled.
func (c *LifecycleComponent) NeedsKnative() bool {
	for _, s := range c.services {
		if s == config.ServiceRun {
			return true
		}
	}
	return false
}

// Start installs everything the enabled services require.
func (c *LifecycleComponent) Start(ctx context.Context) error {
	// Before anything else, so an instance with no backend pod has the
	// namespace too (#571).
	if err := c.installer.EnsureNamespace(ctx); err != nil {
		return err
	}
	// What an earlier release installed and this one does not manage goes
	// before any backend starts, so none shares a volume with it (#780).
	if _, err := c.installer.RemoveRetired(ctx, c.timeout); err != nil {
		return err
	}
	// An earlier release's Cloud Storage files are copied out through its
	// own server before the new one replaces it (#1606).
	migrate := false
	if c.earlierStorage != nil && c.mode == config.ModePersistent && c.enabled(config.ServiceStorage) {
		var err error
		if migrate, err = c.earlierStorage.Export(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrInstallFailed, err)
		}
	}
	// The backends and Knative share nothing but the node, so they are
	// installed at the same time (#1588). With KnativeStart registered,
	// Knative began right after the cluster and KnativeReady joins it;
	// otherwise it runs beside the backends here. Either way `up` is ready
	// only once both are, and the first failure is the one reported.
	if c.NeedsKnative() && c.knative == nil {
		c.startKnative(ctx)
		// The run is this Start's own: once it returns, a later Start
		// begins another rather than joining this finished one. A second
		// Start on the same component, as against a cluster an older
		// release brought up, otherwise skipped Knative entirely, its
		// in-place upgrade and its stamp included.
		defer func() {
			c.stopKnative()
			c.knative = nil
		}()
	}
	var err error
	if backends := c.Backends(); len(backends) > 0 {
		err = c.installer.InstallBackends(ctx, backends, c.timeout)
	}
	if err == nil && migrate {
		if ierr := c.earlierStorage.Import(ctx); ierr != nil {
			err = fmt.Errorf("%w: %w", ErrInstallFailed, ierr)
		}
	}
	if c.NeedsKnative() && !c.knativeJoined {
		if err != nil {
			// A backend that failed stops the Knative install rather
			// than waiting out its bound.
			c.stopKnative()
			return err
		}
		return c.joinKnative(ctx)
	}
	return err
}

// knativeRun is a Knative install running in the background.
type knativeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// startKnative begins ensureKnative in the background.
func (c *LifecycleComponent) startKnative(ctx context.Context) {
	kctx, cancel := context.WithCancel(ctx)
	run := &knativeRun{cancel: cancel, done: make(chan struct{})}
	c.knative = run
	go func() {
		defer close(run.done)
		run.err = c.ensureKnative(kctx)
	}()
}

// joinKnative waits for the background install and returns its result.
func (c *LifecycleComponent) joinKnative(ctx context.Context) error {
	run := c.knative
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopKnative cancels a background install and waits for it to return.
func (c *LifecycleComponent) stopKnative() {
	if run := c.knative; run != nil {
		run.cancel()
		<-run.done
	}
}

// KnativeStart is the component that begins installing Knative, or
// bringing it to the pin, in the background (#1588). Registered straight
// after the cluster, Knative's install, its longest step, runs while the
// images load, the backends roll out and their tunnels bind; KnativeReady,
// registered after those, waits for it. Without Cloud Run both do nothing.
func (c *LifecycleComponent) KnativeStart() *KnativePhase {
	return &KnativePhase{c: c, name: "knative-start", start: true}
}

// KnativeReady is the component that waits for the install KnativeStart
// began, so `up` is not ready until Knative is.
func (c *LifecycleComponent) KnativeReady() *KnativePhase {
	c.knativeJoined = true
	return &KnativePhase{c: c, name: "knative"}
}

// KnativePhase is one end of the background Knative install.
type KnativePhase struct {
	c     *LifecycleComponent
	name  string
	start bool
}

func (k *KnativePhase) Name() string { return k.name }

func (k *KnativePhase) Start(ctx context.Context) error {
	if !k.c.NeedsKnative() {
		return nil
	}
	if k.start {
		k.c.startKnative(ctx)
		return nil
	}
	return k.c.joinKnative(ctx)
}

// Stop cancels an install still running when startup unwinds.
func (k *KnativePhase) Stop(context.Context) error {
	if k.start {
		k.c.stopKnative()
	}
	return nil
}

// ensureKnative installs Knative, or brings an installed one to the pinned
// release, and records the release in the cluster's stamp (#601).
//
// An installed Knative used to be left as it was, so a release that bumped
// KnativeVersion never reached an existing instance. Now the stamp decides:
// the pinned release gets only the settings an install applies; any other,
// or one no stamping CLI recorded, gets the pinned manifests applied in
// place, which is how Knative upgrades. A newer release than the pin is
// refused, since Knative does not support downgrading.
func (c *LifecycleComponent) ensureKnative(ctx context.Context) error {
	kubectl := cluster.Kubectl(c.installer.Kubectl)
	if !c.installer.KnativeInstalled(ctx) {
		fmt.Fprintf(c.out, "  installing Knative Serving %s (this takes a minute)...\n", KnativeVersion)
		if err := c.installer.InstallKnative(ctx, c.timeout); err != nil {
			return err
		}
		return cluster.WriteStamp(ctx, kubectl, cluster.Stamp{KnativeVersion: KnativeVersion})
	}
	stamp, err := cluster.ReadStamp(ctx, kubectl)
	if err != nil {
		return err
	}
	if stamp.KnativeVersion == KnativeVersion {
		fmt.Fprintf(c.out, "  knative %s already installed\n", KnativeVersion)
		// The settings an install applies, for a cluster that was
		// installed before they existed (#568). After stop and up the
		// webhook that validates them is still coming back, so wait for
		// Knative as an install does.
		if err := c.waitKnativeWarm(ctx); err != nil {
			return err
		}
		return c.installer.ConfigureDeployment(ctx)
	}
	if cmp, ok := cluster.CompareKnative(stamp.KnativeVersion, KnativeVersion); ok && cmp > 0 {
		return fmt.Errorf("%w: cluster has Knative %s and this cloudburrow pins %s; Knative cannot be downgraded "+
			"in place. Use a cloudburrow release that pins %s or later, or run `cloudburrow delete` and then `cloudburrow up`",
			cluster.ErrKnativeDowngrade, stamp.KnativeVersion, KnativeVersion, stamp.KnativeVersion)
	}
	from := stamp.KnativeVersion
	if from == "" {
		from = "an unrecorded release"
	}
	fmt.Fprintf(c.out, "  knative on this cluster is %s; applying %s in place...\n", from, KnativeVersion)
	// serving-core carries ConfigMaps Knative's own webhook validates, and
	// after stop and up it is still coming back. A Knative that never
	// becomes ready may be exactly what the new manifests fix, so the
	// apply goes ahead either way and fails on its own terms if it must.
	if err := c.installer.WaitKnative(ctx, c.timeout); err != nil {
		fmt.Fprintf(c.out, "  knative is not ready (%v); applying %s anyway\n", err, KnativeVersion)
	}
	if err := c.installer.InstallKnative(ctx, c.timeout); err != nil {
		return err
	}
	return cluster.WriteStamp(ctx, kubectl, cluster.Stamp{KnativeVersion: KnativeVersion})
}

// waitKnativeWarm waits for an installed Knative to serve.
//
// When this up started the cluster's stopped nodes, Knative's pods were
// started with the node, before its Service network worked: they exit on
// the kubernetes Service they cannot reach, and the kubelet's restart
// backoff then held them back, so a warm start took 23 to 55 s (#1610).
// Their Deployments' status is also the one from before the stop, so
// "Available" answered at once while the webhook still refused calls.
// Here the network is waited for, then Knative's pods are replaced by ones
// started on it, and up waits for those: fresh pods with no backoff, and a
// readiness that is this boot's.
func (c *LifecycleComponent) waitKnativeWarm(ctx context.Context) error {
	if c.restarted == nil || !c.restarted() {
		return c.installer.WaitKnative(ctx, c.timeout)
	}
	if c.serviceNetwork != nil {
		fmt.Fprintln(c.out, "  waiting for the cluster's Service network...")
		if err := c.serviceNetwork(ctx); err != nil {
			if ctx.Err() != nil {
				return err
			}
			// A probe that cannot run (a node image without curl) is no
			// reason to fail the start: Knative comes back as it did
			// before, late.
			fmt.Fprintf(c.out, "  the Service network was not confirmed (%v); waiting for knative as it restarts itself\n", err)
			return c.installer.WaitKnative(ctx, c.timeout)
		}
	}
	return c.installer.RestartKnative(ctx, c.timeout)
}

// Installer exposes the underlying installer.
func (c *LifecycleComponent) Installer() *Installer { return c.installer }

// HasStorage reports whether a storage backend was deployed.
func (c *LifecycleComponent) HasStorage() bool {
	for _, b := range c.Backends() {
		if b.Name == "storage" {
			return true
		}
	}
	return false
}

// ReadyTimeout is the bound used for component readiness.
func (c *LifecycleComponent) ReadyTimeout() time.Duration { return c.timeout }

// Stop is a no-op: components live in the cluster and outlive the CLI process.
// Removing them belongs to `reset`, which is explicit.
func (c *LifecycleComponent) Stop(context.Context) error { return nil }
