package run

// A 2nd gen function and its Cloud Run service (#2049):
//
//   - build_config's runtime_update_policy: automatic_update_policy is the
//     default ("By default, functions have automatic security updates
//     enabled", run/docs/deploy-functions), and on_deploy_update_policy
//     records, in its output-only runtime_version, the base image the
//     latest deploy built on.
//   - The function's service carries Run's build_config: the entry point,
//     base image, source and whether base image updates are automatic, as
//     a function deployed through Cloud Run records them.
//   - A deploy keeps what the function's API does not govern, set on its
//     service through Cloud Run (Write a function's Networking and Volumes,
//     or the service's own Edit and deploy): the execution environment,
//     VPC access, HTTP/2, session affinity, the encryption key and every
//     volume but the function's secret volumes.

import (
	"fmt"
	"strings"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

// functionSecretVolume prefixes the volumes functionService makes from
// service_config.secret_volumes.
const functionSecretVolume = "secret-volume-"

// BaseImage is the Google base image a runtime builds on in region, as
// Cloud Run names it: REGION-docker.pkg.dev/serverless-runtimes/STACK/runtimes/RUNTIME.
func BaseImage(region, runtime string) string {
	return region + "-docker.pkg.dev/serverless-runtimes/google-22-full/runtimes/" + runtime
}

// RuntimeOfBaseImage is the runtime a base image names, "" for none.
func RuntimeOfBaseImage(image string) string {
	if i := strings.LastIndex(image, "/runtimes/"); i >= 0 {
		return image[i+len("/runtimes/"):]
	}
	return ""
}

// defaultUpdatePolicy makes automatic updates the policy when none is set.
func defaultUpdatePolicy(fn *functionspb.Function) {
	if bc := fn.GetBuildConfig(); bc != nil && bc.GetRuntimeUpdatePolicy() == nil {
		bc.RuntimeUpdatePolicy = &functionspb.BuildConfig_AutomaticUpdatePolicy{AutomaticUpdatePolicy: &functionspb.AutomaticUpdatePolicy{}}
	}
}

// functionRegion is a function name's location.
func functionRegion(name string) string {
	if parts := strings.Split(name, "/"); len(parts) > 3 {
		return parts[3]
	}
	return ""
}

// serviceBuildConfig is Run's build_config for a function's service.
func serviceBuildConfig(fn *functionspb.Function, image string) *runpb.BuildConfig {
	bc := fn.GetBuildConfig()
	out := &runpb.BuildConfig{FunctionTarget: bc.GetEntryPoint(), ImageUri: image,
		BaseImage: BaseImage(functionRegion(fn.GetName()), bc.GetRuntime()), EnableAutomaticUpdates: bc.GetOnDeployUpdatePolicy() == nil,
		EnvironmentVariables: bc.GetEnvironmentVariables(), ServiceAccount: bc.GetServiceAccount()}
	if bucket, object, generation, err := sourceOf(bc); err == nil && bucket != "" {
		out.SourceLocation = fmt.Sprintf("gs://%s/%s", bucket, object)
		if generation != 0 {
			out.SourceLocation += fmt.Sprintf("#%d", generation)
		}
	}
	return out
}

// keepRunSettings carries onto svc, the service a deploy renders from the
// function, what existing has that the function's API does not govern.
func keepRunSettings(svc, existing *runpb.Service) {
	et, t := existing.GetTemplate(), svc.GetTemplate()
	if et == nil || t == nil {
		return
	}
	t.ExecutionEnvironment, t.VpcAccess, t.EncryptionKey = et.GetExecutionEnvironment(), et.GetVpcAccess(), et.GetEncryptionKey()
	t.SessionAffinity = et.GetSessionAffinity()
	t.Labels, t.Annotations = et.GetLabels(), et.GetAnnotations()
	var c, ec *runpb.Container
	if len(t.GetContainers()) > 0 {
		c = t.Containers[0]
	}
	if len(et.GetContainers()) > 0 {
		ec = et.Containers[0]
	}
	if c != nil && ec != nil && len(c.GetPorts()) == 0 {
		for _, p := range ec.GetPorts() {
			c.Ports = append(c.Ports, proto.Clone(p).(*runpb.ContainerPort))
		}
	}
	mounts := map[string]*runpb.VolumeMount{}
	for _, m := range ec.GetVolumeMounts() {
		mounts[m.GetName()] = m
	}
	for _, v := range et.GetVolumes() {
		if v.GetSecret() != nil && strings.HasPrefix(v.GetName(), functionSecretVolume) {
			continue // the function's own, made again from service_config
		}
		t.Volumes = append(t.Volumes, proto.Clone(v).(*runpb.Volume))
		if m, ok := mounts[v.GetName()]; ok && c != nil {
			c.VolumeMounts = append(c.VolumeMounts, proto.Clone(m).(*runpb.VolumeMount))
		}
	}
	if svc.Description == "" {
		svc.Description = existing.GetDescription()
	}
	if len(svc.GetAnnotations()) == 0 {
		svc.Annotations = existing.GetAnnotations()
	}
	if svc.Scaling == nil {
		svc.Scaling = existing.GetScaling()
	}
}

// FunctionService is the Cloud Run service fn runs as, with Google's
// defaults filled in, running image: what a console creating a function's
// service through Cloud Run (Write a function's placeholder revision)
// deploys, so it matches what the function's own deploy renders.
func FunctionService(fn *functionspb.Function, image string) *runpb.Service {
	c := cloneFunction(fn)
	if c.ServiceConfig == nil {
		c.ServiceConfig = &functionspb.ServiceConfig{}
	}
	defaultServiceConfig(c)
	defaultUpdatePolicy(c)
	return functionService(c, image)
}
