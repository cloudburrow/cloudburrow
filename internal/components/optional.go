package components

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// Optional emulator components.
//
// Each wraps a Google-published emulator rather than reimplementing a database
// (ADR-0005). Firestore, Bigtable and Spanner are in-memory by design, which is
// why none of them is given a volume: provisioning one would imply durability
// they do not have. Datastore's emulator stores to disk, so in persistent mode
// it is given one (#307).
const (
	// SpannerImage is Google's Spanner emulator, pinned by digest. Unlike the
	// others it ships as its own image rather than inside the Cloud SDK.
	SpannerImage = "gcr.io/cloud-spanner-emulator/emulator@sha256:c6f3402f2599684f295a0fdefb6fbbbfb18a0e43e309ff5456ccb452a4570a79"

	FirestorePort = 8080
	DatastorePort = 8081
	BigtablePort  = 8086
	// BigtableEmulatorPort is the Bigtable emulator's own port, on its
	// pod's loopback alone: its front serves BigtablePort (#1206).
	BigtableEmulatorPort = 8096
	// SpannerPort is the gRPC port the official clients use. SpannerRESTPort
	// is the emulator's REST API (instance and database admin among it),
	// which the Terraform provider uses (#1331); the call front serves both.
	SpannerPort     = 9010
	SpannerRESTPort = 9020

	// The emulators' own ports behind their call fronts
	// (internal/callfront, #1288), on their pods' loopback alone: the
	// fronts serve FirestorePort, DatastorePort and SpannerPort.
	FirestoreEmulatorPort = 8090
	DatastoreEmulatorPort = 8091
	SpannerEmulatorPort   = 9011
	// SpannerEmulatorRESTPort is the emulator's own REST port, behind the
	// front's SpannerRESTPort.
	SpannerEmulatorRESTPort = 9021

	// CloudSQLImage is PostgreSQL 17.11, pinned by digest.
	//
	// Not a Google-published component, and the only backend here that is
	// not. Google publishes no Cloud SQL emulator — the sole Cloud SQL tool
	// they ship is the Auth Proxy, which connects to a real instance in GCP —
	// so there is nothing of theirs to reuse. What runs is the database Cloud
	// SQL runs underneath. See #121 and docs/cloudsql.md.
	CloudSQLImage = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
	CloudSQLPort  = 5432

	// The BigQuery emulator's image is not pulled (#1061): `up` builds it
	// from the patched goccy/bigquery-emulator v0.8.1 this CLI embeds
	// (internal/bigqueryimage), and LifecycleComponent.SetBigQueryImage
	// supplies its reference.
	//
	// Community, MIT-licensed, not Google's: Google publishes no BigQuery
	// emulator. It runs on ZetaSQL, so the SQL it accepts is GoogleSQL's, but
	// how much of BigQuery's behaviour it reproduces is only what the compat
	// suite shows (docs/compatibility.md).
	//
	// BigQueryPort is the REST API; BigQueryStoragePort the gRPC Storage
	// Read API. The REST port is the validating front's (#902); the
	// emulator's own REST port, BigQueryEmulatorPort, is its pod's alone.
	BigQueryPort         = 9050
	BigQueryStoragePort  = 9060
	BigQueryEmulatorPort = 9051
	// BigQueryEmulatorStoragePort is the emulator's own Storage Read
	// port, its pod's alone: the front serves BigQueryStoragePort (#1032).
	BigQueryEmulatorStoragePort = 9061
	// BigQueryFrontStateDir is where the BigQuery front keeps its state,
	// on its emptyDir (#1115).
	BigQueryFrontStateDir = "/var/lib/bigquery-front"
	// BigQueryEngineLivenessPath is the BigQuery front's path that says
	// whether the emulator's engine works (#989), which the supervisor of
	// the emulator's process gets (#1091); it is
	// bigqueryfront.EngineLivenessPath, which a test holds it to.
	BigQueryEngineLivenessPath = "/cloudburrow/bigquery-engine-live"
	// BigQueryEntrypoint is where the locally built emulator image
	// (internal/bigqueryimage, #1061) puts the emulator, which the
	// supervisor runs (#1091); the image must keep it or change it here.
	BigQueryEntrypoint = "/bin/bigquery-emulator"

	// MemorystoreImage is Valkey 8.1.10 (valkey/valkey:8.1-alpine), pinned
	// by the index digest, which carries linux/amd64 and linux/arm64.
	//
	// Not Google-published: Google publishes no Memorystore emulator. Valkey
	// is the open-source, Redis-compatible server Memorystore for Valkey
	// runs, so what an application gets is a real RESP data plane — and none
	// of the Memorystore admin API. See #296 and docs/memorystore.md.
	MemorystoreImage = "valkey/valkey:8.1-alpine@sha256:081c2f5cb575efc901aa80ff9cdbd1ec6a301682fd35e1ebb4b0990a4a4a8507"
	MemorystorePort  = 6379

	// CloudSQLMySQLImage is MySQL 8.4.11 (mysql:8.4, the LTS line), pinned
	// by the index digest, which carries linux/amd64 and linux/arm64.
	//
	// Like CloudSQLImage, not Google-published and not an emulator: it is the
	// database Cloud SQL for MySQL runs. See #297 and docs/cloudsql.md.
	CloudSQLMySQLImage = "mysql:8.4@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d"
	CloudSQLMySQLPort  = 3306
)

