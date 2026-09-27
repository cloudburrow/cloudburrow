# ADR-0004: No authentication, loopback by default, admin separated

- Status: Accepted
- Date: 2026-09-20
- Issue: #1

## Context

CloudBurrow must accept requests from official Google SDKs without real credentials. Those
clients normally attach OAuth bearer tokens and, in emulator mode, disable authentication
entirely — the Go Pub/Sub client's emulator hook sets `WithoutAuthentication` along with
insecure transport.

We could implement a fake credential system that issues and validates local tokens. It would
look reassuring and would be actively harmful: developers would write tests that appear to
verify authorization, and those tests would pass against semantics we invented. A passing
IAM test that proves nothing is worse than no IAM test, because it is believed.

Meanwhile the process holds a writable data directory and, when Cloud Run is enabled, can
create containers via the Docker socket. That is a serious liability if it is reachable
beyond the local machine.

## Decision

**Perform no authentication or authorization.** `Authorization` headers are ignored if
present. CloudBurrow never validates a signature, never contacts Google, and never reads
application default credentials. Signed URLs, when implemented, are accepted on shape alone
without signature verification, and the compatibility matrix records that explicitly.

**Bind `127.0.0.1` by default.** Non-loopback binding requires an explicit flag and emits a
startup warning naming the exposure.

**Admin endpoints are loopback-only always**, on the dedicated control port, refused on
service ports regardless of the bind setting.

*Amended by #553:* a loopback bind does not keep cluster workloads out on Docker Desktop, where
a pod reaches the host's loopback through `host.docker.internal` (measured). The admin API
therefore also requires a per-instance token, minted by `up` and kept owner-only in the state
directory, sent as `Authorization: Bearer`. Health, readiness and metrics stay open. This is the
one exception to "no authentication", and it protects only the endpoints that destroy, reveal
or fault state; the service APIs stay unauthenticated as before.

*Amended by #575:* the container-network relaxation above now has one mechanism. When Cloud Run
is enabled, the CLI-hosted **service APIs** and the metadata server are published to pods under
`cloudburrow-host.<namespace>.svc.cluster.local`, a selector-less Service. Its EndpointSlice points
at the host as the cluster sees it. On Docker Desktop that is `host.docker.internal`, which already
reaches loopback, and nothing more is bound. On Docker Engine it is the kind network's gateway, and a
relay listens on that one address for each published service. The control/admin port is never
published, and `up` announces the publication at startup. Without Cloud Run, nothing is published.

*Amended by #676:* a loopback bind does not keep out the developer's own browser. With **DNS
rebinding**, `attacker.example` first resolves to the attacker's server and then to `127.0.0.1`.
The attacker's page then reaches a CloudBurrow port as a same-origin peer: the browser sends
`Host: attacker.example:9090` with a matching `Origin` and `Sec-Fetch-Site: same-origin`, so an
origin check alone lets it read and change everything. The attacker cannot change the name in
`Host`, so **every HTTP listener checks it against an allowlist** (`internal/hostguard`): the
console, the control port, the metadata server, local AI, every service API port and the
builtin storage server. Any other name is refused with 421 Misdirected Request, and the response
names the rejected host. The allowlist is:

- **any IP address literal**. Rebinding needs a DNS name the attacker controls. A browser sends an
  address only when the page was loaded from that address, so the origin really is that address.
  This covers `127.0.0.1` and `[::1]`, a bind address chosen with `--allow-remote`, the kind
  network gateway where the Linux relay listens, and the pod and cluster IPs that probes use;
- **`localhost` and names under `.localhost`**, which browsers resolve to loopback themselves
  (RFC 6761). This covers `<bucket>.storage.localhost`;
- **names CloudBurrow publishes that nobody can register**: `host.docker.internal`, and
  `cloudburrow-host` as the bare name, as `<ns>.svc` and as `<ns>.svc.cluster.local`. The form
  `cloudburrow-host.<ns>` is left out, because a namespace can share its name with a public
  top-level domain;
- **on the builtin storage server**, its `--host` names, the bucket names under them, and the
  short forms of its Service name; on the metadata server, `metadata.google.internal`.

Cleartext HTTP/2 is not checked. Browsers speak HTTP/2 only over TLS, and every listener is
plaintext, so such a request never comes from a browser. That is how gRPC clients arrive, and
their `:authority` is whatever name they dialed. JSON requests, and gRPC-Web, arrive over
HTTP/1.1 and are checked. The tunnels to upstream emulators (Pub/Sub, Firestore, Datastore,
Spanner, Bigtable, BigQuery) copy TCP to servers CloudBurrow does not write, so they are not
covered. Nor is an attacker on the local network who answers multicast DNS for a `.local` name
on the allowlist, such as `cloudburrow-host.<ns>.svc.cluster.local`; that is a known gap, open
only to someone already on the same network segment.

**Never load application default credentials.** The compatibility harness (issue #10)
additionally refuses non-local endpoints, so a misconfigured test cannot reach real GCP.

## Consequences

**CloudBurrow cannot test authorization, and must say so.** Anything depending on IAM, ACLs,
service-account identity, or signed-URL verification has to be tested elsewhere. This is a
real capability gap, stated in the non-goals rather than papered over.

**Container execution widens exposure beyond loopback by necessity.** Containers cannot reach
the host's loopback interface as the host, so enabling Cloud Run requires binding an address
reachable from the container network. This is the one case where the default posture is
relaxed without a user flag, so it is announced at startup when it happens.

**Admin separation is what makes the relaxation tolerable.** Reset and seed destroy data. By
keeping them on a loopback-only control port, an application container — the least trusted
thing in the system, and the most likely to be running third-party code — can reach the
service APIs it needs and cannot reach the endpoint that wipes state.

**The non-loopback flag is genuinely dangerous** and documented as such: an unauthenticated
service with a writable data directory and Docker access, reachable from a shared network, is
a straightforward path to code execution on the developer's machine.
