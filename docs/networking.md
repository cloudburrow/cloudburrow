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
names.

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

## Why not `sslip.io`

CloudBurrow previously named services under `127.0.0.1.sslip.io`, a public wildcard DNS
service that maps any `*.127.0.0.1.sslip.io` name to loopback. It resolves everywhere,
including with Go's pure resolver — but it requires **DNS egress to a third party** for every
lookup, which is a poor property for a tool whose point is working offline.

`.localhost` needs no network at all where it is supported, and where it is not, the `Host`
header path works without one either. The previous suffix is explicitly removed from
`config-domain` on upgrade: leaving two default suffixes there would have Knative choosing
between them, and services would be named unpredictably.