// The servers of the other database versions an Admin API instance can
// be created with (#1381): each the official image of that major version,
// pinned by its index digest (linux/amd64 and linux/arm64), at the minor
// version Cloud SQL lists for it (docs.cloud.google.com/sql/docs/db-versions,
// 2026-10-16). Like CloudSQLImage, none is Google-published. A node pulls
// one only when an instance of its version is created.
//
// PostgreSQL 9.6 to 12 (#1462) are past their upstream end of life, as
// Cloud SQL's extended support says; each official image's last build,
// the final minor version, is what runs (12.22, 11.22, 10.23, 9.6.24),
// and each index still carries linux/amd64 and linux/arm64 (checked with
// docker buildx imagetools inspect, 2026-10-16). MySQL 5.6 and 5.7 have
// none to run: their official images are linux/amd64 only (5.7.44, last
// built 2023-12; 5.6.51, 2021-12), so an arm64 node cannot run them, and
// SQL Server's image (mcr.microsoft.com/mssql/server, not a Docker Hub
// official image) is linux/amd64 only as well.
const (
	CloudSQLPostgres96Image = "postgres:9.6-alpine@sha256:8342bcb43446694428ec6594e72e4299692854f0fc3aca090b0ab46f4c7f32a1" // 9.6.24
	CloudSQLPostgres10Image = "postgres:10-alpine@sha256:63cfb6eac6b362c7c994f22c3804c61b31898cf0cb52f8e7e86bd99a244f4366"  // 10.23
	CloudSQLPostgres11Image = "postgres:11-alpine@sha256:ea50b9fd617b66c9135816a4536cf6e0697d4eea7014a7194479c95f6edd5ef9"  // 11.22
	CloudSQLPostgres12Image = "postgres:12-alpine@sha256:7c8f4870583184ebadf7f17a6513620aac5f365a7938dc6a6911c1d5df2f481a"  // 12.22
	CloudSQLPostgres13Image = "postgres:13-alpine@sha256:fb9065b6e3e213bdc07edd372a5b2a26245840b7fb65d1fd8b6700106d51805c"  // 13.23
	CloudSQLPostgres14Image = "postgres:14-alpine@sha256:4ea9e5ed06591da7ea23eb65465e8d3187fe79f4d5ec3ae976d29a33b013e77a"  // 14.24
	CloudSQLPostgres15Image = "postgres:15-alpine@sha256:f7d23353e1b15400d22ebe31189f4d314b87a4c129cc400c8c2d8d4ca127bf81"  // 15.19
	CloudSQLPostgres16Image = "postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"  // 16.15
	CloudSQLPostgres18Image = "postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873"  // 18.6
	CloudSQLMySQL80Image    = "mysql:8.0@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"           // 8.0.46
)

// The Percona XtraBackup images a Cloud SQL for MySQL instance's physical
// external sync (#1570) runs as a Job, the official percona/percona-xtrabackup
// of the instance's major version, pinned by the index digest, which
// carries linux/amd64 and linux/arm64 (checked with docker buildx
// imagetools inspect, 2026-10-17): XtraBackup 8.0.35-36 and 8.4.0-7. A
// node pulls one only when a physical sync runs.
const (
	XtraBackup80Image = "percona/percona-xtrabackup:8.0@sha256:368c3429c4ccddc46eb88e7f6afb0dd31db28e5611a053f4bb6401e4251a3319"
	XtraBackup84Image = "percona/percona-xtrabackup:8.4@sha256:bf1478c188271b2478faddb92a27e8378364b672b8e399c0df1c17fef8cb0ceb"
)

// XtraBackupImage is the XtraBackup image for a MySQL databaseVersion, or
// "" for one it has none for.
func XtraBackupImage(version string) string {
	switch {
	case strings.HasPrefix(version, "MYSQL_8_0"):
		return XtraBackup80Image
	case strings.HasPrefix(version, "MYSQL_8_4"):
		return XtraBackup84Image
	}
	return ""
}

// CloudSQLVersionImages are the images of the Cloud SQL databaseVersion
// values a created instance can have, by that value.
var CloudSQLVersionImages = map[string]string{
	"POSTGRES_9_6": CloudSQLPostgres96Image,
	"POSTGRES_10":  CloudSQLPostgres10Image,
	"POSTGRES_11":  CloudSQLPostgres11Image,
	"POSTGRES_12":  CloudSQLPostgres12Image,
	"POSTGRES_13":  CloudSQLPostgres13Image,
	"POSTGRES_14":  CloudSQLPostgres14Image,
	"POSTGRES_15":  CloudSQLPostgres15Image,
	"POSTGRES_16":  CloudSQLPostgres16Image,
	"POSTGRES_17":  CloudSQLImage,
	"POSTGRES_18":  CloudSQLPostgres18Image,
	"MYSQL_8_0":    CloudSQLMySQL80Image,
	"MYSQL_8_4":    CloudSQLMySQLImage,
}

// OptionalBackend returns the backend for an opt-in service, and whether one
// exists.
func OptionalBackend(s config.Service, project string, persistent bool) (Backend, bool) {
	switch s {
	case config.ServiceFirestore:
		return firestoreBackend(project), true
	case config.ServiceDatastore:
		return datastoreBackend(project, persistent), true
	case config.ServiceBigtable:
		return bigtableBackend(project), true
	case config.ServiceSpanner:
		return spannerBackend(), true
	case config.ServiceCloudSQL:
		return cloudSQLBackend(persistent), true
	case config.ServiceBigQuery:
		return bigQueryBackend(project), true
	case config.ServiceMemorystore:
		return memorystoreBackend(persistent), true
	case config.ServiceCloudSQLMySQL:
		// The password is the instance's own; LifecycleComponent.Backends
		// supplies it. Without one the image refuses to initialise, which is
		// the right failure for a backend built without its credentials.
		return CloudSQLMySQLBackend(persistent, MySQLCredentials{}), true
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
	case config.ServiceMemorystore:
		return MemorystorePort
	case config.ServiceCloudSQLMySQL:
		return CloudSQLMySQLPort
	default:
		return 0
	}
}

