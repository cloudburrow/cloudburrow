//go:build compat

package compat

import (
	"context"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// internalLB makes an internal passthrough Network Load Balancer, as
// load-balancing/docs/internal/setting-up-internal does it with the Go
// client: a regional TCP health check, an INTERNAL regional backend
// service (with group as its backend when given) and a forwarding rule on
// network for port, in subnet when given (Google requires it in a custom
// mode network; an auto mode network's own subnet otherwise). It returns the forwarding rule's link, which a service
// attachment publishes (#2140), and deletes all three at cleanup.
func internalLB(ctx context.Context, t *testing.T, opts []option.ClientOption, project, region, network, subnet, name, group string, port int) string {
	t.Helper()
	hcs, err := computeapi.NewRegionHealthChecksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	bss, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	frs, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	scope := "projects/" + project + "/regions/" + region + "/"
	wait(hcs.Insert(ctx, &computepb.InsertRegionHealthCheckRequest{Project: project, Region: region, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(name + "-hc"), Type: proto.String("TCP"), TcpHealthCheck: &computepb.TCPHealthCheck{Port: proto.Int32(int32(port))},
		CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2)}}))
	bs := &computepb.BackendService{Name: proto.String(name), Protocol: proto.String("TCP"), LoadBalancingScheme: proto.String("INTERNAL"),
		HealthChecks: []string{scope + "healthChecks/" + name + "-hc"}, Network: proto.String(network)}
	if group != "" {
		bs.Backends = []*computepb.Backend{{Group: proto.String(group), BalancingMode: proto.String("CONNECTION")}}
	}
	wait(bss.Insert(ctx, &computepb.InsertRegionBackendServiceRequest{Project: project, Region: region, BackendServiceResource: bs}))
	rule := &computepb.ForwardingRule{
		Name: proto.String(name), LoadBalancingScheme: proto.String("INTERNAL"), IPProtocol: proto.String("TCP"),
		Ports: []string{itoa(port)}, Network: proto.String(network), BackendService: proto.String(scope + "backendServices/" + name)}
	if subnet != "" {
		rule.Subnetwork = proto.String(subnet)
	}
	wait(frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: project, Region: region, ForwardingRuleResource: rule}))
	t.Cleanup(func() {
		c := context.Background()
		for _, del := range []func() (*computeapi.Operation, error){
			func() (*computeapi.Operation, error) {
				return frs.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: project, Region: region, ForwardingRule: name})
			},
			func() (*computeapi.Operation, error) {
				return bss.Delete(c, &computepb.DeleteRegionBackendServiceRequest{Project: project, Region: region, BackendService: name})
			},
			func() (*computeapi.Operation, error) {
				return hcs.Delete(c, &computepb.DeleteRegionHealthCheckRequest{Project: project, Region: region, HealthCheck: name + "-hc"})
			},
		} {
			if op, err := del(); err == nil {
				_ = op.Wait(c)
			}
		}
	})
	return scope + "forwardingRules/" + name
}
