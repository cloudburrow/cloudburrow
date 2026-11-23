package main

import (
	"strings"

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
				if et, err := reg.EffectiveTags("//compute.googleapis.com/projects/" + p.ProjectID + "/zones/" + zone + "/instances/" + strAny(vm["id"])); err == nil {
					for _, t := range et {
						tags = append(tags, t.GetTagValue())
					}
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
