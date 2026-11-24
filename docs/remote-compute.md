# Remote compute: the instance's Docker on another host (#2209)

`cloudburrow up --compute ssh://[user@]host[:port]` runs an instance's compute, which is its
Docker (the kind cluster and every container CloudBurrow starts), on a remote Linux host. The
CloudBurrow process, the console, the metadata server, every API endpoint, `gcloud` and the SDKs
stay on this machine and work the same way as with local compute. The host is reached over one
supervised SSH connection, and it opens no inbound port beyond SSH.

This is phase 1 of #2209, the remote host core. The provisioners that create the host in the
user's own AWS or Azure account (phase 2) and scaling (phase 3) are open: see
[gaps.md](gaps.md#remote-compute). They will call the same Go API, `internal/remotecompute`,
that `up --compute` calls, and the console will call it too, so that bringing your own cloud
takes no DevOps work. The CLI flag remains for advanced use.

## Using it

```sh
cloudburrow up --name lab --compute ssh://dev@build-box.example
cloudburrow status --name lab      # compute: ssh://dev@build-box.example (16 CPUs, 62.8 GiB memory, …)
cloudburrow doctor --name lab      # the host's Docker, cgroups, CPU, memory, disk and forwarding
cloudburrow stop --name lab        # stops the cluster's nodes on the host
cloudburrow delete --name lab      # deletes them; the instance can then choose again
```

`up` records the host in the instance directory (`compute.json`), so `status`, `stop`, `reset`,
`delete`, `doctor`, `logs` and the rest reach the same host without `--compute`. If you pass
`--compute` with a different host, the command is refused: the cluster and its state are on the
recorded host, so `cloudburrow delete` it first. `up --compute` is also refused for an instance
whose cluster already exists on this machine's Docker. A `./cloudburrow.json` found in the working
directory cannot set `compute`, as it cannot set `allowRemote`: the host would receive the
instance's Docker and reverse forwards to this machine's endpoints. Pass the flag, set
`CLOUDBURROW_COMPUTE`, or name the file with `--config`.

The console's **Instance** page has a **Compute** card. It shows where the compute runs (this
machine, or the host), the host's size, operating system and Docker version, the connection's
state and reconnect count, and the ports forwarded back from the host. Its **Reconnect** button
needs the admin token, like the page's other actions.

### What the host needs

- Linux with Docker Engine, whose socket (`/var/run/docker.sock`) the SSH account may use (it is
  in the `docker` group). kind is **not** needed on the host: kind runs on this machine against
  the host's Docker. `doctor` reports a kind it finds there anyway.
- cgroup v2, as kind's nodes need.
- sshd allowing public-key sign-in, TCP and stream-local forwarding (the defaults), and
  `GatewayPorts clientspecified`. That last setting lets CloudBurrow bind its reverse forwards
  on the host's Docker network gateways, which only the host and its containers can reach. It
  is the one setting a stock sshd lacks. Without it, `up` and `doctor` say so, in those words.
- At least 2 CPUs, 4 GiB of memory and 20 GiB free under Docker's root directory: `doctor`
  warns below these. One instance with the default services uses about 4 to 6 GiB.

`doctor` prints each of these as a `remote …` row, and `up` refuses a host that is not Linux, or
whose Docker the account cannot use, or whose sshd refuses the gateway forwards. Each refusal is
in plain words with its remedy.

### Keys: CloudBurrow's own

You manage no key. On the first connection to a host, CloudBurrow:

1. generates its own ed25519 key for that host, at
   `<state>/<instance>/remote/<user>_at_<host>[_<port>]/id_ed25519` (mode `0600`, in a `0700`
   directory). There is one key per host, so revoking one leaves the others working;
2. signs in once with **your** ssh agent and config (the system `ssh`, so `~/.ssh/config`'s
   `HostName`, `Port`, `ProxyJump` and the rest apply), and appends the key's public half,
   commented `cloudburrow-<instance>`, to the account's `~/.ssh/authorized_keys`. It does this
   once only; it records the install beside the key and never adds the line twice;
