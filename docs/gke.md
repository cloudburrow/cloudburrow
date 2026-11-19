# Kubernetes Engine API (`container.googleapis.com` v1)

`--services gke` (#1913) serves the Kubernetes Engine API in the CLI process, gRPC and JSON on `--port-gke` (default `9029`). Each cluster it creates is a **real local Kubernetes cluster**: a kind cluster of its own, made the way the instance's own cluster is. Compatibility status, method by method, is in [compatibility.md](compatibility.md#kubernetes-engine-api--googlecontainerv1).

## Why kind, not k3d

The instance already runs on kind and pins its node image (`dependencies.json`), so a Kubernetes Engine cluster is the same Kubernetes, made by the same tool, with nothing new to download. kind only makes nodes when it creates a cluster, so node pools are added afterwards the way kind adds its own workers: a container of the pinned node image on kind's Docker network, with kind's labels (so `kind delete cluster` removes it with the rest), which runs `kubeadm join` with a fresh bootstrap token. k3d's `node create` would have done this natively, but at the cost of a second Kubernetes distribution (k3s) and image to pin, whose versions would not be the instance's.

## What a cluster is

| Kubernetes Engine | Locally |
|---|---|
| Standard cluster | kind control plane, tainted `node-role.kubernetes.io/control-plane:NoSchedule`, plus one kind node per node pool node, labelled `cloud.google.com/gke-nodepool=POOL` |
| Autopilot cluster | one schedulable kind node; node pools are Google's to manage, so their changes are refused |
| Regional cluster | three zones (`REGION-a`, `-b`, `-c`): `initial_node_count` nodes per zone |
| `endpoint` | `127.0.0.1:PORT`, the kind API server (Google's is a bare IP on 443) |
| `masterAuth.clusterCaCertificate` | kind's cluster CA |
| `masterAuth.clientCertificate`/`clientKey` | kind's admin certificate, only with `clientCertificateConfig.issueClientCertificate` (`--issue-client-certificate`) |
| Versions | one per kind node image: `1.36.4-gke.0` for `kindest/node:v1.36.4`; every release channel offers it |

Limits of this machine, not Google's: at most 9 nodes per cluster and 5 clusters per instance, and **below 20 GiB of free disk** a create or a node pool growth answers `RESOURCE_EXHAUSTED` (set `CLOUDBURROW_GKE_MIN_FREE_GB` on `up` to change it).

## Connecting with gcloud and kubectl

`cloudburrow gcloud-setup` writes the `container` override. gcloud's default kubeconfig entry runs `gke-gcloud-auth-plugin`, which mints Google tokens the local API server cannot accept, so create the cluster with a client certificate and tell gcloud to use it:

```sh
export CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true
gcloud container clusters create demo --zone us-central1-a --num-nodes 1 --issue-client-certificate
gcloud container clusters get-credentials demo --zone us-central1-a
kubectl get nodes
```

## Lifetime

An ephemeral instance deletes its clusters when it stops; a persistent one keeps their records (`gke.json` in the instance directory) and `cloudburrow delete` deletes the kind clusters with the instance. `cloudburrow reset` deletes them too. Kubeconfigs are in the instance directory's `gke/`.

## Not yet

Node pool autoscaling, choosing zones, upgrades between versions (only one is served), IP rotation, maintenance and network policies answer 501. Terraform's `google_container_cluster` is pointed at this API (`container_custom_endpoint`) but no test drives it. The console's node pool actions are #1925.
