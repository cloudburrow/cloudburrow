package compute

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
)

// Addresses (https://docs.cloud.google.com/compute/docs/reference/rest/v1/addresses,
// .../globalAddresses and https://docs.cloud.google.com/vpc/docs/ip-addresses,
// re-read 2026-11-19), reserved as Google reserves them:
//
//   - a regional INTERNAL address is in its subnetwork's primary range,
//     never one of the four addresses Google keeps there nor an address
//     already reserved; unset, the first free one is chosen;
//   - a global INTERNAL address of purpose VPC_PEERING is an allocated
//     range (address/prefixLength) for private services access, which no
//     subnet range of its network may overlap; of purpose
//     PRIVATE_SERVICE_CONNECT, one address in its network;
//   - an EXTERNAL address is chosen from documentation ranges
//     (203.0.113.0/24 regional, 198.51.100.0/24 global), since no address
//     here is reachable from the internet; a given external address (BYOIP)
//     is not served.

var (
	regionalPool = cidrs("203.0.113.0/24")[0]
	globalPool   = cidrs("198.51.100.0/24")[0]
)

func (s *Server) insertAddress(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	scope := "global"
	if region != "" {
		scope = "regions/" + region
	}
	rel := "projects/" + project + "/" + scope + "/addresses/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	typ := str(b, "addressType")
	switch typ {
	case "":
		typ = "EXTERNAL"
	case "EXTERNAL", "INTERNAL":
	default:
		return nil, invalid("Invalid value for field 'resource.addressType': '%s'. Must be one of [EXTERNAL, INTERNAL]", typ)
	}
	if v := str(b, "ipVersion"); v != "" && v != "IPV4" {
		return nil, unimplemented("ipVersion %s: IPv6 addresses are not served", v)
	}
	tier := str(b, "networkTier")
	o := obj{"kind": "compute#address", "name": name, "selfLink": rel, "addressType": typ, "status": "RESERVED"}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if region != "" {
		o["region"] = "projects/" + project + "/regions/" + region
	} else {
		o["ipVersion"] = "IPV4"
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"] = l
	}
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	purpose := str(b, "purpose")
	addr := str(b, "address")
	switch typ {
	case "EXTERNAL":
		switch tier {
		case "":
			tier = "PREMIUM"
		case "PREMIUM":
		case "STANDARD":
			if region == "" {
				return nil, invalid("Invalid value for field 'resource.networkTier': 'STANDARD'. A global address must be in the Premium network tier.")
			}
		default:
			return nil, invalid("Invalid value for field 'resource.networkTier': '%s'.", tier)
		}
		o["networkTier"] = tier
		if purpose != "" {
			return nil, invalid("Invalid value for field 'resource.purpose': '%s'. Purpose is set only for internal addresses.", purpose)
		}
		if addr != "" {
			return nil, unimplemented("a given external address (bring your own IP) is not served: leave address unset to have one chosen")
		}
		pool := regionalPool
		if region == "" {
			pool = globalPool
		}
		ip, err := s.freeExternalLocked(pool)
		if err != nil {
			return nil, err
		}
		o["address"] = ip
	case "INTERNAL":
		var err error
		if region != "" {
			err = s.regionalInternal(project, region, b, o, purpose, addr)
		} else {
			err = s.globalInternal(project, b, o, purpose, addr)
		}
		if err != nil {
			return nil, err
		}
	}
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	s.res[rel] = norm(o)
	return s.done(project, scope, "insert", rel, s.res[rel]["id"])
}

