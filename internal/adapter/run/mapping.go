package run

import (
	"fmt"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"regexp"
	"sort"
	"strings"
	"time"

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
		gaps = append(gaps, podTemplateGaps("template", tmpl)...)
		if tmpl.GetSessionAffinity() {
			gaps = append(gaps, "template.sessionAffinity: not mapped")
		}
	}
	if svc.GetBinaryAuthorization() != nil {
		gaps = append(gaps, "binaryAuthorization: not enforced locally")
	}
	gaps = append(gaps, droppedFieldGaps(svc)...)
	if len(gaps) > 0 {
		return apierror.Unimplemented("unsupported Cloud Run configuration: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// podTemplate is what a service's RevisionTemplate and a job's TaskTemplate
// share: the pod-level configuration both render onto a Kubernetes pod.
type podTemplate interface {
	GetContainers() []*runpb.Container
	GetVolumes() []*runpb.Volume
	GetVpcAccess() *runpb.VpcAccess
	GetServiceAccount() string
	GetEncryptionKey() string
	GetExecutionEnvironment() runpb.ExecutionEnvironment
	GetNodeSelector() *runpb.NodeSelector
}

// podTemplateGaps names the pod-level fields neither a Knative revision nor
// a batch Job renders, so a service and a job refuse them in the same words.
// prefix is the template's path in the request: "template" for a service,
// "template.template" for a job.
func podTemplateGaps(prefix string, tmpl podTemplate) []string {
	var gaps []string
	if len(tmpl.GetVolumes()) > 0 {
		gaps = append(gaps, prefix+".volumes: not mapped")
	}
	if tmpl.GetVpcAccess() != nil {
		gaps = append(gaps, prefix+".vpcAccess: no VPC exists locally")
	}
	if tmpl.GetServiceAccount() != "" {
		gaps = append(gaps, prefix+".serviceAccount: CloudBurrow performs no authentication")
	}
	if tmpl.GetEncryptionKey() != "" {
		gaps = append(gaps, prefix+".encryptionKey: customer-managed encryption keys (CMEK) are not supported for Cloud Run")
	}
	// Listed because it was being dropped in silence: there is one runtime
	// locally, so a caller who set it and saw a successful create would
	// believe it took effect.
	if tmpl.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED {
		gaps = append(gaps, prefix+".executionEnvironment: there is one runtime locally, "+
			"so gen1 and gen2 have no meaning")
	}
	if tmpl.GetNodeSelector() != nil {
		gaps = append(gaps, prefix+".nodeSelector: there is one kind node and no GPU")
	}
	for _, c := range tmpl.GetContainers() {
		if len(c.GetVolumeMounts()) > 0 {
			gaps = append(gaps, "container.volumeMounts: not mapped")
			break
		}
	}
	for _, c := range tmpl.GetContainers() {
		add := func(set bool, field, why string) {
			if set {
				gaps = append(gaps, field+": "+why)
			}
		}
		add(len(c.GetDependsOn()) > 0, "container.dependsOn", "container start order is not mapped")
		add(c.GetReadinessProbe() != nil, "container.readinessProbe", "not mapped; use startupProbe")
		add(c.GetBaseImageUri() != "", "container.baseImageUri", "automatic base image updates are not mapped")
		add(c.GetSourceCode() != nil, "container.sourceCode", "source deploys are not run by the Cloud Run adapter")
	}
	return gaps
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
	return renderService(svc, namespace, instance, secrets, nil)
}

// renderService is ToKnative with the environment CloudBurrow injects into
// every container (#576), after the caller's and never over a name the
// caller set.
func renderService(svc *runpb.Service, namespace, instance string, secrets SecretResolver,
	injected []injectedEnv) (string, error) {
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
	for field, m := range map[string]map[string]string{"labels": svc.GetLabels(), "annotations": svc.GetAnnotations(),
		"template.labels": tmpl.GetLabels(), "template.annotations": tmpl.GetAnnotations()} {
		if err := validateMetadata(field, m); err != nil {
			return "", err
		}
	}
	// secretKeyRef env vars, recorded so they read back as set (#581).
	secretEnv := secretEnvRefs(tmpl.GetContainers())

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
`, id, namespace, instance, svc.GetName())
	jsonAnnotation(&b, "    ", annServiceLabels, svc.GetLabels(), len(svc.GetLabels()) == 0)
	jsonAnnotation(&b, "    ", annServiceAnnotations, svc.GetAnnotations(), len(svc.GetAnnotations()) == 0)
	if d := svc.GetDescription(); d != "" {
		fmt.Fprintf(&b, "    %s: %q\n", annDescription, d)
	}
	b.WriteString(`spec:
  template:
    metadata:
      annotations:
`)
	jsonAnnotation(&b, "        ", annTemplateLabels, tmpl.GetLabels(), len(tmpl.GetLabels()) == 0)
	jsonAnnotation(&b, "        ", annTemplateAnnotation, tmpl.GetAnnotations(), len(tmpl.GetAnnotations()) == 0)
	jsonAnnotation(&b, "        ", annSecretEnv, secretEnv, len(secretEnv) == 0)
	record := injectedRecord(tmpl.GetContainers(), injected)
	jsonAnnotation(&b, "        ", annInjectedEnv, record, len(record) == 0)

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
	if err := renderContainers(&b, tmpl.GetContainers(), containerRender{
		project: projectOf(svc.GetName()), secrets: secrets, injected: injected}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// containerRender is what differs between rendering a Knative revision's
// containers and a batch Job's pod's.
type containerRender struct {
	// project resolves a bare secret ID in a secretKeyRef.
	project string
	secrets SecretResolver
	// batch is a batch/v1 Job's pod: Kubernetes requires each container
	// to be named there, and a failing container's log tail is kept as its
	// termination message, which is what an execution reports.
	batch bool
	// injected is environment the runtime sets, rendered after the
	// caller's and skipped where the caller set the same name.
	injected []injectedEnv
}

// injectedEnv is one runtime-provided environment variable: a literal
// value, or a pod field.
type injectedEnv struct {
	name, value, fieldPath string
}

// secretEnvRefs records which Secret Manager secret each secretKeyRef env
// var names, so it reads back as the ValueSource that was set.
func secretEnvRefs(containers []*runpb.Container) map[string]secretEnvRef {
	out := map[string]secretEnvRef{}
	for _, c := range containers {
		for _, e := range c.GetEnv() {
			if ref := e.GetValueSource().GetSecretKeyRef(); ref != nil {
				out[e.GetName()] = secretEnvRef{Secret: ref.GetSecret(), Version: ref.GetVersion()}
			}
		}
	}
	return out
}

// renderContainers writes the containers list items of a pod spec, at the
// indent both a Knative Service and a batch Job place them
// (spec.template.spec.containers). A service and a job share it, so image
// localisation, env, secretKeyRef and resources are mapped once.
func renderContainers(b *strings.Builder, containers []*runpb.Container, o containerRender) error {
	for i, c := range containers {
		image := images.Localise(c.GetImage())
		if err := images.RequireTagged(image); err != nil {
			return apierror.InvalidArgument("%v", err)
		}
		fmt.Fprintf(b, "        - image: %s\n", image)
		if o.batch {
			fmt.Fprintf(b, "          name: %s\n", batchContainerName(c, i))
			// The tail of a failed container's log becomes its termination
			// message, so a failed execution says why without a log query.
			b.WriteString("          terminationMessagePolicy: FallbackToLogsOnError\n")
		}
		fmt.Fprintf(b, "          imagePullPolicy: %s\n", images.PullPolicy(image))
		if wd := c.GetWorkingDir(); wd != "" {
			fmt.Fprintf(b, "          workingDir: %q\n", wd)
		}
		var servingPort int32
		if ports := c.GetPorts(); len(ports) > 0 {
			servingPort = ports[0].GetContainerPort()
		}
		if err := renderProbe(b, "startupProbe", c.GetStartupProbe(), servingPort); err != nil {
			return err
		}
		if err := renderProbe(b, "livenessProbe", c.GetLivenessProbe(), servingPort); err != nil {
			return err
		}
		if len(c.GetCommand()) > 0 {
			fmt.Fprintf(b, "          command: [%s]\n", quote(c.GetCommand()))
		}
		if len(c.GetArgs()) > 0 {
			fmt.Fprintf(b, "          args: [%s]\n", quote(c.GetArgs()))
		}
		if ports := c.GetPorts(); len(ports) > 0 {
			b.WriteString("          ports:\n")
			for _, p := range ports {
				fmt.Fprintf(b, "            - containerPort: %d\n", p.GetContainerPort())
			}
		}
		if err := renderEnv(b, c.GetEnv(), o); err != nil {
			return err
		}
		if r := c.GetResources(); r != nil && len(r.GetLimits()) > 0 {
			b.WriteString("          resources:\n            limits:\n")
			for _, k := range sortedKeys(r.GetLimits()) {
				fmt.Fprintf(b, "              %s: %q\n", k, r.GetLimits()[k])
			}
		}
	}
	return nil
}

// batchContainerName is a container's name in a batch Job's pod: the one
// set, or one derived from its position, since Kubernetes requires a name.
func batchContainerName(c *runpb.Container, i int) string {
	if n := c.GetName(); n != "" {
		return n
	}
	return fmt.Sprintf("container-%d", i)
}

// renderEnv writes one container's env list: the caller's variables in name
// order, secretKeyRef resolved to the Kubernetes Secret, then the runtime's.
func renderEnv(b *strings.Builder, env []*runpb.EnvVar, o containerRender) error {
	set := map[string]bool{}
	for _, e := range env {
		set[e.GetName()] = true
	}
	var injected []injectedEnv
	for _, e := range o.injected {
		if !set[e.name] {
			injected = append(injected, e)
		}
	}
	if len(env) == 0 && len(injected) == 0 {
		return nil
	}
	b.WriteString("          env:\n")
	// Deterministic order so the same template renders identically.
	sorted := append([]*runpb.EnvVar(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })
	for _, e := range sorted {
		if src := e.GetValueSource(); src != nil {
			ref := src.GetSecretKeyRef()
			if ref == nil {
				return apierror.Unimplemented(
					"env %q uses a valueSource that is not a secretKeyRef", e.GetName())
			}
			if o.secrets == nil {
				// Refused rather than skipped: a container started
				// without an environment variable it asked for fails
				// somewhere far from the cause.
				return apierror.FailedPrecondition(
					"env %q references secret %q, but Secret Manager is not enabled on this instance",
					e.GetName(), ref.GetSecret())
			}
			name, key, err := o.secrets.ResolveSecretRef(o.project, ref.GetSecret(), ref.GetVersion())
			if err != nil {
				return err
			}
			fmt.Fprintf(b, "            - name: %s\n", e.GetName())
			fmt.Fprintf(b, "              valueFrom:\n")
			fmt.Fprintf(b, "                secretKeyRef:\n")
			fmt.Fprintf(b, "                  name: %s\n", name)
			fmt.Fprintf(b, "                  key: %s\n", key)
			continue
		}
		fmt.Fprintf(b, "            - name: %s\n              value: %q\n", e.GetName(), e.GetValue())
	}
	for _, e := range injected {
		if e.fieldPath != "" {
			fmt.Fprintf(b, "            - name: %s\n              valueFrom:\n                fieldRef:\n                  fieldPath: %q\n",
				e.name, e.fieldPath)
			continue
		}
		fmt.Fprintf(b, "            - name: %s\n              value: %q\n", e.name, e.value)
	}
	return nil
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
		ObservedGeneration:    k.Status.ObservedGeneration,
		Template:              &runpb.RevisionTemplate{},
		LatestReadyRevision:   k.Status.LatestReadyRevisionName,
		LatestCreatedRevision: k.Status.LatestCreatedRevisionName,
		// What a Terraform refresh or a GitOps controller compares (#581):
		// the etag is the object's resourceVersion, so it changes on every
		// write and a stale one is refused on update and delete.
		Uid:         k.Metadata.UID,
		Etag:        k.Metadata.ResourceVersion,
		Description: k.Metadata.Annotations[annDescription],
	}
	if !k.Metadata.CreationTimestamp.IsZero() {
		svc.CreateTime = timestamppb.New(k.Metadata.CreationTimestamp)
		svc.UpdateTime = timestamppb.New(k.lastTransition(k.Metadata.CreationTimestamp))
	}
	readJSONAnnotation(k.Metadata.Annotations, annServiceLabels, &svc.Labels)
	readJSONAnnotation(k.Metadata.Annotations, annServiceAnnotations, &svc.Annotations)
	tann := k.Spec.Template.Metadata.Annotations
	readJSONAnnotation(tann, annTemplateLabels, &svc.Template.Labels)
	readJSONAnnotation(tann, annTemplateAnnotation, &svc.Template.Annotations)
	secretEnv := map[string]secretEnvRef{}
	readJSONAnnotation(tann, annSecretEnv, &secretEnv)
	var scaling runpb.RevisionScaling
	fmt.Sscan(tann["autoscaling.knative.dev/min-scale"], &scaling.MinInstanceCount)
	fmt.Sscan(tann["autoscaling.knative.dev/max-scale"], &scaling.MaxInstanceCount)
	if scaling.MinInstanceCount > 0 || scaling.MaxInstanceCount > 0 {
		svc.Template.Scaling = &scaling
	}
	if ts := k.Spec.Template.Spec.TimeoutSeconds; ts > 0 {
		svc.Template.Timeout = durationpb.New(time.Duration(ts) * time.Second)
	}

	injected := injectedNames(tann)
	for _, c := range k.Spec.Template.Spec.Containers {
		container := &runpb.Container{Image: c.Image, WorkingDir: c.WorkingDir,
			StartupProbe: c.StartupProbe.toProbe(), LivenessProbe: c.LivenessProbe.toProbe()}
		for _, e := range c.Env {
			if injected[e.Name] {
				continue
			}
			ev := &runpb.EnvVar{Name: e.Name, Values: &runpb.EnvVar_Value{Value: e.Value}}
			if e.ValueFrom != nil {
				// Read back as the Secret Manager reference that was set, not
				// as an empty plain value; without the record, only the name.
				ev.Values = nil
				if ref, ok := secretEnv[e.Name]; ok {
					ev.Values = &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{Secret: ref.Secret, Version: ref.Version}}}
				}
			}
			container.Env = append(container.Env, ev)
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