// callFront is the call front (internal/callfront, #1288) serving port for
// the emulator on upstream on the pod's loopback: it passes every call
// through unread and keeps its method, outcome and duration for the CLI's
// request metrics, request log and traces, so a pod's calls are seen as
// the host's are. Its image is the storage image, which
// LifecycleComponent.Backends sets.
// FrontStateDir is where the Spanner, Bigtable and Firestore fronts keep
// their records and data copies (#1514), on an emptyDir of their pod
// limited to FrontStateSizeLimit: a restart of the front's container keeps
// them, and a restart of the pod loses them with the emulator's data.
const (
	FrontStateDir       = "/var/lib/cloudburrow-front"
	FrontStateSizeLimit = "1Gi"
)

// withState has the front keep its state in FrontStateDir.
func withState(f *Front) *Front {
	f.Args = append(f.Args, "--state-dir", FrontStateDir)
	f.StateDir, f.StateSizeLimit = FrontStateDir, FrontStateSizeLimit
	return f
}

func callFront(port, upstream int) *Front {
	return &Front{
		Name:       "front",
		PullPolicy: "Never",
		Args: []string{"call-front", "--listen", fmt.Sprintf("0.0.0.0:%d", port),
			"--upstream", fmt.Sprintf("127.0.0.1:%d", upstream)},
		UpstreamPort: upstream,
	}
}

func firestoreBackend(project string) Backend {
	return Backend{
		Name:  "firestore",
		Image: PubSubImage, // the same digest-pinned Cloud SDK emulators image
		Port:  FirestorePort,
		Command: []string{"gcloud", "beta", "emulators", "firestore", "start",
			fmt.Sprintf("--host-port=127.0.0.1:%d", FirestoreEmulatorPort)},
		Front: firestoreFront(),
	}
}

// firestoreFront is the call front, serving the Firestore Admin API itself
// (internal/firestoreadmin, #1331): the emulator serves the documents API
// alone; and PartitionQuery, which it does not serve (internal/queryfront,
// #1405).
func firestoreFront() *Front {
	f := callFront(FirestorePort, FirestoreEmulatorPort)
	f.Args = append(f.Args, "--firestore-admin", "--queries", "firestore")
	return withState(f)
}

// datastoreFront is the call front, answering RunAggregationQuery, which
// the emulator does not serve, from its RunQuery (internal/queryfront,
// #1405), and recording the named databases calls name, for a project's
// purge (internal/callfront, #1550).
func datastoreFront() *Front {
	f := callFront(DatastorePort, DatastoreEmulatorPort)
	f.Args = append(f.Args, "--queries", "datastore", "--datastore-databases")
	return f
}

// DatastoreLockFile is the file on persistent Datastore's volume whose lock
// the running emulator holds (#1307).
const DatastoreLockFile = "/data/datastore.lock"

// datastoreBackend runs the Datastore emulator.
//
// In persistent mode its on-disk store is on a volume (#307), and the
// emulator itself is the container's process: `gcloud beta emulators
// datastore start` and the cloud_datastore_emulator script both run the JVM
// as a child, so the SIGTERM a deleted pod gets never reached it, it never
// shut down cleanly, and an entity written shortly before was lost (measured
// in CI). The data directory is created as gcloud's own wrapper does, when it
// is empty, and the JVM is exec'd so it receives the signal.
//
// The emulator loads the store when it starts and saves it when it shuts
// down, and a deleted pod's replacement, which the ReplicaSet creates at
// once on the same volume, could start before the old pod's JVM had even
// been sent SIGTERM: it then loaded the store without the last writes, and
// served and later saved that (#1307, measured with the call front in the
// pod: the new JVM loaded at 26.69 s, the old one was signalled at 27.33 s
// and saved in 3 ms, and TestDatastoreSurvivesAPodRestart lost the entity
// in three runs of three). So the JVM holds an exclusive flock(1) on a file
// in the volume for as long as it runs, an inherited descriptor that exec
// keeps open, and the next pod's emulator waits for it: the old JVM's lock
// goes only when it has exited, after its save, or when it is killed. A
// ReadWriteOnce volume is on one node, so both pods share its kernel's locks.
//
// Ephemeral mode keeps the gcloud wrapper with --no-store-on-disk, because
// writing to a directory nothing preserves only slows startup.
//
// Both modes run as Firestore in Datastore mode (#371), which is what a
// Datastore database in Google Cloud is today, and which is strongly
// consistent. Without it the emulator defaults to --consistency=0.9: about
// one query in ten missed an entity written just before it (19 of 200,
// measured).
func datastoreBackend(project string, persistent bool) Backend {
	b := Backend{Name: "datastore", Image: PubSubImage, Port: DatastorePort,
		Front: datastoreFront()}
	if !persistent {
		b.Command = []string{"gcloud", "beta", "emulators", "datastore", "start",
			"--project=" + project,
			fmt.Sprintf("--host-port=127.0.0.1:%d", DatastoreEmulatorPort),
			"--no-store-on-disk", "--use-firestore-in-datastore-mode"}
		return b
	}
	const emu = "/google-cloud-sdk/platform/cloud-datastore-emulator"
	b.Command = []string{"sh", "-c", fmt.Sprintf(
		`exec 9>`+DatastoreLockFile+`; if ! flock -n 9; then echo "waiting for the previous emulator to save /data/datastore"; flock 9; fi; `+
			`d=/data/datastore; if [ -z "$(ls -A "$d" 2>/dev/null)" ]; then %s/cloud_datastore_emulator create --project_id=%s "$d"; fi; `+
			`exec java -cp %s/CloudDatastore.jar com.google.cloud.datastore.emulator.CloudDatastore start `+
			`--host=127.0.0.1 --port=%d --store_on_disk=true --allow_remote_shutdown --firestore_in_datastore_mode "$d"`,
		emu, project, emu, DatastoreEmulatorPort)}
	b.Persistent, b.MountPath, b.OwnsClaim = true, "/data", true
	return b
}