func (s *Server) regionalInternal(project, region string, b, o obj, purpose, addr string) error {
	switch purpose {
	case "":
		purpose = "GCE_ENDPOINT"
	case "GCE_ENDPOINT", "SHARED_LOADBALANCER_VIP", "DNS_RESOLVER":
	case "VPC_PEERING", "PRIVATE_SERVICE_CONNECT":
		return invalid("Invalid value for field 'resource.purpose': '%s'. This purpose is for global addresses.", purpose)
	default:
		return unimplemented("purpose %s for a regional internal address", purpose)
	}
	o["purpose"] = purpose
	sub := str(b, "subnetwork")
	if sub == "" {
		// The default network's subnet in the region, when there is one.
		sub = subnetRel(project, region, "default")
		if _, ok := s.res[sub]; !ok {
			return required("resource.subnetwork")
		}
	}
	sub = Rel(sub, project)
	if !strings.Contains(sub, "/") {
		sub = subnetRel(project, region, sub)
	}
	so, ok := s.res[sub]
	if !ok {
		return notFound(sub)
	}
	if lastSeg(str(so, "region")) != region {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork must be in region '%s'.", sub, region)
	}
	o["subnetwork"], o["network"] = sub, str(so, "network")
	n, _ := ParseCIDR(str(so, "ipCidrRange"))
	used := s.usedInSubnetLocked(sub)
	if v, ok := b["prefixLength"].(float64); ok && v != 0 && v != 32 {
		return invalid("Invalid value for field 'resource.prefixLength': '%v'. A regional internal address is one address.", v)
	}
	if addr != "" {
		ip := net.ParseIP(addr).To4()
		if ip == nil {
			return invalid("Invalid value for field 'resource.address': '%s'. Must be a valid IPv4 address.", addr)
		}
		if !n.Contains(ip) {
			return invalid("Invalid value for field 'resource.address': '%s'. The specified IP address is not within the IP range of subnetwork '%s' (%s).", addr, lastSeg(sub), n)
		}
		if reservedAddr(n, ip) {
			return invalid("Invalid value for field 'resource.address': '%s'. The address is reserved by Google Cloud in subnetwork '%s'.", addr, lastSeg(sub))
		}
		if u := used[ip.String()]; u != "" {
			switch {
			case str(o, "kind") == "compute#address" && strings.Contains(u, "/instances/"):
				// Promoting an instance's ephemeral internal address to a
				// static one (vpc/docs/reserve-static-internal-ip-address,
				// "Promote an ephemeral internal IP address", read
				// 2026-11-22): the address is reserved IN_USE by it (#2140).
				o["status"], o["users"] = "IN_USE", []any{u}
			case str(o, "kind") != "compute#address" && strings.Contains(u, "/addresses/") && str(s.res[u], "status") == "RESERVED" &&
				str(s.res[u], "purpose") == "GCE_ENDPOINT":
				// An instance given a reserved static internal address as
				// its networkIP takes it (#2140); insertInstance marks it
				// IN_USE.
				o["_static"] = u
			default:
				return errf(http.StatusBadRequest, "invalid", "IP address '%s' is already being used by another resource.", ip)
			}
		}
		o["address"] = ip.String()
		return nil
	}
	lo := ip4(n.IP)
	for v := lo + 2; v < lo+rangeSize(n)-2; v++ {
		if used[fromU32(v).String()] == "" {
			o["address"] = fromU32(v).String()
			return nil
		}
	}
	return errf(http.StatusBadRequest, "ipSpaceExhausted", "IP space of '%s' is exhausted.", sub)
}

func (s *Server) usedInSubnetLocked(sub string) map[string]string {
	used := map[string]string{}
	for k, a := range s.res {
		if strings.Contains(k, "/addresses/") && str(a, "subnetwork") == sub {
			used[str(a, "address")] = k
		}
		if strings.Contains(k, "/instances/") {
			nics, _ := a["networkInterfaces"].([]any)
			for _, e := range nics {
				if nic, ok := e.(map[string]any); ok && str(nic, "subnetwork") == sub {
					used[str(nic, "networkIP")] = k
				}
			}
		}
	}
	return used
}

