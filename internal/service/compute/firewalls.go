package compute

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Firewall rules (https://docs.cloud.google.com/compute/docs/reference/rest/v1/firewalls
// and https://docs.cloud.google.com/firewall/docs/firewalls, re-read
// 2026-11-19): INGRESS (the default) or EGRESS, priority 0 to 65535
// (1000 by default), either allowed or denied entries, each a protocol
// (tcp, udp, icmp, esp, ah, sctp, ipip, all, or a number 0 to 255) with
// ports only for tcp, udp and sctp. An ingress rule with no source
// applies to 0.0.0.0/0; an egress rule with no destination to 0.0.0.0/0.
// Enforced between VM instances by each guest's nftables (vpc.go, #1968).

var (
	protocols = map[string]bool{"tcp": true, "udp": true, "icmp": true, "esp": true, "ah": true, "sctp": true, "ipip": true, "all": true}
	portRE    = regexp.MustCompile(`^(\d{1,5})(?:-(\d{1,5}))?$`)
)

func firewallRel(project, name string) string {
	return "projects/" + project + "/global/firewalls/" + name
}

func (s *Server) insertFirewall(project string, b obj) (obj, error) {
	o, err := s.createFirewallLocked(project, b)
	if err != nil {
		return nil, err
	}
	return s.done(project, "global", "insert", str(o, "selfLink"), o["id"])
}

func (s *Server) createFirewallLocked(project string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := firewallRel(project, name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	o, err := s.firewallFrom(project, rel, b)
	if err != nil {
		return nil, err
	}
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	s.res[rel] = norm(o)
	return s.res[rel], nil
}

// firewallFrom checks b and makes the rule it describes.
func (s *Server) firewallFrom(project, rel string, b obj) (obj, error) {
	nw := str(b, "network")
	if nw == "" {
		nw = "global/networks/default"
	}
	nw = Rel(nw, project)
	if !strings.Contains(nw, "/") {
		nw = networkRel(project, nw)
	}
	if _, ok := s.res[nw]; !ok {
		return nil, notFound(nw)
	}
	dir := str(b, "direction")
	switch dir {
	case "":
		dir = "INGRESS"
	case "INGRESS", "EGRESS":
	default:
		return nil, invalid("Invalid value for field 'resource.direction': '%s'. Must be one of [EGRESS, INGRESS]", dir)
	}
	prio := 1000.0
	if v, ok := b["priority"].(float64); ok {
		if v < 0 || v > 65535 || v != float64(int(v)) {
			return nil, invalid("Invalid value for field 'resource.priority': '%v'. Must be greater than or equal to 0 and less than or equal to 65535", v)
		}
		prio = v
	}
	allowed, _ := b["allowed"].([]any)
	denied, _ := b["denied"].([]any)
	switch {
	case len(allowed) > 0 && len(denied) > 0:
		return nil, invalid("Invalid value for field 'resource.denied': ''. A firewall rule has either allowed or denied entries, not both.")
	case len(allowed) == 0 && len(denied) == 0:
		return nil, required("resource.allowed")
	}
	for i, e := range allowed {
		if err := checkRuleEntry(fmt.Sprintf("resource.allowed[%d]", i), e); err != nil {
			return nil, err
		}
	}
	for i, e := range denied {
		if err := checkRuleEntry(fmt.Sprintf("resource.denied[%d]", i), e); err != nil {
			return nil, err
		}
	}
	src, dst := strsAny(b["sourceRanges"]), strsAny(b["destinationRanges"])
	srcTags, tgtTags := strsAny(b["sourceTags"]), strsAny(b["targetTags"])
	srcSAs, tgtSAs := strsAny(b["sourceServiceAccounts"]), strsAny(b["targetServiceAccounts"])
	for i, r := range src {
		if err := checkFWRange(fmt.Sprintf("resource.sourceRanges[%d]", i), r); err != nil {
			return nil, err
		}
	}
	for i, r := range dst {
		if err := checkFWRange(fmt.Sprintf("resource.destinationRanges[%d]", i), r); err != nil {
			return nil, err
		}
	}
	if len(tgtTags) > 0 && len(tgtSAs) > 0 {
		return nil, invalid("Invalid value for field 'resource.targetServiceAccounts': ''. targetTags and targetServiceAccounts cannot both be set.")
	}
	if len(srcTags) > 0 && len(srcSAs) > 0 {
		return nil, invalid("Invalid value for field 'resource.sourceServiceAccounts': ''. sourceTags and sourceServiceAccounts cannot both be set.")
	}
	if dir == "EGRESS" && (len(srcTags) > 0 || len(srcSAs) > 0) {
		return nil, invalid("Invalid value for field 'resource.sourceTags': ''. An egress rule cannot have source tags or source service accounts.")
	}
	if dir == "INGRESS" && len(src) == 0 && len(srcTags) == 0 && len(srcSAs) == 0 {
		src = []string{"0.0.0.0/0"}
	}
	if dir == "EGRESS" && len(dst) == 0 {
		dst = []string{"0.0.0.0/0"}
	}
	o := obj{"kind": "compute#firewall", "name": lastSeg(rel), "network": nw, "selfLink": rel, "direction": dir,
		"priority": prio, "disabled": b["disabled"] == true, "logConfig": map[string]any{"enable": false}}
	if lc, ok := b["logConfig"].(map[string]any); ok {
		// Firewall Rules Logging (#2180): metadata is INCLUDE_ALL_METADATA
		// (the default once logging is on) or EXCLUDE_ALL_METADATA.
		out := map[string]any{"enable": lc["enable"] == true}
		switch m := str(lc, "metadata"); m {
		case "":
			if lc["enable"] == true {
				out["metadata"] = "INCLUDE_ALL_METADATA"
			}
		case "INCLUDE_ALL_METADATA", "EXCLUDE_ALL_METADATA":
			out["metadata"] = m
		default:
			return nil, invalid("Invalid value for field 'resource.logConfig.metadata': '%s'. Must be one of [EXCLUDE_ALL_METADATA, INCLUDE_ALL_METADATA]", m)
		}
		o["logConfig"] = out
	}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	set := func(k string, l []string) {
		if len(l) > 0 {
			o[k] = l
		}
	}
	set("sourceRanges", src)
	set("destinationRanges", dst)
	set("sourceTags", srcTags)
	set("targetTags", tgtTags)
	set("sourceServiceAccounts", srcSAs)
	set("targetServiceAccounts", tgtSAs)
	if len(allowed) > 0 {
		o["allowed"] = allowed
	} else {
		o["denied"] = denied
	}
	return o, nil
}

func checkRuleEntry(field string, e any) error {
	m, _ := e.(map[string]any)
	p, _ := m["IPProtocol"].(string)
	if p == "" {
		return required(field + ".IPProtocol")
	}
	lp := strings.ToLower(p)
	if !protocols[lp] {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return invalid("Invalid value for field '%s.IPProtocol': '%s'. Invalid IP protocol specification.", field, p)
		}
	}
	ports := strsAny(m["ports"])
	if len(ports) > 0 && lp != "tcp" && lp != "udp" && lp != "sctp" && p != "6" && p != "17" && p != "132" {
		return invalid("Invalid value for field '%s.ports': ''. Ports may be specified only with the tcp, udp and sctp protocols.", field)
	}
	for i, pt := range ports {
		mm := portRE.FindStringSubmatch(pt)
		bad := mm == nil
		if !bad {
			lo, _ := strconv.Atoi(mm[1])
			hi := lo
			if mm[2] != "" {
				hi, _ = strconv.Atoi(mm[2])
			}
			bad = lo > 65535 || hi > 65535 || hi < lo
		}
		if bad {
			return invalid("Invalid value for field '%s.ports[%d]': '%s'. Invalid port specification.", field, i, pt)
		}
	}
	return nil
}

func checkFWRange(field, r string) error {
	if strings.Contains(r, "/") {
		if _, _, err := net.ParseCIDR(r); err == nil {
			return nil
		}
	} else if net.ParseIP(r) != nil {
		return nil
	}
	return invalid("Invalid value for field '%s': '%s'. Must be a CIDR address range that is contained in the RFC1918 or a public IP address range.", field, r)
}

func strsAny(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// updateFirewall is patch (b merged over the rule) or update (b replaces
// it), checked as an insert is.
func (s *Server) updateFirewall(project, name string, b obj, replace bool) (obj, error) {
	rel := firewallRel(project, name)
	cur, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	in := obj{}
	if !replace {
		in = norm(cur)
		// A patch naming allowed replaces denied, and the reverse.
		if _, ok := b["allowed"]; ok {
			delete(in, "denied")
		}
		if _, ok := b["denied"]; ok {
			delete(in, "allowed")
		}
		// Defaults filled at insert are recomputed when a patch changes the
		// direction.
		if d, ok := b["direction"]; ok && d != cur["direction"] {
			delete(in, "sourceRanges")
			delete(in, "destinationRanges")
		}
	}
	for k, v := range b {
		if v == nil {
			delete(in, k)
			continue
		}
		in[k] = v
	}
	if n := str(b, "name"); n != "" && n != name {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. The name of a firewall rule cannot be changed.", n)
	}
	in["name"] = name
	o, err := s.firewallFrom(project, rel, in)
	if err != nil {
		return nil, err
	}
	o["id"], o["creationTimestamp"] = cur["id"], cur["creationTimestamp"]
	s.res[rel] = norm(o)
	op := "patch"
	if replace {
		op = "update"
	}
	return s.done(project, "global", op, rel, cur["id"])
}

// deleteSimple deletes a firewall rule or an address.
func (s *Server) deleteSimple(project, scope, kind, rel string) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if users := strs(o, "users"); len(users) > 0 {
		return nil, inUse(kind, rel, users[0])
	}
	delete(s.res, rel)
	return s.done(project, scope, "delete", rel, o["id"])
}