3. from then on, connects with its own key, offered first.

If neither its key nor your agent can sign in, `up` prints the public line to add by hand. Only
the public half ever leaves this machine. Host keys are trusted on first use and recorded in
`known_hosts` beside the key (your own `~/.ssh/known_hosts` is read too). A changed host key is
refused, as `ssh` refuses it. `delete` keeps the key, so the same host takes the instance again
without signing in with your keys. Delete the directory to revoke it here, and remove its line
from the host's `authorized_keys` to revoke it there.

## Inventory: where CloudBurrow touches Docker, kind and host ports

Phase 1 began with this list. It is every place local compute was assumed, and what carries it
over SSH now.

### Docker and kind

CloudBurrow never links a Docker or kind library. Every call is the `docker` or `kind` CLI, run as
a child process (`internal/cluster`'s and `internal/images`' runners, and `exec.Command("docker",
…)` in 37 files). kind's provider is its docker provider, which is the docker CLI again. So one
variable moves all of it: `DOCKER_HOST`. `up` and every command for a remote instance set it, for
their own process, to a Unix socket that the SSH connection forwards to the host's Docker
socket. `DOCKER_CONTEXT` is unset, so it cannot override that. The callers are:

| Where | What it runs on Docker |
|---|---|
| `internal/cluster` (`cluster.go`, `gke*.go`, `stamp.go`) | kind create/get/export/delete, `docker start/stop/inspect/exec` on the nodes, the Kubernetes Engine API's extra kind clusters, node upgrades and rotations |
| `internal/images`, `internal/prefetch`, `internal/storageimage`, `internal/bigqueryimage` | image builds (`docker build`, whose context is uploaded over the socket), pulls, `kind load`, the offline cache's loads, and the daemon's architecture |
| `internal/nbruntime` | BigQuery Studio's notebook runtime: an `--internal` network, a relay container and the kernel container (`docker run -i`, attached over the socket) |
| `internal/sparkproc` | BigQuery's Spark procedures: an `--internal` network, relay and Spark containers |
| `internal/adapter/run` | Cloud Build's docker build/push steps, Artifact Registry formats, Secure Source Manager pulls, buildpacks |
| `internal/modelgarden`, `internal/tfconvert`, `internal/videodecode`, `internal/service/vertexai` | Vertex AI's model containers, the TensorFlow converter, the video decoder and the prediction runners |
| `internal/doctor`, `internal/dockergc` | `docker info`, and the instance's images and volumes at stop and delete |
| `cmd/cloudburrow` | Cloud Build host steps (`runbuildsteps.go`, which mounts the host's own Docker socket, so the host's), Compute Engine VMs (`computevms.go`), Cloud SQL's physical backups, Dataform's compiler, logs, `gc`, `prefetch`, `localai` |

### Ports the host publishes, which come back here

Each of these is a port a container publishes on the host's loopback. This process, or the
developer, reaches it on this machine's loopback.

| Port | Published by |
|---|---|
| kind's API server, `127.0.0.1:<random>->6443` | kind; the kubeconfig names it, and every `kubectl`, port-forward tunnel (`internal/netfwd`) and the console's terminal (`kubectl exec`) go through it |
| the ingress gateway, `--port-ingress` (base+80) | kind's `extraPortMappings` (`internal/cluster/kindconfig.go`) |
| the extra GKE clusters' API servers | `internal/cluster/gke.go` |
| a model container's or the converter's `127.0.0.1::<port>` | `internal/modelgarden/docker.go`, `internal/tfconvert` |
| a Compute Engine VM's or Cloud SQL physical backup's published ports | `cmd/cloudburrow/computevms.go`, `sqlphysical.go` |

