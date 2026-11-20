package run

// GKE destinations (#1877): a trigger's destination.gke names a service of
// a Kubernetes Engine cluster this instance serves (cluster, location,
// namespace, service, path), as Google's "Route events to GKE" documents
// it: the location defaults to the trigger's region and the namespace to
// "default". Google runs an event forwarder in the cluster that pulls the
// trigger's subscription and POSTs each CloudEvent to the service inside
// the cluster; here the push this process receives is POSTed through the
// cluster's API server proxy to the service's port 80, the port of the
// http://SERVICE.NAMESPACE.svc.cluster.local/PATH the forwarder reaches,
// with no token, as the forwarder sends none.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// GKEAccess is how a cluster's Kubernetes API is reached: its endpoint
// (host:port) and the base64-encoded PEM certificates the Kubernetes
// Engine API returns in MasterAuth.
type GKEAccess struct {
	Endpoint, CACert, ClientCert, ClientKey string
}

// eaDestinationDefaults fills what a destination leaves to the trigger's
// region: a Cloud Run service's region, a GKE cluster's location and its
// namespace.
func eaDestinationDefaults(t *eventarcpb.Trigger, region string) {
	if run := t.GetDestination().GetCloudRun(); run != nil && run.GetRegion() == "" {
		run.Region = region
	}
	if gke := t.GetDestination().GetGke(); gke != nil {
		if gke.GetLocation() == "" {
			gke.Location = region
		}
		if gke.GetNamespace() == "" {
			gke.Namespace = "default"
		}
	}
}

// eaCheckGKE checks a GKE destination's fields.
func eaCheckGKE(g *eventarcpb.GKE) error {
	switch {
	case g.GetCluster() == "":
		return apierror.InvalidArgument("destination.gke.cluster is required")
	case g.GetService() == "":
		return apierror.InvalidArgument("destination.gke.service is required")
	case strings.Contains(g.GetCluster(), "/"):
		return apierror.InvalidArgument("destination.gke.cluster %q must be the cluster's name, not a resource path", g.GetCluster())
	}
	return nil
}

// gkeClusterName is a GKE destination's cluster, in the trigger's project.
func gkeClusterName(project string, g *eventarcpb.GKE) string {
	return "projects/" + project + "/locations/" + g.GetLocation() + "/clusters/" + g.GetCluster()
}

// checkGKEDestination refuses a GKE destination whose cluster this
// instance does not serve, or that is not running.
func (f *FunctionsServer) checkGKEDestination(project string, t *eventarcpb.Trigger) error {
	g := t.GetDestination().GetGke()
	if g == nil {
		return nil
	}
	if f.events == nil || f.events.GKE == nil {
		return apierror.InvalidArgument("destination.gke.cluster %q not found: this instance does not serve the Kubernetes Engine API "+
			"(start it with --services gke)", g.GetCluster())
	}
	if _, ok := f.events.GKE(gkeClusterName(project, g)); !ok {
		return apierror.InvalidArgument("destination.gke.cluster %q not found in %s, or not running", g.GetCluster(), g.GetLocation())
	}
	return nil
}

// deliverGKE posts one CloudEvent to a GKE destination's service, through
// its cluster's API server proxy.
func (f *FunctionsServer) deliverGKE(ctx context.Context, project string, g *eventarcpb.GKE, ev *CloudEvent) (int, error) {
	if f.events.GKE == nil {
		return 0, fmt.Errorf("the Kubernetes Engine API is not served")
	}
	a, ok := f.events.GKE(gkeClusterName(project, g))
	if !ok {
		return 0, fmt.Errorf("the destination cluster %s is not running", gkeClusterName(project, g))
	}
	client, err := gkeClient(a)
	if err != nil {
		return 0, err
	}
	path := g.GetPath()
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := "https://" + a.Endpoint + "/api/v1/namespaces/" + url.PathEscape(g.GetNamespace()) + "/services/" +
		url.PathEscape(g.GetService()) + ":80/proxy" + path
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(ev.Data)))
	if err != nil {
		return 0, err
	}
	for k, v := range ev.Headers {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// gkeClient is an HTTP client that authenticates to a cluster's API
// server with its client certificate.
func gkeClient(a GKEAccess) (*http.Client, error) {
	ca, err := base64.StdEncoding.DecodeString(a.CACert)
	if err != nil {
		return nil, fmt.Errorf("the cluster's CA certificate: %w", err)
	}
	cert, err := base64.StdEncoding.DecodeString(a.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("the cluster's client certificate: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(a.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("the cluster's client key: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("the cluster's client certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("the cluster's CA certificate is not PEM")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}}, nil
}
