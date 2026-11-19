package run

import (
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/genproto/googleapis/api"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// TestCustomConstraintShapes (#1895): a service, job and worker pool
// read by Cloud Run's documented example conditions in their v1 shape,
// with the annotations v1 carries for v2 fields, and without labels.
func TestCustomConstraintShapes(t *testing.T) {
	ctr := []*runpb.Container{{Name: "app", Image: "us-docker.pkg.dev/p/r/app", DependsOn: []string{"side"},
		Resources:     &runpb.ResourceRequirements{Limits: map[string]string{"memory": "512Mi"}, CpuIdle: true, StartupCpuBoost: true},
		LivenessProbe: &runpb.Probe{InitialDelaySeconds: 5}}}
	svc := &runpb.Service{Name: "projects/p/locations/us-central1/services/s", Description: "d", Labels: map[string]string{"team": "a"},
		Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY, LaunchStage: api.LaunchStage_GA, InvokerIamDisabled: true,
		CustomAudiences: []string{"aud"}, BinaryAuthorization: &runpb.BinaryAuthorization{BinauthzMethod: &runpb.BinaryAuthorization_UseDefault{UseDefault: true}},
		Template: &runpb.RevisionTemplate{Containers: ctr, SessionAffinity: true, ExecutionEnvironment: runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2,
			EncryptionKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
			VpcAccess:     &runpb.VpcAccess{Egress: runpb.VpcAccess_ALL_TRAFFIC, NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Network: "default"}}}}}
	v := toV1Service(svc, ksvc{})
	v.Metadata.Annotations = v1ServiceAnnotations(svc)
	v.Spec.Template.Metadata.Annotations = v1RevisionAnnotations(svc.GetTemplate(), v.Spec.Template.Metadata.Annotations)
	res := customResource(v)
	for cond, want := range map[string]bool{
		"resource.metadata.annotations['run.googleapis.com/ingress'] == 'internal'":                                          true,
		"resource.metadata.annotations['run.googleapis.com/launch-stage'] == 'GA'":                                           true,
		"resource.metadata.annotations['run.googleapis.com/binary-authorization'] == 'default'":                              true,
		"resource.metadata.annotations['run.googleapis.com/description'] == 'd'":                                             true,
		"resource.metadata.annotations['run.googleapis.com/invoker-iam-disabled'] == 'true'":                                 true,
		"resource.metadata.annotations['run.googleapis.com/custom-audiences'] == '[\"aud\"]'":                                true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/sessionAffinity'] == 'true'":                        true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/execution-environment'] == 'gen2'":                  true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/vpc-access-egress'] == 'all-traffic'":               true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/cpu-throttling'] == 'true'":                         true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/startup-cpu-boost'] == 'true'":                      true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/container-dependencies'] == '{\"app\":[\"side\"]}'": true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/encryption-key'].endsWith('/k')":                    true,
		"resource.spec.template.spec.containers.all(container, 'memory' in container.resources.limits)":                      true,
		"resource.spec.template.spec.containers.all(container, has(container.livenessProbe.initialDelaySeconds))":            true,
		"has(resource.metadata.labels)": false,
		"has(resource.status)":          false,
		"!('run.googleapis.com/ssh-enabled' in resource.metadata.annotations)": true,
		"has(resource.kind)": false,
	} {
		got, err := orgpolicy.Evaluate(cond, res)
		if err != nil || got != want {
			t.Errorf("service: %s = %v, %v; want %v", cond, got, err, want)
		}
	}

	job := &runpb.Job{Name: "projects/p/locations/us-central1/jobs/j", LaunchStage: api.LaunchStage_BETA,
		BinaryAuthorization: &runpb.BinaryAuthorization{BinauthzMethod: &runpb.BinaryAuthorization_UseDefault{UseDefault: true}, BreakglassJustification: "why"},
		Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{Containers: ctr, VpcAccess: &runpb.VpcAccess{Connector: "c"},
			ExecutionEnvironment: runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN1}}}
	jv := toV1Job(job)
	jv.Metadata.Annotations = cloneAnn(jv.Metadata.Annotations)
	binauthzAnnotations(jv.Metadata.Annotations, job.GetBinaryAuthorization())
	jann := map[string]string{}
	vpcAnnotations(jann, job.GetTemplate().GetTemplate().GetVpcAccess())
	executionEnvAnnotation(jann, job.GetTemplate().GetTemplate().GetExecutionEnvironment())
	jv.Spec.Template.Metadata.Annotations = jann
	jres := customResource(jv)
	for cond, want := range map[string]bool{
		"resource.metadata.annotations['run.googleapis.com/launch-stage'] == 'BETA'":                            true,
		"resource.metadata.annotations['run.googleapis.com/binary-authorization-breakglass'] == 'why'":          true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/vpc-access-connector'] == 'c'":         true,
		"resource.spec.template.metadata.annotations['run.googleapis.com/execution-environment'] == 'gen1'":     true,
		"resource.spec.template.spec.template.spec.containers.all(c, 'memory' in c.resources.limits)":           true,
		"resource.spec.template.spec.template.spec.containers.all(c, has(c.livenessProbe.initialDelaySeconds))": true,
		"has(resource.metadata.labels)": false,
	} {
		got, err := orgpolicy.Evaluate(cond, jres)
		if err != nil || got != want {
			t.Errorf("job: %s = %v, %v; want %v", cond, got, err, want)
		}
	}

	wp := &runpb.WorkerPool{Name: "projects/p/locations/us-central1/workerPools/w", LaunchStage: api.LaunchStage_GA,
		Template: &runpb.WorkerPoolRevisionTemplate{Containers: ctr}}
	wres := customResource(toV1WorkerPool(wp))
	bare := customResource(toV1Job(&runpb.Job{Name: "projects/p/locations/us-central1/jobs/b", Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{}}}))
	if ok, err := orgpolicy.Evaluate("!('run.googleapis.com/launch-stage' in resource.metadata.annotations)", bare); err != nil || !ok {
		t.Errorf("a job with no annotations: %v, %v", ok, err)
	}
	for cond, want := range map[string]bool{
		"resource.metadata.annotations['run.googleapis.com/launch-stage'] == 'GA'":                      true,
		"resource.spec.template.spec.containers.all(container, 'memory' in container.resources.limits)": true,
	} {
		got, err := orgpolicy.Evaluate(cond, wres)
		if err != nil || got != want {
			t.Errorf("worker pool: %s = %v, %v; want %v", cond, got, err, want)
		}
	}
}
