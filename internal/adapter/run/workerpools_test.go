package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TestWorkerPoolRendersADeployment (#1300): a pool is a Deployment whose
// replicas is the manual instance count, its pods labelled with the revision
// and carrying its template, rendered with the shared container mapping.
func TestWorkerPoolRendersADeployment(t *testing.T) {
	s := NewServer(nil, "test", 0)
	three := int32(3)
	wp := &runpb.WorkerPool{Name: "projects/p/locations/l/workerPools/pool",
		Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{Image: "docker.io/library/busybox:1.36",
			Env: []*runpb.EnvVar{{Name: "A", Values: &runpb.EnvVar_Value{Value: "b"}}}}}},
		Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &three}}
	if err := checkWorkerPool(wp); err != nil {
		t.Fatalf("checkWorkerPool: %v", err)
	}
	spec := workerPoolSpec(wp, 1, wp.Name+"/revisions/pool-00001-abc")
	w := workload{kind: kindWorkerPool, id: "pool", name: wp.Name}
	cm, dep, err := s.renderWorkerPool(w, spec, 1, "")
	if err != nil {
		t.Fatalf("renderWorkerPool: %v", err)
	}
	for _, want := range []string{"kind: Deployment", "replicas: 3", labelRevision + ": pool-00001-abc",
		"image: docker.io/library/busybox:1.36", "name: A", labelWorkload + ": worker-pool-pool"} {
		if !strings.Contains(dep, want) {
			t.Errorf("Deployment lacks %q:\n%s", want, dep)
		}
	}
	if !strings.Contains(cm, `"revision-generation":"1"`) {
		t.Errorf("ConfigMap lacks the revision generation: %s", cm)
	}
	// Unset, a pool gets one instance.
	if got := workerPoolSpec(&runpb.WorkerPool{}, 1, "").GetScaling().GetManualInstanceCount(); got != defaultWorkerPoolInstances {
		t.Errorf("default instance count = %d", got)
	}
}

// TestWorkerPoolAndInstanceRefusals: what a Deployment does not render is
// UNIMPLEMENTED, naming the field.
func TestWorkerPoolAndInstanceRefusals(t *testing.T) {
	c := []*runpb.Container{{Image: "docker.io/library/busybox:1.36"}}
	for name, err := range map[string]error{
		"container.ports": checkWorkerPool(&runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{
			Containers: []*runpb.Container{{Image: "x:1", Ports: []*runpb.ContainerPort{{ContainerPort: 80}}}}}}),
		"instanceSplits": checkWorkerPool(&runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: c},
			InstanceSplits: []*runpb.InstanceSplit{{Revision: "old", Percent: 50}}}),
		"template.vpcAccess": checkWorkerPool(&runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: c,
			VpcAccess: &runpb.VpcAccess{}}}),
		"ingress":    checkInstance(&runpb.Instance{Containers: c, Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY}),
		"iapEnabled": checkInstance(&runpb.Instance{Containers: c, IapEnabled: true}),
	} {
		if e := apierror.From(err); e.Code != codes.Unimplemented || !strings.Contains(e.Message, name) {
			t.Errorf("%s: %v; want UNIMPLEMENTED naming it", name, err)
		}
	}
	if err := checkInstance(&runpb.Instance{}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("an instance with no container = %v, want INVALID_ARGUMENT", err)
	}
	split := []*runpb.InstanceSplit{{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 100}}
	if err := checkWorkerPool(&runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: c}, InstanceSplits: split}); err != nil {
		t.Errorf("100%% to the latest revision is refused: %v", err)
	}
}

// TestInstanceRoute (#1348): an instance is served on Cloud Run's port with
// PORT set, through a Service and a KIngress on its own host, and ingress
// ALL is accepted.
func TestInstanceRoute(t *testing.T) {
	c := []*runpb.Container{{Image: "docker.io/library/busybox:1.36"}}
	if err := checkInstance(&runpb.Instance{Containers: c, Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
		InvokerIamDisabled: true}); err != nil {
		t.Fatalf("ingress ALL refused: %v", err)
	}
	run, port := servingContainers(c)
	if port != defaultServingPort || len(c[0].Ports) != 0 {
		t.Fatalf("port = %d, caller's container changed: %v", port, c[0])
	}
	if e := run[0].GetEnv(); len(e) != 1 || e[0].GetName() != "PORT" || e[0].GetValue() != "8080" {
		t.Errorf("PORT not injected: %v", e)
	}
	if _, p := servingContainers([]*runpb.Container{{Image: "a:1"}, {Image: "b:1"}}); p != 0 {
		t.Errorf("two containers, none with a port, served on %d", p)
	}
	s := NewServer(nil, "test", 0)
	w := workload{kind: kindInstance, id: "web", name: "projects/p/locations/l/instances/web"}
	route := s.instanceRoute(w, 9000)
	for _, want := range []string{"kind: Service", "targetPort: 9000", "kind: Ingress", kourierClass,
		`"web.instance.` + WorkloadNamespace + `.cloudburrow.localhost"`, "serviceName: run-instance-web"} {
		if !strings.Contains(route, want) {
			t.Errorf("route lacks %q:\n%s", want, route)
		}
	}
}
