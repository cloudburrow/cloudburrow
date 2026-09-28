# Compatibility tests

Black-box tests that drive CloudBurrow through **official Google Cloud SDKs**. They are the
only evidence that promotes an operation off `Planned` in
[`docs/compatibility.md`](../../docs/compatibility.md).

A test written against handwritten HTTP requests does not count: it verifies our reading of
an API, not a real client's.

## Running

```sh
cloudburrow up --detach --name ct --state-dir "$PWD/state"
cloudburrow wait --name ct --state-dir "$PWD/state"
eval "$(scripts/compat-env.sh --name ct --state-dir "$PWD/state")"
make test-compat
```

`up --detach` returns once the instance is ready and leaves it running in the background;
`wait` returns once it reports ready, at once when it already does. `cloudburrow stop --name ct --state-dir "$PWD/state"`
ends it.

[`scripts/compat-env.sh`](../../scripts/compat-env.sh) takes the flags the instance was started
with and prints an `export` for every `CLOUDBURROW_TEST_*` variable below that the instance has
a value for. It reads `cloudburrow env --format json` for the SDK endpoints,
`cloudburrow status --format json` for the kind cluster, and the instance directory for the
runtime file (the addresses `up` bound), the admin token and the kubeconfig. The variables it
cannot fill, because a service is not enabled, it names on stderr: their tests skip. CI's
compat shards run the same script, each with `--strict --only <its variables>`, which fails on
an empty value instead; `--list` prints every variable it sets and `--help` its options. It
needs bash and `jq`.

Pass an absolute `--state-dir`: the tests run the CLI from `test/compat` with the same flags
(`CLOUDBURROW_TEST_CLI_ARGS`), so a relative one would name a different directory.

### Which instance the CLI and gcloud tests reach

The tests that run `cloudburrow` or gcloud find the instance through `CLOUDBURROW_TEST_CLI_ARGS`
alone, and flags that leave out the instance resolve the default one and its ports, which may
be someone else's long-running instance (#927). Three guards keep them on the harness's
instance:

- `scripts/compat-env.sh` always puts `--name` and `--state-dir` in `CLOUDBURROW_TEST_CLI_ARGS`,
  taking any the flags leave out from the instance directory `env` reports, and refuses a flag
  containing whitespace, since the tests split the value on it. Its default output is quoted for
  `eval`. `--plain` output is not quoted: read it with `env(1)` or `mapfile`, never `source` it.
  The script warns about each value a shell would split or expand.
