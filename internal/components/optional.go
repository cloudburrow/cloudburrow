package components

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/internal/config"
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

	// CloudSQLImage is PostgreSQL 17.11, pinned by digest.
	//
	// Not a Google-published component, and the only backend here that is
	// not. Google publishes no Cloud SQL emulator — the sole Cloud SQL tool
	// they ship is the Auth Proxy, which connects to a real instance in GCP —
	// so there is nothing of theirs to reuse. What runs is the database Cloud
	// SQL runs underneath. See #121 and docs/cloudsql.md.
	CloudSQLImage = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
	CloudSQLPort  = 5432

	// BigQueryImage is goccy/bigquery-emulator v0.8.1, pinned by the index
	// digest, which carries linux/amd64 and linux/arm64. Releases before
	// 0.7.0 are amd64 only, so an older pin would not run on Apple silicon.
	//
	// Community, MIT-licensed, not Google's: Google publishes no BigQuery
	// emulator. It runs on ZetaSQL, so the SQL it accepts is GoogleSQL's, but
	// how much of BigQuery's behaviour it reproduces is only what the compat
	// suite shows (docs/compatibility.md).
	BigQueryImage = "ghcr.io/goccy/bigquery-emulator@sha256:f4e428d265a93dc5ce36c294e1c584c7c9b384117d47ab8ddbb63d8d50b7f393"
	// BigQueryPort is the REST API; BigQueryStoragePort the gRPC Storage
	// Read API.
	BigQueryPort        = 9050
	BigQueryStoragePort = 9060
)

// OptionalBackend returns the backend for an opt-in service, and whether one
// exists.
func OptionalBackend(s config.Service, project string, persistent bool) (Backend, bool) {
	switch s {
	case config.ServiceFirestore:
		return firestoreBackend(project), true
	case config.ServiceDatastore:
		return datastoreBackend(project), true
	case config.ServiceBigtable:
		return bigtableBackend(project), true
	case config.ServiceSpanner:
		return spannerBackend(), true
	case config.ServiceCloudSQL:
		return cloudSQLBackend(persistent), true
	case config.ServiceBigQuery:
		return bigQueryBackend(project), true
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
	case config.ServiceCloudSQL:
		return CloudSQLPort
	case config.ServiceBigQuery:
		return BigQueryPort
	default:
		return 0
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

// bigQueryBackend runs the BigQuery emulator for the instance's project.
//
// The emulator serves exactly one project, the one it is started with: any
// other answers 404 "project ... is not found" (measured, #277). So it is
// given the instance's default project, and clients must use that project.
// It is in-memory — a restart loses every dataset, measured the same way — so
// it is given no volume, whatever the instance's mode.
func bigQueryBackend(project string) Backend {
	return Backend{
		Name:  "bigquery",
		Image: BigQueryImage,
		Port:  BigQueryPort,
		Args: []string{"--project=" + project,
			fmt.Sprintf("--port=%d", BigQueryPort), fmt.Sprintf("--grpc-port=%d", BigQueryStoragePort)},
		ExtraPorts: []NamedPort{{Name: "storage-read", Port: BigQueryStoragePort}},
	}
}

// cloudSQLBackend runs PostgreSQL in the cluster.
//
// Authentication is trust, deliberately and consistently with the rest of
// CloudBurrow: nothing here authenticates a request, and a password would be a
// shared secret that implied otherwise while being printed in the startup
// banner anyway. This is why the endpoint is bound to loopback and why the
// documentation is explicit that it must never be exposed.
//
// Unlike the emulators beside it this is a real database, so it is given a
// volume when the instance is persistent and keeps its data across a restart.
func cloudSQLBackend(persistent bool) Backend {
	return Backend{
		Name:  "cloudsql",
		Image: CloudSQLImage,
		Port:  CloudSQLPort,
		Env: map[string]string{
			// trust, not a password: see above.
			"POSTGRES_HOST_AUTH_METHOD": "trust",
			"POSTGRES_USER":             CloudSQLUser,
			"POSTGRES_DB":               CloudSQLDatabase,
			// The image refuses to initialise into a non-empty mount, which a
			// PVC's lost+found makes it. A subdirectory avoids that.
			"PGDATA": "/var/lib/postgresql/data/pgdata",
		},
		Persistent: persistent,
		MountPath:  "/var/lib/postgresql/data",
		OwnsClaim:  persistent,
	}
}

// The identity an application connects as, and the database it gets.
const (
	CloudSQLUser     = "cloudburrow"
	CloudSQLDatabase = "cloudburrow"
)
