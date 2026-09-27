# Compatibility tests

Black-box tests that drive CloudBurrow through **official Google Cloud SDKs**. They are the
only evidence that promotes an operation off `Planned` in
[`docs/compatibility.md`](../../docs/compatibility.md).

A test written against handwritten HTTP requests does not count: it verifies our reading of
an API, not a real client's.

## Running

```sh
cloudburrow up --name ct --state-dir ./state &

export CLOUDBURROW_TEST_STORAGE=http://127.0.0.1:<storage port>
export CLOUDBURROW_TEST_PUBSUB=127.0.0.1:<pubsub port>
export CLOUDBURROW_TEST_TASKS=127.0.0.1:<tasks port>
export CLOUDBURROW_TEST_RUN=127.0.0.1:<run port>
export CLOUDBURROW_TEST_SECRETS=127.0.0.1:<secretmanager port>
export CLOUDBURROW_TEST_CONSOLE=127.0.0.1:<console port>
export CLOUDBURROW_TEST_LOCALAI=127.0.0.1:<local AI port>   # only if started with -local-ai-model

make test-compat
```

`cloudburrow up` prints every value in its endpoint block.

The credentials tests need the fixture and the metadata endpoint:

```sh
export CLOUDBURROW_TEST_CREDENTIALS=./state/ct/credentials.json
export CLOUDBURROW_TEST_METADATA=127.0.0.1:<metadata port>
```

`GOOGLE_APPLICATION_CREDENTIALS` is deliberately **not** exported for the run: the harness
refuses to start with cloud credentials in the environment, and the credentials test sets the
fixture for its own call only. That is what proves the fixture answered rather than a
developer's real gcloud login.

The prediction tests deploy a container, so they need two more:

```sh
export CLOUDBURROW_TEST_CLUSTER=cloudburrow-ct       # the kind cluster name
export CLOUDBURROW_TEST_KUBECONFIG=./state/ct/kubeconfig
```

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

The `TestTofu*` tests run `cloudburrow terraform --binary tofu`. They use the OpenTofu binary
named by `CLOUDBURROW_TEST_TOFU`, which must then exist, or else `tofu` on PATH, and skip when
there is neither. CI's storage shard sets the variable.

`TestFunctionsFrameworkBuiltWithBuildpacks` (#678) runs only with
`CLOUDBURROW_TEST_FUNCTIONS=1`, and then needs the two variables above, `pack`, and cluster
nodes that are amd64, the only platform Google's builder publishes. It starts a local
registry on a Docker network of its own, builds `testdata/function` twice through
`internal/buildpacks`, and deploys both images through the Cloud Run client. It removes its
registry, network and the function images it made; the builder and run images `pack` pulls
stay cached for the next run. No CI shard runs it; see
[docs/compatibility.md](../../docs/compatibility.md#what-ci-does-not-run-and-why).

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
removed in `t.Cleanup`. Ports are OS-assigned, so two instances can run side by side.

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

