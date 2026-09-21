# Native Kubernetes portability and Knative behaviour

Two dimensions the support matrix tracks **separately from GCP API compatibility**, because a
cluster that answers GCP calls is not automatically a cluster your manifests run on, and
inferring one from the other is how a tool over-promises.

## Running

```sh
cloudburrow up --name k8s --port-control 0 &
export CLOUDBURROW_TEST_KUBECONFIG=~/.cloudburrow/k8s/kubeconfig

make test-integration
```

Helm tests skip when `helm` is not installed, rather than pretending the dimension is covered.

## Portability (#29)

Ordinary manifests applied with `kubectl`, never through a Google API: Namespace, ConfigMap,
Secret, a multi-replica Deployment, Service, Jobs, a PVC, and a CustomResourceDefinition with
an instance of it. Plus a Helm chart install.

The PVC test writes from one pod and reads from **another** — that is what proves the volume
persisted, rather than the data merely living in one container.

## Knative scaling (#30)

Knative is the closest available model for Cloud Run, **not an equivalent one**, so these
measure and record rather than assume a match:

| Behaviour | Result |
|---|---|
| `min-scale: 1` keeps an instance warm | Verified |
| Scale to zero without traffic | Verified, on Knative's schedule |
| `max-scale` reaches the revision | Verified |
| The cap enforced **under load** | **Not tested** |

The last one is deliberate. Proving a ceiling needs sustained concurrency that would make this
suite slow and flaky, and a half-measure would be worse than an honest gap. `compatibility.md`
says `Partial` for that row and states why.

Cloud Run also scales to zero, but on its own schedule. The timing is **not** claimed to match.
