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

The console's VPC network pages list and manage networks, subnets, firewall rules, routes and IP addresses, as Google's VPC network section does. Since #2140 they follow Google's tabs: VPC networks' Networks and Subnets in current project, Firewall policies with its VPC firewall rules table (Google's source and target filters, Allow all or Specified protocols and ports), Route management and Effective routes (a route's next hop may be an instance), External and Internal IP addresses (each VM's ephemeral addresses with Promote to static on an internal one), and **VPC network peering**.

Compute Engine's pages (#1965), under **Compute** in the drawer, at Google's paths:

| Page | What it offers |
|---|---|
| VM instances (`/compute/instances`) | Every instance of the project across zones (the aggregated list): status, zone, machine type, internal and external IP, Connect (the forwarded SSH port). Each row has **SSH**, which answers with the `gcloud compute ssh` and `ssh` commands for its port, and a menu with Start / Resume, Stop, Suspend and Reset as its status allows, and Delete. **Create instance** opens Google's *Create an instance* page: name, labels, region and zone, machine type, boot disk image, type and size, network and subnetwork, external IPv4 address, network tags, service account and access scopes, startup script, metadata, deletion protection |
| An instance's page | Details (basic information, machine configuration, networking, metadata, service account, Connect), Storage, Networking and Serial port 1 (console) tabs; Edit (labels, network tags, the machine type while stopped, deletion protection, metadata); Attach existing disk, Detach disk, and the row's actions |
| Disks (`/compute/disks`) | Every disk across zones with its type, size, zone and user; **Create disk** (blank, from an image, a snapshot or another disk in its zone, with a snapshot schedule); Create snapshot, Clone disk, Create image, Edit snapshot schedule, Edit size, Edit labels and Delete (unavailable while attached) |
| Snapshots (`/compute/snapshots`) | Each snapshot's status, location, sizes, source disk and creation time; Create snapshot, Create disk, Edit labels, Delete |
| Images (`/compute/images`) | The project's custom images and the public images served, and the image projects refused with their reasons; **Create image** from a disk, a snapshot or an image; Deprecate and Delete a custom image |
| Machine types (`/compute/machineTypes`) | The predefined types with their vCPUs and memory (Google's console has no page of its own for them; its Create an instance page lists them) |
| Metadata (`/compute/metadata`) | The project's common instance metadata, `ssh-keys` included, with Edit; the **SSH keys** tab (`/compute/metadata/sshKeys`) lists each key with its username and edits them as a list |
| Instance schedules (`/compute/instanceSchedules`, VM instances' tab) | Each schedule's start, stop, time zone and VMs; Create schedule; Add and Remove instances; Delete while it holds none. The schedule starts and stops its VMs |
| Instance templates (`/compute/instanceTemplates`) | Global and regional templates; Create an instance template (Create an instance's fields, no zone); a template's page; Create VM, Create similar, Delete |
| Instance groups (`/compute/instanceGroups`) | Managed groups (stateless, single zone, a fixed size) made from a template, unmanaged groups of VMs, and GKE's node pool groups; Resize, Restart / Replace VMs, Add and Remove instances, Delete |
| Snapshot schedules (`/compute/snapshotSchedules`, Snapshots' tab) | Hourly, daily or weekly schedules with their retention; a disk takes one at create or with Edit snapshot schedule; the schedule snapshots it and deletes snapshots past the retention |
| Machine images (`/compute/machineImages`) | Each machine image's source VM and location; Create a machine image of a VM, its disks copied; Create instance from it; Delete |

A change returns as soon as the API has started its operation, as Google's pages do: the list shows `PROVISIONING`, `STOPPING` and the like until the operation is done.

## VM instances

The design (#1963, option A): **a VM is a systemd container** in the instance's kind cluster, a privileged pod booting `/sbin/init`, and the API (instances, disks, snapshots, images, machine types, zonal operations) sits on a backend interface (`compute.VMBackend`) so a microVM backend can be added later on Linux hosts with `/dev/kvm`. A container has no kernel of its own, so what needs one is refused or documented, never faked.

| Google | Here |
|---|---|
| Public images `debian-12`, `ubuntu-2404-lts` (`-amd64`, `-arm64`), `rocky-linux-9` | A container image of the same distribution, built on this machine at first use from the official image pinned by digest, plus the distribution's systemd, OpenSSH, sudo and curl packages and the guest scripts in `internal/service/compute/guest` (no code of them is linked into CloudBurrow). Tagged `cloudburrow-vm-FAMILY:HASH` |
| Custom images (`images.insert` from a disk, a snapshot or an image; families, `deprecate`) | A copy of the source's files in a PersistentVolumeClaim, from which disks are seeded as from a public image (#2139); `rawDisk` (a Cloud Storage tarball) is 501 |
| `cos-stable`, Windows, Fedora CoreOS, licensed images | 501, with the reason |
| Instance templates, managed and unmanaged instance groups, machine images (#2139) | Served as Google documents them: a managed group makes its VMs from its template, named `BASE_INSTANCE_NAME-xxxx`, and resizes, recreates, starts and stops them; a machine image copies its VM's disks. Regional managed groups and autoscalers are not served |
| Resource policies (#2139) | Snapshot schedules and instance schedules run every 30 seconds against their cron or hourly/daily/weekly rule: a scheduled snapshot is named `DISK-ZONE-TIMESTAMP-xxxxxxxx` with `autoCreated`, and snapshots past the retention are deleted; an instance schedule starts and stops its VMs in its time zone |
| Spot VMs (`scheduling.provisioningModel: SPOT`) | Accepted with `instanceTerminationAction`, checked as Google documents; nothing preempts them here. `setScheduling` changes a VM between Standard and Spot only while it is stopped (`TERMINATED`), as Google requires; the console's Edit, under Availability policies, offers it then |
| Labels | `setLabels` on instances, disks (zonal and regional), custom images and machine images, under the resource's `labelFingerprint` (a stale one answers 412), each label checked as Google documents: at most 64, keys of 1 to 63 lowercase letters, digits, `_` and `-` (international letters allowed) starting with a letter, values of at most 63 of the same characters (#2139) |
| Machine types | The predefined e2, f1/g1, n1, n2, n2d and c3 types; the container's CPU and memory limits are the type's |
| Boot disk | A PersistentVolumeClaim holding the guest's `/etc`, `/usr`, `/var`, `/home`, `/root`, `/opt` and `/srv`, seeded from the image at first boot: what the guest changes survives stop and start |
| Attached disk | A PersistentVolumeClaim mounted at `/mnt/disks/DEVICE_NAME`: a directory, **not a block device** (no `mkfs`, no `/dev/disk/by-id/google-*`). Attaching or detaching on a running instance restarts its container |
| Disk sizes | Checked as Google documents; kind's local-path storage neither enforces nor grows them |
| Snapshot | A copy of the disk's files in a PersistentVolumeClaim of its own; a disk can be made from it |
| Internal IP | Reserved in the instance's subnet as Google picks it, on the guest's `vpc0`, and routed to the other VMs of its network, with the network's firewall rules enforced: see [VM networking and firewall rules](#vm-networking-and-firewall-rules) |
| External IP | An ephemeral `natIP` of the VM's own from `192.0.2.0/24` (TEST-NET-1, RFC 5737: never routed, so nothing that dials it leaves this machine), stable for the life of its access config, promotable to a static address; the guest's port 22 is forwarded to a port on this machine's `127.0.0.1`, named in the instance's JSON as `cloudburrowSshAddress` |
| `startup-script` | Runs at every boot (`google-startup-scripts.service`), its output on the serial port as `google_metadata_script_runner` writes it |
| `ssh-keys` | The guest agent's account manager (a script polling every 2 seconds) makes each user, its `authorized_keys` and passwordless sudo (`google-sudoers`); project keys apply unless `block-project-ssh-keys` |
| Metadata server | The guest reads its instance and project attributes from the Compute port under a per-instance token (`CLOUDBURROW_GUEST_METADATA`); service account tokens come from the instance's metadata server, which `GCE_METADATA_HOST` names in the guest (in `/etc/environment` too). `metadata.google.internal` is not resolved in the guest |
| Serial port output | The container's console (`kubectl logs`), escape codes removed |
| stop / start, reset | The pod is deleted (the disks kept) and created again |
| suspend / resume | The container's cgroup is frozen on its kind node: memory and processes are kept |
| Disk budget | A new disk, instance or snapshot is refused with `RESOURCE_EXHAUSTED` (429) below 20 GiB free, as Kubernetes Engine refuses a node |

### VM networking and firewall rules

VMs on the same VPC network reach each other at their internal IPs, across the network's subnets, and at their internal DNS names; VMs on different networks cannot, unless the networks are peered: an `ACTIVE` peering (both sides made, #2140) puts the two networks' VMs on one data plane, each side's firewall rules still applied, and exchanges their subnet routes. The network's firewall rules are enforced on each VM as Google evaluates them (https://docs.cloud.google.com/firewall/docs/firewalls, read 2026-11-19), #1968:

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

The guest's port 22 is forwarded to a loopback port, named in `instances.get` and the aggregated list as `cloudburrowSshAddress` (and in the console's Connect column). The instance's `natIP` is its own unrouted `192.0.2.0/24` address (see [External IP addresses](#external-ip-addresses)), not the forwarded port's host.

**gcloud compute ssh always dials port 22 of the natIP**: it has no port flag of its own, and the port cannot be put in the instance's address. Pass the forwarded port, and `127.0.0.1` in place of the natIP, to its `ssh` with `--ssh-flag` (the console's Connect shows this command):

```sh
PORT=$(curl -s http://127.0.0.1:COMPUTE_PORT/compute/v1/projects/PROJECT/zones/us-central1-a/instances/vm-1 | jq -r .cloudburrowSshAddress | cut -d: -f2)
gcloud compute ssh vm-1 --zone us-central1-a --ssh-flag="-p $PORT" --ssh-flag="-o HostName=127.0.0.1" --strict-host-key-checking=no
```

gcloud makes its key (`~/.ssh/google_compute_engine`, or `--ssh-key-file`) and adds it to the project's `ssh-keys` metadata (`projects.setCommonInstanceMetadata`); the guest applies it within seconds. `--strict-host-key-checking=no` is needed because the host key gcloud records is per instance ID, and guest attributes (`hostkeys/`) are not served. `TestGcloudComputeInstances` runs exactly this, with `HOME` and the key in throwaway directories. `--internal-ip`, `--tunnel-through-iap` and OS Login are not served.

Plain `ssh` works the same with any key in `ssh-keys`:

```sh
ssh -p PORT -i KEY USER@127.0.0.1
```

### External IP addresses

As Google documents (vpc/docs/reserve-static-external-ip-address, compute/docs/ip-addresses/configure-static-external-ip-address, read 2026-11-22), #2140:

| Google | Here |
|---|---|
| Ephemeral external IP | An access config without `natIP` gets a distinct address of its own from `192.0.2.0/24`, recorded on the access config; it is a record, not a route (nothing outside the VPC data plane reaches a VM; SSH uses `cloudburrowSshAddress`) |
| Promote an ephemeral IP to static | `addresses.insert` in the VM's region with `address` set to its `natIP`: the address keeps the IP, is `IN_USE` with `users` the instance, of the access config's `networkTier`; another region, or a different tier, is refused |
| Assign a static external IP | `accessConfigs[].natIP` of a `RESERVED` regional `EXTERNAL` address of the VM's region, at insert or with `instances.addAccessConfig` (after `deleteAccessConfig`); one `IN_USE` or of another region is refused; the tier is the address's |
| Release | `addresses.delete` is refused while `IN_USE`; deleting the VM, or `deleteAccessConfig`, returns the address to `RESERVED` |
| A given external address (BYOIP) | 501: no address here is the project's own |

The console's IP addresses page lists each VM's ephemeral external address with Promote to static IP address, the static ones with In use by, and Reserve external static IP address's Attached to assigns the new address to a VM of its region.

### Regional disks

As Google documents (compute/docs/disks/regional-persistent-disk, high-availability-regional-persistent-disk and the `regionDisks` reference, read 2026-11-22), #2139: `regionDisks` insert, get, list, delete, resize, setLabels, createSnapshot and add/removeResourcePolicies, and `regionDiskTypes` get and list.

| Google | Here |
|---|---|
| `replicaZones` | Exactly two distinct zones of the disk's region, or 400 |
| Types and sizes | `pd-balanced` and `pd-ssd` from 10 GB, `pd-standard` from 200 GB, to 65,536 GB; other types (`pd-extreme`, Hyperdisk) are refused for a regional disk |
| Attaching | `instances.insert` and `attachDisk` with `source` `regions/REGION/disks/NAME`, from a VM of either replica zone; a VM of another zone is refused |
| Synchronous replication | The region's zones are one cluster here, so a regional disk is **one** storage volume (a PersistentVolumeClaim) that the VMs of both replica zones mount: every write lands on the one copy, so the replicas are always in sync. A zone outage, and so failover (force-attach, `stopAsyncReplication`), is not simulated |
| Snapshots | `createSnapshot` stores the snapshot in the disk's region; a regional or zonal disk can be made from it |

The console's Create a disk takes Location type Single zone or Regional, with the Replica zone; the Disks list shows a regional disk's two zones, and Attach existing disk on a VM of either zone offers it.

### IPv6

As Google documents (vpc/docs/subnets#ipv6-ranges, create-modify-vpc-networks#ipv6, compute/docs/ip-addresses/configure-ipv6-address, read 2026-11-22), #2140:

| Google | Here |
|---|---|
| ULA internal IPv6 (`enableUlaInternalIpv6`) | The network gets a `/48` of `fd20::/20` as `internalIpv6Range`, or the one given (in `fd20::/20`, not another network's); turning it off is refused while a subnet uses it |
| Dual-stack subnet (`stackType: IPV4_IPV6`) | `ipv6AccessType` `INTERNAL` takes a `/64` of the network's ULA range (`internalIpv6Prefix`), `EXTERNAL` a `/64` of `2600:1900::/28` (`externalIpv6Prefix`); `ipv6CidrRange` is that range. An IPv4-only subnet can be patched to dual-stack, and back while no interface uses IPv6; the access type does not change. `IPV6_ONLY` is 501 |
| Dual-stack VM | A `stackType: IPV4_IPV6` interface on a dual-stack subnet gets a `/96` of the subnet's `/64`: `ipv6Address` with `internalIpv6PrefixLength: 96` (internal), or an `ipv6AccessConfigs` entry `DIRECT_IPV6` with `externalIpv6` (external, Premium tier); an address asked for is a reserved static one of the subnet or a free one of its range; on an IPv4-only subnet it is refused |
| Reachability | The address is on the guest's `vpc0` (`/128`, no DAD) with an on-link route for each IPv6 range of the network (and of a network peered with `stackType: IPV4_IPV6`), and a permanent neighbour for each running dual-stack peer: IPv6 frames ride the network's VXLAN segment, which runs over the cluster's IPv4 underlay, so the kind cluster (IPv4 only, kindnet) need not be dual-stack. External IPv6 addresses are carried between the network's VMs, not routed off the cluster |
| Firewall rules | IPv6 ranges in `sourceRanges` and `destinationRanges` are compiled to `ip6` matches, IPv4 ones to `ip`, so one rule may hold both; a source tag or service account matches the VMs' IPv6 addresses too |
| Static IPv6 addresses | `addresses.insert` with `ipVersion: IPV6` in a region: `INTERNAL` of a dual-stack subnet with internal IPv6, `EXTERNAL` (Premium) of one with external IPv6, a `/96` of its range, given or chosen; a VM takes it as its interface's `ipv6Address` (or `ipv6AccessConfigs[].externalIpv6`), `IN_USE` until the VM goes. A global IPv6 address (load balancers) is 501 |
| IPv6 static routes | `routes.insert` with an IPv6 `destRange` outside the subnets' IPv6 ranges, its next hop the default internet gateway, an IPv6 address of a dual-stack subnet of the network (`nextHopIp`), or a dual-stack instance of it; an IPv4 next hop, or an IPv4-only instance, refused. Recorded and listed as IPv4 static routes are; like them, not applied in the guests |
| Not served | `IPV6_ONLY` subnets (501, unavailable in the console with the reason) |

The console's Create VPC network has ULA internal IPv6, its allocation and the range; a subnet's IP stack type and IPv6 access type; Edit subnet's IP stack type (IPv4 to dual-stack and back); Reserve internal static IP address's IPv6; Create an instance's network interface IP stack type and External IPv4 address (Ephemeral, None or a reserved address of the region); the Subnets list's IP stack type, Internal and External IPv6 ranges columns; a VM's network interfaces list its IPv6 addresses; firewall rules take IPv6 source and destination ranges.

### Terraform

`google_compute_instance`, `google_compute_disk` and `google_compute_attached_disk` apply, plan clean and destroy through `cloudburrow terraform` (`TestTerraformComputeInstance`). With `google_compute_attached_disk`, put `lifecycle { ignore_changes = [attached_disk] }` on the instance, as Google's provider documentation says.

Clean up with `instances delete` (the boot disk is deleted with it); a stopped CloudBurrow keeps the pods, and `cloudburrow delete` removes the cluster with them. The images it built stay in Docker until removed (`docker images 'cloudburrow-vm-*'`).
