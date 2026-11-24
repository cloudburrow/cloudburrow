package compute

import (
	"sort"
	"strings"
)

// Usable subnets (#2140): subnetworks.listUsable
// (https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks,
// read 2026-11-22). A subnet's policy is Shared VPC's "Individual subnets
// (subnet-level permissions)": roles/compute.networkUser granted on a
// host project's subnet lets a service project's principals use that
// subnet only (vpc/docs/shared-vpc). The policy is stored beside the
// subnet (internal/iampolicy, with IAM Conditions, as Compute Engine
// supports them) and read by IAM enforcement (iameval.Compute's
// PolicyCollections) for compute.subnetworks.use and every other
// permission on the subnet. testIamPermissions here answers every
// requested permission; with enforcement on, the caller's wrapper keeps
// those the caller holds, and listUsable's items those it may use
// (cmd/cloudburrow/iamenforcexpn.go).

// usableSubnet is a subnet as listUsable's UsableSubnetwork shows it.
func usableSubnet(o obj) obj {
	u := obj{"subnetwork": o["selfLink"], "network": o["network"], "ipCidrRange": o["ipCidrRange"]}
	for _, k := range []string{"secondaryIpRanges", "stackType", "ipv6AccessType", "purpose", "role", "externalIpv6Prefix", "internalIpv6Prefix"} {
		if v, ok := o[k]; ok && v != nil && v != "" {
			if k == "secondaryIpRanges" {
				var rs []any
				for _, e := range sliceOf(v) {
					r, _ := e.(map[string]any)
					rs = append(rs, obj{"rangeName": r["rangeName"], "ipCidrRange": r["ipCidrRange"]})
				}
				if len(rs) == 0 {
					continue
				}
				v = rs
			}
			u[k] = v
		}
	}
	return u
}

// listUsable is subnetworks.listUsable: the project's subnets and, for a
// Shared VPC service project, its host project's, every region's. With
// serviceProject (Shared VPC only), the project must be the host that
// service project is attached to, and lists its own subnets for it.
func (s *Server) listUsable(c call) (obj, error) {
	if s.projectLocked(c.project) == nil {
		return nil, notFound("projects/" + c.project)
	}
	projects := []string{c.project}
	if svc := first(c.query, "serviceProject"); svc != "" {
		if !s.isXpnHostLocked(c.project) || s.xpnHostOfLocked(svc) != c.project {
			return nil, invalid("Invalid value for field 'serviceProject': '%s'. It is not a service project of the shared VPC host project '%s'.", svc, c.project)
		}
	} else if h := s.xpnHostOfLocked(c.project); h != "" && s.isXpnHostLocked(h) {
		projects = append(projects, h)
	}
	f, err := parseFilter(first(c.query, "filter"))
	if err != nil {
		return nil, err
	}
	var keys []string
	for k := range s.res {
		for _, p := range projects {
			rest, ok := strings.CutPrefix(k, "projects/"+p+"/regions/")
			if ok {
				if parts := strings.Split(rest, "/"); len(parts) == 3 && parts[1] == "subnetworks" {
					keys = append(keys, k)
				}
			}
		}
	}
	sort.Strings(keys)
	var all []obj
	for _, k := range keys {
		if u := usableSubnet(s.res[k]); f.match(u) {
			all = append(all, u)
		}
	}
	items, next, err := page(all, c.query)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []obj{}
	}
	rel := "projects/" + c.project + "/aggregated/subnetworks/listUsable"
	out := obj{"kind": "compute#usableSubnetworksAggregatedList", "id": rel, "items": items, "selfLink": rel}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}