- When `CLOUDBURROW_TEST_CLI` is set, `TestMain` (`main_test.go`) runs
  `cloudburrow env --format json` and `cloudburrow status --format json` with
  `CLOUDBURROW_TEST_CLI_ARGS` before any test, and compares what they report with every
  endpoint variable set (`CONTROL`, `CONSOLE`, `METADATA`, `CREDENTIALS` and each service's).
  If any differs or is missing, or none is set, the whole run fails before any test runs, and the
  message names each difference (`checkTarget` in `target.go`, unit-tested in `target_test.go`
  without an instance).
- A gcloud test runs gcloud and `gcloud-setup` without the caller's `CLOUDSDK_*`, `GOOGLE_*`,
  `GCE_*`, `BOTO_*` or `*_EMULATOR_HOST` variables. It keeps only the endpoint overrides in
  `gcloud-setup`'s configuration that match the harness's endpoint for their service. An
  override for another endpoint fails the test before gcloud runs. An override for a service
  without a harness variable is dropped, so the egress guard refuses that call instead.

A test skips when a variable it needs is unset. An instance started with the default services
leaves most suites skipped, so start one with the services your change touches (`--services`),
and read the `--- SKIP` lines in `go test -v` output rather than a green `ok`.

### Every variable

Set by `scripts/compat-env.sh` from a running instance:

| Variable | From | Needed by |
| --- | --- | --- |
| `CLOUDBURROW_TEST_CREDENTIALS` | `GOOGLE_APPLICATION_CREDENTIALS` from `env` | the credentials test (`credentials_test.go`) and the signed URL tests |
| `CLOUDBURROW_TEST_METADATA` | runtime file, `metadata` | the credentials and IAM Credentials tests |
| `CLOUDBURROW_TEST_CONTROL` | runtime file, `control` | the admin API: events, faults, reset, seed, and the tests that inject faults or reset state through it |
| `CLOUDBURROW_TEST_CONSOLE` | runtime file, `console` | the console tests |
| `CLOUDBURROW_TEST_ADMIN_TOKEN` | `admin-token` in the instance directory | every `/admin` call (`adminauth_test.go`); read from the state directory in `CLOUDBURROW_TEST_CLI_ARGS` when unset |
| `CLOUDBURROW_TEST_KUBECONFIG` | `kubeconfig` in the instance directory, or `--kubeconfig` | the tests that restart a backend, run a pod or port-forward the Knative gateway: Spanner, prediction, Cloud Run, functions, the cluster host services |
| `CLOUDBURROW_TEST_CLUSTER` | `cluster.name` from `status` | the tests that `kind load` a fixture image into the instance's own cluster: prediction, Cloud Run environment, functions, Spanner, the cluster host services |
| `CLOUDBURROW_TEST_CLI` | `--cli`, else `bin/cloudburrow`, else `cloudburrow` on PATH | every test that runs a `cloudburrow` command: `env`, `status`, `logs`, `diagnose`, `state`, `terraform`, `gcloud-setup`, the hooks and the restart tests |
| `CLOUDBURROW_TEST_CLI_ARGS` | the flags given to the script, with `--name` and `--state-dir` added from the instance directory when they are missing | the same tests, to name this instance; checked against the endpoints before any test runs |
| `CLOUDBURROW_TEST_STORAGE` | `STORAGE_EMULATOR_HOST` | Cloud Storage |
| `CLOUDBURROW_TEST_CORS_ORIGIN` | the first `--cors-allow-origin` among the flags given to the script | `TestStorageCORSAllowlistedOriginWorks` |
| `CLOUDBURROW_TEST_PUBSUB` | `PUBSUB_EMULATOR_HOST` | Pub/Sub, and Storage notifications |
| `CLOUDBURROW_TEST_TASKS` | runtime file, `tasks` | Cloud Tasks, and the console and Terraform tests that use it |
| `CLOUDBURROW_TEST_SECRETS` | runtime file, `secretmanager` | Secret Manager, gcloud secrets, seed and Terraform |
| `CLOUDBURROW_TEST_SCHEDULER` | `CLOUDBURROW_SCHEDULER_ENDPOINT` | Cloud Scheduler, gcloud scheduler, Terraform |
| `CLOUDBURROW_TEST_RUN` | runtime file, `run` | Cloud Run |
| `CLOUDBURROW_TEST_KMS` | `CLOUDBURROW_KMS_ENDPOINT` | Cloud KMS, gcloud kms, Terraform |
| `CLOUDBURROW_TEST_LOGGING` | `CLOUDBURROW_LOGGING_ENDPOINT` | Cloud Logging, gcloud logging |
| `CLOUDBURROW_TEST_RESOURCEMANAGER` | `CLOUDBURROW_RESOURCEMANAGER_ENDPOINT` | Resource Manager |
| `CLOUDBURROW_TEST_RUN_STORAGE`, `CLOUDBURROW_TEST_RUN_PUBSUB` | as `STORAGE` and `PUBSUB` | the Cloud Run revision tests; see below |
| `CLOUDBURROW_TEST_RUN_KMS`, `CLOUDBURROW_TEST_RUN_SCHEDULER`, `CLOUDBURROW_TEST_RUN_LOGGING` | as `KMS`, `SCHEDULER` and `LOGGING` | the Cloud Run revision tests; see below |
| `CLOUDBURROW_TEST_RUN_BIGQUERY` | as `BIGQUERY` | the Cloud Run revision refused by the BigQuery front (#874); see below |
| `CLOUDBURROW_TEST_SPANNER` | `SPANNER_EMULATOR_HOST` | Spanner |
| `CLOUDBURROW_TEST_DATASTORE` | `DATASTORE_EMULATOR_HOST` | Datastore |
| `CLOUDBURROW_TEST_FIRESTORE` | `FIRESTORE_EMULATOR_HOST` | Firestore |
| `CLOUDBURROW_TEST_BIGTABLE` | `BIGTABLE_EMULATOR_HOST` | Bigtable |
| `CLOUDBURROW_TEST_MEMORYSTORE` | `REDIS_HOST`:`REDIS_PORT` | Memorystore |
| `CLOUDBURROW_TEST_MYSQL`, `CLOUDBURROW_TEST_MYSQL_PASSWORD` | `MYSQL_HOST`:`MYSQL_PORT`, `MYSQL_PASSWORD` | Cloud SQL for MySQL |
| `CLOUDBURROW_TEST_CLOUDSQL` | `PGHOST`:`PGPORT` | Cloud SQL for PostgreSQL |
| `CLOUDBURROW_TEST_BIGQUERY`, `CLOUDBURROW_TEST_BIGQUERY_STORAGE`, `CLOUDBURROW_TEST_BIGQUERY_PROJECT` | `CLOUDBURROW_BIGQUERY_ENDPOINT`, `CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT`, `GOOGLE_CLOUD_PROJECT` | BigQuery, whose emulator serves the instance's one project |
| `CLOUDBURROW_TEST_LOCALAI` | the `local AI:` line of `up.log` | the generation tests; only with `--local-ai-model` |
| `CLOUDBURROW_TEST_GCLOUD` | `--gcloud`, else `gcloud` on PATH | the gcloud and gsutil tests |
| `CLOUDBURROW_TEST_TOFU` | `--tofu`, else `tofu` on PATH | the `TestTofu*` tests |

Not set by the script, because no instance has a value for them: each is a test mode or a
fixture a job makes for itself. `test/repo` fails when a variable in `test/compat` is in
neither list.

| Variable | Needed by |
| --- | --- |
| `CLOUDBURROW_TEST_KMS_PROBE`, `CLOUDBURROW_TEST_SECRETS_PROBE`, `CLOUDBURROW_TEST_TASKS_PROBE`, `CLOUDBURROW_TEST_SCHEDULER_PROBE` | the restart probes' setup: the file each writes its fixture to |
| `CLOUDBURROW_TEST_KMS_EXPECT`, `CLOUDBURROW_TEST_SECRETS_EXPECT`, `CLOUDBURROW_TEST_TASKS_EXPECT`, `CLOUDBURROW_TEST_SCHEDULER_EXPECT`, `CLOUDBURROW_TEST_DATASTORE_EXPECT`, `CLOUDBURROW_TEST_MEMORYSTORE_EXPECT`, `CLOUDBURROW_TEST_MYSQL_EXPECT`, `CLOUDBURROW_TEST_CLOUDSQL_EXPECT` | the restart probes: `present` after a persistent restart, `absent` after an ephemeral one |
| `CLOUDBURROW_TEST_CLOUDSQL_SETUP` | `TestCloudSQLRestartSetup` |
| `CLOUDBURROW_TEST_STORAGE_RESTART_PROBE` | `TestStorageAcrossRestart`: `<mode>:<file>` |
| `CLOUDBURROW_TEST_STORAGE_VERSIONING_PROBE` | `TestStorageVersioningPersistentMode`, against the builtin `storage-server` |
| `CLOUDBURROW_TEST_SIGNING_KEY`, `CLOUDBURROW_TEST_SIGNING_EMAIL` | the signed URL tests, against a builtin `storage-server` started with `--signing-cert` |
| `CLOUDBURROW_TEST_FUNCTIONS` | `TestFunctionsFrameworkBuiltWithBuildpacks`, `TestFunctionsRebuildReusesLayers` and `TestFunctionsBuildBrokenModulePathFails`, which run only when it is `1` |
| `CLOUDBURROW_TEST_NAMESPACE` | the Spanner restart and in-cluster tests; defaults to `cloudburrow`, the namespace every instance uses |

The restart probes are driven by ci.yml's compat job, which runs each setup, stops the
instance, brings it up again and runs each probe with these set; see
[docs/compatibility.md](../../docs/compatibility.md).

`GOOGLE_APPLICATION_CREDENTIALS` is deliberately **not** exported for the run: the harness
refuses to start with cloud credentials in the environment, and the credentials test sets the
fixture for its own call only. That is what proves the fixture answered rather than a
developer's real gcloud login.

The cluster name is needed to `kind load` the fixture image into **CloudBurrow's own**
cluster, and the kubeconfig to port-forward the Knative gateway — CloudBurrow does not
publish it on a host port. Both are skips, not failures, when unset.

The gcloud, gsutil and bq tests run the gcloud named by `CLOUDBURROW_TEST_GCLOUD`, which must
then exist, and the gsutil and bq beside it, or else the ones on PATH, and skip when there are
none. CI
sets it to the Google Cloud CLI release it installs, 586.0.0, whose archive checksum
dependencies.json records. The Terraform and OpenTofu modules require `hashicorp/google`
exactly, and each test copies `testdata/terraform/.terraform.lock.hcl` into its module before
init, so a provider package that matches no locked hash fails init. Every one of these tests
logs `gcloud version`, `gsutil version`, `bq version`, or `terraform version -json`
(`tofu version -json`).

`TestCloudRunRevisionReachesStorageAndPubSubWithNoClientOptions` needs Cloud Run, Storage and
Pub/Sub in one instance. It reads `CLOUDBURROW_TEST_RUN_STORAGE` and `CLOUDBURROW_TEST_RUN_PUBSUB`
when set, else `CLOUDBURROW_TEST_STORAGE` and `CLOUDBURROW_TEST_PUBSUB`; CI's run shard sets the
first pair, so the Storage and Pub/Sub suites do not run there a second time.
`TestCloudRunRevisionUsesInjectedMetadataADCAndServedEndpoints` also needs KMS, Scheduler and
Logging, read from `CLOUDBURROW_TEST_RUN_KMS`, `CLOUDBURROW_TEST_RUN_SCHEDULER` and
`CLOUDBURROW_TEST_RUN_LOGGING` when set, else `CLOUDBURROW_TEST_KMS`, `CLOUDBURROW_TEST_SCHEDULER`
and `CLOUDBURROW_TEST_LOGGING`.
`TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront` and
`TestCloudRunRevisionDialingTheBigQueryServiceGetsTheFront` (#881) need Cloud Run and BigQuery in one
instance, and read `CLOUDBURROW_TEST_RUN_BIGQUERY` when set, else `CLOUDBURROW_TEST_BIGQUERY`,
with `CLOUDBURROW_TEST_BIGQUERY_PROJECT`.
`TestAPodDialingTheBigQueryServiceGetsTheFrontWithoutCloudRun` (#902) runs a one-off pod on an
instance without Cloud Run, and needs `CLOUDBURROW_TEST_BIGQUERY`, `CLOUDBURROW_TEST_BIGQUERY_PROJECT`,
`CLOUDBURROW_TEST_KUBECONFIG` and `CLOUDBURROW_TEST_CLUSTER`, with docker, kind and go to build the
env probe; it skips where Cloud Run is enabled, whose tests cover it.

The `TestTofu*` tests run `cloudburrow terraform --binary tofu`. They use the OpenTofu binary
named by `CLOUDBURROW_TEST_TOFU`, which must then exist, or else `tofu` on PATH, and skip when
there is neither. CI's storage shard sets the variable.

`TestFunctionsFrameworkBuiltWithBuildpacks` (#678) runs only with
`CLOUDBURROW_TEST_FUNCTIONS=1`, and then needs `CLOUDBURROW_TEST_CLUSTER`,
`CLOUDBURROW_TEST_KUBECONFIG`, `pack`, and cluster
nodes that are amd64, the only platform Google's builder publishes. It starts a local
registry on a Docker network of its own, builds `testdata/function` twice through
`internal/buildpacks`, and deploys both images through the Cloud Run client. It removes its
registry, network and the function images it made; the builder and run images `pack` pulls
stay cached for the next run. No CI shard runs it; see
[docs/compatibility.md](../../docs/compatibility.md#what-ci-does-not-run-and-why).

`TestFunctionsRebuildReusesLayers` and `TestFunctionsBuildBrokenModulePathFails` (#678) are
gated by the same variable and need `pack` and Docker, but no cluster or instance: each
builds into a local registry of its own and runs nothing. The first builds
`testdata/function` twice to one image and asserts the rebuild's log reports
`Reusing layer '…'`; the second builds a copy whose `go.mod` declares a module path with no
dot and asserts the `build failed` error carries the buildpack's message. Both remove their
registry, network and the `pack-cache-*` volumes they created.

`TestPredictionStartupFailureIsReported` takes about **ten minutes**: Knative declares a
revision failed only after its 600s progress deadline. That latency is the finding, not an
accident — see [docs/prediction.md](../../docs/prediction.md).

## Safety

The harness refuses to run against anything but a local instance:

- **Cloud credentials in the environment are a hard failure**, not a skip.
  `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CLOUD_PROJECT` and `GCLOUD_PROJECT` all abort the
  run. A compatibility suite that silently talked to real GCP would be expensive and wrong.
- **Non-loopback endpoints are refused**, and any `*.googleapis.com` or `*.google.com` host
  is rejected outright.
- Clients are built with `WithoutAuthentication`; application default credentials are never
  loaded.

## Isolation

Every test uses a **unique project ID** derived from the clock, so concurrent runs cannot
collide and no test depends on another's leftovers. Buckets, topics and subscriptions are
removed in `t.Cleanup`. The default ports are fixed, from 9000, so a second instance beside
the first needs its own `--port-base` (for example `--port-base 9200`), or port 0 for a
service to have the OS assign one; the script reads whatever each instance bound.

Every call is made under a bounded context, so a hanging operation fails its test instead of
stalling the suite.

## What a skip means

A test skips when its service endpoint is not exported — the service may legitimately not be
deployed. That is not the same as a blanket skip: each service asks for **its own** endpoint,
so an unimplemented operation can never pass by being skipped wholesale.

## Unverified error codes

Some errors are not documented by Google: for example, the code Cloud KMS returns when a
disabled key version is used. A test that asserts one marks it in the test function's doc
comment:

```go
// covers: google.cloud.kms.v1.KeyManagementService/Decrypt
// unverified: google.cloud.kms.v1.KeyManagementService/Decrypt FAILED_PRECONDITION: a DISABLED version
func TestDecryptRefusesADisabledVersion(t *testing.T) { ... }
```

The format is `// unverified: <Service>/<Method> <CODE>: <case>`, where `<CODE>` is a
canonical gRPC code name. In-process tests under `internal/service` may carry it too, for
cases that only a fake clock can reach. `go run ./tools/coverage` lists every annotation in an
**Unverified error codes** table on the service's coverage page and marks the method's row.
`-check` fails, naming the file and line, for an unknown method, a code that is not a gRPC code
name, or an annotation outside a test's doc comment.

**Pinning one.** A maintainer records an observation of Google's real behaviour: a Google
documentation URL, or an issue comment with the captured request and response. They link it
from the test and remove the annotation. No CI job ever calls Google to find out.

UNIMPLEMENTED for a method CloudBurrow does not serve is CloudBurrow's own policy, not a claim
about Google, so it is never marked unverified.

