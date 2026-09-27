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
HTTP/1.1 and are checked. The tunnels to upstream emulators are covered by the amendment below.
An attacker on the local network who answers multicast DNS for a `.local` name on the allowlist,
such as `cloudburrow-host.<ns>.svc.cluster.local`, is not covered; that is a known gap, open only
to someone already on the same network segment.

*Amended by #725:* the port-forward tunnels to the upstream emulators copied bytes, so their HTTP
APIs had no Host check. What each tunnelled port carries was measured against the pinned images,
with `curl` sending `Host: attacker.example` over HTTP/1.1 and a prior-knowledge HTTP/2 request:

| Tunnel | Port | HTTP/1.1 (REST) | Cleartext HTTP/2 (gRPC) | Guarded |
|---|---|---|---|---|
| Pub/Sub | 8085 | served, 200 for `attacker.example` | served | **yes** |
| Firestore | 8080 | served, 200 for `attacker.example` | served | **yes** |
| Datastore | 8081 | served, 200 for `attacker.example` | served | **yes** |
| BigQuery REST | 9050 | served, 200 for `attacker.example` | not served | **yes** |
| Spanner | 9010 | not answered: the reply is HTTP/2 framing, which curl rejects | served | no |
| Bigtable | 8086 | not answered, as for Spanner | served | no |
| BigQuery Storage Read | 9060 | not answered, as for Spanner | served | no |
| Cloud SQL (PostgreSQL, MySQL), Memorystore | 5432, 3306, 6379 | database wire protocol, not HTTP | n/a | no |
| Storage | 4443 | the builtin server checks Host itself (#676) | n/a | in-process |

A **guarded** tunnel is an HTTP proxy in `internal/netfwd` (`guard.go`). kubectl listens on a
loopback port that nothing publishes, and the proxy listens on the published address in front of
it. It speaks HTTP/1.1 and prior-knowledge h2c on the one port, as the emulators do. It checks
every HTTP/1.1 request's `Host`, and the `:authority` of any h2c request that is not gRPC,
against the same allowlist, and refuses anything else with 421 naming the host, before a byte
reaches the emulator. Each request goes upstream in the protocol it arrived in,
with its `Host` unchanged. Streams are flushed as they arrive and trailers are passed through, so
bidirectional gRPC (Pub/Sub `StreamingPull`, Firestore `Listen`) works as it does on a raw
tunnel. A Trailers-Only error stays one frame. The unit tests in `internal/netfwd/guard_test.go`
cover this with a real gRPC server behind the proxy.

gRPC is not checked, as on the listeners CloudBurrow serves itself: browsers speak HTTP/2 only
over TLS, so no page can send it, and a gRPC client's `:authority` is whatever name it dialed, so
checking it would add SDK-compatibility risk and no defence against rebinding.

**Left raw, and why.** Spanner's tunnelled port, Bigtable and the BigQuery Storage Read port
speak only gRPC. They do not answer HTTP/1.1, and browsers do not speak cleartext HTTP/2,
so a page has nothing to send them. A proxy there would add a failure mode and remove no
exposure. Spanner's REST gateway (9020) is not tunnelled at all. The Cloud SQL and Memorystore
ports carry the PostgreSQL, MySQL and RESP wire protocols. A browser can open a TCP connection to
them only by sending an HTTP request, and it cannot read a reply that is not HTTP. Those
servers answer an HTTP request with a protocol error or by closing the connection. Valkey, like
Redis, closes a connection on a `POST` or `Host:` line and logs it as a possible attack. None of
these was measured for #725; they are the servers' documented behaviour.

**Known gap.** kubectl's own listener behind a guarded tunnel is still a plain tunnel on
`127.0.0.1`. Its port is chosen by the OS, is not printed or configured anywhere, and changes on
every `up`, but a page that found it could rebind to it. Closing that would mean replacing
`kubectl port-forward` with streams the CLI opens itself, so that no listener sits behind the
proxy.

*Amended by #677:* the Host check stops a rebound page, not an ordinary cross-site one. A page on
`https://evil.example` that calls `fetch("http://127.0.0.1:9001/storage/v1/b?project=p")` sends
`Host: 127.0.0.1:9001`, which passes. The builtin Cloud Storage server implemented Google's CORS
faithfully (#502): its JSON API allows **any** origin, echoing it, answers DELETE, PATCH and PUT
preflights, and sent `Access-Control-Allow-Credentials: true`. Google can do that because every
call needs an OAuth token that a foreign page does not have. CloudBurrow ignores `Authorization`,
so any page the developer had open could list, read, overwrite and delete every bucket, and read
the answers. The browser's `Origin` is what marks such a request, and a page cannot forge it.

Two options were weighed: keep Google's behaviour and state that any visited page can read and
write local Cloud Storage, or check the origin. **The origin is checked.** The storage server,
on every surface (JSON, uploads, downloads, batch, XML and its own `/_cloudburrow` endpoints),
serves a request that carries an `Origin` only when it is:

- **loopback**: `http://` or `https://` at a loopback address (`127.0.0.1`, any `127.x`, `[::1]`),
  `localhost` or a name under `.localhost`, on any port. Such a page is served from this machine,
  and a local web app under development is the case CORS support exists for;
- **the server's own origin**, the scheme and the `Host` the request came to, which the Host check
  has already vouched for: a page the server itself served;
- **on the allowlist** given by `up --cors-allow-origin` (repeatable or comma-separated;
  `CLOUDBURROW_CORS_ALLOW_ORIGIN`; config `storage.corsAllowOrigins`), passed to the in-cluster
  server as `--cors-allow-origin` in its Deployment's arguments and taken by `cloudburrow
  storage-server` too. Each entry is one exact origin; wildcards and paths are refused. A
  discovered `./cloudburrow.json` cannot set it (#598), since that would let a cloned repository
  open the emulator to a site of its choosing.

Any other origin, `null` included, gets **403 with no CORS headers**: a preflight is refused, so
the browser never sends the request it was for, and a simple request, which the browser sends
without asking, is refused before it runs, so it cannot write either. A request with no `Origin`
is not a browser's cross-origin call (the SDKs, `gcloud`, Terraform and `curl` send none) and is
not checked. Once an origin passes, Google's semantics apply unchanged: the XML API follows the
bucket's `cors` rules, the JSON API allows the origin whatever they say, and a resumable session
URI answers with the origin that started it. The bucket's rules are not an allowlist of their own:
a rule for `*` would otherwise open the server to every site.

`Access-Control-Allow-Credentials` is **dropped**. It was never observed from Google: the storage
oracle is storage-testbench and records no CORS behaviour, and the harness never calls Google.
CloudBurrow reads no cookies, so nothing depended on it. The echoed `Access-Control-Allow-Headers`
stays, still marked UNVERIFIED in the compatibility matrix, because a browser upload with a JSON
body needs it.

**What remains.** A page served from a loopback origin, which includes any other local dev server
and anything the developer serves on `localhost`, can use Cloud Storage fully. So can an origin the
developer allows. That is the intended trust boundary: code the developer chose to run locally.
The other service APIs CloudBurrow serves itself send no CORS headers, so a browser lets no
foreign page read their answers; a simple request a page can still send to them blind is not
covered by #677, and the upstream emulators behind the tunnels were not examined for it.

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
