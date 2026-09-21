package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// valid returns a configuration that passes validation, for tests that then
// break exactly one field.
func valid() Config {
	c := Default()
	c.Mode = ModeEphemeral
	c.StateDir = os.TempDir() // avoids depending on the developer's home dir
	return c
}

func TestValidateAcceptsDefaults(t *testing.T) {
	t.Parallel()
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
		wantMsg   string
	}{
		{
			name:      "empty bind address",
			mutate:    func(c *Config) { c.BindAddress = "" },
			wantField: "bindAddress",
			wantMsg:   "must not be empty",
		},
		{
			name:      "hostname instead of IP",
			mutate:    func(c *Config) { c.BindAddress = "localhost" },
			wantField: "bindAddress",
			wantMsg:   "IP address literal",
		},
		{
			// The security boundary from ADR-0004: binding a routable address
			// exposes an unauthenticated emulator, so it must be confirmed.
			name:      "non-loopback without allow-remote",
			mutate:    func(c *Config) { c.BindAddress = "0.0.0.0" },
			wantField: "bindAddress",
			wantMsg:   "--allow-remote",
		},
		{
			name:      "port too high",
			mutate:    func(c *Config) { c.Endpoints.Run = 70000 },
			wantField: "endpoints.run",
			wantMsg:   "between 0 and 65535",
		},
		{
			name:      "negative port",
			mutate:    func(c *Config) { c.Endpoints.Control = -1 },
			wantField: "endpoints.control",
			wantMsg:   "between 0 and 65535",
		},
		{
			name:      "duplicate ports",
			mutate:    func(c *Config) { c.Endpoints.Storage = c.Endpoints.Control },
			wantField: "endpoints.storage",
			wantMsg:   "duplicates endpoints.control",
		},
		{
			name:      "unknown mode",
			mutate:    func(c *Config) { c.Mode = "transient" },
			wantField: "mode",
			wantMsg:   "must be",
		},
		{
			name:      "invalid instance name",
			mutate:    func(c *Config) { c.Name = "Has Spaces" },
			wantField: "name",
			wantMsg:   "lowercase alphanumeric",
		},
		{
			name:      "unsupported cluster provider",
			mutate:    func(c *Config) { c.Cluster.Provider = "minikube" },
			wantField: "cluster.provider",
			wantMsg:   "only \"kind\" is supported",
		},
		{
			name:      "zero ready timeout",
			mutate:    func(c *Config) { c.ReadyTimeout = 0 },
			wantField: "readyTimeout",
			wantMsg:   "greater than zero",
		},
		{
			name:      "zero shutdown timeout",
			mutate:    func(c *Config) { c.ShutdownTimeout = 0 },
			wantField: "shutdownTimeout",
			wantMsg:   "greater than zero",
		},
		{
			name:      "negative shutdown timeout",
			mutate:    func(c *Config) { c.ShutdownTimeout = Duration(-time.Second) },
			wantField: "shutdownTimeout",
			wantMsg:   "greater than zero",
		},
		{
			name:      "unknown log level",
			mutate:    func(c *Config) { c.LogLevel = "verbose" },
			wantField: "logLevel",
			wantMsg:   "debug, info, warn, error",
		},
		{
			name:      "unknown service",
			mutate:    func(c *Config) { c.Services = []Service{"bigquery"} },
			wantField: "services",
			wantMsg:   "unknown service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() = %T, want *ValidationError", err)
			}
			found := false
			for _, p := range ve.Problems {
				if p.Field == tt.wantField && strings.Contains(p.Message, tt.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("no problem for field %q containing %q; got: %v", tt.wantField, tt.wantMsg, err)
			}
		})
	}
}

// Non-loopback binding is allowed once explicitly confirmed.
func TestAllowRemotePermitsNonLoopback(t *testing.T) {
	t.Parallel()
	c := valid()
	c.BindAddress = "0.0.0.0"
	c.AllowRemote = true
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil with AllowRemote", err)
	}
}

// Port 0 means "OS-assigned", so several zero ports are not duplicates. This is
// what lets compatibility tests run in parallel.
func TestZeroPortsAreNotDuplicates(t *testing.T) {
	t.Parallel()
	c := valid()
	c.Endpoints = Endpoints{} // all zero
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for all OS-assigned ports", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	c := valid()
	c.BindAddress = "not-an-ip"
	c.Mode = "nonsense"
	c.LogLevel = "chatty"
	c.ShutdownTimeout = 0

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate() = %T, want *ValidationError", err)
	}
	// Fixing configuration one restart at a time is miserable; all four
	// problems must surface together.
	if len(ve.Problems) != 4 {
		t.Errorf("got %d problems, want 4: %v", len(ve.Problems), err)
	}
	for _, want := range []string{"bindAddress", "mode", "logLevel", "shutdownTimeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestStateDirValidation(t *testing.T) {
	t.Parallel()

	t.Run("file where a directory is expected", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := valid()
		c.StateDir = path
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("Validate() = %v, want 'not a directory'", err)
		}
	})

	t.Run("empty state dir is rejected", func(t *testing.T) {
		t.Parallel()
		c := valid()
		c.StateDir = ""
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "stateDir") {
			t.Fatalf("Validate() = %v, want a stateDir problem", err)
		}
	})

	t.Run("nonexistent directory is acceptable and created later", func(t *testing.T) {
		t.Parallel()
		c := valid()
		c.StateDir = filepath.Join(t.TempDir(), "does", "not", "exist")
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil for a not-yet-created directory", err)
		}
	})
}