// bigtableEmulator is where the emulators image carries the Bigtable
// emulator: Google's emulators Dockerfile installs the bigtable component
// with the Datastore, Firestore and Pub/Sub emulators.
const bigtableEmulator = "/google-cloud-sdk/platform/bigtable-emulator/cbtemulator"

// bigtableBackend runs the emulator the image carries.
//
// It used to run `gcloud components install bigtable` first, believing the
// component absent from the image. It is not, so the install did nothing but
// fetch the component snapshot from dl.google.com on every start, and a
// restart with no DNS crashed gcloud and the emulator never listened (#611).
// The emulator is the container's command, with no install to fall back to:
// the image is pinned by digest, so the binary is either there on every
// start or on none, and a fallback that reached the network would break
// the promise `up --offline` makes (#604).
//
// The front (internal/bigtablefront, #1206) runs beside it in the pod, as
// `cloudburrow-storage bigtable-front` from the locally built storage image,
// whose reference LifecycleComponent.Backends sets. The emulator panics on
// every RPC it does not implement (backups, schema bundles, UndeleteTable,
// change streams), restarting the pod and losing every table; the front
// answers those UNIMPLEMENTED and forwards the rest. It serves the
// Service's port, so every client goes through it, and the emulator listens
// on BigtableEmulatorPort on the pod's loopback alone.
func bigtableBackend(project string) Backend {
	return Backend{
		Name:    "bigtable",
		Image:   PubSubImage,
		Command: []string{bigtableEmulator, "-host", "127.0.0.1", "-port", fmt.Sprint(BigtableEmulatorPort)},
		Port:    BigtablePort,
		Front: withState(&Front{
			Name:       "front",
			PullPolicy: "Never",
			Args: []string{"bigtable-front", "--listen", fmt.Sprintf("0.0.0.0:%d", BigtablePort),
				"--upstream", fmt.Sprintf("127.0.0.1:%d", BigtableEmulatorPort)},
			UpstreamPort: BigtableEmulatorPort,
		}),
	}
}

// spannerBackend runs the image's gateway on the pod's loopback, its gRPC
// port behind the call front (#1288). Its REST port is behind the same
// front (#1331), on a second Service port, so the calls the Terraform
// provider makes over REST are in the same call log as the clients' gRPC.
func spannerBackend() Backend {
	front := callFront(SpannerPort, SpannerEmulatorPort)
	front.Args = append(front.Args, "--rest-listen", fmt.Sprintf("0.0.0.0:%d", SpannerRESTPort),
		"--rest-upstream", fmt.Sprintf("127.0.0.1:%d", SpannerEmulatorRESTPort),
		"--spanner-instances")
	withState(front)
	front.Extra = []FrontPort{{Port: SpannerRESTPort, Upstream: SpannerEmulatorRESTPort}}
	return Backend{
		Name:  "spanner",
		Image: SpannerImage,
		Port:  SpannerPort,
		Command: []string{"./gateway_main", "--hostname", "127.0.0.1",
			"--grpc_port", fmt.Sprint(SpannerEmulatorPort), "--http_port", fmt.Sprint(SpannerEmulatorRESTPort)},
		ExtraPorts: []NamedPort{{Name: "rest", Port: SpannerRESTPort}},
		Front:      front,
	}
}

