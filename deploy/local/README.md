# Local reference topology

The manifests that were stood up and verified for [ADR-0005](../../docs/adr/0005-kubernetes-foundation-and-upstream-reuse.md).
They are the **reference topology**, not a product feature: `cloudburrow up` replaces them in
#26–#28. They are committed so the verification is reproducible rather than a claim.

See [docs/local-verification.md](../../docs/local-verification.md) for the exact commands and
measured results.

| File | Purpose |
|---|---|
| `kind-cluster.yaml` | Single-node kind cluster at the pinned Kubernetes version, with the Kourier nodePort mapped to the host |
| `backends.yaml` | `cloudburrow` namespace, Pub/Sub emulator, fake-gcs-server with a PVC, and their Services |

Everything carries `cloudburrow.dev/owned: "true"` so cleanup can never touch resources
CloudBurrow did not create.
