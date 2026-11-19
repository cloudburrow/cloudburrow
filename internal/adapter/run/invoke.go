package run

import (
	"context"
	"net"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// InvokeTarget is the Cloud Run service a gateway request's Host names
// (<knative name>.<namespace>.<domain>, or a traffic tag's
// <tag>-<knative name>...), with the service's own IAM policy, for the
// run.routes.invoke check (#1879), or a domain mapping's host (its
// service). ok is false when the host names no
// Cloud Run service: a Knative Service CloudBurrow did not make.
func (s *Server) InvokeTarget(ctx context.Context, host string) (name string, policy *iampolicy.Stored, ok bool) {
	return s.invokeTarget(ctx, host, true)
}

// ServiceAt is the Cloud Run service whose own URL (or a tag's) host is,
// "" for none; a domain mapping's host is not one, as an ID token for a
// custom domain is not the service's audience on Google.
func (s *Server) ServiceAt(ctx context.Context, host string) string {
	name, _, _ := s.invokeTarget(ctx, host, false)
	return name
}

func (s *Server) invokeTarget(ctx context.Context, host string, domains bool) (name string, policy *iampolicy.Stored, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// A domain mapping's host: Knative's DomainMapping of that name refers
	// to the service.
	var dm struct {
		Spec struct {
			Ref struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ref"`
		} `json:"spec"`
	}
	label, _, _ := strings.Cut(host, ".")
	if domains && strings.Contains(host, ".") && s.kn.getObject(ctx, &dm, resKDomain, strings.ToLower(host), "domain mapping "+host) == nil &&
		dm.Spec.Ref.Kind == "Service" && dm.Spec.Ref.Name != "" {
		label = dm.Spec.Ref.Name
	}
	for label != "" {
		if k, err := s.kn.Get(ctx, label); err == nil {
			if name = k.Metadata.Annotations[annRunName]; name == "" {
				return "", nil, false
			}
			o := iamOwner{kind: "service", id: k.Metadata.Name, name: name, apiVersion: "serving.knative.dev/v1",
				objKind: "Service", objName: k.Metadata.Name, uid: k.Metadata.UID}
			p, _, err := s.loadPolicy(ctx, o)
			if err != nil {
				return name, nil, true
			}
			return name, p, true
		}
		_, rest, cut := strings.Cut(label, "-")
		if !cut {
			break
		}
		label = rest
	}
	return "", nil, false
}
