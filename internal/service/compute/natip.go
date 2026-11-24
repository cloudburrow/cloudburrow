package compute

import (
	"net/http"
	"strings"
)

// External IPv4 addresses of VMs (#2140, https://docs.cloud.google.com/vpc/docs/reserve-static-external-ip-address,
// "Promote an ephemeral external IP address" and "Assign a static external
// IP address to a VM", and .../compute/docs/ip-addresses/configure-static-external-ip-address,
// read 2026-11-22):
//
//   - an access config without natIP gets an ephemeral address of its own,
//     distinct and stable for the life of the access config, from
//     192.0.2.0/24 (TEST-NET-1, RFC 5737: never routed on the internet, so
//     nothing that dials it leaves this machine). It is a record, not a
//     route: the instance is reached from this machine through the
//     forwarded SSH port its cloudburrowSshAddress names (127.0.0.1:PORT);
//   - an access config's natIP of a RESERVED regional EXTERNAL address of
//     the instance's region takes it: the address is IN_USE, its users the
//     instance, and goes back to RESERVED when the instance is deleted or
//     the access config removed;
//   - addresses.insert with address set to an instance's ephemeral natIP
//     promotes it: a static address of that IP, IN_USE by the instance, of
//     the access config's network tier.

var ephemeralPool = cidrs("192.0.2.0/24")[0]

// externalInUseLocked is every external IP an access config or an address
// holds.
func (s *Server) externalInUseLocked() map[string]bool {
	used := map[string]bool{}
	for k, o := range s.res {
		switch {
		case strings.Contains(k, "/addresses/"):
			used[str(o, "address")] = true
		case strings.Contains(k, "/instances/"):
			for _, e := range listOf(o["networkInterfaces"]) {
				nic, _ := e.(map[string]any)
				for _, a := range listOf(nic["accessConfigs"]) {
					if ac, ok := a.(map[string]any); ok {
						used[str(ac, "natIP")] = true
					}
				}
			}
		}
	}
	return used
}

// ephemeralNatLocked is a free ephemeral external address.
func (s *Server) ephemeralNatLocked() (string, error) {
	used := s.externalInUseLocked()
	lo := ip4(ephemeralPool.IP)
	for v := lo + 1; v < lo+rangeSize(ephemeralPool)-1; v++ {
		if ip := fromU32(v).String(); !used[ip] {
			return ip, nil
		}
	}
	return "", errf(http.StatusForbidden, "quotaExceeded", "Quota 'IN_USE_ADDRESSES' exceeded: every local ephemeral external address of %s is in use.", ephemeralPool)
}

// regionalExternalLocked is the project's regional external address of ip
// in region, or "".
func (s *Server) regionalExternalLocked(project, region, ip string) string {
	prefix := "projects/" + project + "/regions/" + region + "/addresses/"
	for k, a := range s.res {
		if strings.HasPrefix(k, prefix) && str(a, "address") == ip && str(a, "addressType") != "INTERNAL" {
			return k
		}
	}
	return ""
}

// accessConfigLocked resolves one accessConfigs[] entry of an instance in
// region: its natIP, ephemeral or a reserved static address (whose
// address is returned to be marked IN_USE once the instance holds it).
func (s *Server) accessConfigLocked(project, region string, ac map[string]any, field string) (obj, string, error) {
	name := str(ac, "name")
	if name == "" {
		name = "External NAT"
	}
	if t := str(ac, "type"); t != "" && t != "ONE_TO_ONE_NAT" {
		return nil, "", invalid("Invalid value for field '%s.type': '%s'. Must be ONE_TO_ONE_NAT.", field, t)
	}
	tier := str(ac, "networkTier")
	switch tier {
	case "", "PREMIUM", "STANDARD":
	default:
		return nil, "", invalid("Invalid value for field '%s.networkTier': '%s'.", field, tier)
	}
	out := obj{"kind": "compute#accessConfig", "type": "ONE_TO_ONE_NAT", "name": name}
	static := ""
	if ip := str(ac, "natIP"); ip != "" {
		static = s.regionalExternalLocked(project, region, ip)
		if static == "" {
			return nil, "", invalid("Invalid value for field '%s.natIP': '%s'. The specified external IP address '%s' was not found in region '%s'.", field, ip, ip, region)
		}
		a := s.res[static]
		if str(a, "status") == "IN_USE" {
			return nil, "", errf(http.StatusBadRequest, "invalid", "The IP address '%s' is already being used by another resource.", ip)
		}
		if at := str(a, "networkTier"); tier != "" && at != "" && tier != at {
			return nil, "", invalid("Invalid value for field '%s.networkTier': '%s'. The network tier of the access config must match the network tier of the external IP address (%s).", field, tier, at)
		}
		tier = vpcOrStr(str(a, "networkTier"), "PREMIUM")
		out["natIP"] = ip
	} else {
		ip, err := s.ephemeralNatLocked()
		if err != nil {
			return nil, "", err
		}
		out["natIP"] = ip
	}
	out["networkTier"] = vpcOrStr(tier, "PREMIUM")
	return out, static, nil
}

