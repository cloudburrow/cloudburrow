package run

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/resource"
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
	} else if len(svc.GetTraffic()) == 1 {
		// One target is accepted only when it is what the adapter does
		// anyway: all traffic to the latest revision. Pinning a named
		// revision, or sending less than 100%, would be accepted and ignored.
		t := svc.GetTraffic()[0]
		latest := t.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST ||
			(t.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_UNSPECIFIED && t.GetRevision() == "")
		if !latest || (t.GetPercent() != 0 && t.GetPercent() != 100) || t.GetTag() != "" {
			gaps = append(gaps, "traffic: only 100% to the latest revision is mapped")
		}
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
		// Listed because they were being dropped in silence. Knative has no
		// equivalent for either, so a caller who set one and saw a successful
		// create would believe it took effect.
		if tmpl.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED {
			gaps = append(gaps, "template.executionEnvironment: there is one runtime locally, "+
				"so gen1 and gen2 have no meaning")
		}
		if tmpl.GetSessionAffinity() {
			gaps = append(gaps, "template.sessionAffinity: not mapped")
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

// serviceIDRE is the Kubernetes object-name rule a Knative Service must
// satisfy, narrowed to Cloud Run's 49-character limit.
var serviceIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ValidateServiceID checks a service ID before anything is applied.
//
// Without this, an invalid name reaches the cluster and comes back as an
// apply failure — reported as Internal, which says the fault is ours when it
// is the caller's, and buries the actual rule.
func ValidateServiceID(id string) error {
	if id == "" {
		return apierror.InvalidArgument("serviceId is required")
	}
	if len(id) > 49 {
		return apierror.InvalidArgument(
			"service name %q is %d characters; Cloud Run allows at most 49", id, len(id))
	}
	if !serviceIDRE.MatchString(id) {
		return apierror.InvalidArgument(
			"service name %q is not valid: use lowercase letters, digits and hyphens, "+
				"starting with a letter and not ending with a hyphen", id)
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
// WorkloadNamespace is where Cloud Run workloads are deployed.
//
// It is named here rather than repeated as a literal because a Kubernetes
// Secret is namespace-local: anything a workload references through
// secretKeyRef must live in this same namespace, and two independent
// spellings of it would drift into a reference that cannot resolve.
const WorkloadNamespace = "default"

// SecretResolver resolves a Cloud Run secret reference to the Kubernetes
// Secret and data key that hold its payload.
//
// It is an interface here rather than a concrete dependency so the Cloud Run
// adapter does not import the Secret Manager implementation: the adapter
// needs to know that a secret can be resolved, not how.
type SecretResolver interface {
	// ResolveSecretRef returns the Kubernetes Secret name and data key for a
	// secret reference. project is the service's project, used when the
	// reference names a bare secret ID rather than a full resource name.
	ResolveSecretRef(project, secret, version string) (secretName, dataKey string, err error)
}

func ToKnative(svc *runpb.Service, namespace, instance string, secrets SecretResolver) (string, error) {
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
	// Cloud Run's request timeout is Knative's timeoutSeconds. It was being
	// dropped, so a service deployed with a 10-minute timeout got Knative's
	// default and failed at 5 with nothing on screen to explain it.
	if t := tmpl.GetTimeout(); t != nil && t.GetSeconds() > 0 {
		fmt.Fprintf(&b, "      timeoutSeconds: %d\n", t.GetSeconds())
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
				if src := e.GetValueSource(); src != nil {
					ref := src.GetSecretKeyRef()
					if ref == nil {
						return "", apierror.Unimplemented(
							"env %q uses a valueSource that is not a secretKeyRef", e.GetName())
					}
					if secrets == nil {
						// Refused rather than skipped: a container started
						// without an environment variable it asked for fails
						// somewhere far from the cause.
						return "", apierror.FailedPrecondition(
							"env %q references secret %q, but Secret Manager is not enabled on this instance",
							e.GetName(), ref.GetSecret())
					}
					name, key, err := secrets.ResolveSecretRef(
						projectOf(svc.GetName()), ref.GetSecret(), ref.GetVersion())
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&b, "            - name: %s\n", e.GetName())
					fmt.Fprintf(&b, "              valueFrom:\n")
					fmt.Fprintf(&b, "                secretKeyRef:\n")
					fmt.Fprintf(&b, "                  name: %s\n", name)
					fmt.Fprintf(&b, "                  key: %s\n", key)
					continue
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

// projectOf extracts the project from a Cloud Run service name, so a bare
// secret ID resolves in the same project as the service referencing it.
func projectOf(serviceName string) string {
	parts := strings.Split(serviceName, "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

func quote(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
