package components

import (
	"fmt"

	"github.com/identity-wael/cloudburrow/internal/config"
)

// Optional emulator components.
//
// Each wraps a Google-published emulator rather than reimplementing a database
// (ADR-0005). All four are in-memory by design, which is why none is given a
// volume: provisioning one would imply durability they do not have.
const (
	// SpannerImage is Google's Spanner emulator, pinned by digest. Unlike the
	// others it ships as its own image rather than inside the Cloud SDK.
	SpannerImage = "gcr.io/cloud-spanner-emulator/emulator@sha256:c6f3402f2599684f295a0fdefb6fbbbfb18a0e43e309ff5456ccb452a4570a79"

	FirestorePort = 8080
	DatastorePort = 8081
	BigtablePort  = 8086
	// SpannerPort is the gRPC port; the emulator also serves REST on 9020,
	// which the official clients do not use.
	SpannerPort = 9010
)

// OptionalBackend returns the backend for an opt-in service, and whether one
// exists.
func OptionalBackend(s config.Service, project string) (Backend, bool) {
	switch s {
	case config.ServiceFirestore:
		return firestoreBackend(project), true
	case config.ServiceDatastore:
		return datastoreBackend(project), true
	case config.ServiceBigtable:
		return bigtableBackend(project), true
	case config.ServiceSpanner:
		return spannerBackend(), true
	default:
		return Backend{}, false
	}
}

// OptionalPort returns the container port an opt-in service listens on.
func OptionalPort(s config.Service) int {
	switch s {
	case config.ServiceFirestore:
		return FirestorePort
	case config.ServiceDatastore:
		return DatastorePort
	case config.ServiceBigtable:
		return BigtablePort
	case config.ServiceSpanner:
		return SpannerPort
	default:
		return 0
	}
}

// EmulatorEnvVar returns the official environment variable that redirects a
// client to this emulator, or "" when none exists.
//
// All four optional services have one, which is why they are cheap to support:
// an application needs no code change to use them.
func EmulatorEnvVar(s config.Service) string {
	switch s {
	case config.ServiceFirestore:
		return "FIRESTORE_EMULATOR_HOST"
	case config.ServiceDatastore:
		return "DATASTORE_EMULATOR_HOST"
	case config.ServiceBigtable:
		return "BIGTABLE_EMULATOR_HOST"
	case config.ServiceSpanner:
		return "SPANNER_EMULATOR_HOST"
	default:
		return ""
	}
}

func firestoreBackend(project string) Backend {
	return Backend{
		Name:  "firestore",
		Image: PubSubImage, // the same digest-pinned Cloud SDK emulators image
		Port:  FirestorePort,
		Command: []string{"gcloud", "beta", "emulators", "firestore", "start",
			fmt.Sprintf("--host-port=0.0.0.0:%d", FirestorePort)},
	}
}

func datastoreBackend(project string) Backend {
	return Backend{
		Name:  "datastore",
		Image: PubSubImage,
		Port:  DatastorePort,
		Command: []string{"gcloud", "beta", "emulators", "datastore", "start",
			"--project=" + project,
			fmt.Sprintf("--host-port=0.0.0.0:%d", DatastorePort),
			// The emulator would otherwise write to a local directory that
			// nothing preserves, which only slows startup.
			"--no-store-on-disk"},
	}
}

// bigtableBackend installs the emulator before starting it.
//
// The Bigtable emulator is the one Cloud SDK emulator absent from the
// published emulators image, so it is installed at container start. That needs
// network on first run, which is recorded in the docs rather than discovered
// by a user on a plane.
func bigtableBackend(project string) Backend {
	return Backend{
		Name:  "bigtable",
		Image: PubSubImage,
		Command: []string{"sh", "-c", fmt.Sprintf(
			"gcloud components install bigtable --quiet >/dev/null && "+
				"exec /google-cloud-sdk/platform/bigtable-emulator/cbtemulator -host 0.0.0.0 -port %d",
			BigtablePort)},
		Port: BigtablePort,
	}
}

func spannerBackend() Backend {
	return Backend{
		Name:  "spanner",
		Image: SpannerImage,
		Port:  SpannerPort,
	}
}
