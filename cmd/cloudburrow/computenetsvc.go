package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
	"github.com/cloudburrow/cloudburrow/internal/service/networkservices"
)

// Network Services (#2160), served on the Compute Engine port beside
// Network Security: Secure Web Proxy's gateways read their gateway
// security policies and URL lists from Network Security, their
// certificates from Certificate Manager, their networks and proxy-only
// subnets from Compute Engine, and a client's service accounts and secure
// tags from the VM instance that has its address.

func (s *computeService) useNetSvc(api *compute.Server, ns *networksecurity.Server, nsv *networkservices.Server, cm *certificatemanager.Server) {
	get := func(rel string) (map[string]any, bool) {
		p := strings.Split(rel, "/")
		if len(p) < 4 || p[0] != "projects" {
			return nil, false
		}
		out, err := api.Call("GET", p[1], strings.Join(p[2:], "/"), nil, nil)
		return out, err == nil
	}
	nsv.UseHooks(networkservices.Hooks{
		GatewaySecurityPolicyExists: ns.Exists,
		ServerTLSPolicyExists:       ns.Exists,
		NetSecPolicyExists:          ns.Exists,
		CertificateExists:           cm.CertificateExists,
		Network:                     func(rel string) bool { _, ok := get(rel); return ok },
		Subnetwork: func(rel string) (string, bool) {
			o, ok := get(rel)
			if !ok {
				return "", false
			}
			return relOf(strAny(o["network"])), true
		},
		ProxyOnlySubnet: func(network, region string) bool {
			project := strings.Split(network, "/")[1]
			out, err := api.Call("GET", project, "regions/"+region+"/subnetworks", nil, nil)
			if err != nil {
				return false
			}
			for _, it := range listAny(out["items"]) {
				m, _ := it.(map[string]any)
				if strAny(m["purpose"]) == "REGIONAL_MANAGED_PROXY" && strAny(m["role"]) != "BACKUP" && relOf(strAny(m["network"])) == network {
					return true
				}
			}
			return false
		},
		BackendService: func(rel string) (string, bool) {
			o, ok := get(rel)
			if !ok {
				return "", false
			}
			return strAny(o["loadBalancingScheme"]), true
		},
	})
	nsv.UseSubnetRange(func(rel string) (string, bool) {
		o, ok := get(rel)
		if !ok {
			return "", false
		}
		return strAny(o["ipCidrRange"]), true
	})
	nsv.UsePolicies(ns)
	nsv.UseCertificates(cm.Certificates)
	nsv.UseSource(func(ip string) ([]string, []string) { return s.instanceIdentity(api, ip) })
	// VMs reach the gateways at their VPC addresses through Compute
	// Engine's relay, which keeps their identity (internal/vmrelay).
	api.UseGateways(nsv)
	if s.privateca != nil {
		nsv.UseLeafIssuer(swpLeafIssuer(s.privateca))
	}
	ns.UseGatewayUsers(nsv.GatewaySecurityPolicyUsers)
}

// instanceIdentity is the service accounts and secure tags (tagValues/ID,
// the instance's effective tags) of the VM instance whose network
// interface has ip, in any project this instance keeps; none when no
// instance has it, as for a client on the host.
func (s *computeService) instanceIdentity(api *compute.Server, ip string) (sas, tags []string) {
	if s.registry == nil || ip == "" {
		return nil, nil
	}
	reg := s.registry()
	if reg == nil {
		return nil, nil
	}
	projects, err := reg.List()
	if err != nil {
		return nil, nil
	}
	for _, p := range projects {
		out, err := api.Call("GET", p.ProjectID, "aggregated/instances", nil, nil)
		if err != nil {
			continue
		}
		items, _ := out["items"].(map[string]any)
		for _, scoped := range items {
			sm, _ := scoped.(map[string]any)
			for _, it := range listAny(sm["instances"]) {
				vm, _ := it.(map[string]any)
				if !hasAddress(vm, ip) {
					continue
				}
				for _, sa := range listAny(vm["serviceAccounts"]) {
					if m, ok := sa.(map[string]any); ok && strAny(m["email"]) != "" {
						sas = append(sas, strAny(m["email"]))
					}
				}
				zone := strAny(vm["zone"])
				zone = zone[strings.LastIndex(zone, "/")+1:]
				// The instance's effective tags, bound by its project's ID or
				// number, as for authorization policies (computenetsec.go).
				for _, id := range s.sourceTags("//compute.googleapis.com/projects/" + p.ProjectID + "/zones/" + zone + "/instances/" + strAny(vm["id"])) {
					tags = append(tags, "tagValues/"+strconv.FormatInt(id, 10))
				}
				return sas, tags
			}
		}
	}
	return nil, nil
}

