package compute

import (
	"net"
	"regexp"
	"strings"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Cloud Armor address groups and tiers (#2160), as
// https://docs.cloud.google.com/armor/docs/address-groups-overview, the
// rules language reference and the projects.setCloudArmorTier reference
// describe them (read 2026-11-22):
//
//   - evaluateAddressGroup(NAME, IP[, EXCLUSIONS]) is true when the
//     address (origin.ip, origin.user_ip, or the first address in a
//     named request header) is in one of the project address group's
//     ranges and in none of the exclusions;
//     evaluateOrganizationAddressGroup is the same for an address group
//     of the project's organization;
//   - a group is named by its ID (in location global) or its full name;
//     a project group is usable only in its own project, and only in
//     global backend security policies (type CLOUD_ARMOR), of a project
//     enrolled in Cloud Armor Enterprise; the group must be one for Cloud
//     Armor (purpose CLOUD_ARMOR);
//   - projects.setCloudArmorTier sets a project's tier (CA_STANDARD,
//     CA_ENTERPRISE_PAYGO or CA_ENTERPRISE_ANNUAL), which projects.get
//     answers as cloudArmorTier, CA_STANDARD by default.

// AddressGroups is the Network Security API's address groups as Cloud
// Armor reads them; the NetworkSecurity hook implements it when address
// groups are served.
type AddressGroups interface {
	// AddressGroupItems are a Cloud Armor address group's items, by name
	// (projects/P/locations/global/addressGroups/N or
	// organizations/O/...); ok is false when there is no such group for
	// Cloud Armor.
	AddressGroupItems(name string) (items []string, ok bool)
}

var (
	addressGroupCallRE = regexp.MustCompile(`\b(evaluateAddressGroup|evaluateOrganizationAddressGroup)\(\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")`)
	addressGroupIDRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,62}$`)
	cloudArmorTiers    = []string{"CA_STANDARD", "CA_ENTERPRISE_PAYGO", "CA_ENTERPRISE_ANNUAL"}
)

// addressGroupName resolves a group argument for a policy of project.
func (s *Server) addressGroupName(fn, arg, project string) (string, bool) {
	if fn == "evaluateOrganizationAddressGroup" {
		if strings.HasPrefix(arg, "organizations/") {
			return arg, true
		}
		if s.opts.ProjectOrganization == nil {
			return "", false
		}
		org, _ := s.opts.ProjectOrganization(project)
		if org == "" || !addressGroupIDRE.MatchString(arg) {
			return "", false
		}
		return org + "/locations/global/addressGroups/" + arg, true
	}
	if strings.HasPrefix(arg, "projects/") {
		return arg, strings.HasPrefix(arg, "projects/"+project+"/")
	}
	if !addressGroupIDRE.MatchString(arg) {
		return "", false
	}
	return "projects/" + project + "/locations/global/addressGroups/" + arg, true
}

// armorTier is a project's Cloud Armor tier.
func (s *Server) armorTierLocked(project string) string {
	if t := str(s.res[projectRel(project)], "cloudArmorTier"); t != "" {
		return t
	}
	return "CA_STANDARD"
}

// checkAddressGroupUse checks the address groups a rule's expression
// names: the policy must be a global backend security policy of an
// Enterprise project, and each group must exist for Cloud Armor.
func (s *Server) checkAddressGroupUse(x lbCtx, typ string, rule obj, field string) error {
	e := str(objOf(objOf(rule["match"])["expr"]), "expression")
	calls := addressGroupCallRE.FindAllStringSubmatch(e, -1)
	if len(calls) == 0 {
		return nil
	}
	f := field + ".match.expr.expression"
	if typ != "CLOUD_ARMOR" || x.region != "" {
		return invalid("Invalid value for field '%s': '%s'. Address groups can be used only in global backend security policies.", f, e)
	}
	if s.armorTierLocked(x.project) == "CA_STANDARD" {
		return invalid("Invalid value for field '%s': '%s'. Address groups require the project to be enrolled in Cloud Armor Enterprise.", f, e)
	}
	ag, _ := s.opts.NetSec.(AddressGroups)
	for _, m := range calls {
		arg := strings.Trim(m[2], `'"`)
		name, ok := s.addressGroupName(m[1], arg, x.project)
		if !ok {
			return invalid("Invalid value for field '%s': '%s'. '%s' is not an address group this policy can use.", f, e, arg)
		}
		if ag == nil {
			return invalid("Invalid value for field '%s': '%s'. The address group '%s' does not exist.", f, e, name)
		}
		if _, ok := ag.AddressGroupItems(name); !ok {
			return invalid("Invalid value for field '%s': '%s'. The address group '%s' does not exist or is not for Cloud Armor (purpose CLOUD_ARMOR).", f, e, name)
		}
	}
	return nil
}