kind and docker choose most of these ports as they go, so a **mirror** follows `docker ps` on the
host every 500 ms. It forwards each published `127.0.0.1` or `0.0.0.0` TCP port to the same port on
this machine's `127.0.0.1` as soon as the port appears, and closes the forward when it goes. A
port already taken here is reported once, by number, and skipped. CloudBurrow publishes only on
`127.0.0.1`, so nothing it starts listens on the host's network.

### Listeners here that workloads reach, which go out as reverse forwards

The service APIs, the metadata server, Resource Manager, Cloud KMS for CMEK, the ONNX runner,
the TensorFlow converter, the Spark runner and the local Vertex AI endpoint all listen on this
machine's loopback. Pods reach them as `cloudburrow-host.<namespace>.svc.cluster.local`
(`cmd/cloudburrow/clusterhost.go`), and containers on the host's Docker reach them at the gateway
address `HostAddr` names (#1854). With local compute, Docker Desktop's `host.docker.internal`
serves that, or a relay (`internal/hostrelay`) on the kind network's gateway on Linux. With
remote compute, every relay is a **reverse forward** instead: `ssh -R <gateway>:<port>:127.0.0.1:<port>`,
bound on the host's kind-network gateway (and, for the notebook runtime's and Spark procedures'
relays, the default bridge's gateway, at a port sshd chooses). Only the host and its containers
can reach those addresses. The relays' callers do not change; `hostrelay.SetRemote` hands their
listen address to the SSH connection. `host.docker.internal` is never used on a remote host,
even one running Docker Desktop, because there it would be the host, not this machine.

The admin/control port is never published to the cluster, the same as with local compute
(ADR-0004).

### Containers that mount a path of this machine

A bind mount (`-v /path:/dst`) names the filesystem of the Docker that runs the container, so on a
remote host it would name the host's, where the path does not exist. Every container CloudBurrow
starts with a path of this machine goes through `internal/hostmount` instead:

| Where | Path | Mode |
|---|---|---|
| `internal/modelgarden/docker.go` (Model Garden's vLLM, TEI, llama.cpp, PyTorch, MLX and ONNX containers) | the downloaded model at `/model` | read-only |
| `internal/service/vertexai/exec.go` (the LiteRT-LM runner of Gemini generation) | the model directory at `/models`, where the runtime writes its XNNPACK cache | cache |
| `internal/tfconvert` (the TensorFlow converter and predictor) | the SavedModel at `/model`; the converter's `/out` | read-only; read-write, a scratch volume per run |
| `internal/videodecode` | the request's video at `/in/video` | read-only, removed with the request |
| `internal/sparkproc` (BigQuery's Spark procedures) | the job's directory at `/cb` (relay and Spark), the connector jar, a custom image's Spark and Java trees | read-only and read-write scratch per job; read-only |
| `cmd/cloudburrow/dataform.go` (Dataform's compiler and npm) | the npm cache; the CLI's install; a workspace at `/ws`; a core version's warm-up | cache; read-write; read-write scratch; cache scratch |

With local compute `hostmount` returns the bind mount as before. With remote compute the path's
content goes into a Docker volume on the host, labelled `io.cloudburrow.instance=<name>` (so
`stop` and `delete` remove it with the instance's other volumes), and the container mounts that
volume (`--mount type=volume,…`; a single file with `volume-subpath`, Docker 26 or later):

- **Filling it** needs nothing on the host but its Docker. A helper container is created, never
  started, from the kind node image the host already holds, with the volume mounted, and the
  content is streamed into it (`docker cp - helper:/d`) over the same forwarded socket. A
  manifest kept here (each entry's size, time and mode) means the next use sends only what
  changed and removes only what was removed. A volume that is not the one the manifest
  describes (a new host, a moved instance; each volume carries a `cloudburrow.dev/mount-id`
  label) is filled in full again.
- **Modes.** A read-only mount's files are readable by any user (the volume's files are root's,
  and the image's user is its own); a writable one's directories are writable by any. A
  **read-write** mount is copied back here after the container exits (`docker cp helper:/d/.
  -`, mirrored: what the container removed goes here too). A **cache** mount's writes stay on
  the host for the next use. A **scratch** mount (a job's temporary directory) is removed after
  its run.
- Nothing else about the container changes: its network (`--network none`, the `--internal`
  networks), user and read-only root stay as they were, so egress is exactly as with local
  compute.

### Metal and other host processes

The Apple GPU generator (LiteRT-LM on Metal, `cmd/cloudburrow/localaimetal.go`), the ONNX runner
and every other host process are children of this process, on this machine. They stay here, and
workloads on the host reach them through the reverse forwards above, like any other host
listener.

## The connection

`internal/remotecompute` holds one **ControlMaster** connection made with the system `ssh`:

```
ssh -M -S <ctl> -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=5 -o ServerAliveCountMax=3
    -o IdentitiesOnly=yes -o IdentityFile=<CloudBurrow's key> -o BatchMode=yes
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=<state>/…/known_hosts
    -L <run dir>/docker.sock:/var/run/docker.sock user@host
```

Every other forward is added to that same connection with `ssh -S <ctl> -O forward` (`-L` for
the mirror, `-R` for the relays), and removed with `-O cancel`. The sockets live in a short
`0700` directory under the temporary directory (a Unix socket path must fit 104 bytes on macOS),
which is removed at Close.

The **supervisor** watches the connection. When it ends (a network drop, sshd restarting, the
host rebooting) it reports `disconnected`. It then reconnects with backoff (1 s, doubling to 30 s)
and replays every forward on the new connection: the Docker socket, each mirrored port, and each
reverse forward at the port it was given first, because containers already hold that address.
`docker` and `kubectl` calls made during the gap fail as they would against a stopped Docker, and
the port-forward tunnels re-establish themselves as before (`internal/netfwd`).

### A VM CloudBurrow provisioned

A VM made by `cloudburrow cloud create` or the console's Create ([byoc.md](byoc.md)) is reached
the same way, with two differences, both through `remotecompute.Options`:

- **Key:** `Key` is a `KeySource`. `FileKey` is the file above, for `--compute ssh://`. For a VM
  the key is the keychain item the provisioner made (`StoredKey` over `internal/byoc/keychain`'s
  store, or `PEMKey` with the bytes the provisioner read). It is never written to disk: an SSH
  agent in this process holds it, on a socket in a `0700` directory, and `ssh` signs through that
  agent (`IdentityAgent`, with `IdentitiesOnly` and the key's public half as `IdentityFile`). The
  VM's cloud-init installed the public half, so nothing is installed.
- **Host key:** `HostKey` pins the host key the provisioner made for the VM and gave it through
  cloud-init. The `known_hosts` names only that key (`StrictHostKeyChecking=yes`), so an address
  that now answers with another key is refused.

`remotecompute.Engine` is `internal/byoc`'s `RemoteEngine`. The manager hands it each VM that a
create, start or resize leaves running. It connects as above and writes `compute.json` with
`provisioned: true`. `up`, `stop`, `delete`, `reset`, `status` and `doctor` then read the key,
the pinned host key and the VM's current address from the provisioner on each connect; a stopped
and started VM's new address is used. The cloud-init also sets sshd's `GatewayPorts
clientspecified`, which the reverse forwards need. Deleting the VM closes the connection and
removes the record. The keychain item goes with the VM.

### The Go API

```go
s, err := remotecompute.Connect(ctx, remotecompute.Options{
    Target:   remotecompute.Target{User: "dev", Host: "build-box.example"},
    StateDir: instanceDir, Instance: "lab",
    // Key: remotecompute.StoredKey{Store: store, Name: item}, HostKey: pinned, for a VM
    Progress: func(e remotecompute.Event) { /* stage, message, error: for a console */ },
})
s.DockerHost()            // unix:///…/docker.sock
s.Status()                // connected, reconnects, host size, last error, recent events
s.Reconnect(ctx)          // drop and wait for a new connection with every forward
s.Forward(ctx, l, r)      // and s.Reverse(ctx, listen, target), each with a cancel
s.Checks(ctx)             // the host's prerequisites, each in plain words with its remedy
s.Close()
```

`Event.Stage` is one of `key`, `connecting`, `authorizing`, `prerequisites`, `connected`,
`forward`, `disconnected`, `reconnecting`, `failed`, `closed`. Its `Message` is a sentence the
console can show as it is.

## Moving an instance

`cloudburrow cloud move <instance> --to <vm>|ssh://user@host|local` and the console's **Move
instance here** and **Move back to this machine** move an instance, with its state, from the
Docker it runs on (this machine's, a VM's or a host's) to another: a VM in the same or another
cloud, a host, or this machine ([byoc.md](byoc.md#4-move-an-instance)). `remotecompute.StartCarry`
does the Docker side between any two Dockers (each a `DOCKER_HOST`; between two VMs the streams
pass through this machine, which holds both connections): it stops the instance's containers (its
kind nodes, and every container labelled `io.cloudburrow.instance` or `cloudburrow.instance` for
it), commits each, takes its image across with `docker save | docker load`, makes each network
with the same subnets, streams each volume (mounted, or labelled for the instance) between two
helper containers, and creates each container again from its `docker inspect`, with its address
(each published port once, and without Docker Desktop's own `desktop.docker.io/` labels, which
describe that Docker alone: kind would read a Mac's port label on a Linux VM).
A system bind mount (`/lib/modules`, `/dev`, the Docker socket) stays the target's own; any other
bind mount becomes a volume holding the path (read on the source host, through a helper there,
when the source is remote). A container or volume the target already has by that name stops the
carry before anything is made. `Rollback` removes what was made on the target and starts the
source's containers again; `RemoveSource` removes the source's (and the images an earlier move
left there) once the instance is checked on the target. The state directory never moves: it is
this machine's, with the APIs.

With `CarryOptions.Journal`, the carry records each finished unit (an image loaded, a volume
copied, a container made); an interruption (the context ends) keeps the journal instead of
rolling back, `ResumeCarry` continues from the first unfinished unit, and `OpenCarry` gives a
`Carry` to roll back from the journal alone. `cloud move` keeps its own journal of the move's
phases beside it, so `--resume` and `--rollback` (Resume and Roll back in the console) work after
the process that started the move is gone.

## Egress on the host

The egress guards are of two kinds, and both still apply with the compute on the host:

- **In-process guards** (`internal/egress`, the dispatchers' `Transport`; `internal/netfwd`'s Host
  check). They run in this process, so they are unchanged. A Google host is never dialled from
  here.
- **`--internal` Docker networks**, which the notebook runtime, the Spark procedures and the
  TensorFlow converter run their containers on. They are created by the same `docker network
  create --internal` over the forwarded socket, so they are networks on the host, with no route
  off it. The containers' one way out is the relay, which is a reverse forward to this machine.

The host's own internet access is the host's. CloudBurrow pulls images there (the node image,
Knative, the emulators) as it would pull them on this machine. Nothing else on the host needs
the network.

## Tests

- `internal/remotecompute` unit tests (`session_test.go`) cover the supervisor with an injected
  transport: the first connection forwards the Docker socket; a dropped connection, followed by
  two refused attempts, reconnects and replays every forward, the reverse forward at the port first
  allocated; a cancelled forward is not replayed; Reconnect on request; Close stops a reconnect
  loop that is still failing; the key is owner-only, stable, and one per host; targets parse and
  refuse an option, a password or another scheme; and the mirror follows ports as they appear and
  go.
- `TestStandInConnectInstallsOwnKeyAndForwards` drives the real `ssh` against the stand-in host
  `testdata/remotehost`: a privileged Docker-in-Docker container (docker:27.5.1-dind, pinned by
  digest, Apache-2.0, with Alpine's OpenSSH, BSD) running sshd as a provisioned host's would be
  configured. It checks that the user's config installs CloudBurrow's key, after which that key
  signs in alone; that a port a container publishes on the host answers here; that a container on
  the host reaches a listener here through a reverse forward; and that both survive a reconnect.
  `scripts/remote-compute-standin.sh` starts the stand-in and prints its variables.
- `test/compat`'s `TestRemoteCompute*` (`remotecompute_test.go`), against an instance started with
  `--compute` on the stand-in, check that the instance's kind node is on the host and not on this
  machine's Docker. They then drive Storage, Pub/Sub, Cloud Tasks, Cloud Run (deploy and invoke)
  and Secret Manager with Google's Go clients, all served from the host. The rest of the compat
  suite runs unchanged against such an instance, with the `DOCKER_HOST` that `cloudburrow env`
  exports (so `kind load` reaches the host); `TestCloudSQLInstanceWrites`,
  `TestAPodReachesTheCLIHostedServices` and the Storage, Pub/Sub, Tasks, Run and Secret Manager
  tests were run that way, and the pod-to-CLI test again after the host's sshd session was killed
  and the supervisor reconnected.
- `TestProvisionedVMBecomesTheInstancesCompute` (`internal/remotecompute`) runs phases 1 to 3
  together: the provisioner's fake cloud (`internal/byoc/byoctest`) creates a VM, and the
  stand-in boots as it, with the host key and CloudBurrow's public key from the VM's cloud-init.
  The manager hands the VM to the `Engine`, which signs in with the key from the keychain (an
  in-memory store) against the pinned host key. The test checks that Docker through the
  connection is the stand-in's, that no file holds the private key, and that another host key is
  refused. Delete then removes the record, the key and every tagged resource.
  `cmd/cloudburrow`'s `TestProvisionedComputeRunsDockerOnTheVM` connects an instance whose
  record is a provisioned VM. It uses the provisioner's key and current address, and checks that
  the process's `docker` then reaches the stand-in.
- `TestStandInInterruptedMoveResumes` cuts a carry off half way (its context ends during a
  volume's copy): the journal stays and the source is not restarted; `ResumeCarry` finishes it
  and the 4 MiB of data checks out on the host. A second carry, back, is cut off and undone from
  its journal alone. `TestStandInMoveBetweenTwoHosts` carries from this machine to one stand-in,
  from it to a second (`CLOUDBURROW_TEST_REMOTE_HOST_2`), and back, the data and the address kept
  at each leg.
- `TestStandInModelContainerMountsAPathOfThisMachine` runs a model container through Model
  Garden's own `DockerCLI` on the stand-in: its model directory exists only here, the container
  mounts it as a read-only volume on the host, and its published port serves the model here
  through the mirror; a writable scratch mount's writes come back. `internal/hostmount`'s own
  tests run the same volumes against this machine's Docker: a 0600 file readable by another user,
  a change and a removal sent alone, a read-only mount refusing writes, a container's writes and
  removals mirrored back, a single file, and a refill after the volume is gone.
- `TestStandInMoveRoundTripKeepsData` carries an instance's container (its own network at a fixed
  address, a labelled volume, an anonymous volume, a bind mount of a path here, a published port)
  to the stand-in and back, checking the data, hostname and address at each end and that the
  source is gone, and that a carry onto a volume the target already has rolls back with the
  source running.
- `test/browser`'s `TestRemoteComputeConsole` (`CLOUDBURROW_TEST_REMOTE_COMPUTE` names the host)
  checks the Instance page's Compute card (host, size, system, connection, forwarded ports),
  that Reconnect is refused without the admin token, and that Reconnect with it brings the
  connection back; its WCAG and keyboard subtests check names, contrast and Tab-then-Enter. It runs
  with `TestWelcomePage`, `TestSelectAProjectAndTheNewProjectPage`,
  `TestTerminalRunsACommandAndKeepsTheSession` and, with `bigquery` and `dataform` served,
  `TestBigQueryStudioNotebookRuntimeThroughThePage`, whose runtime container runs on the host.
