package run

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints on run.googleapis.com/Service (#1891), Job and
// WorkerPool (#1895), CREATE and UPDATE, over v2 and v1 (which creates
// and replaces through v2). "Cloud Run custom constraints must be
// specified by using the Cloud Run Admin API v1 field specifications",
// and "Labels and top-level fields are not supported" ("Use custom
// organization policies", Cloud Run, read 2026-11-19): the condition
// reads the resource in its v1 shape (resource.metadata.annotations,
// resource.spec.template.spec.containers...), without its labels, its
// apiVersion and kind, or its status. The v1 shape carries in
// annotations what v2 holds in fields; each one Google's v1 ObjectMeta
// documents for the resource is built from the v2 field it stands for
// (v1ServiceAnnotations, v1RevisionAnnotations...). A violation is
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]".

// The v1 annotations built from v2 fields (Cloud Run Admin API v1,
// ObjectMeta.annotations).
const (
	v1AnnBinaryAuthorization = "run.googleapis.com/binary-authorization"
	v1AnnBreakglass          = "run.googleapis.com/binary-authorization-breakglass"
	v1AnnDefaultURLDisabled  = "run.googleapis.com/default-url-disabled"
	v1AnnDescription         = "run.googleapis.com/description"
	v1AnnCustomAudiences     = "run.googleapis.com/custom-audiences"
	v1AnnBaseImages          = "run.googleapis.com/base-images"
	v1AnnBuildEnv            = "run.googleapis.com/build-environment-variables"
	v1AnnBuildServiceAccount = "run.googleapis.com/build-service-account"
	v1AnnBuildWorkerPool     = "run.googleapis.com/build-worker-pool"
	v1AnnCPUThrottling       = "run.googleapis.com/cpu-throttling"
	v1AnnShutdownHours       = "run.googleapis.com/encryption-key-shutdown-hours"
	v1AnnGPUZonal            = "run.googleapis.com/gpu-zonal-redundancy-disabled"
	v1AnnHealthCheckDisabled = "run.googleapis.com/health-check-disabled"
	v1AnnRevocationAction    = "run.googleapis.com/post-key-revocation-action-type"
	v1AnnSessionAffinity     = "run.googleapis.com/sessionAffinity"
	v1AnnExecutionEnv        = "run.googleapis.com/execution-environment"
	v1AnnVPCConnector        = "run.googleapis.com/vpc-access-connector"
	v1AnnVPCEgress           = "run.googleapis.com/vpc-access-egress"
	v1AnnNetworkInterfaces   = "run.googleapis.com/network-interfaces"
	v1AnnEncryptionKey       = "run.googleapis.com/encryption-key"
	v1AnnContainerDeps       = "run.googleapis.com/container-dependencies"
)

