package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Revisions with traffic and no instances (#1800, #1802).
//
// Google (https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling,
// read 2026-11-18):
//
//   - "Specify a value of 0 to disable the service"; "requests that are
//     currently being processed will be allowed to complete. However, any
//     further requests to the service URL will fail with a Service
//     unavailable or Service disabled error";
//   - "Requests to service revisions that are only active due to traffic
//     tags are not impacted because those revisions are not disabled";
//   - "If the number of revisions receiving traffic exceeds the manual
//     instance count, some of the revisions will have no instances. Traffic
//     sent to those revisions will get the same error as if the revisions
//     were disabled."
//
// The page names no status code. 503 is UNVERIFIED (2026-11-18): it is the
// status of "Service Unavailable", and the body is "Service unavailable".
//
// Locally the service's URL is taken from Knative's route and given its own
// KIngress, <service>.unavailable, which splits the traffic as Knative
// would, revision by revision, except that each share of a revision with no
// instances goes to the unavailable responder: an Envoy (the gateway's own
// image, already on the node) answering every request 503 "Service
// unavailable". Knative stops serving the URL itself when its placeholder
// Service, named after the route, is labelled cluster-local: that is how
// Knative makes one traffic target cluster-local while the others stay
// public, so tag URLs keep their routes, and with them their revisions.
// The URL the service had is kept in annDisabledURL, and the service reads
// back with it.
const (
	labelVisibility     = "networking.knative.dev/visibility"
	visibilityClusterOn = "cluster-local"
	annDisabledURL      = "cloudburrow.dev/disabled-url"

	// unavailableName names the responder's ConfigMap, Deployment and
	// Service in the workload namespace.
	unavailableName = "cloudburrow-run-unavailable"
	// unavailableBody is what the responder answers.
	unavailableBody = "Service unavailable"
	// unavailableSuffix ends a service's own KIngress: a Cloud Run service
	// ID has no dot, so it never names a Knative route's KIngress.
	unavailableSuffix = ".unavailable"
	// gatewayDeployment is net-kourier's Envoy, whose image the responder
	// runs.
	gatewayDeployment = "3scale-kourier-gateway"
)

// unavailableApplied is, per KIngress, the manifest last applied, so the
// CPU driver's passes (every 10 s) apply nothing that is already there.
var unavailableApplied sync.Map

// offPercent is the percent of the traffic that goes to revisions with no
// instances.
func offPercent(shares []revisionShare, scales map[string]revisionScale) int32 {
	var off int32
	for _, sh := range shares {
		if sh.percent > 0 && scales[sh.revision].off {
			off += sh.percent
		}
	}
	return off
}

// applyUnavailable gives k's URL its own route while a revision with
// traffic has no instances, and gives it back to Knative once none has.
func (s *Server) applyUnavailable(ctx context.Context, k ksvc, shares []revisionShare, scales map[string]revisionScale) error {
	off := offPercent(shares, scales)
	_, marked := k.Metadata.Annotations[annDisabledURL]
	ingName := k.Metadata.Name + unavailableSuffix
	if off == 0 {
		if !marked {
			return nil
		}
		// Ours goes first, so two KIngresses never claim one host.
		if err := s.kn.deleteObject(ctx, resKIngress, ingName); err != nil {
			return err
		}
		unavailableApplied.Delete(k.Metadata.Namespace + "/" + ingName)
		if err := s.labelPlaceholder(ctx, k.Metadata.Name, false); err != nil {
			return err
		}
		return s.patchKsvc(ctx, k.Metadata.Name, map[string]any{"metadata": map[string]any{
			"labels": map[string]any{labelVisibility: nil}, "annotations": map[string]any{annDisabledURL: nil}}})
	}
	publicURL := serviceURL(k)
	host := urlHost(publicURL)
	if host == "" {
		return apierror.Internal(fmt.Errorf("no URL"), "route service %s's URL", k.Metadata.Name)
	}
	if !marked {
		if err := s.patchKsvc(ctx, k.Metadata.Name, map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{annDisabledURL: publicURL}}}); err != nil {
			return err
		}
	}
	manifest := unavailableIngress(k, ingName, host, shares, scales, off)
	key := k.Metadata.Namespace + "/" + ingName
	if prev, ok := unavailableApplied.Load(key); ok && prev == manifest {
		return nil
	}
	if err := s.ensureUnavailableResponder(ctx); err != nil {
		return err
	}
	if err := s.labelPlaceholder(ctx, k.Metadata.Name, true); err != nil {
		return err
	}
	s.awaitKnativeReleases(ctx, k.Metadata.Name, host)
	if err := s.kn.Kube.Apply(ctx, manifest, k8s.ApplyOptions{}); err != nil {
		return apierror.Internal(err, "route service %s's URL", k.Metadata.Name)
	}
	unavailableApplied.Store(key, manifest)
	return nil
}

func (s *Server) patchKsvc(ctx context.Context, name string, patch map[string]any) error {
	raw, _ := json.Marshal(patch)
	if err := s.kn.Kube.Patch(ctx, "ksvc", name, string(raw)); err != nil {
		return apierror.Internal(err, "mark service %s", name)
	}
	return nil
}

// labelPlaceholder makes the route's default target cluster-local, or
// public again. The placeholder Service is named after the route.
func (s *Server) labelPlaceholder(ctx context.Context, route string, local bool) error {
	var v any
	if local {
		v = visibilityClusterOn
	}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{labelVisibility: v}}})
	err := s.kn.Kube.Patch(ctx, "service", route, string(raw))
	if err != nil && !(k8s.IsNotFound(err) && !local) {
		return apierror.Internal(err, "set the visibility of service %s's URL", route)
	}
	return nil
}

