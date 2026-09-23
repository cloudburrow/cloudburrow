package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// ingressMappings returns the host-to-node port mapping for the cluster
// ingress gateway.
//
// Only HTTP is mapped. HTTPS would need a certificate CloudBurrow does not
// issue, and publishing a port that answers with a self-signed certificate
// would look like support for something that does not work.
func ingressMappings(cfg config.Config) []cluster.PortMapping {
	if cfg.Endpoints.Ingress == 0 {
		// kind cannot be asked to choose a host port and report it back, so
		// an OS-assigned ingress port is not offered. Nothing is published
		// and the gateway is reached through a port-forward instead.
		return nil
	}
	return []cluster.PortMapping{{
		HostPort:      cfg.Endpoints.Ingress,
		NodePort:      components.IngressHTTPNodePort,
		ListenAddress: cfg.BindAddress,
	}}
}

// checkIngress reports whether the gateway is actually reachable on the host
// port, and says what to do when it is not.
//
// This matters because `extraPortMappings` are applied only when a cluster is
// created. A cluster made before this feature existed, or with a different
// port, keeps running perfectly well and simply has nothing published — and a
// silent nothing is exactly the failure a developer cannot diagnose.
func checkIngress(ctx context.Context, cfg config.Config) (reachable bool, reason string) {
	if cfg.Endpoints.Ingress == 0 {
		return false, "no ingress port is configured"
	}
	addr := net.JoinHostPort(cfg.BindAddress, fmt.Sprint(cfg.Endpoints.Ingress))

	// The gateway takes a moment to answer after the Service is patched, so
	// this is a short poll rather than a single attempt: reporting "not
	// published" for a gateway that was merely slow would send a developer to
	// delete a working cluster.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true, ""
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, fmt.Sprintf("nothing is listening on %s", addr)
		}
		select {
		case <-ctx.Done():
			return false, "cancelled"
		case <-time.After(time.Second):
		}
	}
}

// printIngress reports the gateway, or explains its absence.
func printIngress(w io.Writer, cfg config.Config, reachable bool, reason string) {
	if reachable {
		fmt.Fprintf(w, "  ingress:    http://%s:%d  (Cloud Run services are served here)\n",
			cfg.BindAddress, cfg.Endpoints.Ingress)
		fmt.Fprintf(w, "              a service is reachable at http://<service>.<namespace>.%s:%d\n",
			components.DefaultDomain, cfg.Endpoints.Ingress)
		return
	}
	fmt.Fprintf(w, "  ingress:    NOT PUBLISHED — %s\n", reason)
	fmt.Fprintln(w, "              a host port is mapped only when the cluster is created, so a")
	fmt.Fprintln(w, "              cluster made earlier or with a different port has none.")
	fmt.Fprintln(w, "              `cloudburrow delete` then `cloudburrow up` publishes it.")
	fmt.Fprintf(w, "              until then: kubectl --kubeconfig %s -n %s port-forward svc/%s-internal 8080:80\n",
		cfg.KubeconfigPath(), components.IngressNamespace, components.IngressService)
}
