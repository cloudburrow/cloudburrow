# Local networking and DNS

How a request from your machine, or from a pod, reaches a Cloud Run service.

## Two paths, and they are not the same thing

| Path | Address | Mechanism |
|---|---|---|
| **SDK endpoints** (Storage, Pub/Sub, …) | `127.0.0.1:<port>` | `kubectl port-forward`, one per service |
| **Cloud Run services** | `http://<service>.<namespace>.cloudburrow.localhost:9080` | A **published host port** on the cluster node, into the Knative gateway |

The gateway needs a real published port because a browser opening a Cloud Run URL cannot be
asked to set a `Host` header, and a port-forward cannot serve a gateway that routes by host
for services it has never heard of.

## The ingress gateway

`cloudburrow up` publishes the Knative gateway on **`127.0.0.1:9080`** by default
(`--port-ingress`, `CLOUDBURROW_PORT_INGRESS`).

Two things make that work, and both are visible:

1. **`kind.yaml`**, generated next to the kubeconfig in the instance state directory, maps
   host port 9080 to node port 31080.
2. **kourier is patched to a `NodePort` Service** on 31080. It ships as a `LoadBalancer`,
   which on a local cluster shows `EXTERNAL-IP <pending>` forever and is never reachable.

```
$ cat ~/.cloudburrow/<instance>/kind.yaml
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 31080
        hostPort: 9080
        listenAddress: "127.0.0.1"
        protocol: TCP
```

**The mapping is applied only when the cluster is created.** A cluster made before this
existed, or with a different `--port-ingress`, keeps running perfectly well and simply has
nothing published. `up` checks and says so rather than leaving you with a port that silently
refuses connections:

```
  ingress:    NOT PUBLISHED — nothing is listening on 127.0.0.1:9080
              a host port is mapped only when the cluster is created, so a
              cluster made earlier or with a different port has none.
              `cloudburrow delete` then `cloudburrow up` publishes it.
```

**Loopback only.** The gateway binds `127.0.0.1`, so every service deployed into the cluster
is reachable from your machine and from nowhere else. `test/k8s/ingress_test.go` asserts it
by dialling the port on every non-loopback IPv4 interface.

**HTTP only.** HTTPS would need a certificate CloudBurrow does not issue, and publishing a
port that answers with a self-signed certificate would look like support for something that
does not work.

## DNS: what resolves `*.cloudburrow.localhost`

