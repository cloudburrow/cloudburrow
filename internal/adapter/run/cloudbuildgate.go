package run

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// The host-steps gate (#1382, ADR-0004 "Cloud Build steps get the host's
// Docker socket"). A build step that is not a docker build, docker push or
// pack step runs as a container on the host's Docker with the Docker socket
// mounted, as Google's build worker runs it. That container can start any
// other container on the host, with any of the host's files mounted, so it
// is as good as a shell on the developer's machine. Such a build is
// therefore refused unless the instance was started with
// --cloudbuild-host-steps, and even then it is accepted only from the
// host's loopback: never from a pod, which reaches the instance through
// cloudburrow-host, and never from another machine.

// HostStepsFlag is the `up` flag that lets arbitrary build steps run.
const HostStepsFlag = "--cloudbuild-host-steps"

// AllowHostSteps sets whether CreateBuild and RetryBuild run arbitrary
// steps (#1382): off, the default, refuses every build that has one.
func (b *BuildsServer) AllowHostSteps(on bool) *BuildsServer {
	b.hostSteps = on
	return b
}

// callerKey carries a JSON request's Host and remote address to the
// handler, as gRPC carries them in :authority and the peer.
type callerKey struct{}

type caller struct{ host, remote string }

// withCaller records r's Host and remote address on its context, for
// fromHostLoopback.
func withCaller(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{host: r.Host, remote: r.RemoteAddr}))
}

// callerOf returns the name the request was addressed to and the address
// it came from: a JSON request's Host and remote address, or a gRPC call's
// :authority and peer.
func callerOf(ctx context.Context) (host, remote string) {
	if c, ok := ctx.Value(callerKey{}).(caller); ok {
		return c.host, c.remote
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get(":authority"); len(a) > 0 {
			host = a[0]
		}
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		remote = p.Addr.String()
	}
	return host, remote
}

// loopbackRefusal says why a request did not come from the host's
// loopback, or "" when it did: from a loopback address, addressed to a
// loopback name (127.0.0.0/8, [::1], localhost, *.localhost).
//
// The address alone cannot tell a pod from the host. Docker Desktop
// delivers a pod's connection to host.docker.internal from the host's own
// loopback, and on Linux the relay at the kind network's gateway forwards
// it from loopback too. What marks a pod's request is the name it dialed:
// cloudburrow-host, host.docker.internal, the gateway's address or the
// Service's cluster IP. So both must be loopback. A client in a pod that
// dials cloudburrow-host and writes a loopback Host by hand is not caught:
// ADR-0004 records this.
func loopbackRefusal(ctx context.Context) string {
	host, remote := callerOf(ctx)
	if host == "" || remote == "" {
		return "the request carried no address to check"
	}
	ra, err := netip.ParseAddrPort(remote)
	if err != nil || !ra.Addr().Unmap().IsLoopback() {
		return "it came from " + remote + ", not the host's loopback"
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.ToLower(strings.Trim(name, "[]")), ".")
	switch {
	case hostguard.IsServiceName(name, hostguard.ClusterHostService):
		return "it came through " + hostguard.ClusterHostService + ", the name pods use"
	case name == "host.docker.internal":
		return "it came through host.docker.internal, the name containers use"
	case name == "localhost" || strings.HasSuffix(name, ".localhost"):
		return ""
	}
	if a, err := netip.ParseAddr(name); err == nil && a.Unmap().IsLoopback() {
		return ""
	}
	return "it was addressed to " + host + ", not a loopback address"
}

// hostStepsRefusal refuses a build whose step i (image name) needs the
// host's Docker socket, unless the instance allows host steps and the
// request came from the host's loopback.
func (c *CloudBuildServer) hostStepsRefusal(ctx context.Context, i int, name string) error {
	if !c.builds.hostSteps {
		return apierror.FailedPrecondition("build step %d (%s) is not a docker build, docker push or pack step, "+
			"and this instance runs such steps only when started with %s: each one runs as a container on the "+
			"host's Docker with the host's Docker socket mounted, which gives it control of the host (ADR-0004)",
			i, name, HostStepsFlag)
	}
	if why := loopbackRefusal(ctx); why != "" {
		return apierror.FailedPrecondition("build step %d (%s) is not a docker build, docker push or pack step, "+
			"and such a step runs with the host's Docker socket, so it is accepted only from the host's loopback, "+
			"never from a pod or another machine; this request was refused because %s (ADR-0004)", i, name, why)
	}
	return nil
}
