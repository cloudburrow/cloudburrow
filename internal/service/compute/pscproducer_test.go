package compute

import "testing"

// producerILB makes an internal passthrough Network Load Balancer named
// ilb in project's region: a regional health check, an INTERNAL backend
// service and its forwarding rule on the default network, which a service
// attachment can publish (#2140: targetService must exist and be one).
func producerILB(t *testing.T, s *Server, project, region string) {
	t.Helper()
	r := "regions/" + region + "/"
	for _, c := range []struct {
		path string
		body obj
	}{
		{r + "healthChecks", obj{"name": "ilb-hc", "type": "TCP", "tcpHealthCheck": obj{"port": 80}}},
		{r + "backendServices", obj{"name": "ilb", "protocol": "TCP", "loadBalancingScheme": "INTERNAL",
			"healthChecks": []any{"projects/" + project + "/" + r + "healthChecks/ilb-hc"}}},
		{r + "forwardingRules", obj{"name": "ilb", "loadBalancingScheme": "INTERNAL", "IPProtocol": "TCP", "ports": []any{"80"},
			"network": "global/networks/default", "backendService": "projects/" + project + "/" + r + "backendServices/ilb"}},
	} {
		if _, err := s.Call("POST", project, c.path, nil, c.body); err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
	}
}
