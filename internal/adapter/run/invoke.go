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
// run.routes.invoke check (#1879). ok is false when the host names no
// Cloud Run service: a Knative Service CloudBurrow did not make.
func (s *Server) InvokeTarget(ctx context.Context, host string) (name string, policy *iampolicy.Stored, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, _, _ := strings.Cut(host, ".")
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