// jsonAnn is v as the JSON string an annotation holds.
func jsonAnn(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func setTrue(ann map[string]string, key string, on bool) {
	if on {
		ann[key] = "true"
	}
}

// binauthzAnnotations sets run.googleapis.com/binary-authorization
// ("default" or the policy) and its breakglass justification.
func binauthzAnnotations(ann map[string]string, ba *runpb.BinaryAuthorization) {
	if ba.GetUseDefault() {
		ann[v1AnnBinaryAuthorization] = "default"
	} else if ba.GetPolicy() != "" {
		ann[v1AnnBinaryAuthorization] = ba.GetPolicy()
	}
	if j := ba.GetBreakglassJustification(); j != "" {
		ann[v1AnnBreakglass] = j
	}
}

func launchStageAnnotation(ann map[string]string, ls api.LaunchStage) {
	if ls != api.LaunchStage_LAUNCH_STAGE_UNSPECIFIED {
		ann[v1LaunchStage] = ls.String()
	}
}

// vpcAnnotations sets the VPC access annotations of a revision, task or
// worker pool template: the connector, the egress (all-traffic or
// private-ranges-only), and the Direct VPC network interfaces.
func vpcAnnotations(ann map[string]string, v *runpb.VpcAccess) {
	if v == nil {
		return
	}
	if v.GetConnector() != "" {
		ann[v1AnnVPCConnector] = v.GetConnector()
	}
	switch v.GetEgress() {
	case runpb.VpcAccess_ALL_TRAFFIC:
		ann[v1AnnVPCEgress] = "all-traffic"
	case runpb.VpcAccess_PRIVATE_RANGES_ONLY:
		ann[v1AnnVPCEgress] = "private-ranges-only"
	}
	if nis := v.GetNetworkInterfaces(); len(nis) > 0 {
		type ni struct {
			Network    string   `json:"network,omitempty"`
			Subnetwork string   `json:"subnetwork,omitempty"`
			Tags       []string `json:"tags,omitempty"`
		}
		var out []ni
		for _, n := range nis {
			out = append(out, ni{Network: n.GetNetwork(), Subnetwork: n.GetSubnetwork(), Tags: n.GetTags()})
		}
		ann[v1AnnNetworkInterfaces] = jsonAnn(out)
	}
}

// cmekAnnotations sets the customer-managed encryption key annotations.
func cmekAnnotations(ann map[string]string, key string, action runpb.EncryptionKeyRevocationAction, shutdown *durationpb.Duration) {
	if key != "" {
		ann[v1AnnEncryptionKey] = key
	}
	switch action {
	case runpb.EncryptionKeyRevocationAction_PREVENT_NEW:
		ann[v1AnnRevocationAction] = "PREVENT_NEW"
	case runpb.EncryptionKeyRevocationAction_SHUTDOWN:
		ann[v1AnnRevocationAction] = "SHUTDOWN"
	}
	if shutdown != nil {
		ann[v1AnnShutdownHours] = strconv.FormatInt(int64(shutdown.AsDuration().Hours()), 10)
	}
}

func executionEnvAnnotation(ann map[string]string, e runpb.ExecutionEnvironment) {
	switch e {
	case runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN1:
		ann[v1AnnExecutionEnv] = "gen1"
	case runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2:
		ann[v1AnnExecutionEnv] = "gen2"
	}
}

// containerAnnotations sets what a revision's containers hold that v1
// carries in annotations: their dependencies, CPU throttling and
// startup boost.
func containerAnnotations(ann map[string]string, cs []*runpb.Container) {
	deps := map[string][]string{}
	for _, c := range cs {
		if len(c.GetDependsOn()) > 0 {
			deps[c.GetName()] = c.GetDependsOn()
		}
		if r := c.GetResources(); r != nil {
			if _, set := ann[v1AnnCPUThrottling]; !set || r.GetCpuIdle() {
				ann[v1AnnCPUThrottling] = strconv.FormatBool(r.GetCpuIdle())
			}
			setTrue(ann, v1StartupCPUBoost, r.GetStartupCpuBoost())
		}
	}
	if len(deps) > 0 {
		ann[v1AnnContainerDeps] = jsonAnn(deps)
	}
}

func gpuZonalAnnotation(ann map[string]string, disabled *bool) {
	if disabled != nil {
		ann[v1AnnGPUZonal] = strconv.FormatBool(*disabled)
	}
}

// v1ServiceAnnotations are a service's metadata.annotations in v1: the
// caller's, its service-level scaling, and those Google documents for a
// Service, from their v2 fields.
func v1ServiceAnnotations(svc *runpb.Service) map[string]string {
	ann := map[string]string{}
	for k, v := range v1ServiceScalingAnnotations(svc) {
		ann[k] = v
	}
	if svc.GetClient() != "" {
		ann[v1ClientName] = svc.GetClient()
	}
	if svc.GetClientVersion() != "" {
		ann[v1ClientVersion] = svc.GetClientVersion()
	}
	if svc.GetDescription() != "" {
		ann[v1AnnDescription] = svc.GetDescription()
	}
	switch svc.GetIngress() {
	case runpb.IngressTraffic_INGRESS_TRAFFIC_ALL:
		ann[v1Ingress] = "all"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY:
		ann[v1Ingress] = "internal"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER:
		ann[v1Ingress] = "internal-and-cloud-load-balancing"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_NONE:
		ann[v1Ingress] = "none"
	}
	launchStageAnnotation(ann, svc.GetLaunchStage())
	binauthzAnnotations(ann, svc.GetBinaryAuthorization())
	setTrue(ann, v1AnnDefaultURLDisabled, svc.GetDefaultUriDisabled())
	setTrue(ann, v1InvokerIAMDisabled, svc.GetInvokerIamDisabled())
	if aud := svc.GetCustomAudiences(); len(aud) > 0 {
		ann[v1AnnCustomAudiences] = jsonAnn(aud)
	}
	if b := svc.GetBuildConfig(); b != nil {
		set := func(k, v string) {
			if v != "" {
				ann[k] = v
			}
		}
		set(v1BuildName, b.GetName())
		set(v1BuildSourceLocation, b.GetSourceLocation())
		set(v1BuildFunctionTarget, b.GetFunctionTarget())
		set(v1BuildImageURI, b.GetImageUri())
		set(v1BuildBaseImage, b.GetBaseImage())
		set(v1AnnBuildWorkerPool, b.GetWorkerPool())
		set(v1AnnBuildServiceAccount, b.GetServiceAccount())
		setTrue(ann, v1BuildAutoUpdates, b.GetEnableAutomaticUpdates())
		if len(b.GetEnvironmentVariables()) > 0 {
			ann[v1AnnBuildEnv] = jsonAnn(b.GetEnvironmentVariables())
		}
	}
	base := map[string]string{}
	for _, c := range svc.GetTemplate().GetContainers() {
		if c.GetBaseImageUri() != "" {
			base[c.GetName()] = c.GetBaseImageUri()
		}
	}
	if len(base) > 0 {
		ann[v1AnnBaseImages] = jsonAnn(base)
	}
	return ann
}

// v1RevisionAnnotations are a service's spec.template.metadata.annotations
// in v1: toV1Template's, and those Google documents for a Revision.
func v1RevisionAnnotations(t *runpb.RevisionTemplate, base map[string]string) map[string]string {
	ann := map[string]string{}
	for k, v := range base {
		ann[k] = v
	}
	vpcAnnotations(ann, t.GetVpcAccess())
	cmekAnnotations(ann, t.GetEncryptionKey(), t.GetEncryptionKeyRevocationAction(), t.GetEncryptionKeyShutdownDuration())
	executionEnvAnnotation(ann, t.GetExecutionEnvironment())
	setTrue(ann, v1AnnSessionAffinity, t.GetSessionAffinity())
	setTrue(ann, v1AnnHealthCheckDisabled, t.GetHealthCheckDisabled())
	gpuZonalAnnotation(ann, t.GpuZonalRedundancyDisabled)
	containerAnnotations(ann, t.GetContainers())
	return ann
}

// customResource is v, a v1 object, as a condition reads it: without
// apiVersion, kind and status, and without labels at any level.
func customResource(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "apiVersion")
	delete(m, "kind")
	delete(m, "status")
	dropLabels(m)
	// A resource with no annotations has an empty map, which a condition
	// asks with 'k' in resource.metadata.annotations (the page's
	// launch-stage and Binary Authorization examples).
	withAnnotations(m)
	if spec, ok := m["spec"].(map[string]any); ok {
		if tmpl, ok := spec["template"].(map[string]any); ok {
			withAnnotations(tmpl)
		}
	}
	return m
}

