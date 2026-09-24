package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// TestEnvAndUpExportTheSameEmulatorVariables.
//
// `cloudburrow env` exported only the Storage and Pub/Sub variables, because
// the opt-in emulators' ports were OS-assigned and known only to `up`. So
// `eval "$(cloudburrow env)"` left Firestore, Datastore, Bigtable and Spanner
// clients pointed at real Google for exactly the services the developer had
// asked to emulate (#272). Every combination of those services must now yield
// the same variables, with the same values, from both commands.
func TestEnvAndUpExportTheSameEmulatorVariables(t *testing.T) {
	optional := []config.Service{
		config.ServiceFirestore, config.ServiceDatastore,
		config.ServiceBigtable, config.ServiceSpanner,
	}
	for mask := 0; mask < 1<<len(optional); mask++ {
		services := []config.Service{config.ServiceStorage, config.ServicePubSub}
		var names []string
		for i, s := range optional {
			if mask&(1<<i) != 0 {
				services = append(services, s)
				names = append(names, string(s))
			}
		}
		t.Run("with="+strings.Join(names, "+"), func(t *testing.T) {
			cfg := config.Default()
			cfg.Services = services

			// What `up` prints: one endpoint per forwarder, from the same table.
			fromUp := map[string]string{}
			for _, f := range buildForwarders(cfg, true) {
				svc := strings.TrimPrefix(f.Name(), "forward:")
				if !config.Service(svc).IsOptional() || netfwd.EnvVarFor(svc) == "" {
					continue
				}
				if f.HostAddr() == "" {
					t.Fatalf("%s has no host address before start", svc)
				}
				fromUp[netfwd.EnvVarFor(svc)] = netfwd.EnvValueFor(svc, f.HostAddr())
			}

			// What `env` exports.
			fromEnv := map[string]string{}
			for _, v := range envVars(cfg, "demo-local", "/tmp/adc.json") {
				if strings.HasSuffix(v.Name, "_EMULATOR_HOST") &&
					v.Name != "STORAGE_EMULATOR_HOST" && v.Name != "PUBSUB_EMULATOR_HOST" {
					fromEnv[v.Name] = v.Value
				}
			}

			if len(fromUp) != len(names) {
				t.Fatalf("up exports %d emulator variables for %d enabled emulators: %v",
					len(fromUp), len(names), fromUp)
			}
			for k, v := range fromUp {
				if fromEnv[k] != v {
					t.Errorf("%s: up says %q, env says %q", k, v, fromEnv[k])
				}
			}
			for k := range fromEnv {
				if _, ok := fromUp[k]; !ok {
					t.Errorf("env exports %s for a service that is not enabled", k)
				}
			}
		})
	}
}

// TestAnOSAssignedEmulatorPortIsNotExported.
//
// A port of 0 is chosen at start and known only to `up`. Exporting a guess, or
// an empty value, is worse than nothing: most clients read an empty variable as
// unset and fall back to real Google. It is left out and reported instead.
func TestAnOSAssignedEmulatorPortIsNotExported(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceSpanner}
	cfg.Endpoints.Spanner = 0

	for _, v := range envVars(cfg, "demo-local", "/tmp/adc.json") {
		if v.Name == "SPANNER_EMULATOR_HOST" {
			t.Fatalf("exported SPANNER_EMULATOR_HOST=%q for a port only `up` knows", v.Value)
		}
	}
	got := unexportableEmulators(cfg)
	if len(got) != 1 || got[0] != config.ServiceSpanner {
		t.Fatalf("unexportableEmulators = %v, want [spanner]", got)
	}
}

// TestOptionalEmulatorPortsAreDistinctByDefault.
//
// Fixed defaults are what make `env` able to answer at all, and they only work
// if they cannot collide with each other or with the core services.
func TestOptionalEmulatorPortsAreDistinctByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeEphemeral
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default ports do not validate: %v", err)
	}
	for _, s := range []config.Service{config.ServiceFirestore, config.ServiceDatastore,
		config.ServiceBigtable, config.ServiceSpanner} {
		if cfg.Endpoints.OptionalPort(s) == 0 {
			t.Errorf("%s has no fixed default port, so env cannot export it", s)
		}
	}
	// And a clash with a core service is refused.
	cfg.Endpoints.Spanner = cfg.Endpoints.Storage
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "spanner") {
		t.Fatalf("a Spanner port equal to Storage's was accepted: %v", err)
	}
}