// bigQueryBackend runs the BigQuery emulator for the instance's project.
//
// The emulator serves exactly one project, the one it is started with: any
// other answers 404 "project ... is not found" (measured, #277). So it is
// given the instance's default project, and clients must use that project.
// It is in-memory — a restart loses every dataset, measured the same way — so
// it is given no volume, whatever the instance's mode.
//
// The validating front (internal/bigqueryfront, #861) runs beside it in the
// pod (#902), as `cloudburrow-storage bigquery-front` from the locally built
// storage image, whose reference LifecycleComponent.Backends sets. It serves
// the Service's REST port, so every client goes through it: the host's
// tunnel, and any pod that dials bigquery.<namespace>, with or without
// Cloud Run and without pods reaching the host (#575). The emulator's REST port moves to
// BigQueryEmulatorPort, which the Service does not publish. The front
// serves the Storage Read API's port (gRPC) too (#1032), and the emulator's
// moves to BigQueryEmulatorStoragePort, which the Service does not publish
// either.
func bigQueryBackend(project string) Backend {
	return Backend{
		Name: "bigquery",
		// Built locally and loaded into kind (#1061), never pulled; the
		// reference is LifecycleComponent.SetBigQueryImage's.
		PullPolicy: "Never",
		Port:       BigQueryPort,
		// The image's entrypoint, which the supervisor runs (#1091).
		Command: []string{BigQueryEntrypoint},
		// On the pod's loopback alone (#1114): its host defaults to
		// 0.0.0.0, and a pod that reached its ports at the pod's IP would
		// pass every check of the front's, the ones that keep a request
		// from crashing it among them.
		Args: []string{"--project=" + project, "--host=127.0.0.1",
			fmt.Sprintf("--port=%d", BigQueryEmulatorPort), fmt.Sprintf("--grpc-port=%d", BigQueryEmulatorStoragePort)},
		ExtraPorts: []NamedPort{{Name: "storage-read", Port: BigQueryStoragePort}},
		Front: &Front{
			Name:       "front",
			PullPolicy: "Never",
			Args: []string{"bigquery-front", "--listen", fmt.Sprintf("0.0.0.0:%d", BigQueryPort),
				"--upstream", fmt.Sprintf("127.0.0.1:%d", BigQueryEmulatorPort),
				"--storage-read-listen", fmt.Sprintf("0.0.0.0:%d", BigQueryStoragePort),
				"--storage-read-upstream", fmt.Sprintf("127.0.0.1:%d", BigQueryEmulatorStoragePort),
				"--state-dir", BigQueryFrontStateDir},
			UpstreamPort: BigQueryEmulatorPort,
			// The Storage Write API's streams, the rows they hold, and the
			// functions the front knows of are kept here (#1115): a
			// restart of the front's container keeps them, a restart of
			// the pod loses them with the emulator's tables, as Pub/Sub's
			// front state (#898).
			StateDir: BigQueryFrontStateDir,
			Extra:    []FrontPort{{Port: BigQueryStoragePort, Upstream: BigQueryEmulatorStoragePort}},
			// The front fails it once the emulator's SQL engine has
			// failed for good (#989, bigqueryfront.EngineLivenessPath),
			// and the supervisor then restarts the emulator's process in
			// its container at once, with no Kubernetes back-off (#1091).
			BackendLivenessPath: BigQueryEngineLivenessPath,
			SuperviseBackend:    true,
		},
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

// memorystoreBackend runs Valkey in the cluster.
//
// No password, for the reason Cloud SQL has none: nothing in CloudBurrow
// authenticates a request, and the endpoint is bound to loopback on the host.
// Protected mode is off because it refuses every non-loopback client of a
// server with no password — which, inside a pod, is every client.
//
// Persistent mode keeps an append-only file on a volume, fsynced every
// second, which is Valkey's own recommended durability setting. Ephemeral
// mode writes nothing to disk at all: no RDB snapshots and no AOF, so a
// restart starts empty, which is what ephemeral means everywhere else.
func memorystoreBackend(persistent bool) Backend {
	// maxmemory is the instance's size, as Memorystore sizes one: 1 GB, the
	// smallest Basic tier instance, which the admin API reports as
	// memory_size_gb (#1302), with Memorystore's default eviction policy.
	return MemorystoreServer("memorystore", persistent, 1<<30, "", nil)
}

// MemorystoreServer is a Valkey server of the image and settings the
// instance `up` runs (memorystoreBackend), named name: the one `up` runs,
// and each instance the Memorystore admin API's CreateInstance makes
// (#1302), with that instance's maxmemory in bytes, AUTH string (""
// for none) and further settings, by Valkey's names. It follows the
// instance's mode as the first does: an append-only file on a volume of
// its own when persistent, nothing on disk when ephemeral.
func MemorystoreServer(name string, persistent bool, maxmemory int64, password string, settings map[string]string) Backend {
	return MemorystoreServerOf(MemorystoreSettings{Name: name, Persistent: persistent, MaxMemory: maxmemory,
		Password: password, Settings: settings})
}

// MemorystoreSettings are what MemorystoreServerOf starts a server with.
type MemorystoreSettings struct {
	Name       string
	Persistent bool
	// MaxMemory is the server's maxmemory, in bytes.
	MaxMemory int64
	// Password is its AUTH string, "" for none.
	Password string
	// Settings are further server settings, by their own names.
	Settings map[string]string
	// Image is the server's image: "" is MemorystoreImage (Valkey,
	// REDIS_7_2), or one of the Redis images of an older Memorystore
	// version (#1386).
	Image string
	// RDBSeconds, above 0, is RDB persistence (#1384): a snapshot once that
	// many seconds have passed with a change, written again when the server
	// stops, in place of the append-only file. In persistent mode the
	// snapshot is on the server's volume, so its data comes back with it.
	RDBSeconds int
	// ReplicaOf, host:port, starts the server as a replica of it
	// (--replicaof): a Memorystore Standard tier node (#1412).
	ReplicaOf string
	// TLSCert, TLSKey and TLSCA, PEM, are in-transit encryption (#1413):
	// the server serves TLS only, on MemorystoreTLSPort, with this
	// certificate, and replicates over TLS trusting the CA. "" for none.
	TLSCert, TLSKey, TLSCA string
	// TLSPort is the port the server serves TLS on: 0 is
	// MemorystoreTLSPort. A Redis Cluster node serves its own (#1549).
	TLSPort int
	// Dir, in persistent mode, is the server's data directory on its
	// volume in place of /data itself, made before the server starts: a
	// Redis Cluster or Valkey node's, one per formation (#1557), so a node
	// never loads files a previous formation left. When Settings has
	// appendonly, it is the node's persistence and the mode's own
	// append-only file is not added.
	Dir string
}

// MemorystoreTLSPort is the port a Memorystore server with in-transit
// encryption serves TLS on, as Memorystore's does (#1413).
const MemorystoreTLSPort = 6378

// memorystoreTLSDir is where a server's TLS files are written in its
// container, from its environment, before it starts.
const memorystoreTLSDir = "/tmp/tls"

// Redis images of Memorystore's older versions (#1386), each the last
// release of its line, pinned by the index digest (linux/amd64 and
// linux/arm64 among others). Both are BSD-3-Clause, as every Redis before
// 7.4 is. Memorystore's REDIS_6_X is 6.2, and REDIS_7_0 is 7.0.
const (
	MemorystoreRedis62Image = "redis:6.2.24-alpine@sha256:b362b5dff9d14d961dc3352db4776aba6a8c53ca2661d7c74ad2c121f82cdaea"
	MemorystoreRedis70Image = "redis:7.0.15-alpine@sha256:c9d92d840fd011c908f040592857c724ae6d877f2aba5c40ad963276507386b2"
)

// Valkey images of Memorystore for Valkey's other engine versions (#1557),
// each the latest release of its line, pinned by the index digest
// (linux/amd64 and linux/arm64 among others): VALKEY_7_2 is Valkey 7.2.14
// and VALKEY_9_0 is Valkey 9.0.6. MemorystoreImage is VALKEY_8_0's.
const (
	MemorystoreValkey72Image = "valkey/valkey:7.2-alpine@sha256:c6c2f0020be34bbf684a53a0ef716ab16ff70295186d4d9f2518435beb7afbff"
	MemorystoreValkey90Image = "valkey/valkey:9.0-alpine@sha256:187679e3bd4036959631e3f03983ab2ba503ab21e6fd0454d508e909db2ee989"
)

// MemorystoreBinary is the prefix of image's server programs: "valkey"
// (valkey-server, valkey-cli, valkey-check-rdb) for the Valkey image, and
// "redis" for a Redis one.
func MemorystoreBinary(image string) string {
	if image == "" || strings.HasPrefix(image, "valkey/") {
		return "valkey"
	}
	return "redis"
}

// MemorystoreServerOf is MemorystoreServer with every setting, the image
// and RDB persistence among them.
func MemorystoreServerOf(o MemorystoreSettings) Backend {
	name, persistent, maxmemory, password, settings := o.Name, o.Persistent, o.MaxMemory, o.Password, o.Settings
	image := o.Image
	if image == "" {
		image = MemorystoreImage
	}
	args := []string{MemorystoreBinary(image) + "-server", "--protected-mode", "no", "--maxmemory", fmt.Sprint(maxmemory)}
	if maxmemory == 1<<30 {
		args[len(args)-1] = "1gb"
	}
	policy := "volatile-lru"
	if p, ok := settings["maxmemory-policy"]; ok {
		policy = p
	}
	args = append(args, "--maxmemory-policy", policy)
	keys := make([]string, 0, len(settings))
	for k := range settings {
		if k != "maxmemory-policy" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--"+k, settings[k])
	}
	if password != "" {
		args = append(args, "--requirepass", password)
	}
	if h, p, err := net.SplitHostPort(o.ReplicaOf); err == nil {
		args = append(args, "--replicaof", h, p)
	}
	dir := ""
	if persistent && o.Dir != "" && strings.HasPrefix(o.Dir, "/data/") {
		dir = o.Dir
	}
	_, ownPersistence := settings["appendonly"]
	switch {
	case dir != "" && ownPersistence:
		args = append(args, "--dir", dir)
	case dir != "":
		args = append(args, "--dir", dir, "--appendonly", "yes", "--appendfsync", "everysec")
	case o.RDBSeconds > 0 && persistent:
		args = append(args, "--dir", "/data", "--appendonly", "no", "--save", fmt.Sprintf("%d 1", o.RDBSeconds))
	case o.RDBSeconds > 0:
		args = append(args, "--appendonly", "no", "--save", fmt.Sprintf("%d 1", o.RDBSeconds))
	case persistent:
		args = append(args, "--dir", "/data", "--appendonly", "yes", "--appendfsync", "everysec")
	default:
		args = append(args, "--save", "", "--appendonly", "no")
	}
	b := Backend{
		Name:       name,
		Image:      image,
		Port:       MemorystorePort,
		Args:       args,
		Persistent: persistent,
		MountPath:  "/data",
		OwnsClaim:  persistent,
	}
	if dir != "" && o.TLSCert == "" {
		// The directory is made, then the image's own entrypoint starts
		// the server as it always does, owning it.
		b.Args = append([]string{"sh", "-c", `set -e; mkdir -p "$0"; exec docker-entrypoint.sh "$@"`, dir}, args...)
	}
	if o.TLSCert != "" {
		// TLS only (--port 0), clients not asked for a certificate
		// (SERVER_AUTHENTICATION), replication over TLS. The files are
		// written from the environment, then the image's own entrypoint
		// starts the server as it always does.
		d := memorystoreTLSDir
		tlsPort := o.TLSPort
		if tlsPort == 0 {
			tlsPort = MemorystoreTLSPort
		}
		args = append(args, "--port", "0", "--tls-port", fmt.Sprint(tlsPort),
			"--tls-cert-file", d+"/server.crt", "--tls-key-file", d+"/server.key", "--tls-ca-cert-file", d+"/ca.crt",
			"--tls-auth-clients", "no", "--tls-replication", "yes")
		mk := ""
		if dir != "" {
			mk = `mkdir -p ` + dir + `; `
		}
		script := `set -e; ` + mk + `mkdir -p ` + d + `; printf '%s' "$MEMORYSTORE_TLS_CERT" > ` + d + `/server.crt; ` +
			`printf '%s' "$MEMORYSTORE_TLS_KEY" > ` + d + `/server.key; printf '%s' "$MEMORYSTORE_TLS_CA" > ` + d + `/ca.crt; ` +
			`chmod 0755 ` + d + `; chmod 0644 ` + d + `/*; exec docker-entrypoint.sh "$@"`
		b.Args = append([]string{"sh", "-c", script, "sh"}, args...)
		b.Port = tlsPort
		b.Env = map[string]string{"MEMORYSTORE_TLS_CERT": o.TLSCert, "MEMORYSTORE_TLS_KEY": o.TLSKey, "MEMORYSTORE_TLS_CA": o.TLSCA}
	}
	return b
}

// MemorystorePrimaryService is the Service of a Standard tier Memorystore
// instance (#1385), named name, selecting the pod of node, its primary's
// server: a failover applies it again with the new primary.
func MemorystorePrimaryService(name, node, namespace, instance string, port int) string {
	if port == 0 {
		port = MemorystorePort
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
    cloudburrow.dev/memorystore-primary: %s
spec:
  selector:
    app: %s
  ports:
    - name: api
      port: %d
      targetPort: %d
`, name, namespace, instance, node, node, port, port)
}

// MySQLCredentials are an instance's generated MySQL passwords.
type MySQLCredentials struct {
	Password     string `json:"password"`
	RootPassword string `json:"rootPassword"`
}

// CloudSQLMySQLBackend runs MySQL in the cluster.
//
// Unlike PostgreSQL's it has a password: the MySQL image has no trust mode,
// and an empty root password is a switch (MYSQL_ALLOW_EMPTY_PASSWORD) whose
// name tells a reader something is wrong. The password is generated per
// instance and kept on the host, so it is local and stable across restarts
// — the data directory is initialised with it once.
//
// The application's user owns the default database. The root password is
// for CloudBurrow's reset, which drops every database the application made.
func CloudSQLMySQLBackend(persistent bool, creds MySQLCredentials) Backend {
	return Backend{
		Name:  "cloudsql-mysql",
		Image: CloudSQLMySQLImage,
		Port:  CloudSQLMySQLPort,
		Env: map[string]string{
			"MYSQL_DATABASE":      CloudSQLDatabase,
			"MYSQL_USER":          CloudSQLUser,
			"MYSQL_PASSWORD":      creds.Password,
			"MYSQL_ROOT_PASSWORD": creds.RootPassword,
		},
		Persistent: persistent,
		MountPath:  "/var/lib/mysql",
		OwnsClaim:  persistent,
		// The image refuses to initialise into a non-empty directory, which a
		// PVC's lost+found makes it; mysqld's --ignore-db-dir went in 8.0.
		Args: []string{"mysqld", "--datadir=/var/lib/mysql/data"},
	}
}

// The identity an application connects as, and the database it gets.
const (
	CloudSQLUser     = "cloudburrow"
	CloudSQLDatabase = "cloudburrow"
)

// SQLInstancePrefix starts the name of every Deployment, Service and
// volume of a Cloud SQL instance created through the Admin API (#1330).
// The prefix keeps them apart from the servers `up` runs.
const SQLInstancePrefix = "sqlinst-"

// SQLInstanceFlag is one database flag of a created instance.
type SQLInstanceFlag struct{ Name, Value string }

// CloudSQLInstanceBackend is a Cloud SQL instance created through the Admin
// API's instances.insert (#1330): the same image and the same settings as
// cloudSQLBackend or CloudSQLMySQLBackend, under its own name, with
// Cloud SQL's default user for the engine (postgres, or root with
// rootPassword) and its database flags passed to the server, as -c
// name=value to postgres and --name=value to mysqld, on the image of its
// databaseVersion, version (CloudSQLVersionImages).
func CloudSQLInstanceBackend(name, version string, persistent bool, rootPassword string, flags []SQLInstanceFlag) Backend {
	var b Backend
	mysql := strings.HasPrefix(version, "MYSQL_")
	if mysql {
		b = CloudSQLMySQLBackend(persistent, MySQLCredentials{RootPassword: rootPassword})
		b.Env = map[string]string{"MYSQL_ROOT_PASSWORD": rootPassword}
		if rootPassword == "" {
			// Cloud SQL's root of an instance created with no password.
			b.Env = map[string]string{"MYSQL_ALLOW_EMPTY_PASSWORD": "yes"}
		}
		b.Args = append([]string{}, b.Args...)
		for _, f := range flags {
			b.Args = append(b.Args, "--"+f.Name+"="+f.Value)
		}
	} else {
		b = cloudSQLBackend(persistent)
		b.Env["POSTGRES_USER"] = "postgres"
		b.Env["POSTGRES_DB"] = "postgres"
		if len(flags) > 0 {
			b.Args = []string{"postgres"}
			for _, f := range flags {
				switch f.Name {
				case "cloudsql.logical_decoding":
					// Cloud SQL's switch for logical decoding (#1524).
					if strings.EqualFold(f.Value, "on") {
						b.Args = append(b.Args, "-c", "wal_level=logical")
					}
				case "cloudsql.enable_pglogical":
					// Recorded: an external replica's sync uses the
					// built-in logical replication, and the images have
					// no pglogical.
				default:
					b.Args = append(b.Args, "-c", f.Name+"="+f.Value)
				}
			}
		}
	}
	if img, ok := CloudSQLVersionImages[version]; ok {
		b.Image = img
	}
	b.Name = SQLInstancePrefix + name
	return b
}

// SQLReplicaSource is the primary a read replica replicates from: its
// Service in the namespace and the password of its replication user,
// cloudsqlreplica, as Cloud SQL names it.
type SQLReplicaSource struct {
	Host     string
	Password string
	// ServerID is the replica's MySQL server_id, which must differ from
	// its primary's.
	ServerID uint32
	// User and Port are an external primary's replication user and port
	// (#1501); "" and 0 are SQLReplicaUser and the primary's 3306.
	User string
	Port int
	// Logical is a PostgreSQL replica of an external primary (#1524): its
	// server starts empty, and the provisioner subscribes it to the
	// source's databases by logical replication at startExternalSync.
	Logical bool
}

// SQLReplicaUser is the replication user a replica connects to its
// primary as.
const SQLReplicaUser = "cloudsqlreplica"

// CloudSQLReplicaBackend makes b, a created instance's server, a read
// replica of src (#1330). A PostgreSQL replica's data directory is a
// pg_basebackup of the primary, taken once, written with -R so the server
// starts as a hot standby streaming from it. A MySQL replica runs with
// GTIDs and read_only; it is given the primary's data and its replication
// source once it serves (cmd/cloudburrow's provisioner), as MySQL's own
// GTID replication setup does.
func CloudSQLReplicaBackend(b Backend, mysql bool, src SQLReplicaSource) Backend {
	b.Env = copyEnv(b.Env)
	b.Env["CLOUDSQL_REPLICATION_PASSWORD"] = src.Password
	b.Env["CLOUDSQL_PRIMARY_HOST"] = src.Host
	if src.User != "" {
		b.Env["CLOUDSQL_PRIMARY_USER"] = src.User
	}
	if src.Port != 0 {
		b.Env["CLOUDSQL_PRIMARY_PORT"] = fmt.Sprint(src.Port)
	}
	if src.Logical {
		return b
	}
	if mysql {
		b.Args = append(append([]string{}, b.Args...), fmt.Sprintf("--server-id=%d", src.ServerID),
			"--gtid-mode=ON", "--enforce-gtid-consistency=ON", "--read-only=ON")
		return b
	}
	args := b.Args
	if len(args) == 0 {
		args = []string{"postgres"}
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	b.Command = []string{"sh", "-c", `set -e
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  mkdir -p "$PGDATA" && chown -R postgres:postgres "$(dirname "$PGDATA")" && chmod 700 "$PGDATA"
  asuser=$(command -v su-exec || command -v gosu)
  until PGPASSWORD="$CLOUDSQL_REPLICATION_PASSWORD" "$asuser" postgres pg_basebackup -h "$CLOUDSQL_PRIMARY_HOST" -p ` +
		fmt.Sprint(CloudSQLPort) + ` -U ` + SQLReplicaUser + ` -D "$PGDATA" -R -X stream -c fast; do
    echo "waiting for the primary $CLOUDSQL_PRIMARY_HOST"; rm -rf "$PGDATA"/* ; sleep 2
  done
fi
exec docker-entrypoint.sh ` + strings.Join(quoted, " ")}
	b.Args = nil
	return b
}

func copyEnv(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SQLStandbyPrefix starts the name of the Deployment, Service and volume
// of the second server of a Cloud SQL high-availability instance (#1408):
// "sqlha-" and the instance's name fit the 63 characters a Service's name
// may have, as "sqlinst-" and it do.
const SQLStandbyPrefix = "sqlha-"

// SQLStandbyApplicationName is the application_name a high-availability
// standby streams as, which its primary's synchronous_standby_names names,
// so a read replica of the primary is never the synchronous standby.
const SQLStandbyApplicationName = "cloudsqlstandby"

// CloudSQLStandbyBackend makes b, one of the two PostgreSQL servers of a
// high-availability instance, a standby built at epoch (#1408). When its
// volume's recorded epoch differs, the server's data directory is
// replaced by a pg_basebackup of the primary, host (the instance's
// Service, which selects the primary), and the server starts as a hot
// standby streaming from it as SQLStandbyApplicationName. When the epoch
// is the recorded one, the data is kept: a standby resumes streaming, and
// a standby promoted since starts as the primary it is now.
func CloudSQLStandbyBackend(b Backend, host string, epoch int) Backend {
	b.Env = copyEnv(b.Env)
	b.Env["CLOUDSQL_PRIMARY_HOST"] = host
	b.Env["CLOUDSQL_STANDBY_EPOCH"] = fmt.Sprint(epoch)
	args := b.Args
	if len(args) == 0 {
		args = []string{"postgres"}
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	b.Command = []string{"sh", "-c", `set -e
marker="$(dirname "$PGDATA")/cloudsql-standby-epoch"
if [ "$(cat "$marker" 2>/dev/null)" != "$CLOUDSQL_STANDBY_EPOCH" ]; then
  mkdir -p "$PGDATA" && rm -rf "$PGDATA" && mkdir -p "$PGDATA"
  chown -R postgres:postgres "$(dirname "$PGDATA")" && chmod 700 "$PGDATA"
  asuser=$(command -v su-exec || command -v gosu)
  until "$asuser" postgres pg_basebackup -h "$CLOUDSQL_PRIMARY_HOST" -p ` + fmt.Sprint(CloudSQLPort) + ` -U ` + SQLReplicaUser +
		` -D "$PGDATA" -R -X stream -c fast; do
    echo "waiting for the primary $CLOUDSQL_PRIMARY_HOST"; rm -rf "$PGDATA" && mkdir -p "$PGDATA" && chown postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"; sleep 2
  done
  echo "primary_conninfo = 'host=$CLOUDSQL_PRIMARY_HOST port=` + fmt.Sprint(CloudSQLPort) + ` user=` + SQLReplicaUser +
		` application_name=` + SQLStandbyApplicationName + `'" >> "$PGDATA/postgresql.auto.conf"
  echo "$CLOUDSQL_STANDBY_EPOCH" > "$marker"
fi
exec docker-entrypoint.sh ` + strings.Join(quoted, " ")}
	b.Args = nil
	return b
}

// SQLStandbyUser is the replication user a MySQL high-availability
// standby connects to its primary as (#1408), apart from the read
// replicas' cloudsqlreplica, whose password each replica's creation sets.
const SQLStandbyUser = "cloudsqlstandby"

// CloudSQLMySQLStandbyBackend makes b, one of the two MySQL servers of a
// high-availability instance, a standby built at epoch (#1408): when its
// volume's recorded epoch differs, its data directory is removed, so the
// image initialises a new one, which the provisioner fills from the
// primary and starts replicating (cmd/cloudburrow's sqlha.go). It runs
// with GTIDs and serverID; when the epoch is the recorded one, the data is
// kept, as for a standby promoted since.
func CloudSQLMySQLStandbyBackend(b Backend, host string, epoch int, serverID uint32) Backend {
	b.Env = copyEnv(b.Env)
	b.Env["CLOUDSQL_PRIMARY_HOST"] = host
	b.Env["CLOUDSQL_STANDBY_EPOCH"] = fmt.Sprint(epoch)
	args := append(append([]string{}, b.Args...), fmt.Sprintf("--server-id=%d", serverID),
		"--gtid-mode=ON", "--enforce-gtid-consistency=ON")
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	b.Command = []string{"sh", "-c", `set -e
marker=/var/lib/mysql/cloudsql-standby-epoch
if [ "$(cat "$marker" 2>/dev/null)" != "$CLOUDSQL_STANDBY_EPOCH" ]; then
  rm -rf /var/lib/mysql/data
  echo "$CLOUDSQL_STANDBY_EPOCH" > "$marker"
fi
exec docker-entrypoint.sh ` + strings.Join(quoted, " ")}
	b.Args = nil
	return b
}