func hasAddress(vm map[string]any, ip string) bool {
	for _, ni := range listAny(vm["networkInterfaces"]) {
		m, _ := ni.(map[string]any)
		if strAny(m["networkIP"]) == ip {
			return true
		}
	}
	return false
}

// netSvcAuthzExtensions are Network Services' authorization extensions as
// the load balancers call them (compute.AuthzExtensions, #2160).
type netSvcAuthzExtensions struct{ nsv *networkservices.Server }

func (a netSvcAuthzExtensions) AuthzExtension(name string) (compute.AuthzExtension, bool) {
	e, ok := a.nsv.AuthzExtension(name)
	if !ok {
		return compute.AuthzExtension{}, false
	}
	return compute.AuthzExtension{Name: e.Name, Service: e.Service, Authority: e.Authority, WireFormat: e.WireFormat,
		Timeout: e.Timeout, FailOpen: e.FailOpen, ForwardHeaders: e.ForwardHeaders, Metadata: e.Metadata}, true
}

// xdsEndpointPolicies are a project's endpoint policies as the xDS API
// gives an xDS-enabled gRPC server its server TLS policy (#2160). It reads
// only Network Services' records, so it is safe under Compute's lock.
func xdsEndpointPolicies(nsv *networkservices.Server) func(project string) []compute.XDSEndpointPolicy {
	return func(project string) []compute.XDSEndpointPolicy {
		var out []compute.XDSEndpointPolicy
		for _, e := range nsv.EndpointPolicies(project) {
			ml := e.GetEndpointMatcher().GetMetadataLabelMatcher()
			labels := map[string]string{}
			for _, l := range ml.GetMetadataLabels() {
				labels[l.GetLabelName()] = l.GetLabelValue()
			}
			out = append(out, compute.XDSEndpointPolicy{Name: e.GetName(), Type: e.GetType().String(),
				MatchAll: ml.GetMetadataLabelMatchCriteria().String() == "MATCH_ALL", Labels: labels,
				Ports: e.GetTrafficPortSelector().GetPorts(), ServerTLSPolicy: e.GetServerTlsPolicy()})
		}
		return out
	}
}

// swpLeafIssuer issues Secure Web Proxy's interception certificates from a
// CA pool of this instance's Certificate Authority Service, one
// certificate request per host, as Google's direct leaf provisioning does
// (secure-web-proxy/docs/tls-inspection-overview): each is a Certificate of
// the pool (an Enterprise pool keeps it, a DevOps pool does not).
func swpLeafIssuer(ca *privateCAService) func(ctx context.Context, pool, csrPEM string, lifetime time.Duration) ([]string, error) {
	return func(ctx context.Context, pool, csrPEM string, lifetime time.Duration) ([]string, error) {
		srv := ca.API()
		if srv == nil {
			return nil, status.Error(codes.Unavailable, "Certificate Authority Service has not started yet")
		}
		sum := sha256.Sum256([]byte(csrPEM))
		id := fmt.Sprintf("swp-%d-%s", time.Now().Unix(), hex.EncodeToString(sum[:6]))
		c, err := srv.API().CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: id,
			Certificate: &privatecapb.Certificate{CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csrPEM},
				Lifetime: durationpb.New(lifetime)}})
		if err != nil {
			return nil, err
		}
		return append([]string{c.GetPemCertificate()}, c.GetPemCertificateChain()...), nil
	}
}