func withAnnotations(m map[string]any) {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		m["metadata"] = meta
	}
	if _, ok := meta["annotations"]; !ok {
		meta["annotations"] = map[string]any{}
	}
}

func dropLabels(v any) {
	switch t := v.(type) {
	case map[string]any:
		if meta, ok := t["metadata"].(map[string]any); ok {
			delete(meta, "labels")
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k != "annotations" {
				dropLabels(t[k])
			}
		}
	case []any:
		for _, x := range t {
			dropLabels(x)
		}
	}
}

// checkRunCustom checks v (the resource in its v1 shape) against the
// custom constraints on resourceType enforced on name's project.
func checkRunCustom(ctx context.Context, name, resourceType, method string, v any) error {
	project := orgpolicy.ProjectOf(name)
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	err := orgpolicy.CheckCustom(ctx, project, resourceType, method, customResource(v))
	var cv *orgpolicy.CustomViolation
	if errors.As(err, &cv) {
		return apierror.FailedPrecondition("%s", cv.Error())
	}
	return nil
}

// checkServiceCustom checks a service as a create or update leaves it.
func checkServiceCustom(ctx context.Context, svc *runpb.Service, method string) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	v := toV1Service(svc, ksvc{})
	v.Metadata.Annotations = v1ServiceAnnotations(svc)
	if v.Spec != nil && v.Spec.Template != nil {
		v.Spec.Template.Metadata.Annotations = v1RevisionAnnotations(svc.GetTemplate(), v.Spec.Template.Metadata.Annotations)
	}
	return checkRunCustom(ctx, svc.GetName(), orgpolicy.TypeRunService, method, v)
}