// Pub/Sub must never be described as persistent: the upstream audit measured
// Google's emulator losing state across a restart even with --data-dir. The
// model is not allowed to express a guarantee the backend lacks.
func TestPubSubIsNeverPersistent(t *testing.T) {
	t.Parallel()
	if got := ServicePubSub.Persistence(); got != PersistenceNone {
		t.Errorf("ServicePubSub.Persistence() = %v, want %v", got, PersistenceNone)
	}
	c := valid()
	c.Mode = ModePersistent
	found := false
	for _, s := range c.EphemeralServices() {
		if s == ServicePubSub {
			found = true
		}
	}
	if !found {
		t.Error("EphemeralServices() omitted pubsub in persistent mode; its state does not survive restart")
	}
}

func TestEphemeralModeMakesEverythingEphemeral(t *testing.T) {
	t.Parallel()
	c := valid()
	c.Mode = ModeEphemeral
	if got, want := len(c.EphemeralServices()), len(AllServices()); got != want {
		t.Errorf("EphemeralServices() = %d services, want all %d in ephemeral mode", got, want)
	}
}

func TestInstanceScoping(t *testing.T) {
	t.Parallel()
	a, b := valid(), valid()
	a.Name, b.Name = "alpha", "beta"
	if a.ClusterName() == b.ClusterName() {
		t.Errorf("two instances share a cluster name: %s", a.ClusterName())
	}
	for _, c := range []Config{a, b, valid()} {
		if !strings.HasPrefix(c.ClusterName(), "cloudburrow") {
			t.Errorf("ClusterName() = %q, want a cloudburrow prefix so ownership is identifiable", c.ClusterName())
		}
	}
	if got := valid().ClusterName(); got != "cloudburrow" {
		t.Errorf("default ClusterName() = %q, want %q without a doubled prefix", got, "cloudburrow")
	}
	if a.KubeconfigPath() == b.KubeconfigPath() {
		t.Error("two instances share a kubeconfig path")
	}
	if a.OwnerLabels()["cloudburrow.dev/instance"] != "alpha" {
		t.Errorf("OwnerLabels() = %v, want the instance name", a.OwnerLabels())
	}
}

func TestNodeImageMustBePinned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		image string
		ok    bool
	}{
		{"kindest/node:v1.36.4", true},
		{"kindest/node@sha256:" + strings.Repeat("a", 64), true},
		{"kindest/node", false},  // mutable: resolves to latest
		{"kindest/node:", false}, // empty tag
		{"", false},              // unset
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			t.Parallel()
			c := valid()
			c.Cluster.NodeImage = tt.image
			err := c.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate() = %v, want nil for %q", err, tt.image)
			}
			if !tt.ok && err == nil {
				t.Errorf("Validate() = nil, want rejection of unpinned image %q", tt.image)
			}
		})
	}
}

func TestEnabledServicesIsDeterministic(t *testing.T) {
	t.Parallel()
	c := valid()
	c.Services = []Service{ServiceRun, ServiceStorage, ServiceTasks}
	want := []Service{ServiceStorage, ServiceTasks, ServiceRun}
	for i := 0; i < 20; i++ {
		got := c.EnabledServices()
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("EnabledServices() = %v, want %v", got, want)
			}
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	t.Parallel()
	d := Duration(90 * time.Second)
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1m30s"` {
		t.Errorf("MarshalJSON() = %s, want \"1m30s\"", b)
	}
	var back Duration
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Errorf("round trip = %s, want %s", back, d)
	}
}

// Optional services must be selectable but never started by default: a
// developer should not pay for databases they did not ask for.
func TestOptionalServicesAreOptIn(t *testing.T) {
	t.Parallel()
	defaults := valid().EnabledServices()
	for _, opt := range OptionalServices() {
		for _, d := range defaults {
			if d == opt {
				t.Errorf("%s is started by default; optional services must be opt-in", opt)
			}
		}
		if !opt.IsOptional() {
			t.Errorf("%s is not reported as optional", opt)
		}
	}

	c := valid()
	c.Services = []Service{ServiceStorage, ServiceFirestore, ServiceSpanner}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected optional services: %v", err)
	}
	got := c.EnabledServices()
	if len(got) != 3 {
		t.Fatalf("EnabledServices() = %v, want 3", got)
	}
}

// Every optional emulator is in-memory, so none may be described as durable.
func TestOptionalServicesAreNeverPersistent(t *testing.T) {
	t.Parallel()
	for _, opt := range OptionalServices() {
		if opt.Persistence() != PersistenceNone {
			t.Errorf("%s reports %v; these emulators are in-memory", opt, opt.Persistence())
		}
	}
	c := valid()
	c.Mode = ModePersistent
	c.Services = OptionalServices()
	eph := c.EphemeralServices()
	if len(eph) != len(OptionalServices()) {
		t.Errorf("EphemeralServices() = %v; every optional service must be listed even in persistent mode", eph)
	}
}
