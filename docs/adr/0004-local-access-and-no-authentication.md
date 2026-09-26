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
