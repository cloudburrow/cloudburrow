package components

import (
	"context"
	"fmt"
	"io"
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
	// signingKeys are the public keys the storage server verifies RSA
	// signed URLs against, by service account email (#577).
	signingKeys map[string][]byte
	// corsOrigins are the origins, beyond loopback, whose browser requests
	// the storage server answers (#677).
	corsOrigins []string
}

// SetStorageSigningKeys records the PEM public keys, by service account
// email, the builtin storage server accepts RSA signed URLs from.
func (c *LifecycleComponent) SetStorageSigningKeys(keys map[string][]byte) { c.signingKeys = keys }

// SetBuiltinStorageImage records the locally built image of the builtin
// Cloud Storage server, which `up` builds and loads before installing. The
// Pub/Sub front runs from it too (#873).
func (c *LifecycleComponent) SetBuiltinStorageImage(ref string) { c.storageImage = ref }

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
				// BigQuery's front runs from the storage image (#902).
				if s == config.ServiceBigQuery {
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
					// the same Cloud Storage (#944), looks up an extract's
					// bucket there (#939) and writes its own extracts
					// there (#957).
					b.Front.Args = append(append([]string{}, b.Front.Args...), "--storage", "http://"+InClusterBuiltinStorageHost(c.installer.Namespace))
				}
				out = append(out, b)
			}
		case config.ServiceStorage:
			// One Deployment serves the host and the cluster (#514).
			out = append(out, BuiltinStorageBackend(c.installer.Namespace, c.storageImage, persistent, c.enabled(config.ServicePubSub), c.signingKeys, c.corsOrigins))
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
	if backends := c.Backends(); len(backends) > 0 {
		if err := c.installer.InstallBackends(ctx, backends, c.timeout); err != nil {
			return err
		}
	}
	if c.NeedsKnative() {
		return c.ensureKnative(ctx)
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
		if err := c.installer.WaitKnative(ctx, c.timeout); err != nil {
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
