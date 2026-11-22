package compute

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Subnet-level IAM and usable subnets (#2140): subnetworks.getIamPolicy,
// setIamPolicy, testIamPermissions and listUsable
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

// subnetPolicyKey is where a subnet keeps its policy (hidden from the
// subnet's own JSON, as every "_" field is).
const subnetPolicyKey = "_iamPolicy"

// storedPolicy is o's policy, nil for none.
func storedPolicy(o obj) *iampolicy.Stored {
	v, ok := o[subnetPolicyKey]
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var p iampolicy.Stored
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// SubnetworkPolicy is the policy of the subnet named
// "projects/P/regions/R/subnetworks/S", nil for none or no such subnet:
// IAM enforcement's policy reader for Compute Engine.
func (s *Server) SubnetworkPolicy(name string) *iampolicy.Stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.res[name]
	if !ok {
		return nil
	}
	return storedPolicy(o)
}

// policyJSON is p as Compute's REST API renders a Policy.
func policyJSON(p *iampb.Policy) (obj, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(p)
	if err != nil {
		return nil, err
	}
	out := obj{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if v, ok := out["version"].(float64); ok {
		out["version"] = int(v)
	}
	return out, nil
}

// iamError is an internal/iampolicy refusal in Compute's words.
func iamError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return invalid("%v", err)
	}
	switch st.Code() {
	case codes.Aborted, codes.FailedPrecondition:
		return errf(http.StatusPreconditionFailed, "conditionNotMet", "%s", st.Message())
	case codes.Unimplemented:
		return unimplemented("%s", st.Message())
	}
	return invalid("%s", st.Message())
}

// subnetIAM serves getIamPolicy, setIamPolicy and testIamPermissions on
// the subnet rel.
func (s *Server) subnetIAM(c call, rel, verb string) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	switch {
	case verb == "getIamPolicy" && c.method == http.MethodGet:
		var v int64
		if q := first(c.query, "optionsRequestedPolicyVersion"); q != "" {
			n, err := strconv.ParseInt(q, 10, 32)
			if err != nil {
				return nil, invalid("Invalid value for field 'optionsRequestedPolicyVersion': '%s'. Must be an integer.", q)
			}
			v = n
		}
		p, err := iampolicy.Get(storedPolicy(o), &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(v)})
		if err != nil {
			return nil, iamError(err)
		}
		return policyJSON(p)
	case verb == "setIamPolicy" && c.method == http.MethodPost:
		raw, has := c.body["policy"]
		if !has {
			// The deprecated flattened form: bindings and etag beside
			// the policy.
			flat := obj{}
			for _, k := range []string{"bindings", "etag"} {
				if v, ok := c.body[k]; ok {
					flat[k] = v
				}
			}
			if len(flat) == 0 {
				return nil, required("policy")
			}
			raw = flat
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, invalid("Invalid value for field 'policy': %v", err)
		}
		var pb iampb.Policy
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &pb); err != nil {
			return nil, invalid("Invalid value for field 'policy': %v", err)
		}
		next, err := iampolicy.SetConditional(storedPolicy(o), &iampb.SetIamPolicyRequest{Resource: rel, Policy: &pb})
		if err != nil {
			return nil, iamError(err)
		}
		nb, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		var stored any
		if err := json.Unmarshal(nb, &stored); err != nil {
			return nil, err
		}
		o[subnetPolicyKey] = stored
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		out, err := iampolicy.Get(&next, &iampb.GetPolicyOptions{RequestedPolicyVersion: 3})
		if err != nil {
			return nil, iamError(err)
		}
		return policyJSON(out)
	case verb == "testIamPermissions" && c.method == http.MethodPost:
		var perms []string
		for _, e := range sliceOf(c.body["permissions"]) {
			p, _ := e.(string)
			if strings.Contains(p, "*") {
				return nil, invalid("Invalid value for field 'permissions': '%s'. Permissions with wildcards (such as '*' or 'storage.*') are not allowed.", p)
			}
			perms = append(perms, p)
		}
		out := obj{}
		if len(perms) > 0 {
			l := make([]any, len(perms))
			for i, p := range perms {
				l[i] = p
			}
			out["permissions"] = l
		}
		return out, nil
	}
	return nil, unimplemented("%s %s/%s", c.method, rel, verb)
}

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
