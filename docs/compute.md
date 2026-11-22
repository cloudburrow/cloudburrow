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
| Instance groups (`/compute/instanceGroups`) | Managed groups (stateless) made from a template, in a single zone or in multiple zones of a region (Target distribution shape Even, Balanced, Any or Any single zone), with autoscaling on CPU utilization (On, Only scale out, Off; minimum and maximum, target, initialization period, scale-in controls) or a fixed size; unmanaged groups of VMs; GKE's node pool groups; Port mapping (named ports); a group's page with its VMs and their template, Start, Stop, Suspend, Resume and Delete instance on checked VMs, Autoscaling (recommended size, status); Update VMs (set the template for new VMs), Edit autoscaling, Edit port mapping, Resize (not while autoscaled), Restart / Replace VMs, Add and Remove instances, Delete |
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
| Instance templates, managed and unmanaged instance groups, machine images (#2139) | Served as Google documents them: a managed group makes its VMs from its template, named `BASE_INSTANCE_NAME-xxxx`, and resizes, recreates, starts and stops them; setInstanceTemplate changes the template of new VMs only, and recreateInstances re-makes a VM of another template from the current one, by the same name. A regional group (`regionInstanceGroupManagers`, `regionInstanceGroups`) spreads its VMs, each a zonal instance, over its `distributionPolicy.zones` (three of the region's when none are given): EVEN (the default; PROACTIVE redistribution), BALANCED and ANY spread alike here (every zone has the capacity), ANY_SINGLE_ZONE keeps them in one zone. Named ports are checked as Google documents (a name of the RFC 1035 rule, a port 1-65535, a stale fingerprint 412). Autoscalers and regional autoscalers scale on the VMs' real CPU, read every 15 seconds from each VM's cgroup (`cpu.stat`): recommended size = ceil(serving VMs × average utilization / target), VMs in their initialization period (`coolDownPeriodSec`) left out, clamped to the minimum and maximum, scaling out at once and in only to the highest recommendation of the last 10 minutes and as `scaleInControl` allows; mode OFF still recommends, ONLY_SCALE_OUT only adds. A zonal group's resize is overridden by its autoscaler at its next pass; an autoscaled regional group's resize is refused, as Google refuses it. The same readings are `compute.googleapis.com/instance/cpu/utilization` in Cloud Monitoring (gce_instance), sampled every 60 seconds. Predictive autoscaling, custom metric, load balancing and schedule signals answer 501 |
| Resource policies (#2139) | Snapshot schedules and instance schedules run every 30 seconds against their cron or hourly/daily/weekly rule: a scheduled snapshot is named `DISK-ZONE-TIMESTAMP-xxxxxxxx` with `autoCreated`, and snapshots past the retention are deleted; an instance schedule starts and stops its VMs in its time zone |
| Spot VMs (`scheduling.provisioningModel: SPOT`) | Accepted with `instanceTerminationAction`, checked as Google documents; nothing preempts them here |
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

### Cloud Router and Cloud NAT

Routers (`compute.routers.*`, #2140) are served in the region they are made in, on a network of the project, many per region and network, as on Google (https://docs.cloud.google.com/network-connectivity/docs/router, https://docs.cloud.google.com/nat/docs/overview, read 2026-11-22):

| Google | Here |
|---|---|
| `bgp` | `asn` an RFC 6996 private ASN (64512-65534, 4200000000-4294967294) or 16550, else 400; `keepaliveInterval` 20 by default, 20 to 60; `advertisedGroups` (`ALL_SUBNETS`) and `advertisedIpRanges` only with `advertiseMode: CUSTOM`; `identifierRange` link-local, /30 or larger |
| `interfaces`, `bgpPeers` | An interface links at most one of a VPN tunnel, an Interconnect attachment or a subnetwork. Cloud VPN and Cloud Interconnect are not served, so the first two are refused 501; a subnetwork interface (a router appliance) is checked against the router's network and region, its `privateIpAddress` in the subnet. A peer names an interface, a `peerAsn`, and its addresses in the interface's range; Google's defaults: `advertisedRoutePriority` 100, `enable` `TRUE`, BFD `DISABLED` at 1000 ms and multiplier 5 |
| `getRouterStatus` | Every BGP peer `DOWN` (no peer router exists locally), with the routes it would be advertised (the network's subnets in the region, or in every region with GLOBAL dynamic routing, or the custom groups and ranges); `bestRoutes` empty; each NAT gateway's addresses, its VMs and `minExtraNatIpsNeeded` |
| `nats[]` | `sourceSubnetworkIpRangesToNat` `ALL_SUBNETWORKS_ALL_IP_RANGES`, `ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES` or `LIST_OF_SUBNETWORKS` (subnets of the router's region and network, `ALL_IP_RANGES`, `PRIMARY_IP_RANGE` or named secondary ranges); a subnet is translated by one gateway only, and a gateway of all subnets is its region and network's only one. `MANUAL_ONLY` takes reserved regional external addresses, which become `IN_USE` with the router as user (and `RESERVED` again when the gateway, or the router, goes); `AUTO_ONLY` is given an address of the local pool. Ports: 64 per VM by default (2 to 57344), 32 to 65536 in powers of 2 with dynamic port allocation, which Endpoint-Independent Mapping excludes; the five timeouts (UDP 30, ICMP 30, TCP established 1200, transitory 30, TIME_WAIT 120 seconds by default); `logConfig` (`ALL`, `ERRORS_ONLY`, `TRANSLATIONS_ONLY`); `rules` with `MANUAL_ONLY`. `type: PRIVATE` (Network Connectivity Center and hybrid spokes) is refused 501 |
| `getNatMappingInfo`, `getNatIpInfo` | Each translated VM (no external IP on its primary interface, on a subnet the gateway covers) given `minPortsPerVm` ports of a NAT IP from port 1024, as static allocation gives them |
| Internet egress | **A VM whose primary interface has no external IP reaches the internet only through a Cloud NAT gateway of its subnet**, as on Google: until one translates it, its guest's ruleset drops egress over its pod interface to every address outside RFC 1918 and 100.64.0.0/10 (`no external IP and no Cloud NAT`), whatever the egress rules allow. Private addresses stay reachable, so the VM still reaches this machine's CloudBurrow endpoints. With a gateway (or an external IP) the VM's egress leaves through this machine's own address: no address translation to the NAT IP happens, and NAT logs are not written |

The routers' console pages are Network Connectivity's **Cloud Routers** (`/hybrid/routers/list`: Create router, a router's Router details, BGP sessions, Cloud NAT gateways and Interfaces, Edit, Delete) and Network services' **Cloud NAT** (`/net-services/nat/list`: Create Cloud NAT gateway with Google's sections, Create new router inline, Automatic or Manual addresses, Advanced configurations; a gateway's details, Allocated external IP addresses and Mapping information, Edit, Delete).

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

### Shared VPC

A project becomes a Shared VPC host with `projects.enableXpnHost`, and service projects attach to it with `enableXpnResource` (#2140), as https://docs.cloud.google.com/vpc/docs/shared-vpc and provisioning-shared-vpc describe (read 2026-11-22):

| Google | Here |
|---|---|
| Organization | Shared VPC is for projects of an organization. A project the Resource Manager registry keeps without a parent (a top-level project) is refused as a host or service project (400); a project under the local organization or one of its folders may be either. A project the registry does not keep (made by another service's call only) is not checked |
| Host and service projects | A host cannot be attached as a service project, a service project cannot be enabled as a host, and a service project attaches to one host ("A service project can only attach to one host project"); each refusal is 400 |
| Detach and disable | `disableXpnResource` is refused while the service project's VMs or addresses use the host's network; `disableXpnHost` while any project is attached |
| `getXpnHost`, `getXpnResources`, `listXpnHosts` | The service project's host (an empty project when none), the host's attached projects (`type` `PROJECT`, paged), and the organization's host projects (the body's `organization`, else the caller's own) |
| Using the host's subnets | A service project's VM (`networkInterfaces[].subnetwork`, or `network` of an auto mode host network) and internal address (`subnetwork`) may name a subnet of its host project; any other project's subnet is refused: "Cross project referencing is not allowed for this resource". IAM enforcement checks `compute.subnetworks.use` on the host's subnet, as Google does. Such a VM is on the host network's data plane, reached by the host's other VMs and filtered by the host project's firewall rules |
| Individual subnets (subnet-level permissions) | `subnetworks.setIamPolicy` is not served, so only Share all subnets (project-level `compute.networkUser`) is offered |

The console's **Shared VPC** page (`/networking/xpn`, VPC network's menu) offers Set up Shared VPC (Enable host project, Select subnets, Project names) to a project that is neither; a host project's page has Subnets in host project and Attached projects tabs with Attach projects, Detach and Disable Shared VPC; a service project's lists the host's subnets it can use (as `gcloud compute networks subnets list-usable` does).

### Private Service Connect

Endpoints and published services (#2140), as https://docs.cloud.google.com/vpc/docs/private-service-connect and its configure pages describe (read 2026-11-22):

| Google | Here |
|---|---|
| Endpoint for Google APIs | `globalForwardingRules.insert` with `target` `all-apis` or `vpc-sc`, no `loadBalancingScheme`, `network`, and `IPAddress` a global internal address of purpose `PRIVATE_SERVICE_CONNECT` (by link, name or address) in that network, which turns `IN_USE` with the rule as its user and `RESERVED` again when the rule is deleted; the name 1-20 lowercase letters and digits starting with a letter; `pscConnectionStatus` `ACCEPTED`, a `pscConnectionId`. A rule with a load-balancing scheme or a load balancer's target is a load balancer's ([Load balancing](#load-balancing)) |
| Endpoint for a published service | `forwardingRules.insert` in a region with `target` a service attachment of that region (404 when it does not exist), `network`, and `IPAddress` a regional internal address or none (an address of `subnetwork` is chosen); `pscConnectionStatus` `ACCEPTED` under `ACCEPT_AUTOMATIC` or when `consumerAcceptLists` names the project (or one of its networks), `REJECTED` when `consumerRejectLists` names it, `PENDING` otherwise, re-decided at each patch of the attachment, `CLOSED` once it is deleted. A target pool, target instance, proxy or backend service is a load balancer's ([Load balancing](#load-balancing)) |
| Service attachment | `serviceAttachments` insert, get, list, aggregatedList, patch (under the fingerprint) and delete: `targetService` required, `natSubnets` subnets of purpose `PRIVATE_SERVICE_CONNECT` in its region, `connectionPreference` `ACCEPT_AUTOMATIC` or `ACCEPT_MANUAL`, accept and reject lists, `enableProxyProtocol`, `domainNames`; `connectedEndpoints` lists every endpoint targeting it with its status and consumer network. A subnet or network an endpoint or attachment uses cannot be deleted |
| Traffic | An endpoint carries no traffic: an attachment's `targetService` is kept as given and no packet travels from an endpoint to it; a load balancer's Private Service Connect network endpoint group does reach the attachment's producer forwarding rule ([Load balancing](#load-balancing)); VMs reach the local Google APIs over the host whatever endpoint exists (docs/gaps.md) |

Memorystore's PSC auto connections (`internal/service/redisadmin`) keep naming their forwarding rules as before, without a Compute resource behind them. The console's **Private Service Connect** page (`/net-services/psc/list`, Network services' menu) has the Connected endpoints tab with Connect endpoint (Target All Google APIs, VPC-SC or Published service, Endpoint name, Network, IP address reserved for it as Create IP address does) and the Published services tab with Publish service (Load balancer, Service name, Subnets, connection preference with accepted and rejected projects, Use Proxy Protocol, domain name), each with its page and Delete; a published service's page lists its connected endpoints and edits its accept and reject lists.

### Load balancing

Cloud Load Balancing (#2153) is served through the Compute Engine API, as https://docs.cloud.google.com/load-balancing/docs and its setup pages describe it (read 2026-11-22), and it carries real traffic:

| Google | Here |
|---|---|
| Forwarding rules | `globalForwardingRules` and `forwardingRules` with a `loadBalancingScheme` (`EXTERNAL`, `EXTERNAL_MANAGED`, `INTERNAL`, `INTERNAL_MANAGED`) and a target proxy, target pool, target instance or (regional) backend service; `portRange` or up to 64 `ports`; an ephemeral external rule's `IPAddress` is `127.0.0.1`, as a VM's `natIP` is, and a reserved address turns `IN_USE`; a regional managed rule needs a proxy-only subnet in its region. Each port is listened on at `127.0.0.1`, on a port of its own, given in the rule's `cloudburrowAddress` (and `cloudburrowAddresses`, `PORT=HOST:PORT` for each), kept across restarts. `patch` (`allowGlobalAccess`, `networkTier`, `sourceIpRanges`) and `setTarget` are served. A UDP rule (`IPProtocol` `UDP`, a backend service, target pool or target instance) listens for datagrams at `127.0.0.1` on its port and keeps each client's flow on one backend until the backend service's `connectionTrackingPolicy.idleTimeoutSec` (600 seconds by default) passes without one; hybrid endpoints are reached directly, instances through a relay in the guest (`perl`, which Google's Debian and Ubuntu images have) |
| Target proxies | Target HTTP, HTTPS, TCP and SSL proxies, global and regional (SSL global only), with their `set*` methods; `proxyHeader` `PROXY_V1` writes a PROXY protocol v1 header to the backend; an HTTPS or SSL proxy serves its SSL certificates by SNI under its SSL policy (profile, minimum TLS version, custom features); a `certificateMap` (Certificate Manager, global proxies) chooses the certificate by SNI from its entries, then its PRIMARY entry, and `sslCertificates` may name Certificate Manager certificates of a regional proxy; target gRPC proxies (`validateForProxyless`) are the target of an `INTERNAL_SELF_MANAGED` rule at `0.0.0.0`; `serverTlsPolicy` and `authorizationPolicy` are 501 |
| URL maps | Host rules, path matchers with path rules and route rules (`matchRules` on prefix, full path, regex and path templates, headers, query parameters; priorities), `defaultService`, `defaultUrlRedirect` and redirects, `routeAction` (URL rewrite including path templates, weighted backend services, timeout, retry policy, fault injection, request mirroring, CORS), header actions at every level, `tests`, `validate` and `invalidateCache`. A classic (`EXTERNAL`) map refuses the advanced features, as Google does |
| Backend services and buckets | Global and regional backend services with every documented check of scheme, protocol, port name, health check, timeout, connection draining, session affinity and locality policy; backends of instance groups (managed or not) and network endpoint groups, with balancing modes `UTILIZATION`, `RATE` and `CONNECTION`, their maxima and `capacityScaler` (0 drains); `getHealth`; backend buckets of Cloud Storage buckets; `securityPolicy`, `edgeSecurityPolicy`, `enableCDN` with `cdnPolicy` and signed URL keys, and `iap` (below). `CUSTOM_METRICS` is 501 |
| Cloud Armor | `securityPolicies` and `regionSecurityPolicies` of type `CLOUD_ARMOR` (backend), `CLOUD_ARMOR_EDGE` (global, before the cache) and `CLOUD_ARMOR_NETWORK` (regional, passthrough flows), with `addRule`, `getRule`, `patchRule`, `removeRule`, `setLabels`, `listPreconfiguredExpressionSets`, and `networkEdgeSecurityServices`. Every request through the proxy is evaluated in priority order: `SRC_IPS_V1` ranges, or an expression in Google's rules language (CEL: `request.headers`, `.path`, `.query`, `.method`, `.scheme`, `.body` up to the inspection size, `origin.ip`, `inIpRange`, `lower`, `base64Decode`, `urlDecode`, ...) including `evaluatePreconfiguredWaf` and `evaluatePreconfiguredExpr` over the OWASP CRS 3.3 signatures at Google's rule IDs and sensitivity levels; `allow`, `deny(403|404|502)` with Google's error page, `redirect`, `throttle` and `rate_based_ban` keyed as configured, `preview` rules evaluated but not enforced, header actions. Threat intelligence, address groups, Adaptive Protection and reCAPTCHA tokens are not served |
| Cloud CDN | `enableCDN` on an external backend service or bucket caches responses in the proxy as `cdnPolicy` says: `cacheMode` (`CACHE_ALL_STATIC`, `USE_ORIGIN_HEADERS`, `FORCE_CACHE_ALL`), `defaultTtl`, `maxTtl`, `clientTtl`, negative caching, `serveWhileStale`, the cache key policy, bypass headers; `Age` on hits, `{cdn_cache_status}` and `{cdn_cache_id}` in custom response headers; `urlMaps.invalidateCache` by host, path (with `*`) or cache tags; signed URLs, URL prefixes and the `Cloud-CDN-Cookie` with `addSignedUrlKey` keys |
| Identity-Aware Proxy | `iap.enabled` on a backend service refuses a request without an ID token for the IAP OAuth client (`iap.oauth2ClientId` or a programmatic client) with Google's 401, one whose principal lacks `roles/iap.httpsResourceAccessor` on `projects/P/iap_web/compute/services/S` with 403, and passes the rest with `x-goog-iap-jwt-assertion` (ES256, keys at `GET /cloudburrow/iap/public_key-jwk`), `x-goog-authenticated-user-email` and `-id`. Tokens are this instance's (the metadata server, `generateIdToken`, `gcloud auth print-identity-token`); policies and settings are the IAP API's |
| Health checks | `healthChecks` and `regionHealthChecks` (TCP, SSL, HTTP, HTTPS, HTTP/2, gRPC, gRPC with TLS; port, named port or serving port; request path, response, proxy header) and the legacy `httpHealthChecks` and `httpsHealthChecks` of target pools. Probes are real, at each check interval, with the thresholds, and come from Google's probe ranges: a VM's network needs ingress firewall rules allowing `35.191.0.0/16` and `130.211.0.0/22` (target pools: `35.191.0.0/16`, `209.85.152.0/22`, `209.85.204.0/22`) on the probed port, or every probe fails, as on Google |
| Network endpoint groups | Zonal (`GCE_VM_IP_PORT`, `GCE_VM_IP`), hybrid (`NON_GCP_PRIVATE_IP_PORT`), serverless (Cloud Run service, tag or URL mask; Cloud Functions), internet (`INTERNET_FQDN_PORT`, `INTERNET_IP_PORT`) and Private Service Connect (a service attachment, or a Google API host); attach, detach and list endpoints with their health. App Engine and API Gateway serverless groups are 501 |
| Service attachments | As in [Private Service Connect](#private-service-connect); a Private Service Connect network endpoint group reaches the producer forwarding rule's listener once the attachment accepts the consumer |
| SSL certificates and policies | Self-managed certificates (RSA of at least 2048 bits, ECDSA P-256 or P-384, checked against their key) and Google-managed ones: `PROVISIONING` until a forwarding rule serves their proxy, then `ACTIVE`, issued for 90 days by this instance's own certificate authority, whose certificate is at `GET /cloudburrow/lb/ca.pem` on the Compute Engine port; SSL policies with their profiles and `listAvailableFeatures` |

The proxy routes each request as Google's does: the URL map chooses a backend service, bucket or redirect; within the backend service the backends with capacity left take requests in proportion to it, round robin by default (`LEAST_REQUEST`, `RANDOM`; `RING_HASH` and `MAGLEV` hash the client), and session affinity (`CLIENT_IP*`, `GENERATED_COOKIE` with Google's `GCLB` cookie, `HEADER_FIELD`, `HTTP_COOKIE`) keeps a client on one healthy endpoint; a request no healthy endpoint can take is answered `503 no healthy upstream`. The proxy adds `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Cloud-Trace-Context`, `Via: 1.1 google` and the backend service's custom headers. Instances are reached through a tunnel to their pod's port, as their SSH port is; Cloud Run services through the cluster's ingress under the service's host (an internal-only service refuses an external load balancer); buckets through the local Cloud Storage JSON API, which needs `allUsers` as an object viewer, with the website's main page and not-found page; internet endpoints behind the local-only guard, which never lets a request reach Google. A passthrough load balancer keeps the rule's port to its backends; a target pool fails over to its backup pool below `failoverRatio`.

The console's **Load balancing** page (`/net-services/loadbalancing/list/loadBalancers`, Network services' menu) has the Load balancers, Backends and Frontends tabs; **Create load balancer** walks Google's steps (load balancer type, frontend, backend, routing rules) and makes every part; a load balancer's page shows its frontend with its local address, its routing rules and its backends. Compute Engine's menu has **Health checks** and **Network endpoint groups**, and Certificate Manager's **Classic Certificates** lists the SSL certificates. App Hub discovers forwarding rules, backend services and service attachments as services and instance groups as workloads.

### Serverless VPC Access

Serverless VPC Access (`vpcaccess.googleapis.com` v1, #2140) is served on the Compute Engine port, JSON under `/v1/projects/P/locations/R/`: `connectors` create, get, list and delete, their operations and the locations. Use the REST transport of the official client (`vpcaccess.NewRESTClient` with `option.WithEndpoint("http://" + computeAddr)`); there is no gRPC listener. A connector needs a network and an unused /28 `ipCidrRange` of the private ranges, or an existing /28 `subnet` that nothing else uses; `minInstances` 2 to 9, `maxInstances` 3 to 10, the machine type `e2-micro`, `f1-micro` or `e2-standard-4`. It is `READY` at once. The console's VPC network menu has **Serverless VPC access** with Create connector.

Cloud Run services and jobs then name a connector in `template.vpcAccess.connector` (or a bare connector ID, expanded), or use Direct VPC egress (`networkInterfaces` with a network and subnet of the region), each checked as Google checks it and recorded on the service and its revisions; the Run console's Networking tab shows and edits them. No traffic is carried: Cloud Run's pods are not on the VMs' VPC segment ([gaps.md](gaps.md)).

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