`.localhost` is reserved by [RFC 6761](https://www.rfc-editor.org/rfc/rfc6761#section-6.3),
which says resolvers should resolve it to loopback and should not send it to a DNS server.
**Not every resolver does.** Measured, not assumed:

| Resolver | `foo.cloudburrow.localhost` | Notes |
|---|---|---|
| macOS system resolver | ✅ `::1`, `127.0.0.1` | What `curl`, browsers and cgo-linked Go binaries use |
| musl (Alpine) | ✅ `127.0.0.1` | |
| glibc, no `systemd-resolved` | ❌ no resolution | A minimal Debian container, for example |
| **Go's pure resolver** (`CGO_ENABLED=0`, or `GODEBUG=netdns=go`) | ❌ `no such host` | **Sends the query to the configured DNS server**, which answers NXDOMAIN |

The Go finding is the one that bites. A statically linked Go program — including anything
built with `CGO_ENABLED=0`, which is most container images — will **not** resolve
`*.cloudburrow.localhost`, and will leak the lookup to whatever nameserver `/etc/resolv.conf`
names. The release CLI is one of those (`CGO_ENABLED=0`), but the Cloud Tasks and Cloud
Scheduler targets it dispatches itself are dialled on loopback without a lookup; see
[install.md](install.md#how-the-cli-is-linked).

`systemd-resolved` does resolve `.localhost`; CloudBurrow has not measured that directly and
so does not claim it as verified.

### The fallback that always works

Address the gateway by IP and name the service in the `Host` header. No resolver is involved:

```sh
curl -H 'Host: hello.default.cloudburrow.localhost' http://127.0.0.1:9080/
```

This is what `test/k8s/ingress_test.go` uses for its host-header case, and what the
compatibility tests use, so the suite does not depend on the resolver of whatever machine it
runs on.

### If you want a name that always resolves

Add the specific service to `/etc/hosts`. Wildcards are not possible there, so this is
per service:

```
127.0.0.1  hello.default.cloudburrow.localhost
```

## Addressing from inside the cluster

A pod's loopback is the pod, not your machine. In-cluster callers use the cluster-local name
and ignore all of the above:

```
http://hello.default.svc.cluster.local
```

The acceptance workflow ([`test/e2e`](../test/e2e)) uses exactly this for Pub/Sub push
delivery, because the push originates inside the cluster.

## Reaching your machine from a pod

Cloud Tasks, Secret Manager, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager, the Cloud
Run API and the metadata server run in the CLI, not the cluster. When Cloud Run is enabled, `up`
publishes them to pods under one name (#575):

```
cloudburrow-host.cloudburrow.svc.cluster.local:<port>
```

where `<port>` is the service's own host port. `up` prints it in the in-cluster column,
`status --format json` reports it as each service's `in_cluster`, and `up` announces the
publication in one line. The **control and admin port is never published** (ADR-0004).
Cloud Run revisions and job tasks are given these addresses as `CLOUDBURROW_*_ENDPOINT` and
`GCE_METADATA_HOST`, beside the emulators' in-cluster `*_EMULATOR_HOST` (#576); `cloudburrow env
--format kubernetes` prints the same list for any other pod
([configuration.md](configuration.md#--format-kubernetes)).

It is a selector-less Service whose EndpointSlice points at your machine as the cluster sees it:

| Runtime | Address pods reach | What `up` binds |
|---|---|---|
| Docker Desktop | `host.docker.internal`, resolved inside the kind node | Nothing more: Docker Desktop already forwards it to the host's loopback (#553, measured) |
| Docker Engine on Linux | The `kind` network's gateway | A relay on the gateway address for each published service, forwarding to its loopback listener; the service itself stays on loopback |

BigQuery needs none of this. Its emulator runs in the cluster, and its validating front
([compatibility.md](compatibility.md), BigQuery) runs beside it in the same pod (#902), serving the
`bigquery` Service's REST port, 9050; the emulator's own REST port is the pod's alone. A pod that
dials `bigquery.<namespace>.svc.cluster.local:9050`, the address `env --format kubernetes` prints
and Cloud Run injects, gets the same refusals as the host's clients, with or without Cloud Run, and so does the host's
tunnel; the traffic stays in the cluster, so it does not depend on pods reaching your machine. The Storage Read port, 9060, is the emulator's,
and not checked.

### Which engines this works on

Only two engines have been run with pods reaching the CLI; the full table, with the evidence for
each row, is in [install.md](install.md#container-engines).

| Engine | Path pods take to the CLI | Status |
|---|---|---|
| Docker Desktop, macOS | `host.docker.internal` | **supported**: measured 2026-09-26 (#553) |
| Docker Engine, rootful, Linux | Relay on the kind gateway | **supported**: `TestAPodReachesTheCLIHostedServices` |
| Docker Desktop, Linux | `host.docker.internal` | unverified |
| Docker Desktop, Windows | — | unsupported: the CLI does not build for Windows |
| colima, OrbStack, Rancher Desktop | `host.docker.internal` if it resolves in the kind node; the kind gateway is a VM address | unverified |
| Podman | None: the gateway is in the podman machine or a rootless namespace | unsupported |
| Rootless Docker | None: the gateway is in rootlesskit's network namespace | unsupported |

Before binding a relay, `up` checks that the gateway can be bound on this machine. When it cannot,
`up` fails before it applies anything, with a message naming the engine `docker info` reports and
the workaround: Docker Desktop or rootful Docker Engine, or `--services` without `run`. `cloudburrow
doctor` warns about the same engines before `up` is run.

**Exposure.** The gateway address is reachable by other containers on the same Docker network,
which on a developer machine means other local containers. That matches what Docker Desktop
already allows. The service APIs stay unauthenticated (ADR-0004), and admin needs its token and is
not on this address. Without Cloud Run, nothing is published.

### The console and the admin token

The console binds loopback, and on Docker Desktop a pod reaches the host's loopback, the console's
port included, as `host.docker.internal`. The console's own guards do not stop it: the Host check
accepts `host.docker.internal`, because that is how pods legitimately reach CloudBurrow on Docker
Desktop, and the same-origin check refuses what a browser marks as cross-site but passes a client
that sends no `Origin` and no fetch metadata, which is what a workload is. So the console's
**Fault injection** screen (`/faults`, #800) holds no admin token: a console that added one would
be a token-free path to `/admin/faults` for any workload under test (#553).

Decided in #800: the page asks the developer for the admin token, the contents of
`<state-dir>/<name>/admin-token`, keeps it in the tab's session storage, and sends it as
`Authorization: Bearer` with each fault request. The console passes that header, and nothing it
holds, to the admin API's own handlers in process, and the admin API's token check decides. A
request without the token is refused with the admin API's 401 whatever Host, `Origin` or
`Sec-Fetch-Site` it carries, and changes nothing (`TestConsoleFaultsRefuseARequestWithoutTheAdminToken`;
against an instance, `TestConsoleFaultRuleFailsTheSDKCall`). The same-origin and Host checks still
apply, so a page on another site is refused even with the token
(`TestConsoleFaultsRefuseACrossOriginRequest`).

The **Instance** page (`/instance`, #801), which saves, loads, resets and seeds the instance,
follows the same decision, with the same session key, so a token pasted on either page serves
both. Its endpoints pass the page's `Authorization` header, and nothing the console holds, to the
admin API's handlers in process; without the token each answers the admin API's 401, changes
nothing and records nothing in the operations ledger
(`TestConsoleInstanceRefusesARequestWithoutTheAdminToken`), and a page on another site is refused
even with it (`TestConsoleInstanceRefusesACrossOriginRequest`).

A Pub/Sub push subscription can target a process on your machine by this name too. That works
only if the process listens where the name leads: on Docker Engine, the gateway address or
`0.0.0.0`; on Docker Desktop, loopback. CloudBurrow cannot bind your server for you.
`TestAPodReachesTheCLIHostedServices` runs a Job in the `default` namespace, the one Cloud Run
revisions use. The Job reads a secret and creates a task through the official clients at these
addresses, and the host then sees the task.

## Why not `sslip.io`

CloudBurrow previously named services under `127.0.0.1.sslip.io`, a public wildcard DNS
service that maps any `*.127.0.0.1.sslip.io` name to loopback. It resolves everywhere,
including with Go's pure resolver — but it requires **DNS egress to a third party** for every
lookup, which is a poor property for a tool whose point is working offline.

`.localhost` needs no network at all where it is supported, and where it is not, the `Host`
header path works without one either. The previous suffix is explicitly removed from
`config-domain` on upgrade: leaving two default suffixes there would have Knative choosing
between them, and services would be named unpredictably.
