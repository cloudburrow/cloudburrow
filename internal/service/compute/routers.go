package compute

import (
	"fmt"
	"math/bits"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Cloud Router and Cloud NAT (#2140): routers.insert, get, list, patch,
// update, delete, aggregatedList, getRouterStatus, getNatMappingInfo and
// getNatIpInfo, as https://docs.cloud.google.com/compute/docs/reference/rest/v1/routers
// and the Cloud Router and Cloud NAT docs (network-connectivity/docs/router,
// nat/docs/overview, nat/docs/ports-and-addresses, read 2026-11-22)
// describe them:
//
//   - a router is in one region and one network; a region and network may
//     have many routers. Its bgp.asn is an RFC 6996 private ASN (64512 to
//     65534, 4200000000 to 4294967294) or Google's 16550; custom
//     advertisements (advertisedGroups ALL_SUBNETS, advertisedIpRanges)
//     need advertiseMode CUSTOM; keepaliveInterval is 20 to 60 seconds.
//   - an interface links at most one of a VPN tunnel, an Interconnect
//     attachment or a subnetwork (a router appliance). Cloud VPN and
//     Interconnect are not served, so the first two are refused 501; a
//     subnetwork interface is checked against the router's network and
//     region. A BGP peer names an existing interface, a peer ASN, and on a
//     subnetwork interface the router appliance VM it peers with.
//   - a router appliance peer's BGP session is real (bgpspeakers.go):
//     GoBGP speaks for each subnetwork interface at its privateIpAddress
//     on the network's VXLAN segment, getRouterStatus reports each
//     session's state, uptime and learned routes, and the network's VMs
//     route what the peers advertise to the appliance. A peer whose
//     speaker has not reported is DOWN, with the routes the router would
//     advertise to it.
//   - a NAT gateway (nats[]) translates the subnets it names, in the
//     router's region and network; a subnet is NAT'd by one gateway only,
//     and a gateway of ALL_SUBNETWORKS_* is a region and network's only one.
//     MANUAL_ONLY uses reserved regional external addresses, which become
//     IN_USE with the router as their user; AUTO_ONLY is given addresses.
//     A VM with no external IP reaches the internet only through it: the
//     VM's guest drops its egress to public addresses otherwise (vpc.go).

// Google's default and limits of a NAT gateway's ports and timeouts.
const (
	natPortsPerIP     = 64512 // ports 1024 to 65535 of each NAT IP
	natDefaultMin     = 64
	natDefaultMinDyn  = 32
	natDefaultMaxDyn  = 65536
	natStaticMaxMin   = 57344
	bgpDefaultKeep    = 20
	bgpDefaultPrio    = 100
	natTypePublic     = "PUBLIC"
	natTypePrivate    = "PRIVATE"
	natAllAll         = "ALL_SUBNETWORKS_ALL_IP_RANGES"
	natAllPrimary     = "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES"
	natList           = "LIST_OF_SUBNETWORKS"
	natAutoOnly       = "AUTO_ONLY"
	natManualOnly     = "MANUAL_ONLY"
	routerKind        = "compute#router"
	privateASNMessage = "ASN must be a private ASN (64512-65534 or 4200000000-4294967294) or 16550."
)

// NATTimeoutDefaults are a NAT gateway's timeouts when unset, in seconds.
var NATTimeoutDefaults = map[string]int{"udpIdleTimeoutSec": 30, "icmpIdleTimeoutSec": 30,
	"tcpEstablishedIdleTimeoutSec": 1200, "tcpTransitoryIdleTimeoutSec": 30, "tcpTimeWaitTimeoutSec": 120}

func routerRel(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/routers/" + name
}

// routers serves a region's routers.
func (s *Server) routers(c call, region string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/regions/" + region + "/routers"
	get, post := c.method == http.MethodGet, c.method == http.MethodPost
	switch {
	case n == 3 && get:
		return s.listOut("compute#routerList", coll, s.res, c.query)
	case n == 3 && post:
		return s.insertRouter(c.project, region, c.body)
	case n == 4 && get:
		return s.getRes(coll + "/" + p[3])
	case n == 4 && c.method == http.MethodDelete:
		return s.deleteRouter(c.project, region, p[3])
	case n == 4 && (c.method == http.MethodPatch || c.method == http.MethodPut):
		return s.updateRouter(c.project, region, p[3], c.body, c.method == http.MethodPut)
	case n == 5 && get && p[4] == "getRouterStatus":
		return s.routerStatus(c.project, region, p[3])
	case n == 5 && get && p[4] == "getNatMappingInfo":
		return s.natMappingInfo(c, region, p[3])
	case n == 5 && get && p[4] == "getNatIpInfo":
		return s.natIPInfo(c, region, p[3])
	case n == 5 && routePolicyMethods[p[4]] != "":
		return s.routePolicies(c, region, p[3], p[4])
	case n == 5 && post && p[4] == "preview":
		r, err := s.checkRouter(c.project, region, c.body, nil)
		if err != nil {
			return nil, err
		}
		return obj{"resource": r}, nil
	}
	return nil, unimplemented("%s %s/%s: CloudBurrow serves routers' insert, get, list, patch, update, delete, preview, getRouterStatus, getNatMappingInfo, getNatIpInfo, route policies and named sets; listBgpRoutes is not served",
		c.method, coll, strings.Join(p[3:], "/"))
}

func (s *Server) insertRouter(project, region string, b obj) (obj, error) {
	r, err := s.checkRouter(project, region, b, nil)
	if err != nil {
		return nil, err
	}
	rel := str(r, "selfLink")
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	r["id"], r["creationTimestamp"] = newID(), s.stamp()
	s.claimNatIPsLocked(rel, nil, r)
	s.res[rel] = norm(r)
	return s.done(project, "regions/"+region, "insert", rel, s.res[rel]["id"])
}

func (s *Server) updateRouter(project, region, name string, b obj, replace bool) (obj, error) {
	rel := routerRel(project, region, name)
	old, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	merged := obj{}
	if !replace {
		for k, v := range deepCopy(old).(obj) {
			merged[k] = v
		}
	}
	for k, v := range norm(b) {
		merged[k] = v
	}
	if nm := str(b, "name"); nm != "" && nm != name {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. The name of a router cannot be changed.", nm)
	}
	merged["name"] = name
	if nw := str(b, "network"); nw != "" {
		if rn := s.routerNetwork(project, nw); rn != str(old, "network") {
			return nil, invalid("Invalid value for field 'resource.network': '%s'. The network of a router cannot be changed.", nw)
		}
	}
	if merged["network"] == nil {
		merged["network"] = old["network"]
	}
	r, err := s.checkRouter(project, region, merged, old)
	if err != nil {
		return nil, err
	}
	r["id"], r["creationTimestamp"] = old["id"], old["creationTimestamp"]
	s.claimNatIPsLocked(rel, old, r)
	s.res[rel] = norm(r)
	op := "patch"
	if replace {
		op = "update"
	}
	return s.done(project, "regions/"+region, op, rel, old["id"])
}

func (s *Server) deleteRouter(project, region, name string) (obj, error) {
	rel := routerRel(project, region, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	s.claimNatIPsLocked(rel, o, nil)
	delete(s.res, rel)
	return s.done(project, "regions/"+region, "delete", rel, o["id"])
}

func (s *Server) routerNetwork(project, nw string) string {
	rel, ok := NetworkName(nw, project)
	if !ok {
		return nw
	}
	return rel
}

// checkRouter is b checked and completed as the router rel would be
// stored; old is the router it replaces, if any.
func (s *Server) checkRouter(project, region string, b, old obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := routerRel(project, region, name)
	nw := str(b, "network")
	if nw == "" {
		return nil, required("resource.network")
	}
	nwRel, ok := NetworkName(nw, project)
	if !ok {
		return nil, invalid("Invalid value for field 'resource.network': '%s'. The URL is malformed.", nw)
	}
	if _, ok := s.res[nwRel]; !ok {
		return nil, notFound(nwRel)
	}
	r := obj{"kind": routerKind, "name": name, "selfLink": rel, "network": nwRel,
		"region": "projects/" + project + "/regions/" + region}
	for _, k := range []string{"_routePolicies", "_namedSets"} {
		if old != nil && old[k] != nil {
			r[k] = old[k] // kept across patch and update (routepolicies.go)
		}
	}
	if d := str(b, "description"); d != "" {
		r["description"] = d
	}
	if v, ok := b["encryptedInterconnectRouter"].(bool); ok && v {
		return nil, unimplemented("encryptedInterconnectRouter: HA VPN over Cloud Interconnect is not served")
	}
	if l := sliceOf(b["md5AuthenticationKeys"]); len(l) > 0 {
		var keys []any
		for i, e := range l {
			m, _ := e.(map[string]any)
			f := "resource.md5AuthenticationKeys[" + strconv.Itoa(i) + "]"
			if err := checkName(f+".name", str(m, "name")); err != nil {
				return nil, err
			}
			k := str(m, "key")
			if k == "" {
				k = str(m, "_key") // kept from the router a patch updates
			}
			if k == "" || len(k) > 80 {
				return nil, invalid("Invalid value for field '%s.key'. The key must be 1 to 80 printable ASCII characters.", f)
			}
			// The key is write-only: Google never returns it.
			keys = append(keys, obj{"name": str(m, "name"), "_key": k})
		}
		r["md5AuthenticationKeys"] = keys
	}
	var asn float64
	if bgp, ok := b["bgp"].(map[string]any); ok && len(bgp) > 0 {
		out, err := checkBGP(bgp, "resource.bgp", true)
		if err != nil {
			return nil, err
		}
		asn, _ = out["asn"].(float64)
		r["bgp"] = out
	}
	ifaces, err := s.checkInterfaces(project, region, nwRel, sliceOf(b["interfaces"]))
	if err != nil {
		return nil, err
	}
	if len(ifaces) > 0 {
		r["interfaces"] = ifaces
	}
	peers, err := s.checkPeers(project, region, nwRel, asn, ifaces, r, sliceOf(b["bgpPeers"]))
	if err != nil {
		return nil, err
	}
	if len(peers) > 0 {
		r["bgpPeers"] = peers
	}
	nats, err := s.checkNats(project, region, nwRel, rel, old, sliceOf(b["nats"]))
	if err != nil {
		return nil, err
	}
	if len(nats) > 0 {
		r["nats"] = nats
	}
	return r, nil
}

// privateASN reports whether asn is one a Cloud Router may take.
func privateASN(asn float64) bool {
	return asn == 16550 || (asn >= 64512 && asn <= 65534) || (asn >= 4200000000 && asn <= 4294967294)
}

// checkBGP checks a router's bgp (top true) or a peer's advertisement
// fields.
func checkBGP(b map[string]any, field string, top bool) (obj, error) {
	out := obj{}
	if top {
		asn, ok := b["asn"].(float64)
		if !ok || asn == 0 {
			return nil, required(field + ".asn")
		}
		if !privateASN(asn) {
			return nil, invalid("Invalid value for field '%s.asn': '%d'. %s", field, int64(asn), privateASNMessage)
		}
		out["asn"] = asn
		keep := float64(bgpDefaultKeep)
		if v, ok := b["keepaliveInterval"].(float64); ok && v != 0 {
			if v < 20 || v > 60 {
				return nil, invalid("Invalid value for field '%s.keepaliveInterval': '%d'. Must be between 20 and 60 seconds.", field, int64(v))
			}
			keep = v
		}
		out["keepaliveInterval"] = keep
		if ir := str(b, "identifierRange"); ir != "" {
			n, ok := ParseCIDR(ir)
			if !ok || !linkLocal.Contains(n.IP) || maskBits(n) > 30 {
				return nil, invalid("Invalid value for field '%s.identifierRange': '%s'. Must be a link-local IPv4 range in 169.254.0.0/16 of /30 or larger.", field, ir)
			}
			out["identifierRange"] = n.String()
		}
	}
	mode := str(b, "advertiseMode")
	switch mode {
	case "":
		mode = "DEFAULT"
	case "DEFAULT", "CUSTOM":
	default:
		return nil, invalid("Invalid value for field '%s.advertiseMode': '%s'. Must be one of [CUSTOM, DEFAULT]", field, mode)
	}
	out["advertiseMode"] = mode
	groups, ranges := strsAny(b["advertisedGroups"]), sliceOf(b["advertisedIpRanges"])
	if mode != "CUSTOM" && (len(groups) > 0 || len(ranges) > 0) {
		return nil, invalid("Invalid value for field '%s.advertisedGroups': custom advertised groups and IP ranges can be set only when advertiseMode is CUSTOM.", field)
	}
	for _, g := range groups {
		if g != "ALL_SUBNETS" {
			return nil, invalid("Invalid value for field '%s.advertisedGroups': '%s'. Must be one of [ALL_SUBNETS]", field, g)
		}
	}
	if len(groups) > 0 {
		out["advertisedGroups"] = toAny(groups)
	}
	var rs []any
	seen := map[string]bool{}
	for i, e := range ranges {
		m, _ := e.(map[string]any)
		cidr := str(m, "range")
		if cidr != "" && !strings.Contains(cidr, "/") {
			cidr += "/32"
		}
		nr, ok := ParseCIDR(cidr)
		if !ok {
			return nil, invalid("Invalid value for field '%s.advertisedIpRanges[%d].range': '%s'. Must be a valid IPv4 CIDR range.", field, i, str(m, "range"))
		}
		if seen[nr.String()] {
			return nil, invalid("Invalid value for field '%s.advertisedIpRanges[%d].range': '%s'. Duplicate advertised IP range.", field, i, nr)
		}
		seen[nr.String()] = true
		er := obj{"range": nr.String()}
		if d := str(m, "description"); d != "" {
			er["description"] = d
		}
		rs = append(rs, er)
	}
	if len(rs) > 0 {
		out["advertisedIpRanges"] = rs
	}
	return out, nil
}

var linkLocal = cidrs("169.254.0.0/16")[0]

func maskBits(n *net.IPNet) int {
	o, _ := n.Mask.Size()
	return o
}

func (s *Server) checkInterfaces(project, region, network string, l []any) ([]any, error) {
	var out []any
	seen := map[string]bool{}
	for i, e := range l {
		m, _ := e.(map[string]any)
		f := "resource.interfaces[" + strconv.Itoa(i) + "]"
		name := str(m, "name")
		if err := checkName(f+".name", name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Duplicate interface name.", f, name)
		}
		seen[name] = true
		o := obj{"name": name, "managementType": "MANAGED_BY_USER"}
		linked := 0
		for _, k := range []string{"linkedVpnTunnel", "linkedInterconnectAttachment", "subnetwork"} {
			if str(m, k) != "" {
				linked++
			}
		}
		if linked > 1 {
			return nil, invalid("Invalid value for field '%s': an interface links only one of linkedVpnTunnel, linkedInterconnectAttachment and subnetwork.", f)
		}
		if str(m, "linkedVpnTunnel") != "" {
			return nil, unimplemented("%s.linkedVpnTunnel: Cloud VPN tunnels are not served, so a router interface cannot link one", f)
		}
		if str(m, "linkedInterconnectAttachment") != "" {
			return nil, unimplemented("%s.linkedInterconnectAttachment: Cloud Interconnect is not served, so a router interface cannot link a VLAN attachment", f)
		}
		if ir := str(m, "ipRange"); ir != "" {
			ip, _, err := net.ParseCIDR(ir)
			if err != nil || ip.To4() == nil || !linkLocal.Contains(ip) {
				return nil, invalid("Invalid value for field '%s.ipRange': '%s'. Must be an IPv4 address and mask in 169.254.0.0/16.", f, ir)
			}
			// Google keeps the interface's own address in the range.
			o["ipRange"] = ir
			o["ipVersion"] = "IPV4"
		}
		if sub := str(m, "subnetwork"); sub != "" {
			sr := Rel(sub, project)
			if !strings.Contains(sr, "/") {
				sr = subnetRel(project, region, sr)
			}
			so, ok := s.res[sr]
			if !ok {
				return nil, notFound(sr)
			}
			if str(so, "network") != network || lastSeg(str(so, "region")) != region {
				return nil, invalid("Invalid value for field '%s.subnetwork': '%s'. The subnetwork must be in the router's network and region.", f, sub)
			}
			o["subnetwork"] = sr
			ip := str(m, "privateIpAddress")
			if ip == "" {
				return nil, required(f + ".privateIpAddress")
			}
			sn, _ := ParseCIDR(str(so, "ipCidrRange"))
			if pip := net.ParseIP(ip).To4(); pip == nil || sn == nil || !sn.Contains(pip) {
				return nil, invalid("Invalid value for field '%s.privateIpAddress': '%s'. The address must be in subnetwork '%s' (%s).", f, ip, lastSeg(sr), sn)
			}
			o["privateIpAddress"] = ip
		}
		if ri := str(m, "redundantInterface"); ri != "" {
			o["redundantInterface"] = ri
		}
		out = append(out, o)
	}
	for i, e := range out {
		if ri := str(e.(obj), "redundantInterface"); ri != "" && !seen[ri] {
			return nil, invalid("Invalid value for field 'resource.interfaces[%d].redundantInterface': '%s'. No interface of that name.", i, ri)
		}
	}
	return out, nil
}

func (s *Server) checkPeers(project, region, network string, asn float64, ifaces []any, r obj, l []any) ([]any, error) {
	byName := map[string]obj{}
	for _, e := range ifaces {
		byName[str(e.(obj), "name")] = e.(obj)
	}
	keys := map[string]bool{}
	for _, e := range sliceOf(r["md5AuthenticationKeys"]) {
		keys[str(e.(obj), "name")] = true
	}
	var out []any
	seen := map[string]bool{}
	for i, e := range l {
		m, _ := e.(map[string]any)
		f := "resource.bgpPeers[" + strconv.Itoa(i) + "]"
		name := str(m, "name")
		if err := checkName(f+".name", name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Duplicate BGP peer name.", f, name)
		}
		seen[name] = true
		if asn == 0 {
			return nil, invalid("Invalid value for field '%s': a router needs bgp.asn before it can have BGP peers.", f)
		}
		in := str(m, "interfaceName")
		if in == "" {
			return nil, required(f + ".interfaceName")
		}
		iface, ok := byName[in]
		if !ok {
			return nil, invalid("Invalid value for field '%s.interfaceName': '%s'. The router has no interface of that name.", f, in)
		}
		pa, _ := m["peerAsn"].(float64)
		if pa <= 0 || pa > 4294967294 {
			return nil, invalid("Invalid value for field '%s.peerAsn': '%v'. Must be an ASN between 1 and 4294967294.", f, m["peerAsn"])
		}
		adv, err := checkBGP(m, f, false)
		if err != nil {
			return nil, err
		}
		o := obj{"name": name, "interfaceName": in, "peerAsn": pa, "managementType": "MANAGED_BY_USER", "enable": "TRUE",
			"enableIpv4": true, "enableIpv6": false}
		for k, v := range adv {
			o[k] = v
		}
		prio := float64(bgpDefaultPrio)
		if v, ok := m["advertisedRoutePriority"].(float64); ok {
			if v < 0 || v > 65535 {
				return nil, invalid("Invalid value for field '%s.advertisedRoutePriority': '%d'. Must be between 0 and 65535.", f, int64(v))
			}
			prio = v
		}
		o["advertisedRoutePriority"] = prio
		switch en := str(m, "enable"); en {
		case "", "TRUE":
		case "FALSE":
			o["enable"] = en
		default:
			return nil, invalid("Invalid value for field '%s.enable': '%s'. Must be one of [FALSE, TRUE]", f, en)
		}
		var ipRange *net.IPNet
		var ifaceIP string
		if ir := str(iface, "ipRange"); ir != "" {
			if ip, n, err := net.ParseCIDR(ir); err == nil {
				ipRange, ifaceIP = n, ip.String()
			}
		}
		for _, k := range []string{"ipAddress", "peerIpAddress"} {
			v := str(m, k)
			if v == "" {
				continue
			}
			ip := net.ParseIP(v).To4()
			if ip == nil {
				return nil, invalid("Invalid value for field '%s.%s': '%s'. Must be a valid IPv4 address.", f, k, v)
			}
			if ipRange != nil && !ipRange.Contains(ip) {
				return nil, invalid("Invalid value for field '%s.%s': '%s'. The address must be in the interface's IP range %s.", f, k, v, ipRange)
			}
			o[k] = ip.String()
		}
		if ip := str(m, "ipAddress"); ip == "" && str(iface, "privateIpAddress") != "" {
			o["ipAddress"] = str(iface, "privateIpAddress")
		} else if ip == "" && ifaceIP != "" {
			o["ipAddress"] = ifaceIP
		}
		if ra := str(m, "routerApplianceInstance"); ra != "" {
			vm := Rel(ra, project)
			vo, ok := s.res[vm]
			if !ok || !strings.Contains(vm, "/instances/") {
				return nil, notFound(vm)
			}
			if regionOfZone(lastSeg(str(vo, "zone"))) != region {
				return nil, invalid("Invalid value for field '%s.routerApplianceInstance': '%s'. The instance must be in the router's region.", f, ra)
			}
			if str(iface, "subnetwork") == "" {
				return nil, invalid("Invalid value for field '%s.routerApplianceInstance': a router appliance peer needs an interface with a subnetwork.", f)
			}
			o["routerApplianceInstance"] = vm
			if str(m, "peerIpAddress") == "" {
				if v, ok := vmNetOf(vm, vo); ok {
					o["peerIpAddress"] = v.ip
				}
			}
		} else if str(iface, "subnetwork") != "" {
			return nil, required(f + ".routerApplianceInstance")
		}
		if k := str(m, "md5AuthenticationKeyName"); k != "" {
			if !keys[k] {
				return nil, invalid("Invalid value for field '%s.md5AuthenticationKeyName': '%s'. The router has no MD5 authentication key of that name.", f, k)
			}
			o["md5AuthenticationKeyName"] = k
		}
		bfd := obj{"sessionInitializationMode": "DISABLED", "minTransmitInterval": 1000.0, "minReceiveInterval": 1000.0, "multiplier": 5.0}
		if in, ok := m["bfd"].(map[string]any); ok {
			switch mode := str(in, "sessionInitializationMode"); mode {
			case "":
			case "ACTIVE", "PASSIVE", "DISABLED":
				bfd["sessionInitializationMode"] = mode
			default:
				return nil, invalid("Invalid value for field '%s.bfd.sessionInitializationMode': '%s'. Must be one of [ACTIVE, DISABLED, PASSIVE]", f, mode)
			}
			for k, lim := range map[string][2]float64{"minTransmitInterval": {1000, 30000}, "minReceiveInterval": {1000, 30000}, "multiplier": {5, 16}} {
				if v, ok := in[k].(float64); ok && v != 0 {
					if v < lim[0] || v > lim[1] {
						return nil, invalid("Invalid value for field '%s.bfd.%s': '%d'. Must be between %d and %d.", f, k, int64(v), int64(lim[0]), int64(lim[1]))
					}
					bfd[k] = v
				}
			}
		}
		o["bfd"] = bfd
		if cl := sliceOf(m["customLearnedIpRanges"]); len(cl) > 0 {
			var rs []any
			for j, e := range cl {
				cm, _ := e.(map[string]any)
				cidr := str(cm, "range")
				if cidr != "" && !strings.Contains(cidr, "/") {
					cidr += "/32"
				}
				nr, ok := ParseCIDR(cidr)
				if !ok {
					return nil, invalid("Invalid value for field '%s.customLearnedIpRanges[%d].range': '%s'.", f, j, str(cm, "range"))
				}
				rs = append(rs, obj{"range": nr.String()})
			}
			o["customLearnedIpRanges"] = rs
			clp := float64(100)
			if v, ok := m["customLearnedRoutePriority"].(float64); ok {
				if v < 0 || v > 65535 {
					return nil, invalid("Invalid value for field '%s.customLearnedRoutePriority': '%d'. Must be between 0 and 65535.", f, int64(v))
				}
				clp = v
			}
			o["customLearnedRoutePriority"] = clp
		}
		if err := peerPolicies(f, m, o, r); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// natSubnetsLocked are the subnets (and how) the router's region and
// network's NAT gateways translate, other than those of skip/skipNat.
type natClaim struct {
	router, nat, mode, typ string
	subnets                map[string]bool
}

func (s *Server) natClaimsLocked(network, region, skipRouter string) []natClaim {
	var out []natClaim
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") || k == skipRouter || str(r, "network") != network || lastSeg(str(r, "region")) != region {
			continue
		}
		for _, e := range sliceOf(r["nats"]) {
			m, _ := e.(map[string]any)
			c := natClaim{router: k, nat: str(m, "name"), mode: str(m, "sourceSubnetworkIpRangesToNat"), typ: natTypeOf(m), subnets: map[string]bool{}}
			for _, se := range sliceOf(m["subnetworks"]) {
				sm, _ := se.(map[string]any)
				c.subnets[str(sm, "name")] = true
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].router+out[i].nat < out[j].router+out[j].nat })
	return out
}

func isPow2(v int) bool { return v > 0 && bits.OnesCount(uint(v)) == 1 }

func (s *Server) checkNats(project, region, network, router string, old obj, l []any) ([]any, error) {
	claims := s.natClaimsLocked(network, region, router)
	var out []any
	seen := map[string]bool{}
	for i, e := range l {
		m, _ := e.(map[string]any)
		f := "resource.nats[" + strconv.Itoa(i) + "]"
		name := str(m, "name")
		if err := checkName(f+".name", name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Duplicate NAT name.", f, name)
		}
		seen[name] = true
		typ := str(m, "type")
		switch typ {
		case "", natTypePublic:
			typ = natTypePublic
		case natTypePrivate:
		default:
			return nil, invalid("Invalid value for field '%s.type': '%s'. Must be one of [PRIVATE, PUBLIC]", f, typ)
		}
		o := obj{"name": name, "type": typ, "endpointTypes": []any{"ENDPOINT_TYPE_VM"}}
		for _, et := range strsAny(m["endpointTypes"]) {
			if et != "ENDPOINT_TYPE_VM" {
				return nil, unimplemented("%s.endpointTypes %s: Secure Web Proxy and Google-managed endpoints are not served", f, et)
			}
		}
		mode := str(m, "sourceSubnetworkIpRangesToNat")
		cl := natClaim{router: router, nat: name, mode: mode, typ: typ, subnets: map[string]bool{}}
		switch mode {
		case "":
			return nil, required(f + ".sourceSubnetworkIpRangesToNat")
		case natAllAll, natAllPrimary:
			if len(sliceOf(m["subnetworks"])) > 0 {
				return nil, invalid("Invalid value for field '%s.subnetworks': subnetworks are listed only when sourceSubnetworkIpRangesToNat is LIST_OF_SUBNETWORKS.", f)
			}
		case natList:
			subs := sliceOf(m["subnetworks"])
			if len(subs) == 0 {
				return nil, invalid("Invalid value for field '%s.subnetworks': at least one subnetwork is required when sourceSubnetworkIpRangesToNat is LIST_OF_SUBNETWORKS.", f)
			}
			var os []any
			for j, se := range subs {
				sm, _ := se.(map[string]any)
				sf := f + ".subnetworks[" + strconv.Itoa(j) + "]"
				sr := Rel(str(sm, "name"), project)
				if sr == "" {
					return nil, required(sf + ".name")
				}
				if !strings.Contains(sr, "/") {
					sr = subnetRel(project, region, sr)
				}
				so, ok := s.res[sr]
				if !ok {
					return nil, notFound(sr)
				}
				if str(so, "network") != network || lastSeg(str(so, "region")) != region {
					return nil, invalid("Invalid value for field '%s.name': '%s'. The subnetwork must be in the router's network and region.", sf, str(sm, "name"))
				}
				if cl.subnets[sr] {
					return nil, invalid("Invalid value for field '%s.name': '%s'. The subnetwork is listed twice.", sf, lastSeg(sr))
				}
				cl.subnets[sr] = true
				ranges := strsAny(sm["sourceIpRangesToNat"])
				if len(ranges) == 0 {
					ranges = []string{"ALL_IP_RANGES"}
				}
				for _, rg := range ranges {
					switch rg {
					case "ALL_IP_RANGES", "PRIMARY_IP_RANGE", "LIST_OF_SECONDARY_IP_RANGES":
					default:
						return nil, invalid("Invalid value for field '%s.sourceIpRangesToNat': '%s'. Must be one of [ALL_IP_RANGES, LIST_OF_SECONDARY_IP_RANGES, PRIMARY_IP_RANGE]", sf, rg)
					}
				}
				if hasStr(ranges, "ALL_IP_RANGES") && len(ranges) > 1 {
					return nil, invalid("Invalid value for field '%s.sourceIpRangesToNat': ALL_IP_RANGES cannot be combined with other values.", sf)
				}
				names := strsAny(sm["secondaryIpRangeNames"])
				if hasStr(ranges, "LIST_OF_SECONDARY_IP_RANGES") != (len(names) > 0) {
					return nil, invalid("Invalid value for field '%s.secondaryIpRangeNames': secondary IP range names are given exactly when sourceIpRangesToNat has LIST_OF_SECONDARY_IP_RANGES.", sf)
				}
				have := map[string]bool{}
				for _, sec := range secondaries(so) {
					have[str(sec, "rangeName")] = true
				}
				for _, nm := range names {
					if !have[nm] {
						return nil, invalid("Invalid value for field '%s.secondaryIpRangeNames': '%s'. Subnetwork '%s' has no secondary range of that name.", sf, nm, lastSeg(sr))
					}
				}
				se := obj{"name": sr, "sourceIpRangesToNat": toAny(ranges)}
				if len(names) > 0 {
					se["secondaryIpRangeNames"] = toAny(names)
				}
				os = append(os, se)
			}
			o["subnetworks"] = os
		default:
			return nil, invalid("Invalid value for field '%s.sourceSubnetworkIpRangesToNat': '%s'. Must be one of [ALL_SUBNETWORKS_ALL_IP_RANGES, ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES, LIST_OF_SUBNETWORKS]", f, mode)
		}
		o["sourceSubnetworkIpRangesToNat"] = mode
		// One gateway per subnet; an ALL_SUBNETWORKS gateway is its
		// region and network's only one (nat/docs/overview, "Cloud NAT
		// rules and limits").
		for _, c := range claims {
			if c.typ != typ {
				// A subnet can use a Public NAT and a Private NAT gateway
				// at once (nat/docs/private-nat).
				continue
			}
			switch {
			case c.mode != natList || cl.mode != natList:
				return nil, invalid("Invalid value for field '%s.sourceSubnetworkIpRangesToNat': '%s'. NAT gateway '%s' of router '%s' already translates subnetworks of network '%s' in region '%s'; a gateway that translates all subnetworks must be the only one.",
					f, mode, c.nat, lastSeg(c.router), lastSeg(network), region)
			default:
				for sub := range cl.subnets {
					if c.subnets[sub] {
						return nil, invalid("Invalid value for field '%s.subnetworks': subnetwork '%s' is already translated by NAT gateway '%s' of router '%s'.", f, lastSeg(sub), c.nat, lastSeg(c.router))
					}
				}
			}
		}
		claims = append(claims, cl)
		if typ == natTypePrivate {
			if err := checkPrivateNat(f, m); err != nil {
				return nil, err
			}
		} else if err := s.checkNatIPs(project, region, router, f, m, o); err != nil {
			return nil, err
		}
		if err := checkNatPorts(f, m, o); err != nil {
			return nil, err
		}
		for k := range NATTimeoutDefaults {
			if v, ok := m[k].(float64); ok && v != 0 {
				if v < 1 || v > 86400 {
					return nil, invalid("Invalid value for field '%s.%s': '%d'. Must be between 1 and 86400 seconds.", f, k, int64(v))
				}
				o[k] = v
			}
		}
		lc := obj{"enable": false, "filter": "ALL"}
		if in, ok := m["logConfig"].(map[string]any); ok {
			if v, ok := in["enable"].(bool); ok {
				lc["enable"] = v
			}
			switch fl := str(in, "filter"); fl {
			case "":
			case "ALL", "ERRORS_ONLY", "TRANSLATIONS_ONLY":
				lc["filter"] = fl
			default:
				return nil, invalid("Invalid value for field '%s.logConfig.filter': '%s'. Must be one of [ALL, ERRORS_ONLY, TRANSLATIONS_ONLY]", f, fl)
			}
		}
		o["logConfig"] = lc
		rules, err := s.checkNatRules(project, region, network, f, sliceOf(m["rules"]), o)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			o["rules"] = rules
		}
		// AUTO_ONLY's addresses, kept across updates of the gateway.
		if str(o, "natIpAllocateOption") == natAutoOnly {
			for _, oe := range sliceOf(old["nats"]) {
				if om, _ := oe.(map[string]any); str(om, "name") == name && om["_autoIps"] != nil {
					o["_autoIps"] = om["_autoIps"]
				}
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// addrRef resolves a NAT IP reference to a regional external address.
func (s *Server) addrRef(project, region, field, ref string) (string, error) {
	rel := Rel(ref, project)
	if !strings.Contains(rel, "/") {
		rel = "projects/" + project + "/regions/" + region + "/addresses/" + rel
	}
	a, ok := s.res[rel]
	if !ok || !strings.Contains(rel, "/addresses/") {
		return "", notFound(rel)
	}
	if str(a, "addressType") != "EXTERNAL" || lastSeg(str(a, "region")) != region {
		return "", invalid("Invalid value for field '%s': '%s'. A NAT IP must be a regional external IP address in region '%s'.", field, ref, region)
	}
	return rel, nil
}

func (s *Server) checkNatIPs(project, region, router, f string, m, o obj) error {
	opt := str(m, "natIpAllocateOption")
	ips, drain := strsAny(m["natIps"]), strsAny(m["drainNatIps"])
	switch opt {
	case "":
		return required(f + ".natIpAllocateOption")
	case natAutoOnly:
		if len(ips) > 0 || len(drain) > 0 {
			return invalid("Invalid value for field '%s.natIps': NAT IPs must be empty when natIpAllocateOption is AUTO_ONLY.", f)
		}
		tier := str(m, "autoNetworkTier")
		switch tier {
		case "":
			tier = "PREMIUM"
		case "PREMIUM", "STANDARD":
		default:
			return invalid("Invalid value for field '%s.autoNetworkTier': '%s'. Must be one of [PREMIUM, STANDARD]", f, tier)
		}
		o["autoNetworkTier"] = tier
	case natManualOnly:
		if len(ips) == 0 {
			return invalid("Invalid value for field '%s.natIps': at least one NAT IP is required when natIpAllocateOption is MANUAL_ONLY.", f)
		}
	default:
		return invalid("Invalid value for field '%s.natIpAllocateOption': '%s'. Must be one of [AUTO_ONLY, MANUAL_ONLY]", f, opt)
	}
	o["natIpAllocateOption"] = opt
	seen := map[string]bool{}
	for k, l := range map[string][]string{"natIps": ips, "drainNatIps": drain} {
		var rels []any
		for j, ref := range l {
			rel, err := s.addrRef(project, region, fmt.Sprintf("%s.%s[%d]", f, k, j), ref)
			if err != nil {
				return err
			}
			if seen[rel] {
				return invalid("Invalid value for field '%s.%s[%d]': '%s'. The address is given twice.", f, k, j, ref)
			}
			seen[rel] = true
			for _, u := range strs(s.res[rel], "users") {
				if u != router {
					return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
						"The address resource '%s' is already being used by '%s'", rel, u)
				}
			}
			rels = append(rels, rel)
		}
		if len(rels) > 0 {
			o[k] = rels
		}
	}
	return nil
}

func checkNatPorts(f string, m, o obj) error {
	dyn, _ := m["enableDynamicPortAllocation"].(bool)
	eim, eimSet := m["enableEndpointIndependentMapping"].(bool)
	if dyn && eim {
		return invalid("Invalid value for field '%s.enableEndpointIndependentMapping': Endpoint-Independent Mapping cannot be enabled with dynamic port allocation.", f)
	}
	if !eimSet && !dyn {
		// Google's default for a new public gateway (nat/docs/ports-and-addresses).
		eim = false
	}
	o["enableEndpointIndependentMapping"] = eim
	o["enableDynamicPortAllocation"] = dyn
	min, minSet := m["minPortsPerVm"].(float64)
	if minSet && min != 0 {
		if !dyn && (min < 2 || min > natStaticMaxMin) {
			// nat/docs/set-up-manage-network-address-translation: 2 to
			// 57344 with static port allocation.
			return invalid("Invalid value for field '%s.minPortsPerVm': '%d'. Must be between 2 and %d.", f, int64(min), natStaticMaxMin)
		}
		if dyn && (!isPow2(int(min)) || min < 32) {
			return invalid("Invalid value for field '%s.minPortsPerVm': '%d'. With dynamic port allocation it must be a power of 2 of at least 32.", f, int64(min))
		}
		o["minPortsPerVm"] = min
	}
	if max, ok := m["maxPortsPerVm"].(float64); ok && max != 0 {
		if !dyn {
			return invalid("Invalid value for field '%s.maxPortsPerVm': maxPortsPerVm can be set only with dynamic port allocation.", f)
		}
		lo := float64(natDefaultMinDyn)
		if minSet && min != 0 {
			lo = min
		}
		if !isPow2(int(max)) || max <= lo || max > natDefaultMaxDyn {
			return invalid("Invalid value for field '%s.maxPortsPerVm': '%d'. Must be a power of 2 greater than minPortsPerVm (%d), at most 65536.", f, int64(max), int64(lo))
		}
		o["maxPortsPerVm"] = max
	}
	return nil
}

func (s *Server) checkNatRules(project, region, network, f string, l []any, nat obj) ([]any, error) {
	if str(nat, "type") == natTypePrivate {
		return s.checkPrivateNatRules(project, region, network, f, l)
	}
	var out []any
	seen := map[int]bool{}
	for i, e := range l {
		m, _ := e.(map[string]any)
		rf := f + ".rules[" + strconv.Itoa(i) + "]"
		num, ok := m["ruleNumber"].(float64)
		if !ok {
			return nil, required(rf + ".ruleNumber")
		}
		if num < 0 || num > 65000 {
			return nil, invalid("Invalid value for field '%s.ruleNumber': '%d'. Must be between 0 and 65000.", rf, int64(num))
		}
		if seen[int(num)] {
			return nil, invalid("Invalid value for field '%s.ruleNumber': '%d'. Rule numbers are unique in a NAT gateway.", rf, int64(num))
		}
		seen[int(num)] = true
		match := strings.TrimSpace(str(m, "match"))
		if match == "" {
			return nil, required(rf + ".match")
		}
		if !strings.Contains(match, "destination.ip") {
			return nil, invalid("Invalid value for field '%s.match': '%s'. A public NAT rule matches on destination.ip, with inIpRange(destination.ip, 'CIDR') or destination.ip == 'IP'.", rf, match)
		}
		if str(nat, "natIpAllocateOption") != natManualOnly {
			return nil, invalid("Invalid value for field '%s': NAT rules need natIpAllocateOption MANUAL_ONLY.", rf)
		}
		if nat["enableEndpointIndependentMapping"] == true {
			return nil, invalid("Invalid value for field '%s': NAT rules cannot be used with Endpoint-Independent Mapping.", rf)
		}
		o := obj{"ruleNumber": num, "match": match}
		if d := str(m, "description"); d != "" {
			o["description"] = d
		}
		act, _ := m["action"].(map[string]any)
		oa := obj{}
		for _, k := range []string{"sourceNatActiveIps", "sourceNatDrainIps"} {
			var rels []any
			for j, ref := range strsAny(act[k]) {
				rel, err := s.addrRef(project, region, fmt.Sprintf("%s.action.%s[%d]", rf, k, j), ref)
				if err != nil {
					return nil, err
				}
				rels = append(rels, rel)
			}
			if len(rels) > 0 {
				oa[k] = rels
			}
		}
		if oa["sourceNatActiveIps"] == nil {
			return nil, required(rf + ".action.sourceNatActiveIps")
		}
		o["action"] = oa
		out = append(out, o)
	}
	return out, nil
}

// natAddrs are the addresses a router's gateways use.
func natAddrs(r obj) []string {
	var out []string
	for _, e := range sliceOf(r["nats"]) {
		m, _ := e.(map[string]any)
		out = append(out, strsAny(m["natIps"])...)
		out = append(out, strsAny(m["drainNatIps"])...)
		for _, re := range sliceOf(m["rules"]) {
			rm, _ := re.(map[string]any)
			a, _ := rm["action"].(map[string]any)
			out = append(out, strsAny(a["sourceNatActiveIps"])...)
			out = append(out, strsAny(a["sourceNatDrainIps"])...)
		}
	}
	return out
}

// claimNatIPsLocked moves the router as a user from old's NAT addresses to
// cur's: an address is IN_USE while a gateway uses it, RESERVED again once
// none does. AUTO_ONLY gateways are given their addresses here.
func (s *Server) claimNatIPsLocked(router string, old, cur obj) {
	keep := map[string]bool{}
	if cur != nil {
		for _, a := range natAddrs(cur) {
			keep[a] = true
		}
	}
	if old != nil {
		for _, a := range natAddrs(old) {
			if keep[a] {
				continue
			}
			if ao, ok := s.res[a]; ok {
				var users []any
				for _, u := range strs(ao, "users") {
					if u != router {
						users = append(users, u)
					}
				}
				if len(users) == 0 {
					delete(ao, "users")
					ao["status"] = "RESERVED"
				} else {
					ao["users"] = users
				}
			}
		}
	}
	for a := range keep {
		if ao, ok := s.res[a]; ok {
			if !hasStr(strs(ao, "users"), router) {
				ao["users"] = append(sliceOf(ao["users"]), router)
			}
			ao["status"] = "IN_USE"
		}
	}
	if cur == nil {
		return
	}
	given := map[string]bool{}
	for _, e := range sliceOf(cur["nats"]) {
		m, _ := e.(map[string]any)
		for _, ip := range strsAny(m["_autoIps"]) {
			given[ip] = true
		}
	}
	for _, e := range sliceOf(cur["nats"]) {
		m, _ := e.(map[string]any)
		if str(m, "natIpAllocateOption") != natAutoOnly || m["_autoIps"] != nil {
			continue
		}
		// Each gateway is given one address of the local pool: enough for
		// natPortsPerIP/minPortsPerVm VMs.
		used := s.natAutoIPsLocked()
		for k, a := range s.res {
			if strings.Contains(k, "/addresses/") {
				used[str(a, "address")] = true
			}
		}
		lo := ip4(regionalPool.IP)
		for v := lo + 1; v < lo+rangeSize(regionalPool)-1; v++ {
			if ip := fromU32(v).String(); !used[ip] && !given[ip] {
				m["_autoIps"], given[ip] = []any{ip}, true
				break
			}
		}
	}
}

// natAutoIPsLocked are the addresses AUTO_ONLY gateways were given.
func (s *Server) natAutoIPsLocked() map[string]bool {
	out := map[string]bool{}
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") {
			continue
		}
		for _, e := range sliceOf(r["nats"]) {
			m, _ := e.(map[string]any)
			for _, ip := range strsAny(m["_autoIps"]) {
				out[ip] = true
			}
		}
	}
	return out
}

// natVM is a VM a gateway translates.
type natVM struct {
	rel, name, ip, zone, subnet string
}

// natCovers reports whether the gateway m of a router in network and
// region translates the VM's primary interface on subnet sub.
func natCovers(m map[string]any, sub string) bool {
	switch str(m, "sourceSubnetworkIpRangesToNat") {
	case natAllAll, natAllPrimary:
		return true
	case natList:
		for _, se := range sliceOf(m["subnetworks"]) {
			sm, _ := se.(map[string]any)
			if str(sm, "name") == sub {
				ranges := strsAny(sm["sourceIpRangesToNat"])
				return hasStr(ranges, "ALL_IP_RANGES") || hasStr(ranges, "PRIMARY_IP_RANGE")
			}
		}
	}
	return false
}

// natVMsLocked are the VMs the gateway translates: those of its router's
// network and region whose primary interface has no external IP and is on
// a subnet it covers, by name.
func (s *Server) natVMsLocked(r obj, m map[string]any) []natVM {
	network, region := str(r, "network"), lastSeg(str(r, "region"))
	var out []natVM
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		nics := sliceOf(o["networkInterfaces"])
		if len(nics) == 0 {
			continue
		}
		nic, _ := nics[0].(map[string]any)
		if str(nic, "network") != network || regionOfZone(lastSeg(str(o, "zone"))) != region || len(sliceOf(nic["accessConfigs"])) > 0 {
			continue
		}
		if natCovers(m, str(nic, "subnetwork")) {
			out = append(out, natVM{rel: k, name: str(o, "name"), ip: str(nic, "networkIP"), zone: lastSeg(str(o, "zone")), subnet: str(nic, "subnetwork")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// natInternetLocked reports whether a Cloud NAT gateway translates the
// VM's primary interface, so it reaches the internet without an external
// IP.
func (s *Server) natInternetLocked(inst obj) bool {
	nics := sliceOf(inst["networkInterfaces"])
	if len(nics) == 0 {
		return false
	}
	nic, _ := nics[0].(map[string]any)
	network, sub := str(nic, "network"), str(nic, "subnetwork")
	region := regionOfZone(lastSeg(str(inst, "zone")))
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") || str(r, "network") != network || lastSeg(str(r, "region")) != region {
			continue
		}
		for _, e := range sliceOf(r["nats"]) {
			if m, _ := e.(map[string]any); natTypeOf(m) == natTypePublic && natCovers(m, sub) {
				return true
			}
		}
	}
	return false
}

// natIPsOf are a gateway's active NAT IP addresses (the address strings),
// and for MANUAL_ONLY their resources.
func (s *Server) natIPsOf(m map[string]any) (ips, resources []string) {
	if str(m, "natIpAllocateOption") == natAutoOnly {
		return strsAny(m["_autoIps"]), nil
	}
	for _, a := range strsAny(m["natIps"]) {
		if ao, ok := s.res[a]; ok {
			ips = append(ips, str(ao, "address"))
			resources = append(resources, a)
		}
	}
	return ips, resources
}

func natMinPorts(m map[string]any) int {
	if v, ok := m["minPortsPerVm"].(float64); ok && v > 0 {
		return int(v)
	}
	if m["enableDynamicPortAllocation"] == true {
		return natDefaultMinDyn
	}
	return natDefaultMin
}

func (s *Server) routerOf(project, region, name string) (obj, string, error) {
	rel := routerRel(project, region, name)
	r, ok := s.res[rel]
	if !ok {
		return nil, rel, notFound(rel)
	}
	return r, rel, nil
}

// routerStatus is getRouterStatus: each peer's session as its speaker
// reported it (bgpspeakers.go), DOWN until it has, with what it is
// advertised; the routes the router learned; each NAT gateway's
// addresses and the VMs it translates.
func (s *Server) routerStatus(project, region, name string) (obj, error) {
	r, rel, err := s.routerOf(project, region, name)
	if err != nil {
		return nil, err
	}
	adv := s.advertisedLocked(r)
	var peers []any
	for _, e := range sliceOf(r["bgpPeers"]) {
		m, _ := e.(map[string]any)
		ps := obj{"name": str(m, "name"), "status": "DOWN", "state": "Idle", "statusReason": "STATUS_REASON_UNSPECIFIED",
			"numLearnedRoutes": 0.0, "enableIpv4": true, "enableIpv6": false}
		for _, k := range []string{"ipAddress", "peerIpAddress", "routerApplianceInstance", "md5AuthenticationKeyName"} {
			if v := str(m, k); v != "" {
				ps[k] = v
			}
		}
		if str(m, "enable") != "FALSE" {
			s.peerStatusLocked(rel, r, m, ps)
			// The routes it is advertised (once the session is up).
			var routes []any
			for _, c := range advertisedFor(m, r, adv) {
				routes = append(routes, obj{"kind": "compute#route", "destRange": c, "nextHopIp": str(m, "ipAddress"),
					"priority": m["advertisedRoutePriority"], "routeType": "BGP"})
			}
			if len(routes) > 0 {
				ps["advertisedRoutes"] = routes
			}
		}
		peers = append(peers, ps)
	}
	var nats []any
	for _, e := range sliceOf(r["nats"]) {
		m, _ := e.(map[string]any)
		_, resources := s.natIPsOf(m)
		ips, slots := s.natAllocLocked(r, m)
		vms := slots
		ns := obj{"name": str(m, "name"), "numVmEndpointsWithNatMappings": float64(len(vms)), "minExtraNatIpsNeeded": 0.0}
		if str(m, "natIpAllocateOption") == natAutoOnly {
			ns["autoAllocatedNatIps"] = toAny(ips)
		} else {
			ns["userAllocatedNatIps"] = toAny(ips)
			ns["userAllocatedNatIpResources"] = toAny(resources)
			var drain []any
			for _, a := range strsAny(m["drainNatIps"]) {
				drain = append(drain, str(s.res[a], "address"))
			}
			if len(drain) > 0 {
				ns["drainUserAllocatedNatIps"] = drain
			}
			if need := natNeed(m, len(vms)); need > len(ips) {
				ns["minExtraNatIpsNeeded"] = float64(need - len(ips))
			}
		}
		var rs []any
		for _, re := range sliceOf(m["rules"]) {
			rm, _ := re.(map[string]any)
			a, _ := rm["action"].(map[string]any)
			var act []any
			for _, x := range strsAny(a["sourceNatActiveIps"]) {
				act = append(act, str(s.res[x], "address"))
			}
			rs = append(rs, obj{"ruleNumber": rm["ruleNumber"], "activeNatIps": act, "numVmEndpointsWithNatMappings": float64(len(vms)), "minExtraIpsNeeded": 0.0})
		}
		if len(rs) > 0 {
			ns["ruleStatus"] = rs
		}
		nats = append(nats, ns)
	}
	best, mine := s.routerBestRoutesLocked(rel, r)
	res := obj{"network": str(r, "network"), "bestRoutes": best, "bestRoutesForRouter": mine}
	if len(peers) > 0 {
		res["bgpPeerStatus"] = peers
	}
	if len(nats) > 0 {
		res["natStatus"] = nats
	}
	return obj{"kind": "compute#routerStatusResponse", "result": res}, nil
}

// advertisedLocked are the subnet ranges a router advertises by default:
// those of its network in its region, or in every region when the
// network's dynamic routing mode is GLOBAL.
func (s *Server) advertisedLocked(r obj) []string {
	network, region := str(r, "network"), lastSeg(str(r, "region"))
	global := false
	if rc, ok := s.res[network]["routingConfig"].(map[string]any); ok && str(rc, "routingMode") == "GLOBAL" {
		global = true
	}
	var out []string
	for k, o := range s.res {
		if strings.Contains(k, "/subnetworks/") && str(o, "network") == network && (global || lastSeg(str(o, "region")) == region) {
			out = append(out, str(o, "ipCidrRange"))
		}
	}
	sort.Strings(out)
	return out
}

// advertisedFor is what a peer is advertised: the router's default
// subnets, or the peer's (else the router's) custom groups and ranges.
func advertisedFor(peer map[string]any, r obj, subnets []string) []string {
	src := peer
	if str(peer, "advertiseMode") != "CUSTOM" {
		src, _ = r["bgp"].(map[string]any)
	}
	if str(src, "advertiseMode") != "CUSTOM" {
		return subnets
	}
	var out []string
	if hasStr(strsAny(src["advertisedGroups"]), "ALL_SUBNETS") {
		out = append(out, subnets...)
	}
	for _, e := range sliceOf(src["advertisedIpRanges"]) {
		m, _ := e.(map[string]any)
		out = append(out, str(m, "range"))
	}
	return out
}

// natMappingInfo is getNatMappingInfo: the ports of each NAT IP every
// translated VM is given, minPortsPerVm each from port 1024, as Google
// allocates them statically.
func (s *Server) natMappingInfo(c call, region, name string) (obj, error) {
	r, rel, err := s.routerOf(c.project, region, name)
	if err != nil {
		return nil, err
	}
	only := first(c.query, "natName")
	var all []obj
	for _, e := range sliceOf(r["nats"]) {
		m, _ := e.(map[string]any)
		if (only != "" && str(m, "name") != only) || natTypeOf(m) == natTypePrivate {
			continue
		}
		_, slots := s.natAllocLocked(r, m)
		for _, sl := range slots {
			mp := obj{"sourceVirtualIp": sl.vm.ip, "numTotalNatPorts": float64(sl.hi - sl.lo + 1), "numTotalDrainNatPorts": 0.0, "natIpPortRanges": []any{}}
			if sl.ip != "" {
				mp["natIpPortRanges"] = []any{fmt.Sprintf("%s:%d-%d", sl.ip, sl.lo, sl.hi)}
			} else {
				mp["numTotalNatPorts"] = 0.0
			}
			all = append(all, obj{"instanceName": sl.vm.name, "interfaceNatMappings": []any{mp}})
		}
	}
	items, next, err := page(all, c.query)
	if err != nil {
		return nil, err
	}
	out := obj{"kind": "compute#vmEndpointNatMappingsList", "id": rel, "selfLink": rel + "/getNatMappingInfo", "result": items}
	if items == nil {
		out["result"] = []obj{}
	}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

// natIPInfo is getNatIpInfo: each gateway's addresses, in use or draining.
func (s *Server) natIPInfo(c call, region, name string) (obj, error) {
	r, _, err := s.routerOf(c.project, region, name)
	if err != nil {
		return nil, err
	}
	only := first(c.query, "natName")
	var result []any
	for _, e := range sliceOf(r["nats"]) {
		m, _ := e.(map[string]any)
		if only != "" && str(m, "name") != only {
			continue
		}
		var mappings []any
		ips, slots := s.natAllocLocked(r, m)
		used := natUsage(slots)
		origin := "MANUAL"
		if str(m, "natIpAllocateOption") == natAutoOnly {
			origin = "AUTO"
		}
		for _, ip := range ips {
			usage := "UNUSED"
			if used[ip] {
				usage = "IN_USE"
			}
			mappings = append(mappings, obj{"natIp": ip, "mode": "ACTIVE", "usage": usage, "origin": origin})
		}
		for _, a := range strsAny(m["drainNatIps"]) {
			mappings = append(mappings, obj{"natIp": str(s.res[a], "address"), "mode": "DRAIN", "usage": "UNUSED", "origin": "MANUAL"})
		}
		result = append(result, obj{"natName": str(m, "name"), "natIpInfoMappings": mappings})
	}
	if result == nil {
		result = []any{}
	}
	return obj{"result": result}, nil
}

// routerUsesSubnet reports whether a router's interfaces or NAT gateways
// name the subnet, which keeps it from being deleted.
func routerUsesSubnet(r obj, sub string) bool {
	for _, e := range sliceOf(r["interfaces"]) {
		if m, _ := e.(map[string]any); str(m, "subnetwork") == sub {
			return true
		}
	}
	for _, e := range sliceOf(r["nats"]) {
		m, _ := e.(map[string]any)
		for _, se := range sliceOf(m["subnetworks"]) {
			if sm, _ := se.(map[string]any); str(sm, "name") == sub {
				return true
			}
		}
	}
	return false
}
