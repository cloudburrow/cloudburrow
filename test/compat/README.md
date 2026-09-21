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

make test-compat
```

`cloudburrow up` prints every value in its endpoint block.

The prediction tests deploy a container, so they need two more:

```sh
export CLOUDBURROW_TEST_CLUSTER=cloudburrow-ct       # the kind cluster name
export CLOUDBURROW_TEST_KUBECONFIG=./state/ct/kubeconfig
```

The cluster name is needed to `kind load` the fixture image into **CloudBurrow's own**
cluster, and the kubeconfig to port-forward the Knative gateway — CloudBurrow does not
publish it on a host port. Both are skips, not failures, when unset.

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
