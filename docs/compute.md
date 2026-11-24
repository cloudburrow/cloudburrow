# Compute Engine (`compute.googleapis.com` v1)

`--services compute` (#1936) serves the VPC networking part of the Compute Engine API in the CLI process: networks (auto and custom mode), subnetworks with their secondary ranges, firewall rules, routes, global and regional addresses, the global, regional and zonal operations, and the regions and zones. It is JSON under `/compute/v1/` on `--port-compute` (default `9031`), as Google's paths are, plus the batch endpoint `/batch/compute/v1` that gcloud sends its calls to. Compatibility status, method by method, is in [compatibility.md](compatibility.md#compute-engine-vpc-networking--computegoogleapiscom-v1).

VM instances, disks, snapshots, machine types and the public images are served too (#1963); see [VM instances](#vm-instances) below.

## What a network is here

A network, its subnets, its firewall rules and its routes are records, validated as Google validates them, that the other services check their own network fields against. Between VM instances they also carry traffic, with the firewall rules enforced: see [VM networking and firewall rules](#vm-networking-and-firewall-rules). For the other services' resources nothing is routed:

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
| Internal IP | Reserved in the instance's subnet as Google picks it, on the guest's `vpc0`, and routed to the other VMs of its network, with the network's firewall rules enforced: see [VM networking and firewall rules](#vm-networking-and-firewall-rules) |
| External IP | `127.0.0.1`: the guest's port 22 is forwarded to a port on this machine, named in the instance's JSON as `cloudburrowSshAddress` |
| `startup-script` | Runs at every boot (`google-startup-scripts.service`), its output on the serial port as `google_metadata_script_runner` writes it |
| `ssh-keys` | The guest agent's account manager (a script polling every 2 seconds) makes each user, its `authorized_keys` and passwordless sudo (`google-sudoers`); project keys apply unless `block-project-ssh-keys` |
| Metadata server | The guest reads its instance and project attributes from the Compute port under a per-instance token (`CLOUDBURROW_GUEST_METADATA`); service account tokens come from the instance's metadata server, which `GCE_METADATA_HOST` names in the guest (in `/etc/environment` too). `metadata.google.internal` is not resolved in the guest |
| Serial port output | The container's console (`kubectl logs`), escape codes removed |
| stop / start, reset | The pod is deleted (the disks kept) and created again |
| suspend / resume | The container's cgroup is frozen on its kind node: memory and processes are kept |
| Disk budget | A new disk, instance or snapshot is refused with `RESOURCE_EXHAUSTED` (429) below 20 GiB free, as Kubernetes Engine refuses a node |

### Regional disks and Asynchronous Replication (#2206)

A regional disk (`regionDisks`, `disks create --region --replica-zones`) has two replica zones, distinct and in its region, and one PersistentVolumeClaim behind it: a VM in either replica zone attaches the same files, so what a VM in one zone wrote is what a VM in the other reads. Google's failover is `instances.attachDisk` with `forceAttach=true` (gcloud `--force-attach`) from a VM in the other zone while the disk is still attached read-write: here the new VM attaches it and the old VM is restarted with the disk mounted read-only, as a forced attach leaves it unable to write. Types and sizes are Google's for regional disks (`pd-standard` from 200 GB, `pd-balanced` and `pd-ssd` from 10 GB, `hyperdisk-balanced-high-availability` from 4 GB; `regionDiskTypes`), attached (Persistent Disk) to E2, N1, N2 and N2D machine types, read-only to at most 10 VMs.

Asynchronous Replication pairs a primary disk with a blank secondary of its size made with `asyncPrimaryDisk`, in another region of the same continent. `startAsyncReplication` copies the primary to the secondary, and the secondary is copied again before a clone by `bulkInsert` reads it and when `stopAsyncReplication` or `stopGroupAsyncReplication` stops the pair, so a reader never sees older data than Google's one-minute RPO allows. A secondary is not attached or snapshotted while it replicates, a stopped pair does not restart, and replication must stop before either disk is deleted. Disk consistency groups (`resourcePolicies` with `diskConsistencyGroupPolicy`) group up to 128 disks of one region, primaries or secondaries but not both; other resource policy kinds are 501.

Hyperdisk Balanced High Availability takes Google's documented limits (https://docs.cloud.google.com/compute/docs/disks/hd-types/hyperdisk-balanced-ha, read 2026-11-24): 4 GiB to 64 TiB; IOPS 3,000 to 100,000 at most 500 per GiB (4 GiB fixed at 2,000, 5 GiB at 2,500); throughput at least max(140, IOPS/256) and at most min(2,400, IOPS/4) MiB/s; the documented defaults when unset; IOPS or throughput only with a size. It attaches to the A3, A5X, C3, C3D, C4, C4A, C4D, C4N, G4, M3, M4N, N4, N4A, N4D, X5, Z3, Z4D and Z4M series (of the served machine types, C3); C2D, E2, N1, N2 and N2D need Google's account team, so they are refused. Its access mode is `READ_WRITE_SINGLE` (default) or `READ_WRITE_MANY`; multi-writer attaches it read-write to up to 8 C3, C3D, C4, C4A, M3, N4, N4A, N4D or Z3 instances (one volume: every writer sees the same files), never as a boot disk, auto-deleted or made with the instance, and the mode changes only while the disk is detached.

`disks.update` and `regionDisks.update` (gcloud `compute disks update`) change the fields their update mask (`paths` or `updateMask`) names: `sizeGb` (grow only), `provisionedIops` and `provisionedThroughput` (Hyperdisk Balanced High Availability, within its limits), `accessMode` and `architecture`. A volume's size changes at most twice and its performance once in 4 hours, as Google documents. The update mask also takes `licenses` and `licenseCodes` (only the documented eligible OS licenses, RHEL, SLES, Ubuntu, Ubuntu Pro, Windows Server and SQL Server, are added or removed, and not while a running instance uses the disk), `userLicenses` (a beta field, set over REST) and `guestOsFeatures` (#2206).

### Customer-managed encryption keys and snapshot schedules (#2206)

With `--services compute,kms`, `diskEncryptionKey.kmsKeyName` (gcloud `--kms-key`, Terraform `disk_encryption_key.kms_key_self_link`, the console's Create a disk **Encryption**) names a CryptoKey of this instance's Cloud KMS in the disk's region, a multi-region containing it, or `global`. The disk gets a 256-bit data key wrapped by the key's primary version, which `kmsKeyName` answers (`…/cryptoKeyVersions/N`). `disks.updateKmsKey` and `regionDisks.updateKmsKey` (gcloud `compute disks update-kms-key`, the console's **Update KMS key**) rotate to the primary version or change the key. While the version that wrapped the data key is disabled or destroyed, the disk is not attached, snapshotted or cloned, its instance does not start, and the rotation is refused; enabled again, all of it works again.

**Encryption at rest.** A disk with a customer-managed or customer-supplied key is encrypted with its data key whenever no running VM has it open: its volume then holds only `.cloudburrow-sealed`, its files as a tar stream sealed with AES-256-GCM (64 KiB chunks, each authenticated, the last one marked so a truncated file is refused), made in the CloudBurrow process with Go's standard library from what a helper pod of the Debian image reads with `tar`, so the data key never reaches the cluster and the plaintext never touches this machine's disk. A VM's start (or attach to a running VM) unwraps the data key and opens the volume; its stop (or detach) seals it again; a snapshot is sealed with the disk's key, and a clone or a disk from a snapshot is re-sealed with its own. While a VM runs, the volume it has open holds its files in the clear, as a dm-crypt volume is open to its guest: the VM runtime is a container whose disks are directories, with no block device to put dm-crypt under (`TestComputeDiskEncryptionAndInstancePoliciesThroughTheGoClient` reads the stopped VM's volume on the node).

**Customer-supplied keys** (CSEK, which Google marks deprecated and still serves): `diskEncryptionKey.rawKey` (a 256-bit key, base64) or `rsaEncryptedKey` (the key RSA-OAEP-wrapped, SHA-1, with the certificate the Compute port serves at `/cloudburrow/compute/csek-ingress.pem`, CloudBurrow's own in place of Google's `google-cloud-csek-ingress.pem`) wraps the disk's data key; the disk answers only `sha256`. The key is given again to attach the disk, to start its VM (`instances.startWithEncryptionKey`; plain `start` is refused), to snapshot it (`sourceDiskEncryptionKey`, with an optional `snapshotEncryptionKey`) and to make a disk from such a snapshot (`sourceSnapshotEncryptionKey`); a wrong key is refused. Its VM can't be suspended, and `updateKmsKey` refuses it. The console's Create a disk offers **Customer-supplied encryption key**, and a stopped VM with such a disk offers **Start with encryption key**, which asks for each disk's key.

### Instance schedules and placement policies (#2206)

`instances.addResourcePolicies` and `removeResourcePolicies` (gcloud `compute instances add-resource-policies`, `instances create --resource-policies`, Terraform `google_compute_instance.resource_policies`) attach a region's resource policies to a VM, one of each type:

- **Instance schedules** (`instanceSchedulePolicy`, gcloud `compute resource-policies create instance-schedule`, the console's **Instance schedules** page): unix-cron start and stop schedules in a time zone, between optional initiation and end dates, at most one start and one stop an hour. While `up` runs, each minute's tick starts the schedule's stopped VMs and stops its running ones as each operation falls due (VMs attached after it fell due wait for the next one), and the policy reports `resourceStatus.instanceSchedulePolicy` (`lastRunStartTime`, `nextRunStartTime`). A schedule is not run while `up` is stopped.
- **Placement policies** (`groupPlacementPolicy`, gcloud `compute resource-policies create group-placement`): a spread policy gives each VM the least used of its availability domains (`scheduling.availabilityDomain`); a compact policy (`collocation: COLLOCATED`) is added to a stopped N2, N2D or C3 VM and reports its `resourceStatus.physicalHostTopology`. CloudBurrow runs every VM on this machine: the placement is reported, as the API answers it, not a physical spread.
- **Workload policies** (`workloadPolicy`) are validated and kept; they apply to managed instance groups, which are not served, and an instance refuses them, as Google's does.

Snapshot schedules are resource policies with a `snapshotSchedulePolicy` (gcloud `compute resource-policies create snapshot-schedule`, Terraform `google_compute_resource_policy`, the console's **Snapshot schedules** page): hourly, daily or weekly, starting on the hour in UTC, with a retention policy. A disk takes up to 10 (`addResourcePolicies`, a disk's `resourcePolicies` at creation). While `up` runs, each schedule takes its disks' snapshots within the hour of each start, named `DISK-SCOPE-YYYYMMDDHHMMSS-xxxxxxxx` with `autoCreated` and `sourceSnapshotSchedulePolicy`, and deletes those older than `maxRetentionDays` while a newer snapshot of the disk exists (a deleted disk's only with `APPLY_RETENTION_POLICY`). A schedule is not run while `up` is stopped: a missed start is taken at the next start.

Terraform: `google_compute_region_disk`, `google_compute_resource_policy` and `google_compute_region_disk_resource_policy_attachment` (`TestTerraformComputeRegionDisk`); a Hyperdisk Balanced High Availability `google_compute_region_disk` with `provisioned_iops`, `provisioned_throughput` and `access_mode` changed in place (`TestTerraformComputeRegionDiskHyperdiskHA`).

### VM networking and firewall rules

VMs on the same VPC network reach each other at their internal IPs, across the network's subnets, and at their internal DNS names; VMs on different networks cannot (network peering is not served). The network's firewall rules are enforced on each VM as Google evaluates them (https://docs.cloud.google.com/firewall/docs/firewalls, read 2026-11-19), #1968:

| Google | Here |
|---|---|
| The VPC network's data plane | One VXLAN segment per network, inside the VMs' own network namespaces. Each guest has a `vpc0` VXLAN device whose VNI is derived from the network's ID, holding its internal IP (`/32`, MAC `42:01:` and the IP, as Google's guests see), an on-link route for each subnet range of the network, and a permanent neighbour and forwarding entry for each running peer on the network, whose packets travel to the peer's pod IP over the cluster's own network (UDP 4789). A VM of another network has another VNI and no forwarding entry, so nothing it sends arrives; overlapping ranges in two networks do not collide |
| Firewall rules | Compiled per VM into an nftables table (`inet cloudburrow`) the guest applies atomically, before its `vpc0` exists at boot: the rules that apply to the VM (no target, one of its network tags in `targetTags`, or its service account in `targetServiceAccounts`), by priority (the lowest number first), a deny before an allow of the same priority, then the implied rules: deny every other ingress, allow every other egress. Stateful (replies of an allowed connection pass, as on Google). Disabled rules are left out |
| Sources and destinations | An ingress rule's sources are the union of its `sourceRanges` and the primary internal IPs of the network's VMs that have one of its `sourceTags` or `sourceServiceAccounts`; `destinationRanges` (egress, and ingress when set) match the destination. Protocols `tcp`, `udp`, `sctp` (with ports), `icmp`, `esp`, `ah`, `ipip`, `all` and numbers |
| `default` network | Its `default-allow-internal` (`10.128.0.0/9`, every TCP and UDP port, ICMP), `-ssh`, `-rdp` and `-icmp` rules, at priority 65534, are enforced like any other |
| Internal DNS | `INSTANCE.ZONE.c.PROJECT.internal` (and the short name) of every VM on the network, in a managed block of the guest's `/etc/hosts`; as on Google, a VM of another network does not resolve them |
| Always allowed | The metadata server, and over the pod interface the cluster's own ranges (`10.244.0.0/16`, `10.96.0.0/12`: the guest's metadata and cluster DNS), are exempt from egress rules, as Google always allows `169.254.169.254` |

How it stays current: the guest's `cloudburrow-vpc.service` polls the Compute port every 2 seconds under its own token (`/cloudburrow/guest/TOKEN/cloudburrow/vpc`), reporting its pod IP and reading back its segment, peers, hosts and ruleset, and applies them when they change. A firewall rule inserted, patched or deleted, a VM's `setTags`, or a VM started or stopped takes effect on the other VMs within about 2 seconds (a stopped VM stops being a peer within 30).

Why this mechanism: it needs nothing on the kind nodes or in the CNI (kindnet), works across nodes, keeps each network's addresses off the pod network (so subnets may overlap the way they can on Google), and puts deny rules and priorities where NetworkPolicy cannot (it has neither). Its limits: ingress over the pod interface is dropped except the VXLAN underlay from the network's own peers, so a VM is not reachable around its firewall at its pod IP; but the guest runs as root in a privileged container, so a guest's own root can remove its own rules (its own ingress and egress only: it cannot change another VM's). SSH from this machine reaches the guest through the forwarded port on its loopback, not the VPC, so it is not subject to the rules (on Google, SSH to the external IP needs a rule such as `default-allow-ssh`). Firewall logging, hierarchical and network firewall policies, and secure tags are not enforced; a subnet range overlapping the cluster's `10.244.0.0/16` or `10.96.0.0/12` is not reachable from VMs.

As `TestComputeVMNetworkingThroughTheGoClient` does with the Go client: a custom network with rules allowing TCP 8080, 8081 and 7070 at 1000, denying 8081 from the subnet at 900, allowing 9090 from the `client` tag to the `server` tag and denying the `client`'s egress to 7070, then VMs `server` and `client` on it; from `client`, `nc -z SERVER_IP 8080` connects, 8081 (the deny at 900 wins), 8082 (no rule: the implied deny) and 7070 (its egress deny) do not, 9090 does; `SERVER.us-central1-a.c.PROJECT.internal` resolves; a VM on `default` reaches another on `default` on any port (`default-allow-internal`) and reaches neither of them.

Every mutation answers a zonal operation (global for snapshots) that is `RUNNING` while the backend works and `DONE` (with its error when it failed) after; `zoneOperations.wait` waits for it. A resource's operations run one after another, in the order they were asked for: a `stop` asked while an `attachDisk` is still restarting the VM's container runs once the attach is done, and the instance ends `TERMINATED` (#2052).

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