// awaitKnativeReleases waits, at most 30 s, for Knative's KIngress of the
// route to stop claiming host, so two KIngresses never claim it at once.
func (s *Server) awaitKnativeReleases(ctx context.Context, route, host string) {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		var ing struct {
			Spec struct {
				Rules []struct {
					Hosts      []string `json:"hosts"`
					Visibility string   `json:"visibility"`
				} `json:"rules"`
			} `json:"spec"`
		}
		if err := s.kn.getObject(ctx, &ing, resKIngress, route, "route of "+route); err != nil {
			return
		}
		claimed := false
		for _, r := range ing.Spec.Rules {
			for _, h := range r.Hosts {
				if h == host {
					claimed = true
				}
			}
		}
		if !claimed {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// unavailableIngress renders k's own KIngress: host, split as Knative
// splits it (each revision's Service, port 80, with the revision headers
// Knative's activator reads), the revisions that are off folded into one
// share for the responder. It is owned by the Knative Service, so it goes
// with it.
func unavailableIngress(k ksvc, name, host string, shares []revisionShare, scales map[string]revisionScale, off int32) string {
	ns := k.Metadata.Namespace
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: networking.internal.knative.dev/v1alpha1
kind: Ingress
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "true"
  annotations:
    networking.knative.dev/ingress.class: %s
  ownerReferences:
    - apiVersion: serving.knative.dev/v1
      kind: Service
      name: %s
      uid: %s
spec:
  httpOption: Enabled
  rules:
    - hosts: [%q]
      visibility: ExternalIP
      http:
        paths:
          - splits:
`, name, ns, labelOwned, kourierClass, k.Metadata.Name, k.Metadata.UID, host)
	on := map[string]int32{}
	var order []string
	for _, sh := range shares {
		if sh.percent <= 0 || scales[sh.revision].off {
			continue
		}
		if _, seen := on[sh.revision]; !seen {
			order = append(order, sh.revision)
		}
		on[sh.revision] += sh.percent
	}
	for _, rev := range order {
		fmt.Fprintf(&b, `              - serviceName: %[1]s
                serviceNamespace: %[2]s
                servicePort: 80
                percent: %[3]d
                appendHeaders:
                  Knative-Serving-Namespace: %[2]s
                  Knative-Serving-Revision: %[1]s
`, rev, ns, on[rev])
	}
	fmt.Fprintf(&b, `              - serviceName: %s
                serviceNamespace: %s
                servicePort: 80
                percent: %d
`, unavailableName, ns, off)
	return b.String()
}

// ensureUnavailableResponder applies the responder, an Envoy answering
// every request 503 "Service unavailable", in the workload namespace. It
// runs the image the gateway runs, which the node already has.
func (s *Server) ensureUnavailableResponder(ctx context.Context) error {
	image, err := s.kn.Kube.Do(ctx, "", "get", "deployment", gatewayDeployment, "-n", components.IngressNamespace,
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	image = strings.TrimSpace(image)
	if err != nil || image == "" {
		return apierror.Internal(fmt.Errorf("%v %q", err, image), "find the gateway's image for the unavailable responder")
	}
	if err := s.kn.Kube.Apply(ctx, unavailableResponder(s.kn.namespace(), s.instance, image), k8s.ApplyOptions{}); err != nil {
		return apierror.Internal(err, "apply the unavailable responder")
	}
	return nil
}

// unavailableResponder renders the responder's ConfigMap, Deployment and
// Service.
func unavailableResponder(ns, instance, image string) string {
	envoy := fmt.Sprintf(`static_resources:
  listeners:
    - name: unavailable
      address: {socket_address: {address: 0.0.0.0, port_value: 8080}}
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: unavailable
                codec_type: AUTO
                route_config:
                  virtual_hosts:
                    - name: all
                      domains: ["*"]
                      routes:
                        - match: {prefix: "/"}
                          direct_response: {status: 503, body: {inline_string: %q}}
                          response_headers_to_add:
                            - header: {key: content-type, value: text/plain; charset=utf-8}
                              append_action: OVERWRITE_IF_EXISTS_OR_ADD
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`, unavailableBody)
	indented := "    " + strings.ReplaceAll(strings.TrimRight(envoy, "\n"), "\n", "\n    ")
	labels := fmt.Sprintf("    %s: \"true\"\n    %s: %q\n    app: %s\n", labelOwned, labelInstance, instance, unavailableName)
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
%[3]sdata:
  envoy.yaml: |
%[4]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
%[3]sspec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: envoy
          image: %[5]s
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/envoy"]
          args: ["-c", "/etc/unavailable/envoy.yaml", "--log-level", "warn"]
          ports:
            - containerPort: 8080
          readinessProbe:
            tcpSocket: {port: 8080}
          resources:
            requests: {cpu: 10m, memory: 32Mi}
          volumeMounts:
            - name: config
              mountPath: /etc/unavailable
      volumes:
        - name: config
          configMap:
            name: %[1]s
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
%[3]sspec:
  type: ClusterIP
  selector:
    app: %[1]s
  ports:
    - name: http
      port: 80
      targetPort: 8080
`, unavailableName, ns, labels, indented, image)
}

// urlHost is the host of a URL, "" when it has none.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
