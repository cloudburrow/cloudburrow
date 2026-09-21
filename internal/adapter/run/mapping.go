package run

import (
	"fmt"
	"sort"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/images"
	"github.com/identity-wael/cloudburrow/internal/resource"
)

// Unsupported reports Cloud Run configuration this adapter does not map.
//
// Returning these as errors rather than ignoring them is the point: a caller
// who set a field we silently dropped would believe it took effect. Cloud Run
// and Knative are different systems, and the gaps belong in the open.
func Unsupported(svc *runpb.Service) error {
	var gaps []string

	if len(svc.GetTraffic()) > 1 {
		// Knative supports traffic splitting, but the mapping is not
		// implemented or tested, so it is not claimed. See #30.
		gaps = append(gaps, "traffic: splitting across multiple revisions is not mapped")
	}
	if tmpl := svc.GetTemplate(); tmpl != nil {
		if len(tmpl.GetVolumes()) > 0 {
			gaps = append(gaps, "template.volumes: not mapped")
		}
		if tmpl.GetVpcAccess() != nil {
			gaps = append(gaps, "template.vpcAccess: no VPC exists locally")
		}
		if tmpl.GetServiceAccount() != "" {
			gaps = append(gaps, "template.serviceAccount: CloudBurrow performs no authentication")
		}
		if tmpl.GetEncryptionKey() != "" {
			gaps = append(gaps, "template.encryptionKey: no KMS exists locally")
		}
		for _, c := range tmpl.GetContainers() {
			if len(c.GetVolumeMounts()) > 0 {
				gaps = append(gaps, "container.volumeMounts: not mapped")
				break
			}
		}
	}
	if svc.GetBinaryAuthorization() != nil {
		gaps = append(gaps, "binaryAuthorization: not enforced locally")
	}
	if len(gaps) > 0 {
		return apierror.Unimplemented("unsupported Cloud Run configuration: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// ServiceID extracts the Cloud Run service ID from a resource name.
func ServiceID(name string) (string, error) {
	n, err := resource.Parse(name)
	if err != nil {
		return "", apierror.InvalidArgument("%v", err)
	}
	if n.Collection != "services" {
		return "", apierror.InvalidArgument("%q is not a Cloud Run service name", name)
	}
	return n.ID, nil
}

// ToKnative renders a Cloud Run v2 Service as a Knative Service manifest.
//
// Image references are localised: Knative resolves tags to digests by
// contacting the registry, so a locally built image must carry a prefix
// Knative skips, and must never be pulled (docs/local-verification.md §5.1).
func ToKnative(svc *runpb.Service, namespace, instance string) (string, error) {
	if err := Unsupported(svc); err != nil {
		return "", err
	}
	id, err := ServiceID(svc.GetName())
	if err != nil {
		return "", err
	}
	tmpl := svc.GetTemplate()
	if tmpl == nil || len(tmpl.GetContainers()) == 0 {
		return "", apierror.InvalidArgument("service requires template.containers")
	}

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
  annotations:
    cloudburrow.dev/cloud-run-name: %q
spec:
  template:
    metadata:
      annotations:
`, id, namespace, instance, svc.GetName())

	// Scaling. Cloud Run's min/max instances map onto Knative's autoscaling
	// annotations, which is one of the few places the two line up directly.
	if s := tmpl.GetScaling(); s != nil {
		if s.GetMinInstanceCount() > 0 {
			fmt.Fprintf(&b, "        autoscaling.knative.dev/min-scale: %q\n", fmt.Sprint(s.GetMinInstanceCount()))
		}
		if s.GetMaxInstanceCount() > 0 {
			fmt.Fprintf(&b, "        autoscaling.knative.dev/max-scale: %q\n", fmt.Sprint(s.GetMaxInstanceCount()))
		}
	}
	fmt.Fprintf(&b, "        cloudburrow.dev/managed: \"true\"\n")

	b.WriteString("    spec:\n")
	if c := tmpl.GetMaxInstanceRequestConcurrency(); c > 0 {
		fmt.Fprintf(&b, "      containerConcurrency: %d\n", c)
	}
	b.WriteString("      containers:\n")

	for _, c := range tmpl.GetContainers() {
		image := images.Localise(c.GetImage())
		if err := images.RequireTagged(image); err != nil {
			return "", apierror.InvalidArgument("%v", err)
		}
		fmt.Fprintf(&b, "        - image: %s\n", image)
		fmt.Fprintf(&b, "          imagePullPolicy: %s\n", images.PullPolicy(image))
		if len(c.GetCommand()) > 0 {
			fmt.Fprintf(&b, "          command: [%s]\n", quote(c.GetCommand()))
		}
		if len(c.GetArgs()) > 0 {
			fmt.Fprintf(&b, "          args: [%s]\n", quote(c.GetArgs()))
		}
		if ports := c.GetPorts(); len(ports) > 0 {
			b.WriteString("          ports:\n")
			for _, p := range ports {
				fmt.Fprintf(&b, "            - containerPort: %d\n", p.GetContainerPort())
			}
		}
		if env := c.GetEnv(); len(env) > 0 {
			b.WriteString("          env:\n")
			// Deterministic order so the same Service renders identically.
			sorted := append([]*runpb.EnvVar(nil), env...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })
			for _, e := range sorted {
				if e.GetValueSource() != nil {
					return "", apierror.Unimplemented(
						"env %q uses valueSource, which requires Secret Manager and is not mapped", e.GetName())
				}
				fmt.Fprintf(&b, "            - name: %s\n              value: %q\n", e.GetName(), e.GetValue())
			}
		}
		if r := c.GetResources(); r != nil && len(r.GetLimits()) > 0 {
			b.WriteString("          resources:\n            limits:\n")
			keys := make([]string, 0, len(r.GetLimits()))
			for k := range r.GetLimits() {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "              %s: %q\n", k, r.GetLimits()[k])
			}
		}
	}
	return b.String(), nil
}

// FromKnative renders a Knative Service back as a Cloud Run v2 Service.
func FromKnative(k ksvc, parent string) *runpb.Service {
	name := k.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]
	if name == "" {
		name = fmt.Sprintf("%s/services/%s", parent, k.Metadata.Name)
	}

	svc := &runpb.Service{
		Name:                  name,
		Uri:                   k.Status.URL,
		Generation:            k.Metadata.Generation,
		Template:              &runpb.RevisionTemplate{},
		LatestReadyRevision:   k.Status.LatestReadyRevisionName,
		LatestCreatedRevision: k.Status.LatestCreatedRevisionName,
	}

	for _, c := range k.Spec.Template.Spec.Containers {
		container := &runpb.Container{Image: c.Image}
		for _, e := range c.Env {
			container.Env = append(container.Env, &runpb.EnvVar{
				Name:   e.Name,
				Values: &runpb.EnvVar_Value{Value: e.Value},
			})
		}
		for _, p := range c.Ports {
			container.Ports = append(container.Ports, &runpb.ContainerPort{ContainerPort: int32(p.ContainerPort)})
		}
		svc.Template.Containers = append(svc.Template.Containers, container)
	}
	if cc := k.Spec.Template.Spec.ContainerConcurrency; cc > 0 {
		svc.Template.MaxInstanceRequestConcurrency = int32(cc)
	}

	// Readiness is reported truthfully, including the failure reason. A caller
	// that only saw a URL would treat a failed revision as merely slow.
	ready, reason := k.Ready()
	cond := &runpb.Condition{Type: "Ready", State: runpb.Condition_CONDITION_PENDING}
	if ready {
		cond.State = runpb.Condition_CONDITION_SUCCEEDED
	} else if reason != "" {
		cond.State = runpb.Condition_CONDITION_FAILED
		cond.Message = reason
	}
	svc.TerminalCondition = cond
	return svc
}

func quote(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