// takeAddressLocked marks the static address rel IN_USE by instance.
func (s *Server) takeAddressLocked(rel, instance string) {
	if a, ok := s.res[rel]; ok {
		a["status"], a["users"] = "IN_USE", []any{instance}
	}
}

// releaseAddressOfLocked returns the static address of ip that instance
// holds to RESERVED.
func (s *Server) releaseAddressOfLocked(instance, ip string) {
	for k, a := range s.res {
		if strings.Contains(k, "/addresses/") && str(a, "address") == ip && hasStr(strs(a, "users"), instance) {
			a["status"] = "RESERVED"
			delete(a, "users")
		}
	}
}

// promoteExternalLocked fills o, a new regional EXTERNAL address of ip, as
// a promotion of an instance's ephemeral natIP; it says false when no
// instance of the region holds ip.
func (s *Server) promoteExternalLocked(project, region, ip, tier string, o obj) (bool, error) {
	prefix := "projects/" + project + "/zones/"
	for k, inst := range s.res {
		if !strings.HasPrefix(k, prefix) || !strings.Contains(k, "/instances/") {
			continue
		}
		for _, e := range listOf(inst["networkInterfaces"]) {
			nic, _ := e.(map[string]any)
			for _, a := range listOf(nic["accessConfigs"]) {
				ac, _ := a.(map[string]any)
				if str(ac, "natIP") != ip {
					continue
				}
				if region == "" || regionOfZone(lastSeg(str(inst, "zone"))) != region {
					return true, invalid("Invalid value for field 'resource.address': '%s'. An ephemeral external IP address is promoted to a static address of its instance's region, %s.", ip, regionOfZone(lastSeg(str(inst, "zone"))))
				}
				at := vpcOrStr(str(ac, "networkTier"), "PREMIUM")
				if tier != "" && tier != at {
					return true, invalid("Invalid value for field 'resource.networkTier': '%s'. The network tier must be the ephemeral address's, %s.", tier, at)
				}
				o["address"], o["networkTier"], o["status"], o["users"] = ip, at, "IN_USE", []any{k}
				return true, nil
			}
		}
	}
	return false, nil
}

// addAccessConfig is instances.addAccessConfig: an access config on a
// network interface that has none.
func (s *Server) addAccessConfig(c call, scope, rel string, o obj) (obj, error) {
	nic, err := nicNamed(o, first(c.query, "networkInterface"))
	if err != nil {
		return nil, err
	}
	if len(listOf(nic["accessConfigs"])) > 0 {
		return nil, invalid("Invalid value for field 'resource.accessConfigs': ''. At most one access config currently supported.")
	}
	ac, static, err := s.accessConfigLocked(c.project, regionOfZone(lastSeg(str(o, "zone"))), c.body, "resource")
	if err != nil {
		return nil, err
	}
	nic["accessConfigs"] = []any{norm(ac)}
	nic["fingerprint"] = newFingerprint()
	if static != "" {
		s.takeAddressLocked(static, rel)
	}
	return s.done(c.project, scope, "addAccessConfig", rel, o["id"])
}

// deleteAccessConfig is instances.deleteAccessConfig: the access config
// named accessConfig goes, its static address back to RESERVED.
func (s *Server) deleteAccessConfig(c call, scope, rel string, o obj) (obj, error) {
	nic, err := nicNamed(o, first(c.query, "networkInterface"))
	if err != nil {
		return nil, err
	}
	name := first(c.query, "accessConfig")
	if name == "" {
		return nil, required("accessConfig")
	}
	var keep []any
	gone := ""
	for _, a := range listOf(nic["accessConfigs"]) {
		ac, _ := a.(map[string]any)
		if str(ac, "name") == name {
			gone = str(ac, "natIP")
			continue
		}
		keep = append(keep, a)
	}
	if gone == "" {
		return nil, invalid("Invalid value for field 'accessConfig': '%s'. No access config named '%s' on network interface '%s'.", name, name, str(nic, "name"))
	}
	if len(keep) == 0 {
		delete(nic, "accessConfigs")
	} else {
		nic["accessConfigs"] = keep
	}
	nic["fingerprint"] = newFingerprint()
	s.releaseAddressOfLocked(rel, gone)
	return s.done(c.project, scope, "deleteAccessConfig", rel, o["id"])
}

// nicNamed is the instance's network interface of that name (nic0 when
// empty, as gcloud sends it).
func nicNamed(o obj, name string) (map[string]any, error) {
	if name == "" {
		return nil, required("networkInterface")
	}
	for _, e := range listOf(o["networkInterfaces"]) {
		if nic, ok := e.(map[string]any); ok && str(nic, "name") == name {
			return nic, nil
		}
	}
	return nil, invalid("Invalid value for field 'networkInterface': '%s'. The network interface does not exist.", name)
}
