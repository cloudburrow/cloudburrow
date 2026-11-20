# Compute Engine (`compute.googleapis.com` v1)

`--services compute` (#1936) serves the VPC networking part of the Compute Engine API in the CLI process: networks (auto and custom mode), subnetworks with their secondary ranges, firewall rules, routes, global and regional addresses, the global, regional and zonal operations, and the regions and zones. It is JSON under `/compute/v1/` on `--port-compute` (default `9031`), as Google's paths are, plus the batch endpoint `/batch/compute/v1` that gcloud sends its calls to. Compatibility status, method by method, is in [compatibility.md](compatibility.md#compute-engine-vpc-networking--computegoogleapiscom-v1).

VM instances, disks, snapshots, machine types and the public images are served too (#1963); see [VM instances](#vm-instances) below.

## What a network is here

Nothing carries traffic. A network, its subnets, its firewall rules and its routes are records, validated as Google validates them, that the other services check their own network fields against:

| Service | With `compute` | Without it |
|---|---|---|
| Memorystore for Redis | `authorized_network` must exist; a `reserved_ip_range` must not overlap a subnet or an allocated range of it (a range chosen for the instance is moved off them); with `PRIVATE_SERVICE_ACCESS`, a range name must be an allocated range of the network | any network and range |
| Redis Cluster, Memorystore for Valkey | each PSC network must exist | any network |
| Kubernetes Engine | the cluster's network must exist, and its subnetwork be that network's in the cluster's region | any network |
| Cloud SQL | `settings.ipConfiguration.privateNetwork` must exist, and `allocatedIpRange` be one of its allocated ranges | any network |

Every project starts with its `default` network, as a project with the Compute Engine API enabled does: auto mode, one `/20` subnet per region (the documented auto mode ranges, `10.128.0.0/20` in `us-central1`), the `default-allow-internal`, `-ssh`, `-rdp` and `-icmp` firewall rules, and the default route to the internet. Delete it and it is not made again.

Every insert, patch and delete completes at once and answers with a `DONE` operation (`compute#operation`), global, regional or zonal as Google scopes it; the clients' `Wait` returns at once.

External addresses are chosen from documentation ranges (`203.0.113.0/24` regional, `198.51.100.0/24` global), since no address here is reachable from the internet. Internal addresses are chosen from their subnet's primary range, skipping the four addresses Google reserves.

## Ports

Compute Engine is an API of its own, so it has a port of its own rather than sharing the Kubernetes Engine API's. The Kubernetes Engine port has long answered the one Compute call Terraform's `google_container_node_pool` makes (`instanceGroupManagers.get`, for a pool's size). With `compute` enabled, the Compute port answers it too, from the same node pools, and `cloudburrow terraform` points `compute_custom_endpoint` at the Compute port; without `compute`, nothing changes.

## Clients

```sh
cloudburrow up --services compute
eval "$(cloudburrow env)"            # CLOUDBURROW_COMPUTE_ENDPOINT, CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE
gcloud compute networks create vpc --subnet-mode custom
gcloud compute networks subnets create nodes --network vpc --region us-central1 --range 10.0.0.0/24 \
    --secondary-range pods=10.4.0.0/16
gcloud compute addresses create db --region us-central1 --subnet nodes
```

The Go client takes the endpoint with its scheme:

```go
c, err := compute.NewNetworksRESTClient(ctx,
    option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_COMPUTE_ENDPOINT")),
    option.WithoutAuthentication())
```

Terraform's `google_compute_network` and `google_compute_subnetwork` run through `cloudburrow terraform`.

## Console

The console's VPC network pages list and manage networks, subnets, firewall rules, routes and IP addresses, as Google's VPC network section does.

Compute Engine's pages (#1965), under **Compute** in the drawer, at Google's paths:

| Page | What it offers |
|---|---|
| VM instances (`/compute/instances`) | Every instance of the project across zones (the aggregated list): status, zone, machine type, internal and external IP, Connect (the forwarded SSH port). Each row has **SSH**, which answers with the `gcloud compute ssh` and `ssh` commands for its port, and a menu with Start / Resume, Stop, Suspend and Reset as its status allows, and Delete. **Create instance** opens Google's *Create an instance* page: name, labels, region and zone, machine type, boot disk image, type and size, network and subnetwork, external IPv4 address, network tags, service account and access scopes, startup script, metadata, deletion protection |
| An instance's page | Details (basic information, machine configuration, networking, metadata, service account, Connect), Storage, Networking and Serial port 1 (console) tabs; Edit (labels, network tags, the machine type while stopped, deletion protection, metadata); Attach existing disk, Detach disk, and the row's actions |
| Disks (`/compute/disks`) | Every disk across zones with its type, size, zone and user; **Create disk** (blank, from an image or from a snapshot); Create snapshot, Edit size, Edit labels and Delete (unavailable while attached) |
| Snapshots (`/compute/snapshots`) | Each snapshot's status, location, sizes, source disk and creation time; Create snapshot, Create disk, Edit labels, Delete |
| Images (`/compute/images`) | The public images served, and the image projects refused with their reasons |
| Machine types (`/compute/machineTypes`) | The predefined types with their vCPUs and memory (Google's console has no page of its own for them; its Create an instance page lists them) |
| Metadata (`/compute/metadata`) | The project's common instance metadata, `ssh-keys` included, with Edit |

A change returns as soon as the API has started its operation, as Google's pages do: the list shows `PROVISIONING`, `STOPPING` and the like until the operation is done.

## VM instances

The design (#1963, option A): **a VM is a systemd container** in the instance's kind cluster, a privileged pod booting `/sbin/init`, and the API (instances, disks, snapshots, images, machine types, zonal operations) sits on a backend interface (`compute.VMBackend`) so a microVM backend can be added later on Linux hosts with `/dev/kvm`. A container has no kernel of its own, so what needs one is refused or documented, never faked.

| Google | Here |
|---|---|
| Public images `debian-12`, `ubuntu-2404-lts` (`-amd64`, `-arm64`), `rocky-linux-9` | A container image of the same distribution, built on this machine at first use from the official image pinned by digest, plus the distribution's systemd, OpenSSH, sudo and curl packages and the guest scripts in `internal/service/compute/guest` (no code of them is linked into CloudBurrow). Tagged `cloudburrow-vm-FAMILY:HASH` |
| `cos-stable`, Windows, Fedora CoreOS, licensed images, custom images | 501, with the reason |
| Machine types | The predefined e2, f1/g1, n1, n2, n2d and c3 types; the container's CPU and memory limits are the type's |
| Boot disk | A PersistentVolumeClaim holding the guest's `/etc`, `/usr`, `/var`, `/home`, `/root`, `/opt` and `/srv`, seeded from the image at first boot: what the guest changes survives stop and start |
| Attached disk | A PersistentVolumeClaim mounted at `/mnt/disks/DEVICE_NAME`: a directory, **not a block device** (no `mkfs`, no `/dev/disk/by-id/google-*`). Attaching or detaching on a running instance restarts its container |
| Disk sizes | Checked as Google documents; kind's local-path storage neither enforces nor grows them |
| Snapshot | A copy of the disk's files in a PersistentVolumeClaim of its own; a disk can be made from it |
| Internal IP | Reserved in the instance's subnet as Google picks it, and added to the guest's `eth0` beside the pod's own address; it is not routed between VMs (they reach each other only by pod IP), and firewall rules are not enforced: both are #1968, which enforces rules as NetworkPolicies per network |
| External IP | `127.0.0.1`: the guest's port 22 is forwarded to a port on this machine, named in the instance's JSON as `cloudburrowSshAddress` |
| `startup-script` | Runs at every boot (`google-startup-scripts.service`), its output on the serial port as `google_metadata_script_runner` writes it |
| `ssh-keys` | The guest agent's account manager (a script polling every 2 seconds) makes each user, its `authorized_keys` and passwordless sudo (`google-sudoers`); project keys apply unless `block-project-ssh-keys` |
| Metadata server | The guest reads its instance and project attributes from the Compute port under a per-instance token (`CLOUDBURROW_GUEST_METADATA`); service account tokens come from the instance's metadata server, which `GCE_METADATA_HOST` names in the guest (in `/etc/environment` too). `metadata.google.internal` is not resolved in the guest |
| Serial port output | The container's console (`kubectl logs`), escape codes removed |
| stop / start, reset | The pod is deleted (the disks kept) and created again |
| suspend / resume | The container's cgroup is frozen on its kind node: memory and processes are kept |
| Disk budget | A new disk, instance or snapshot is refused with `RESOURCE_EXHAUSTED` (429) below 20 GiB free, as Kubernetes Engine refuses a node |

Every mutation answers a zonal operation (global for snapshots) that is `RUNNING` while the backend works and `DONE` (with its error when it failed) after; `zoneOperations.wait` waits for it.

With the Go client (`cloud.google.com/go/compute/apiv1`), as `TestComputeVMsThroughTheGoClient` does: `InstancesClient.Insert` with a boot disk from `projects/debian-cloud/global/images/family/debian-12` and a `startup-script` item, `Operation.Wait`, then `GetSerialPortOutput`.

With gcloud (`cloudburrow gcloud-setup` points `compute` at the instance), as `TestGcloudComputeInstances` does:

```sh
gcloud compute instances create vm-1 --zone us-central1-a --machine-type e2-small \
  --image-family debian-12 --image-project debian-cloud \
  --metadata startup-script='#!/bin/bash
echo hello'
gcloud compute instances list
gcloud compute instances get-serial-port-output vm-1 --zone us-central1-a
gcloud compute instances add-metadata vm-1 --zone us-central1-a --metadata team=burrow
gcloud compute disks create data-1 --zone us-central1-a --size 10GB
gcloud compute instances attach-disk vm-1 --zone us-central1-a --disk data-1 --device-name data
gcloud compute instances stop vm-1 --zone us-central1-a
gcloud compute instances start vm-1 --zone us-central1-a
gcloud compute instances delete vm-1 --zone us-central1-a
```

`--image-family` sends the zonal image family view (`projects/debian-cloud/zones/-/imageFamilyViews/debian-12`) as the boot disk's source image, which is accepted as the family.

### SSH

The guest's port 22 is forwarded to a loopback port, named in `instances.get` and the aggregated list as `cloudburrowSshAddress` (and in the console's Connect column). The instance's `natIP` is `127.0.0.1`.

**gcloud compute ssh always dials port 22 of the natIP**: it has no port flag of its own, and the port cannot be put in the instance's address. Pass the forwarded port to its `ssh` with `--ssh-flag`:

```sh
PORT=$(curl -s http://127.0.0.1:COMPUTE_PORT/compute/v1/projects/PROJECT/zones/us-central1-a/instances/vm-1 | jq -r .cloudburrowSshAddress | cut -d: -f2)
gcloud compute ssh vm-1 --zone us-central1-a --ssh-flag="-p $PORT" --strict-host-key-checking=no
```

gcloud makes its key (`~/.ssh/google_compute_engine`, or `--ssh-key-file`) and adds it to the project's `ssh-keys` metadata (`projects.setCommonInstanceMetadata`); the guest applies it within seconds. `--strict-host-key-checking=no` is needed because the host key gcloud records is per instance ID, and guest attributes (`hostkeys/`) are not served. `TestGcloudComputeInstances` runs exactly this, with `HOME` and the key in throwaway directories. `--internal-ip`, `--tunnel-through-iap` and OS Login are not served.

Plain `ssh` works the same with any key in `ssh-keys`:

```sh
ssh -p PORT -i KEY USER@127.0.0.1
```

### Terraform

`google_compute_instance`, `google_compute_disk` and `google_compute_attached_disk` apply, plan clean and destroy through `cloudburrow terraform` (`TestTerraformComputeInstance`). With `google_compute_attached_disk`, put `lifecycle { ignore_changes = [attached_disk] }` on the instance, as Google's provider documentation says.

Clean up with `instances delete` (the boot disk is deleted with it); a stopped CloudBurrow keeps the pods, and `cloudburrow delete` removes the cluster with them. The images it built stay in Docker until removed (`docker images 'cloudburrow-vm-*'`).