// checkJobCustom checks a job as a create or update leaves it: the
// job's metadata carries its launch stage, client and Binary
// Authorization; its execution template's, what the task template
// holds of VPC access, CMEK and the execution environment.
func checkJobCustom(ctx context.Context, job *runpb.Job, method string) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	v := toV1Job(job)
	ann := cloneAnn(v.Metadata.Annotations)
	if ann == nil {
		ann = map[string]string{}
	}
	binauthzAnnotations(ann, job.GetBinaryAuthorization())
	v.Metadata.Annotations = ann
	tt := job.GetTemplate().GetTemplate()
	tann := cloneAnn(v.Spec.Template.Metadata.Annotations)
	if tann == nil {
		tann = map[string]string{}
	}
	vpcAnnotations(tann, tt.GetVpcAccess())
	cmekAnnotations(tann, tt.GetEncryptionKey(), runpb.EncryptionKeyRevocationAction_ENCRYPTION_KEY_REVOCATION_ACTION_UNSPECIFIED, nil)
	executionEnvAnnotation(tann, tt.GetExecutionEnvironment())
	gpuZonalAnnotation(tann, tt.GpuZonalRedundancyDisabled)
	v.Spec.Template.Metadata.Annotations = tann
	return checkRunCustom(ctx, job.GetName(), orgpolicy.TypeRunJob, method, v)
}

// checkWorkerPoolCustom checks a worker pool as a create or update
// leaves it: its metadata as toV1WorkerPool renders it, with its Binary
// Authorization; its revision template's, what the template holds of
// VPC access, CMEK, GPU redundancy and its containers.
func checkWorkerPoolCustom(ctx context.Context, wp *runpb.WorkerPool, method string) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	v := toV1WorkerPool(wp)
	ann := cloneAnn(v.Metadata.Annotations)
	if ann == nil {
		ann = map[string]string{}
	}
	binauthzAnnotations(ann, wp.GetBinaryAuthorization())
	if wp.GetDescription() != "" {
		ann[v1AnnDescription] = wp.GetDescription()
	}
	if aud := wp.GetCustomAudiences(); len(aud) > 0 {
		ann[v1AnnCustomAudiences] = jsonAnn(aud)
	}
	v.Metadata.Annotations = ann
	t := wp.GetTemplate()
	tann := cloneAnn(v.Spec.Template.Metadata.Annotations)
	if tann == nil {
		tann = map[string]string{}
	}
	vpcAnnotations(tann, t.GetVpcAccess())
	cmekAnnotations(tann, t.GetEncryptionKey(), t.GetEncryptionKeyRevocationAction(), t.GetEncryptionKeyShutdownDuration())
	gpuZonalAnnotation(tann, t.GpuZonalRedundancyDisabled)
	containerAnnotations(tann, t.GetContainers())
	v.Spec.Template.Metadata.Annotations = tann
	return checkRunCustom(ctx, wp.GetName(), orgpolicy.TypeRunWorkerPool, method, v)
}
