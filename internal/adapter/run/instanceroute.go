package run

import (
	"context"
	"fmt"
	"strconv"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// A standalone instance's route (#1348): a ClusterIP Service selecting its
// pod, and a Knative networking Ingress (KIngress) the gateway (Kourier)
// serves, as Knative routes a service's revision. The host is
// <id>.instance.<namespace>.<domain>: an extra label, so it cannot collide
// with a service's <name>.<namespace>.<domain>.

const (
	resService  = "services"
	resKIngress = "ingresses.networking.internal.knative.dev"
	// kourierClass is the ingress class the gateway reconciles.
	kourierClass = "kourier.ingress.networking.knative.dev"
	// defaultServingPort is Cloud Run's: the PORT a container listens on
	// when it declares none.
	defaultServingPort = 8080
)

// instanceHost is the host an instance is reached at.
func (s *Server) instanceHost(w workload) string {
	return w.id + ".instance." + s.kn.namespace() + "." + components.DefaultDomain
}

// servingContainers returns the containers as they run: the ingress
// container (the one that declares a port, or the only one) given the
// port and, unless the caller set it, the PORT variable, as Cloud Run's
// container contract does. The caller's containers are not changed, so
// the stored spec reads back as set.
func servingContainers(cs []*runpb.Container) ([]*runpb.Container, int32) {
	idx := -1
	for i, c := range cs {
		if len(c.GetPorts()) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 && len(cs) == 1 {
		idx = 0
	}
	if idx < 0 {
		return cs, 0
	}
	out := make([]*runpb.Container, len(cs))
	copy(out, cs)
	c := proto.Clone(cs[idx]).(*runpb.Container)
	if len(c.Ports) == 0 {
		c.Ports = []*runpb.ContainerPort{{ContainerPort: defaultServingPort}}
	}
	port := c.Ports[0].GetContainerPort()
	if port == 0 {
		port = defaultServingPort
		c.Ports[0].ContainerPort = port
	}
	hasPort := false
	for _, e := range c.GetEnv() {
		hasPort = hasPort || e.GetName() == "PORT"
	}
	if !hasPort {
		c.Env = append(c.Env, &runpb.EnvVar{Name: "PORT", Values: &runpb.EnvVar_Value{Value: strconv.Itoa(int(port))}})
	}
	out[idx] = c
	return out, port
}

// instanceRoute renders the Service and KIngress of an instance.
func (s *Server) instanceRoute(w workload, port int32) string {
	ns := s.kn.namespace()
	labels := fmt.Sprintf("    %s: \"true\"\n    %s: %q\n    %s: %s\n", labelOwned, labelInstance, s.instance,
		labelWorkload, w.kind+"-"+w.id)
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
%[3]s  annotations:
    %[4]s: %[5]q
spec:
  type: ClusterIP
  selector:
    %[6]s: %[7]s
  ports:
    - name: http
      port: 80
      targetPort: %[8]d
---
apiVersion: networking.internal.knative.dev/v1alpha1
kind: Ingress
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
%[3]s  annotations:
    %[4]s: %[5]q
    networking.knative.dev/ingress.class: %[9]s
spec:
  httpOption: Enabled
  rules:
    - hosts: [%[10]q]
      visibility: ExternalIP
      http:
        paths:
          - splits:
              - serviceName: %[1]s
                serviceNamespace: %[2]s
                servicePort: 80
                percent: 100
`, w.objectName(), ns, labels, annRunName, w.name, labelWorkload, w.kind+"-"+w.id, port, kourierClass, s.instanceHost(w))
}

// applyInstanceRoute writes an instance's route. An instance with no
// serving container (several containers, none declaring a port) has none.
func (s *Server) applyInstanceRoute(ctx context.Context, w workload, port int32) error {
	if port == 0 {
		return s.deleteInstanceRoute(ctx, w)
	}
	if err := s.kn.Kube.Apply(ctx, s.instanceRoute(w, port), k8s.ApplyOptions{}); err != nil {
		return apierror.Internal(err, "apply the route of %s", w.name)
	}
	return nil
}

// deleteInstanceRoute removes an instance's route; a missing one is fine.
func (s *Server) deleteInstanceRoute(ctx context.Context, w workload) error {
	if err := s.kn.deleteObject(ctx, resKIngress, w.objectName()); err != nil {
		return err
	}
	return s.kn.deleteObject(ctx, resService, w.objectName())
}

// kingress is the subset of a KIngress its readiness is read from.
type kingress struct {
	Metadata kmeta `json:"metadata"`
	Status   struct {
		ObservedGeneration int64           `json:"observedGeneration"`
		Conditions         []ksvcCondition `json:"conditions"`
	} `json:"status"`
}

// instanceRouteReady reports whether the gateway serves an instance's host;
// an instance with no route is ready without one.
func (s *Server) instanceRouteReady(ctx context.Context, w workload) (bool, string) {
	var ing kingress
	if err := s.kn.getObject(ctx, &ing, resKIngress, w.objectName(), "route of "+w.name); err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return true, ""
		}
		return false, "the route is not readable: " + apierror.From(err).Message
	}
	if ing.Status.ObservedGeneration < ing.Metadata.Generation {
		return false, "the route is being programmed"
	}
	for _, c := range ing.Status.Conditions {
		if c.Type == "Ready" {
			if c.Status == "True" {
				return true, ""
			}
			return false, "the route is not ready: " + c.Message
		}
	}
	return false, "the route is being programmed"
}