func (s *Server) globalInternal(project string, b, o obj, purpose, addr string) error {
	nw := str(b, "network")
	if nw == "" {
		return required("resource.network")
	}
	nw = Rel(nw, project)
	if !strings.Contains(nw, "/") {
		nw = networkRel(project, nw)
	}
	if _, ok := s.res[nw]; !ok {
		return notFound(nw)
	}
	o["network"] = nw
	switch purpose {
	case "VPC_PEERING":
		o["purpose"] = purpose
		pl, _ := b["prefixLength"].(float64)
		if pl == 0 {
			return required("resource.prefixLength")
		}
		if pl < 8 || pl > 29 {
			return invalid("Invalid value for field 'resource.prefixLength': '%v'. Must be between 8 and 29.", pl)
		}
		o["prefixLength"] = pl
		ranges := s.rangesLocked(nw, "")
		if addr != "" {
			n, ok := ParseCIDR(fmt.Sprintf("%s/%d", addr, int(pl)))
			if !ok {
				return invalid("Invalid value for field 'resource.address': '%s'. The address must be the first address of a /%d range.", addr, int(pl))
			}
			if _, err := checkSubnetRange("resource.address", n.String()); err != nil {
				return err
			}
			if err := checkFree(n, ranges); err != nil {
				return err
			}
			o["address"] = n.IP.String()
			return nil
		}
		// Google picks a free block of the private ranges; here, the first
		// one in 10.0.0.0/8 outside 10.128.0.0/9.
		size := uint32(1) << (32 - int(pl))
		base := ip4(net.IPv4(10, 0, 0, 0))
		for v := base; v < base+(1<<24); v += size {
			n := &net.IPNet{IP: fromU32(v), Mask: net.CIDRMask(int(pl), 32)}
			if Overlaps(n, autoBlock) || checkFree(n, ranges) != nil {
				continue
			}
			o["address"] = n.IP.String()
			return nil
		}
		return errf(http.StatusBadRequest, "ipSpaceExhausted", "No free /%d range for network '%s'.", int(pl), lastSeg(nw))
	case "PRIVATE_SERVICE_CONNECT":
		o["purpose"] = purpose
		if addr == "" {
			return required("resource.address")
		}
		ip := net.ParseIP(addr).To4()
		if ip == nil {
			return invalid("Invalid value for field 'resource.address': '%s'. Must be a valid IPv4 address.", addr)
		}
		for _, r := range s.rangesLocked(nw, "") {
			if r.n.Contains(ip) {
				return invalid("Invalid value for field 'resource.address': '%s'. The address conflicts with %s of '%s'.", addr, r.n, lastSeg(r.holder))
			}
		}
		o["address"] = ip.String()
		return nil
	case "":
		return required("resource.purpose")
	}
	return invalid("Invalid value for field 'resource.purpose': '%s'. A global internal address is VPC_PEERING or PRIVATE_SERVICE_CONNECT.", purpose)
}

func (s *Server) freeExternalLocked(pool *net.IPNet) (string, error) {
	used := map[string]bool{}
	for k, a := range s.res {
		if strings.Contains(k, "/addresses/") {
			used[str(a, "address")] = true
		}
	}
	lo := ip4(pool.IP)
	for v := lo + 1; v < lo+rangeSize(pool)-1; v++ {
		if ip := fromU32(v).String(); !used[ip] {
			return ip, nil
		}
	}
	return "", errf(http.StatusForbidden, "quotaExceeded", "Quota 'STATIC_ADDRESSES' exceeded: every local external address of %s is reserved.", pool)
}

func labelFingerprint(v any) string {
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v;", k, m[k])
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)[:8])
}

// setLabels replaces an address's labels under its label fingerprint.
func (s *Server) setLabels(project, scope, rel string, b obj) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if fp := str(b, "labelFingerprint"); fp != str(o, "labelFingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Labels fingerprint either invalid or resource labels have changed")
	}
	l, _ := b["labels"].(map[string]any)
	if len(l) == 0 {
		delete(o, "labels")
	} else {
		o["labels"] = l
	}
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	return s.done(project, scope, "setLabels", rel, o["id"])
}
