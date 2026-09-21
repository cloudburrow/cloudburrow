package components

import (
	"context"
	"fmt"
)

// Ingress constants.
//
// The node ports are fixed because they must match the host mapping written
// into the kind configuration at cluster creation, and the two are set at
// different times: the mapping when the cluster is made, the Service when
// Knative is installed. A value discovered at install time could not be fed
// back into a cluster that already exists.
const (
	// IngressHTTPNodePort is the NodePort kourier serves HTTP on.
	IngressHTTPNodePort = 31080
	// IngressHTTPSNodePort is the NodePort kourier serves HTTPS on.
	IngressHTTPSNodePort = 31443

	// IngressNamespace is where the networking layer lives.
	IngressNamespace = "kourier-system"
	// IngressService is the Service a request enters the cluster through.
	IngressService = "kourier"
)

// DefaultDomain is the DNS suffix Knative gives services.
//
// `.localhost` is reserved by RFC 6761 and resolves to loopback without DNS
// egress, an /etc/hosts entry or a third-party wildcard service. It does not
// resolve everywhere — see docs/networking.md for the measured matrix — but
// where it fails, addressing the gateway directly with a Host header always
// works, so nothing is unreachable either way.
const DefaultDomain = "cloudburrow.localhost"

// PreviousDomain is the suffix CloudBurrow used before #86.
//
// It is removed rather than left in place: config-domain holding two default
// suffixes leaves Knative choosing between them, so an upgraded cluster would
// name services unpredictably.
const PreviousDomain = "127.0.0.1.sslip.io"

// ConfigureIngress publishes the Knative gateway on a fixed NodePort and sets
// the domain services are named under.
//
// Kourier ships as a LoadBalancer Service, which never gets an address on a
// local cluster: `kubectl get svc` shows `EXTERNAL-IP <pending>` forever. A
// NodePort is what a kind port mapping can actually reach.
func (i *Installer) ConfigureIngress(ctx context.Context, domain string) error {
	if domain == "" {
		domain = DefaultDomain
	}

	patch := fmt.Sprintf(`{"spec":{"type":"NodePort","ports":[`+
		`{"name":"http2","port":80,"targetPort":8080,"protocol":"TCP","nodePort":%d},`+
		`{"name":"https","port":443,"targetPort":8443,"protocol":"TCP","nodePort":%d}]}}`,
		IngressHTTPNodePort, IngressHTTPSNodePort)

	if _, err := i.kubectl(ctx, "", "patch", "service/"+IngressService,
		"-n", IngressNamespace, "--type", "merge", "-p", patch); err != nil {
		return fmt.Errorf("%w: publish the ingress gateway on a node port: %w", ErrInstallFailed, err)
	}

	// The domain replaces Knative's default `svc.cluster.local`, so a service
	// gets a name that means something outside the cluster.
	//
	// The previously configured suffix is explicitly removed. A merge patch
	// only adds keys, and two default suffixes in config-domain leave Knative
	// choosing between them — so an upgraded cluster would name services
	// unpredictably. null is a JSON merge patch deletion.
	domainPatch := fmt.Sprintf(`{"data":{%q:null,%q:""}}`, PreviousDomain, domain)
	if _, err := i.kubectl(ctx, "", "patch", "configmap/config-domain",
		"-n", "knative-serving", "--type", "merge", "-p", domainPatch); err != nil {
		return fmt.Errorf("%w: configure domain: %w", ErrInstallFailed, err)
	}
	return nil
}
