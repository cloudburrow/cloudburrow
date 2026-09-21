# Acceptance workflow

The single workflow the first release is judged by (#19):

1. **Upload** an object to Cloud Storage
2. **Publish** an event naming it to Pub/Sub
3. **Run** a worker — a Cloud Run service on Knative — that receives the push and reads the object
4. **Save** the result as a new object, read back from the host

**Every step uses an official Google SDK.** If any step needed a CloudBurrow-specific client,
the release would have failed its own test. The worker itself uses
`cloud.google.com/go/storage`, not raw HTTP, for the same reason.

## Running

```sh
cloudburrow up --name e2e --port-control 0 --port-storage 0 --port-pubsub 0 --port-run 0 &

export CLOUDBURROW_TEST_STORAGE=http://127.0.0.1:<storage port>
export CLOUDBURROW_TEST_PUBSUB=127.0.0.1:<pubsub port>
export CLOUDBURROW_TEST_STORAGE_INCLUSTER=storage.cloudburrow.svc.cluster.local:4443
export CLOUDBURROW_TEST_KUBECONFIG=~/.cloudburrow/e2e/kubeconfig
export CLOUDBURROW_TEST_CLUSTER=cloudburrow-e2e

make test-e2e
```

## Why two storage addresses

The host reads the result at `127.0.0.1:<port>`. The worker, running **inside** the cluster,
must use `storage.cloudburrow.svc.cluster.local:4443` — a pod's loopback is the pod itself, so
the host address is unreachable from it.

Handing a workload the host address is the most common way this goes wrong, which is why the
test passes both forms explicitly rather than deriving one from the other.

## Why `dev.local/`

The worker image is built locally and loaded straight into the cluster. Knative resolves image
tags to digests **by contacting the registry**, so a local image must carry a prefix Knative
skips (`dev.local/`) and must never be pulled (`imagePullPolicy: Never`).