// addressGroupBinding is the late-bound evaluateAddressGroup of a
// request under a policy of project.
func (s *Server) addressGroupBinding(fn, project string, a *armorReq) func(string, ...ref.Val) ref.Val {
	return func(_ string, args ...ref.Val) ref.Val {
		if len(args) < 2 {
			return types.False
		}
		arg, _ := args[0].(types.String)
		who, _ := args[1].(types.String)
		ip := string(who)
		if net.ParseIP(ip) == nil {
			// A header name: its first address.
			ip = strings.TrimSpace(strings.Split(a.r.Header.Get(ip), ",")[0])
		}
		if net.ParseIP(ip) == nil {
			return types.False
		}
		if len(args) > 2 {
			if l, ok := args[2].(traits.Lister); ok {
				for _, ex := range celStrings(l) {
					if inIPRange(ip, strings.TrimSpace(ex)) {
						return types.False
					}
				}
			}
		}
		name, ok := s.addressGroupName(fn, string(arg), project)
		ag, _ := s.opts.NetSec.(AddressGroups)
		if !ok || ag == nil {
			return types.False
		}
		items, ok := ag.AddressGroupItems(name)
		if !ok {
			return types.False
		}
		for _, it := range items {
			if inIPRange(ip, it) {
				return types.True
			}
		}
		return types.False
	}
}

// AddressGroupReference is a security policy rule that names an address
// group.
type AddressGroupReference struct {
	// SecurityPolicy is the policy's URL; RulePriority the rule's.
	SecurityPolicy string
	RulePriority   int32
}

// AddressGroupReferences are the security policy rules that match an
// address group (for ListAddressGroupReferences, and to keep it from
// being deleted).
func (s *Server) AddressGroupReferences(name string) []AddressGroupReference {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AddressGroupReference
	for rel, o := range s.res {
		if !strings.Contains(rel, "/securityPolicies/") || strings.Count(rel, "/") != 4 {
			continue
		}
		project := strings.Split(rel, "/")[1]
		for _, e := range listOf(o["rules"]) {
			r := objOf(e)
			for _, m := range addressGroupCallRE.FindAllStringSubmatch(str(objOf(objOf(r["match"])["expr"]), "expression"), -1) {
				if n, ok := s.addressGroupName(m[1], strings.Trim(m[2], `'"`), project); ok && n == name {
					out = append(out, AddressGroupReference{SecurityPolicy: "https://www.googleapis.com/compute/v1/" + rel, RulePriority: int32(rulePriority(r))})
					break
				}
			}
		}
	}
	return out
}

// setCloudArmorTier is projects.setCloudArmorTier.
func (s *Server) setCloudArmorTier(c call) (obj, error) {
	if _, err := s.project(c); err != nil {
		return nil, err
	}
	t := str(c.body, "cloudArmorTier")
	ok := false
	for _, v := range cloudArmorTiers {
		ok = ok || v == t
	}
	if !ok {
		return nil, invalid("Invalid value for field 'cloudArmorTier': '%s'. Must be one of [CA_STANDARD, CA_ENTERPRISE_PAYGO, CA_ENTERPRISE_ANNUAL].", t)
	}
	o := s.res[projectRel(c.project)]
	o["cloudArmorTier"] = t
	return s.done(c.project, "global", "setCloudArmorTier", "projects/"+c.project, o["id"])
}
