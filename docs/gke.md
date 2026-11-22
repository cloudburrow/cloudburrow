# Kubernetes Engine API (`container.googleapis.com` v1)

`--services gke` (#1913) serves the Kubernetes Engine API in the CLI process, gRPC and JSON on `--port-gke` (default `9030`). Each cluster it creates is a **real local Kubernetes cluster**: a kind cluster of its own, made the way the instance's own cluster is. Compatibility status, method by method, is in [compatibility.md](compatibility.md#kubernetes-engine-api--googlecontainerv1).

## Why kind, not k3d

The instance already runs on kind and pins its node image (`dependencies.json`), so a Kubernetes Engine cluster is the same Kubernetes, made by the same tool, with nothing new to download. kind only makes nodes when it creates a cluster, so node pools are added afterwards the way kind adds its own workers: a container of the pinned node image on kind's Docker network, with kind's labels (so `kind delete cluster` removes it with the rest), which runs `kubeadm join` with a fresh bootstrap token. k3d's `node create` would have done this natively, but at the cost of a second Kubernetes distribution (k3s) and image to pin, whose versions would not be the instance's.

## What a cluster is

| Kubernetes Engine | Locally |
|---|---|
| Standard cluster | kind control plane, tainted `node-role.kubernetes.io/control-plane:NoSchedule`, plus one kind node per node pool node, labelled `cloud.google.com/gke-nodepool=POOL` |
| Autopilot cluster | one schedulable kind node; node pools are Google's to manage, so their changes are refused |
| Regional cluster | three zones (`REGION-a`, `-b`, `-c`): `initial_node_count` nodes per zone |
| Zones (`locations`, `--node-locations`) | each node is labelled `topology.kubernetes.io/zone` and `region` when it joins; a new node goes to the pool's zone with the fewest nodes and keeps it; removing a zone (`SetLocations`, `UpdateNodePool` `locations`) deletes that zone's nodes; a pool may have zones of its own in the cluster's region |
| Node labels and taints (`nodeConfig.labels`, `taints`, `--node-labels`, `--node-taints`) | set on every node of the pool, which registers with them; an update replaces them on the nodes it has |
| Machine type, boot disk (`machineType`, `diskType`, `diskSizeGb`, `--machine-type`) | recorded, and each node labelled `node.kubernetes.io/instance-type` and `cloud.google.com/gke-boot-disk`; a node has this machine's CPU and memory; a change re-creates the nodes with the pool's upgrade strategy |
| Kubelet configuration (`kubeletConfig`, `--system-config-from-file`) | the nodes' kubelets run it (their `/configz` shows it); a change re-creates the nodes with the pool's upgrade strategy, as GKE's standard strategy does |
| `endpoint` | `127.0.0.1:PORT`, the kind API server (Google's is a bare IP on 443) |
| `masterAuth.clusterCaCertificate` | kind's cluster CA |
| `masterAuth.clientCertificate`/`clientKey` | kind's admin certificate, only with `clientCertificateConfig.issueClientCertificate` (`--issue-client-certificate`) |
| Versions | one per pinned kind node image: `1.36.4-gke.0` for `kindest/node:v1.36.4` (the default, the instance's), and `1.35.8-gke.0` for `kindest/node:v1.35.8`; every release channel offers both, the newest its default and upgrade target |

Limits of this machine, not Google's: at most 9 nodes per cluster and 5 clusters per instance, and **below 20 GiB of free disk** a create or a node pool growth answers `RESOURCE_EXHAUSTED` (set `CLOUDBURROW_GKE_MIN_FREE_GB` on `up` to change it).

## Connecting with gcloud and kubectl

`cloudburrow gcloud-setup` writes the `container` override. gcloud's default kubeconfig entry runs `gke-gcloud-auth-plugin`, which mints Google tokens the local API server cannot accept, so create the cluster with a client certificate and tell gcloud to use it:

```sh
export CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true
gcloud container clusters create demo --zone us-central1-a --num-nodes 1 --issue-client-certificate
gcloud container clusters get-credentials demo --zone us-central1-a
kubectl get nodes
```

## Upgrades

As Google's [upgrading a cluster](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster) has it (#2069): the control plane first (`UpdateMaster`, or `UpdateCluster` `desired_master_version`; operation `UPGRADE_MASTER`, the cluster `RECONCILING`), one minor version at a time, then each node pool (`UpdateNodePool` `node_version`, or `UpdateCluster` `desired_node_version` with `desired_node_pool_id`, required with more than one pool; `-` is the control plane's version; operation `UPGRADE_NODES`, with `progress` metrics `NODES_TOTAL` and `NODES_DONE` and a `detail` line). A node pool is never newer than the control plane, nor more than two minor versions older; the control plane is never downgraded.

kind cannot move a cluster to another node image, so the upgrade is done the way kubeadm upgrades a cluster in place, which keeps etcd's state as a GKE control plane upgrade does:

1. **Control plane.** A helper container of the newer node image, with no network, runs only containerd; its control-plane images (kube-apiserver, kube-controller-manager, kube-scheduler, kube-proxy, etcd, CoreDNS, pause) are exported, and its `kubeadm`, `kubelet` and `kubectl` copied out, into the control plane's node. `kubeadm upgrade apply vX.Y.Z` rewrites the static pods (etcd's data kept), then the control plane's kubelet is replaced and restarted. Nothing is downloaded. The API server is briefly unavailable, as a zonal cluster's is on Google; workloads keep running.
2. **Nodes.** Nodes are replaced, never changed in place, as GKE replaces VMs, per the pool's `upgradeSettings` ([node pool upgrade strategies](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pool-upgrade-strategies)):
   - **Surge** (the default, `maxSurge: 1`, `maxUnavailable: 0`): per zone, `maxSurge + maxUnavailable` nodes at a time; `maxSurge` new nodes of the newer image join and are Ready before as many old ones are cordoned, drained and deleted; the rest of the batch is drained, deleted and recreated. Both 0, or a sum above 100, is `INVALID_ARGUMENT`.
   - **Blue-green**: the green nodes join (`CREATING_GREEN_POOL`), the blue ones are cordoned (`CORDONING_BLUE_POOL`), drained in batches of `batchNodeCount` or `batchPercentage` with `batchSoakDuration` between (`DRAINING_BLUE_POOL`), the pool soaks `nodePoolSoakDuration` (default one hour, at most seven days in all; `NODE_POOL_SOAKING`; `CompleteNodePoolUpgrade` ends it), then the blue nodes are deleted (`DELETING_BLUE_POOL`). The phase is in the pool's `updateInfo.blueGreenInfo`.

   A drain goes through the eviction API, so PodDisruptionBudgets hold, for up to an hour; the pods still there are then deleted, as Google evicts them. A new node is given the images loaded onto the node it replaces (`kind load`), since a kind node pulls nothing its predecessor had been given.

**Cancel, resume and roll back** (#2076; [upgrading a cluster](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster), [`gcloud container node-pools rollback`](https://docs.cloud.google.com/sdk/gcloud/reference/container/node-pools/rollback)):

- `CancelOperation` on a node pool upgrade pauses it: the operation is `ABORTING` until the nodes that had started complete, then `DONE` with `CANCELLED`; nodes not started keep the old version, and the pool its version, so it may run two. A blue-green upgrade stays in its phase (its `updateInfo` kept); cancelling one in `DELETING_BLUE_POOL` is `FAILED_PRECONDITION`, as that phase "can't be paused". Upgrading again to the same version resumes it, a blue-green one keeping the green nodes it made.
- `RollbackNodePoolUpgrade` (`UPGRADE_NODES`) after a cancelled, failed or paused upgrade recreates the nodes already upgraded on the pool's version, one at a time (the new node Ready before the old is drained); a pool with nothing to roll back, a successfully upgraded one among them, is `FAILED_PRECONDITION`, and so is a running surge upgrade (cancel it first). A blue-green upgrade can be rolled back while it runs, in any phase before `DELETING_BLUE_POOL`: it stops, the pool's phase is `ROLLBACK_STARTED`, the blue nodes are uncordoned and the green ones drained and deleted. `respect_pdb` drains through the eviction API; without it (the default) the pods are deleted.
- A pool made on an older version than the control plane (`nodePools[].version` at create) runs that version's nodes.

**Maintenance windows and exclusions** ([docs](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/maintenance-windows-and-exclusions)): `maintenancePolicy` at create and `SetMaintenancePolicy` (with `resourceVersion`; a stale one is `FAILED_PRECONDITION`) take a daily window (four hours from `startTime`, UTC), a recurring window (`FREQ=DAILY` or `FREQ=WEEKLY;BYDAY=...`) and up to 20 exclusions, at most three of them "no upgrades". Manual upgrades start at once and ignore both, as documented. Automatic upgrades, checked every minute, move a cluster whose control plane is older than its channel's target (the newest version), then each pool with `management.autoUpgrade` that is older than the control plane, only inside the window (any time without one) and outside every exclusion whose scope covers them: `NO_UPGRADES` all, `NO_MINOR_UPGRADES` minor ones, `NO_MINOR_OR_NODE_UPGRADES` minor ones and every node upgrade. An automatic surge upgrade that runs past the window's end is paused before its next batch, as Google documents ("If tasks run beyond the maintenance window, GKE attempts to pause the tasks"): its operation is `DONE` with no error and a `statusMessage` saying so, the pool keeps its version and says the same, and the next window resumes it. A blue-green upgrade "continue[s] until completion" past a window, as documented.

The windows are hours or days away, so the Kubernetes Engine clock can be moved forward with a CloudBurrow extension (Google has none): `POST /cloudburrow/v1/clock:advance` with `{"duration": "3600s"}` on the API's port, never back, answers `{"now", "advancedBy"}` and starts the automatic upgrades now due; the clusters list's **Advance Kubernetes Engine clock (CloudBurrow extension)** does the same. The clock is the instance's, kept in memory.

```sh
gcloud container clusters create demo --zone us-central1-a --num-nodes 1 --issue-client-certificate --cluster-version 1.35
gcloud container clusters upgrade demo --zone us-central1-a --master --cluster-version 1.36
gcloud container clusters upgrade demo --zone us-central1-a --node-pool default-pool
gcloud container operations cancel OPERATION_ID --zone us-central1-a
gcloud container node-pools rollback default-pool --cluster demo --zone us-central1-a --respect-pdb=true
```

## Terraform

`cloudburrow terraform` gives the provider `container_custom_endpoint`, and `compute_custom_endpoint` at the same port: the provider reads a node pool's size from its Compute Engine instance group managers, which this port answers for its own pools (every other Compute Engine call is 501). Google's recommended layout, a `google_container_cluster` with `remove_default_node_pool = true` and a separately managed `google_container_node_pool`, applies, plans clean and updates labels and `node_count` in place (`TestTerraformContainerClusterAndNodePool`). Set `deletion_protection = false` to destroy, as on Google.

## Console

Kubernetes clusters lists the project's clusters beside the instance's own. **Create cluster** makes an Autopilot or Standard cluster; a cluster's page has **Edit**, **Upgrade** (to a newer served version; each node pool's **Upgrade** up to the control plane's, and **Complete upgrade** while a blue-green upgrade soaks, and **Roll back**, with Respect pod disruption budgets, after a cancelled, failed or paused upgrade or during a blue-green one, #2076), **Edit maintenance** (the maintenance window and exclusions) and **Delete**, the Node pools tab **Add node pool** and, on each pool, **Resize**, **Edit** and **Delete** (#1925), and the Operations tab each operation, a running one with **Cancel** (a cancelled one shows Cancelled); the clusters list has **Advance Kubernetes Engine clock (CloudBurrow extension)**. The instance's own cluster's Workloads list has row checkboxes and one **Delete** for the checked workloads, confirmed with a click as Google's is (a Kubernetes system object checked refuses the selection with the reason); a workload's Overview tab charts CPU, Memory and Disk, its managed pods' kubelet readings summed; and Object Browser filters by API group and kind with a checkbox tree (#2070).

## Lifetime

An ephemeral instance deletes its clusters when it stops; a persistent one keeps their records (`gke.json` in the instance directory) and `cloudburrow delete` deletes the kind clusters with the instance. `cloudburrow reset` deletes them too. Kubeconfigs are in the instance directory's `gke/`.

## Not yet

A local node has this machine's CPU and memory whatever its pool's machine type (a machine type change re-creates the nodes, labelled with the new type). The remaining differences (a rollback-safe upgrade cannot go back a minor version, the support end dates, monitoring components other than system components, the log forwarder running in the CLI) are listed in [docs/compatibility.md](compatibility.md#kubernetes-engine-api--googlecontainerv1).
