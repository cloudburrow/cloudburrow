# Bring your own cloud: run the compute on your AWS or Azure VM

CloudBurrow can run an instance's compute (its kind cluster and the containers it starts) on a VM
in **your own AWS account or Azure subscription**, while the APIs, the console, the metadata
server and every endpoint stay on your machine (#2209). It is for teams moving to Google Cloud,
and for startups whose credits, reserved capacity or GPUs are on AWS or Azure, who want to build
and test against Google's APIs now on compute they already pay for.

Everything is point and click in the console, with a CLI equivalent for each step. You never
install a cloud CLI, write a policy, open a port or manage an SSH key.

> **Not yet tested against real AWS or Azure.** Every flow below is tested against local fakes
> that speak each cloud's API (see [Testing](#how-it-is-tested)); the first run against the real
> clouds is the user's: [Test it on your account](#test-it-on-your-account) lists the clicks and
> what each should show. Report anything that differs.
>
> **The VM becomes the instance's compute through phase 1's engine**
> ([remote-compute.md](remote-compute.md#a-vm-cloudburrow-provisioned)). When a create, start or
> resize leaves the VM running, the engine signs in with the VM's key from the keychain, checks
> its Docker and records it as the instance's compute; the instance's next `up` runs its cluster
> there. A cluster already running on this machine stays where it is until the instance is
> stopped and started again, and its state does not move with it.

## 1. Connect a cloud

Open **Settings and utilities** (the gear) > **Compute**, or go to `/settings/compute`. The page
asks for the instance's admin token once per tab: a workload in the cluster can reach the
console, and this page acts on your cloud account.

**Connect AWS**

- **Sign in with IAM Identity Center.** Paste your AWS access portal URL (IAM Identity Center >
  Settings > *AWS access portal URL*) and the Identity Center region. CloudBurrow opens AWS's own
  page in your browser and shows a code; confirm the code there. Then pick the account and role.
  CloudBurrow never sees your password.
- **Use an access key instead**, for an account without IAM Identity Center: paste an access key
  ID and secret (and a session token if it is temporary).

**Connect Azure**

- **Sign in with Microsoft.** Microsoft's own sign-in page opens in your browser (the MSAL
  interactive browser flow that azidentity's `InteractiveBrowserCredential` is built on). Then
  pick the subscription the VM is billed to. A tenant is optional.

After connecting, CloudBurrow checks the permissions it needs and, in the Create wizard, names any
that are missing in plain words (below).

CLI:

```sh
cloudburrow cloud connect aws --start-url https://d-1234567890.awsapps.com/start --sso-region us-east-1
cloudburrow cloud connect aws --access-key          # reads the key from the terminal, never echoed
cloudburrow cloud connect azure [--tenant T] [--subscription ID]
cloudburrow cloud disconnect aws-111122223333        # removes the credentials from the keychain
```

## 2. Create the VM

**Create VM** opens a wizard with four choices, and the price before anything is made:

1. **Region**: the closest to you is chosen first (from this machine's time zone and the tz
   database's coordinates; nothing is asked of the network).
2. **Size**, as presets:

   | Preset | AWS | Azure |
   |---|---|---|
   | Small: 4 vCPU, 16 GB | `m7i.xlarge` | `Standard_D4s_v5` |
   | Medium: 16 vCPU, 64 GB | `m7i.4xlarge` | `Standard_D16s_v5` |
   | Large: 32 vCPU, 128 GB | `m7i.8xlarge` | `Standard_D32s_v5` |
   | GPU | `g6.xlarge`: 4 vCPU, 16 GB, 1 × NVIDIA L4, the same GPU as Google's G2 | `Standard_NC4as_T4_v3`: 4 vCPU, 28 GB, 1 × NVIDIA T4. Azure lists no NVIDIA L4 VM, so this is the nearest that runs NVIDIA's standard data-centre driver. The wizard says so. |

3. **Auto-stop** after idle time: never, 30 minutes, or 1, 2, 4 or 8 hours (CLI: any duration up
   to a week). It can be changed later, on Compute capacity.
4. **Connection**, how CloudBurrow reaches the VM:
   - **Session Manager: nothing open to the internet** (AWS, the default). The VM's SSM agent
     dials out to AWS Systems Manager, and CloudBurrow reaches the VM's SSH through
     [Session Manager port forwarding](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-sessions-start.html#sessions-remote-port-forwarding):
     the security group allows nothing inbound. CloudBurrow speaks Session Manager's data-channel
     protocol itself, so you install no plugin. The VM gets an IAM role with AWS's
     `AmazonSSMManagedInstanceCore` policy.
   - **SSH from your address only** (Azure, which has no Session Manager, and AWS's fallback for
     a network that blocks Session Manager): the firewall allows TCP 22 from your public address
     alone, and follows your address when it changes (see
     [Security model](#security-model)).

The **Monthly estimate** beside the form is the on-demand Linux price per hour from the cloud's
own pricing API (the AWS Price List API, Azure's Retail Prices API) times 730 hours, plus the
disk. The CLI prints the same before it asks to create.

**Create** then shows its progress, step by step: permissions, your public address, the keys,
network, firewall, image, VM, boot. A failure rolls back whatever was made, and says why:

- **a missing permission**: which, and why it is needed; nothing is left behind;
- **quota or capacity**: choose a smaller size or another region, or request quota in the cloud's
  console; nothing is left behind;
- **the sign-in expired**: connect again.

CLI:

```sh
cloudburrow cloud create --size medium --auto-stop 1h   # prints the price, then asks
cloudburrow cloud create --region eu-west-1 --size gpu --yes
cloudburrow cloud create --connect ssh --yes              # AWS over SSH from your address
```

## 3. Compute capacity

**Compute capacity** (`/compute-capacity`, also in Settings and utilities) shows each VM with
its state, region, machine type, price per hour, external IP, connection (Session Manager, or
the one address SSH is open to and when it was set) and auto-stop. There:

- **Size slider**: move it (by mouse or the arrow keys) through Small, Medium, Large and GPU;
  the price, and its difference from now, follow the slider. Within a family it is **Resize**:
  AWS resizes a stopped instance, so a running one is stopped, resized and started again; Azure
  resizes in place where the hardware allows, else deallocates, resizes and starts. The disk, and
  the cluster's state on it, are kept.
- **Change VM** (to or from GPU): no cloud resizes across these families in place, as a GPU VM's
  NVIDIA driver and container toolkit are installed by cloud-init when it is made. The button
  reads Change VM, and its dialog shows the plan and the price before anything happens: a new VM
  of the size (`<name>-gpu` or `<name>-cpu`) in the same connection and region, with the same
  auto-stop and connection mode; the instance moved onto it with its state (as in [Move](#4-move-an-instance));
  then the old VM deleted. If the move fails, the new VM is deleted and the instance stays where
  it was. CLI: `cloudburrow cloud scale <vm> --size gpu` shows the same plan and asks.
- **Start** and **Stop**. A stopped VM keeps its disk. On Azure, Stop *deallocates*, so the
  compute is no longer billed.
- **Auto-stop**: choose another idle time and **Save**. On a running VM CloudBurrow rewrites the
  setting over its connection at once; on a stopped VM it is recorded and written when the VM
  next starts.
- **Allow my address** (SSH connections): moves the firewall's SSH rule to this machine's
  current public address now. CloudBurrow also does so before each connection and reconnection,
  and every three minutes while the instance runs.
- **Delete** (type the instance's name to confirm).
- **View resources**: everything tagged `cloudburrow-instance=<name>`.

CLI: `cloudburrow cloud status|scale [<vm>] --size large|start|stop|delete|resources`,
`cloudburrow cloud set [<instance>] --auto-stop 30m` (`0` is never) and
`cloudburrow cloud allow-my-ip [<instance>]`. Each takes `--name` for the instance.

## 4. Move an instance

An instance moves, with its state, in every direction: from this machine onto a VM, from one VM
to another (in the same cloud or another: AWS to Azure), and from a VM back to this machine.
Compute capacity says where the instance runs (**Instance <name>: Runs on**).

A VM created for an instance that already runs on this machine does not take it over silently:
the instance's cluster and its state (the kind node's etcd, the volumes its pods write, Cloud SQL
data, every container and volume labelled for it) are on this machine's Docker, so Create ends
with "The VM is ready: move the instance there". **Move instance here**, on the VM's card in
Compute capacity, or the CLI, moves it:

```sh
cloudburrow cloud move lab --to lab          # onto the VM `cloud create --name lab` made
cloudburrow cloud move lab --to gpu-box      # onto another VM; gpu-box is then lab's compute
cloudburrow cloud move lab --to lab-az       # from its AWS VM to an Azure VM
cloudburrow cloud move lab --to local        # from its VM back to this machine
cloudburrow cloud move lab --to ssh://dev@build-box.example   # onto a host of your own
cloudburrow cloud move lab --resume          # an interrupted move, from where it stopped
cloudburrow cloud move lab --rollback        # or undo it
```

In the console: **Move instance here** on a VM's card (from this machine or another VM), and
**Move back to this machine** in the instance's section. A move between two VMs streams through
this machine, which holds both connections (the clouds need no route to each other); it costs
the clouds' egress for the bytes carried.

The move, step by step (each step is a line in the console and the terminal):

1. **stop**: the instance's `up` stops (its arguments were recorded when it started);
2. **connect**: the VM is reached and checked as `up --compute` checks a host;
3. **carry**: each of the instance's containers (the kind nodes first) is committed with `docker
   commit`, which keeps a node's `/etc/kubernetes` and `/kind`, and its image goes across with
   `docker save | docker load`; each volume it mounts, and each volume labelled for the
   instance, is streamed as a tar between two helper containers; each network is made with the
   same subnets, so a kind node keeps its address, which its certificates name; each container
   is made again from its own `docker inspect` (name, labels, privileges, tmpfs, published
   ports, address, aliases, mounts). A bind mount of a path of this machine becomes a volume
   holding the path's content. Progress shows the bytes copied;
4. **start**: the VM is recorded as the instance's compute and `up` starts again with the same
   arguments, so it runs there;
5. **verify**: `up` must report ready, and the cluster's nodes must run on the VM;
6. **cleanup**: only then are the instance's containers, volumes and the committed images
   removed from this machine.

A failure at any of steps 2 to 5 **rolls back**: what was made at the destination is removed, the
source's record comes back, and the instance starts where it ran, as it was, with its data.
Nothing is removed from the source before the instance runs at the destination.

A move is **resumable**. Each step, and within the carry each unit (an image loaded, a volume
copied, a container made), is written to the instance's move journal (`move-journal.json`, with
the carry's own in `move-carry.json`) before the next starts. A move that was interrupted (^C, the
process killed, the laptop asleep or restarted, a dropped link) is not rolled back: the console
shows **The move … was interrupted** with **Resume move** and **Roll back move**, and the CLI has
`--resume` and `--rollback`. Resume goes on from the first unfinished unit (one cut off half way
is done again from its start); Roll back removes what was made and starts the instance where it
ran. Until then `up` refuses to start the instance, as its cluster is half carried.

The console restarts during a move, as `up` serves it;
the page follows the move's progress (`<state>/<instance>/move.json`) across the restart, and
the move runs in a process of its own (`cloudburrow cloud move`), so closing the page does not
stop it.

## What it costs

You pay your cloud, at its on-demand prices, for:

- **the VM while it runs**: the hourly price the wizard shows;
- **the boot disk while it exists**, running or stopped: 100 GiB gp3 on AWS, a 128 GiB Premium
  SSD (P10) on Azure;
- **the public IP**: AWS bills a public IPv4 address per hour, including while it is attached to
  a stopped instance; Azure bills a Standard static public IP per hour;
- **data transfer out** of the cloud, at its rates.

Auto-stop ends the VM's hourly charge while you are not using it. Delete ends every charge. Prices
are read live when you open the wizard or the slider; they are list prices and do not reflect
credits, savings plans or reservations.

## What it creates

Every resource carries the tag `cloudburrow-instance=<instance name>`:

| | AWS | Azure |
|---|---|---|
| Network | a VPC `10.209.0.0/16`, a subnet, an internet gateway and a route table | a resource group `cloudburrow-<name>`, a VNet `10.209.0.0/16` and a subnet |
| Firewall | a security group allowing **nothing inbound** (Session Manager), or **only TCP 22 from your public address /32** (`--connect ssh`) | a network security group allowing **only TCP 22 from your public address**, then Azure's default deny |
| Session Manager | an IAM role `cloudburrow-<name>-ssm` trusted by EC2, with `AmazonSSMManagedInstanceCore` attached, and its instance profile; not made with `--connect ssh` | none |
| VM | Ubuntu 24.04 (Canonical's image), `InstanceInitiatedShutdownBehavior=stop`, IMDSv2 required | Ubuntu 24.04 (Canonical), a system-assigned managed identity |
| Disk | an encrypted gp3 EBS root volume, deleted with the instance | a Premium SSD managed OS disk, deleted with the VM |
| Address | a public IPv4 | a Standard static public IP |
| Auto-stop | none extra: the VM shuts itself down, which stops it | a role assignment: *Virtual Machine Contributor* for the VM's own identity, scoped to that VM only, so it can deallocate itself |

**On the VM**, cloud-init installs, pinned (also in `dependencies.json` under `byoc`):

- Docker `5:29.9.3` and containerd `2.4.2` from Docker's apt repository, its signing key checked
  against Docker's published fingerprint;
- kind `v0.33.0`, checked against the release's sha256;
- on the GPU presets: NVIDIA's `580-server` driver through `ubuntu-drivers`, and the NVIDIA
  Container Toolkit `1.20.2` (its key checked), set as Docker's default runtime; the VM reboots
  once to load the driver;
- in Session Manager mode, AWS's SSM agent: Ubuntu's images ship it as the `amazon-ssm-agent`
  snap; cloud-init makes sure it is installed, enabled and running;
- auto-stop: a systemd timer that, every minute, reads the idle time from
  `/var/lib/cloudburrow/auto-stop` (minutes; `0` is never; owned by the login user, so
  CloudBurrow rewrites it when you change the setting), checks for an established SSH connection
  (CloudBurrow's tunnel is one, through Session Manager too) and stops the VM after that idle
  time. Time before the latest boot does not count. On Azure it deallocates through the VM's
  managed identity, and shuts the OS down only if that is refused.

Nothing else listens on the VM; in Session Manager mode nothing at all is reachable from the
internet.

## How to delete

**Compute capacity > Delete**, or `cloudburrow cloud delete`. CloudBurrow:

1. finds every resource tagged with the instance (on Azure, the resource group and anything
   tagged outside it, and the VM's role assignment);
2. deletes them, the VM first and the network last, waiting for each;
3. **lists again**, and reports any resource still there by type and ID. The VM's record stays,
   marked failed, so you can Delete again, or remove what is named in the cloud's own console.

Disconnecting a cloud that still has a VM is refused: without its credentials CloudBurrow could
no longer stop or delete it. Delete first.

If you ever need to clean up by hand: on AWS, search the EC2 and VPC consoles for the tag
`cloudburrow-instance`; on Azure, delete the resource group `cloudburrow-<name>`.

## Security model

- **You sign in, on your cloud's own page.** CloudBurrow never sees a password. It never creates
  cloud accounts, users or access keys.
- **Credentials stay on your machine, in the OS keychain**: the macOS Keychain (through
  `/usr/bin/security`, the secret on its standard input, not its command line) or the Secret
  Service (GNOME Keyring, KWallet) on Linux, through the pure-Go
  [go-keyring](https://github.com/zalando/go-keyring), so the Linux CLI stays static. They are
  held per connection, never written to a file in plain text, never logged, and never returned by
  the console's API. **Disconnect** deletes them.
  - AWS IAM Identity Center: the Identity Center token and its refresh token, and short-lived role
    credentials, renewed as they expire.
  - AWS access key: the key you pasted.
  - Azure: MSAL's token cache for your account.
- **What is on disk**: `<state dir>/byoc/byoc.json` (owner-only) holds each connection's cloud,
  account ID and name, and each VM's region, size, address, host key and resource IDs. No secret.
- **SSH**: CloudBurrow makes an ed25519 key on your machine; the private half goes to the
  keychain and never leaves the machine, only the public half goes to the VM. It also makes the
  VM's **host key** locally and installs it through cloud-init, so the first connection is
  verified, not trusted on first use. The host key's private half is in the VM's user data,
  which anyone in your account allowed to read instance attributes can read; that is the
  cloud's normal boundary.
- **No inbound access on AWS.** In Session Manager mode the VM's agent connects out to AWS, and
  CloudBurrow's tunnel is a listener on `127.0.0.1` of your machine: each connection to it is a
  stream over a Session Manager session (`StartSession` with `AWS-StartPortForwardingSession` to
  port 22, its WebSocket signed with your credentials), and the system `ssh` signs in through it
  against the pinned host key, as over SSH. Session Manager's own logging and IAM controls apply.
- **SSH from your address only, elsewhere.** On Azure, and on AWS with `--connect ssh`, your
  public address is read from `https://checkip.amazonaws.com`. When it changes (another
  network, a new lease), CloudBurrow moves the firewall's rule to the new address before the next
  connection or reconnection, and every three minutes while the instance runs; the old address
  is removed. **Allow my address** and `cloudburrow cloud allow-my-ip` do it at once. No service
  is opened to the internet.
- **Least privilege.** The permission check lists exactly what CloudBurrow calls:
  - AWS, through IAM `SimulatePrincipalPolicy` on your role or user: `ec2:DescribeRegions`,
    `DescribeImages`, `CreateVpc`, `CreateSubnet`, `CreateInternetGateway`,
    `AttachInternetGateway`, `CreateRouteTable`, `CreateRoute`, `AssociateRouteTable`,
    `CreateSecurityGroup`, `AuthorizeSecurityGroupIngress`, `RevokeSecurityGroupIngress`,
    `RunInstances`, `CreateTags`,
    `DescribeInstances`, `DescribeVpcs`, `DescribeSubnets`, `DescribeSecurityGroups`,
    `DescribeInternetGateways`, `DescribeRouteTables`, `DescribeVolumes`, `StartInstances`,
    `StopInstances`, `ModifyInstanceAttribute`, `TerminateInstances`, `DeleteVolume`,
    `DeleteSecurityGroup`, `DisassociateRouteTable`, `DeleteRouteTable`, `DeleteSubnet`,
    `DetachInternetGateway`, `DeleteInternetGateway`, `DeleteVpc`, and `pricing:GetProducts`;
    for Session Manager, `iam:CreateRole`, `TagRole`, `GetRole`, `AttachRolePolicy`,
    `DetachRolePolicy`, `DeleteRole`, `CreateInstanceProfile`, `TagInstanceProfile`,
    `GetInstanceProfile`, `AddRoleToInstanceProfile`, `RemoveRoleFromInstanceProfile`,
    `DeleteInstanceProfile`, `PassRole`, and `ssm:DescribeInstanceInformation`, `StartSession`,
    `TerminateSession`.
  - Azure, through *Permissions - List* on the subscription: resource groups
    (write, read, delete), virtual networks (write, delete, `subnets/join/action`), network
    security groups (write, read, delete), public IP addresses and network interfaces (write, delete), virtual machines
    (write, read, delete, `start/action`, `deallocate/action`), `disks/delete`,
    `roleAssignments/write` (for auto-stop), and `tags/read`.
- **Nothing is sent to Google Cloud.** The cloud calls go only to the cloud you connected.

## How it is tested

Tests never sign in to AWS or Azure, never read `~/.aws`, `~/.azure` or a real keychain, and
never reach a real endpoint:

- `internal/byoc/awsprov/awsfake` and `internal/byoc/azureprov/azurefake` are local HTTP fakes
  implementing only the calls CloudBurrow makes, their request and response shapes recorded from
  each cloud's API reference (EC2 Query XML, IAM/STS query XML, the Pricing API's JSON, the SSO
  OIDC and portal APIs, Systems Manager's JSON API and the Session Manager data channel's
  WebSocket, with a stand-in SSM agent; Microsoft Entra's token endpoint for MSAL, Azure
  Resource Manager and the Retail Prices API), with failure injection: a denied action, a quota
  error, an expired device code, a revoked token, a resource Delete cannot remove, an agent that
  is not connected.
- `internal/byoc/awsprov/ssmtunnel`'s tests check the data channel's framing, acknowledgements,
  resending of lost frames, multiplexing, the basic mode of agents before 3.0.196.0, and a
  session ended by the agent.
- With the stand-in remote host (`scripts/remote-compute-standin.sh`), the system `ssh` signs in
  to it through the Session Manager tunnel to awsfake's agent, and auto-stop changed after create
  is checked in the host's file (`go test ./internal/remotecompute -run 'SessionManager|Provisioned'`).
- Unit and integration tests drive the official SDKs (AWS SDK for Go v2, the Azure SDK for Go)
  against them: sign-in, credential refresh, the permission check, prices, create with rollback,
  resize, start, stop, delete and its check (`go test ./internal/byoc/... ./cmd/cloudburrow -run Cloud`).
- Browser tests drive the whole console flow against the fakes, with WCAG and keyboard subtests
  (`go test -tags browser ./test/browser -run Byoc`). `TestByocMoveAnInstanceOntoTheVM` checks
  Move: the dialog, the progress, a rolled-back failure in an alert, a stopped VM's reason.
- `TestByocChangeVMAndResumeAnInterruptedMove` checks Change VM (the GPU stop of the slider, the
  plan with the price difference, the progress) and an interrupted move's Resume and Roll back.
- **The first-run walk-through**, `TestByocFirstRunWalkThrough`, is the whole no-DevOps flow in
  the browser on a real instance whose clouds are the fakes (`test/tools/byocfakes` serves them
  in a process of its own and writes the file `CLOUDBURROW_BYOC_FAKES` names, every endpoint on
  127.0.0.1) and whose VMs boot as two stand-ins: connect AWS (IAM Identity Center's device
  sign-in) and Azure (Microsoft's sign-in), create a VM in each, move the instance onto the AWS
  VM, deploy a Cloud Run service from the console there, move the instance from AWS to Azure
  (the revision comes back on the Azure VM's node), scale, stop and start the Azure VM, move it
  back to this machine, and delete both VMs, nothing left in either cloud, with WCAG and keyboard
  subtests. `CLOUDBURROW_BYOC_FAKES` is refused unless every endpoint is a loopback address, and
  keeps the fakes' credentials in a directory, never the keychain.
- Moves are tested for real against the stand-ins: `TestStandInInterruptedMoveResumes` cuts a
  carry off half way, resumes it with the data whole, and rolls a second one back from its
  journal alone; `TestStandInMoveBetweenTwoHosts` carries from this machine to one host, to a
  second, and back.
- A move is tested for real against the Docker-in-Docker sshd stand-in
  ([remote-compute.md](remote-compute.md#tests)): `TestStandInMoveRoundTripKeepsData` carries a
  container with a labelled volume, an anonymous volume, a bind mount, a fixed address and a
  published port to the stand-in and back, and a refused carry that rolls back; and an instance
  (Storage, BigQuery, Dataform) with a ConfigMap and a Cloud Storage object was moved with
  `cloudburrow cloud move <instance> --to ssh://` and both were read back from it on the host.

## Test it on your account

The flows above have run against the local fakes only; this checklist is the first run on your
own AWS account and Azure subscription. It costs a few dollars (Small VMs for about an hour;
skip the GPU step to stay under a dollar). Start an instance first (`cloudburrow up --name lab`),
open its console, and keep a terminal on `cloudburrow cloud status --name lab`.

| # | Click | What it should show |
|---|---|---|
| 1 | **Settings > Compute**, enter the admin token (`cat <state>/lab/admin-token`) | Two cards, *Connect AWS* and *Connect Azure*; Create VM disabled. |
| 2 | *Connect AWS*: your IAM Identity Center start URL and its region, **Sign in** | A code on the page and AWS's sign-in page in your browser showing the same code; after you approve, a list of your accounts and roles. Pick one: the connection appears as `aws-<account>`. Without Identity Center, use **Use an access key instead**. |
| 3 | *Connect Azure*: **Sign in** | Microsoft's sign-in page in your browser; after it, your subscriptions. Pick one: `azure-<subscription>` appears. |
| 4 | **Create VM**, connection `aws-…`, keep the closest region, **Small**, auto-stop 1 hour | The permission check says *Every permission CloudBurrow needs is granted.* (or names what is missing, and why, in plain words); the hourly and monthly price from AWS's Price List, before Create. |
| 5 | **Create** | Progress: permissions, address, key, network, firewall, VM, disk, boot (cloud-init installs Docker and kind; 3 to 6 minutes), then *The VM is ready … Move it to the VM*. In AWS's console: one VPC, subnet, security group with **no inbound rule** (Session Manager), one instance and volume, all tagged `cloudburrow-instance=lab`. |
| 6 | **Compute capacity**: on the VM's card, **Move instance here**, **Move** | Steps stop, connect, stop, image, volume (bytes copied), carried, start, verify, cleanup, done; the console restarts once (enter the new admin token). *Runs on: VM lab*. `cloudburrow status --name lab` names the VM. |
| 7 | **Cloud Run > Deploy container**: image `ghcr.io/knative/helloworld-go:latest`, Allow public access, **Create** | The service turns ready; its URL answers through this machine as before the move. |
| 8 | **Create VM** on `azure-…`, name `lab-az`, Small; then **Move instance here** on its card | A second VM in Azure (resource group, VNet, NSG with SSH from **your** address only, public IP, NIC, VM, disk); the move goes from AWS to Azure; *Runs on: VM lab-az*; the service still answers. |
| 9 | On `lab-az`: the size slider to Medium, **Resize** | The slider shows Medium's price and *+$…/hour*; the machine type becomes `Standard_D16s_v5`; the instance reconnects. |
| 10 | On `lab-az`: **Stop**, then **Start** | *Stopped* (Azure deallocates: compute billing stops), then *Running*; the instance reconnects; the service answers. |
| 11 | Change networks (a phone hotspot), wait 3 minutes | The NSG's SSH rule moves to the new address (*Connection: SSH from …* changes); the instance reconnects. |
| 12 | (Optional, GPU quota needed) the slider to **GPU**, **Change VM** | A dialog with the plan (`lab-az-gpu`, move, delete `lab-az`) and the price difference; then a new GPU VM, the move, and `lab-az` deleted. |
| 13 | Interrupt a move: start **Move back to this machine**, and during *volume* stop the move's process (`kill <pid>` of `cloudburrow cloud move`) | *The move … was interrupted* with **Resume move** and **Roll back move**; **Resume move** finishes it: *Runs on: This machine's Docker*. |
| 14 | **Delete** each VM (type its name) | Progress ends *Nothing tagged cloudburrow-instance=… is left*; in each cloud's console nothing tagged is left; Settings > Compute: **Disconnect** removes the keychain items. |

Report each row that differs (a message, a step that took much longer, something left in a
cloud) on #2209.
