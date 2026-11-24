package compute

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Allow policies on the served resources that keep one (#2112): VM
// instances and disks (zonal), snapshots (global) and subnetworks
// (regional), through each one's getIamPolicy, setIamPolicy and
// testIamPermissions, as the Compute Engine v1 reference serves them
// (instances.setIamPolicy takes a ZoneSetPolicyRequest, snapshots' a
// GlobalSetPolicyRequest, subnetworks' a RegionSetPolicyRequest: a policy,
// or bindings and an etag; getIamPolicy reads optionsRequestedPolicyVersion).
// Compute Engine is one of the services Google lists as allowing conditions
// in its resources' allow policies
// (https://docs.cloud.google.com/iam/docs/resource-types-with-conditional-roles,
// read 2026-11-21), so conditional bindings are stored with the version
// rules of internal/iampolicy. A stale etag is 409, as IAM's "Policy
// concurrency" documents. The policy is kept on the resource's record
// (_iamPolicy) and goes with it. Images (only the public ones are served)
// and instance templates (not served) have none here.
//
// With --iam-enforcement=on, internal/iameval reads them (Policy) beside
// the project's, and Cloud Asset's IAM policy analysis lists the resources
// (PolicyResourceNames) as compute.googleapis.com/Instance, Disk, Snapshot
// and Subnetwork assets.

// iamKey is the record field a resource's policy is kept in.
const iamKey = "_iamPolicy"

// iamVerbs are the IAM methods, by their last path segment.
var iamVerbs = map[string]bool{"getIamPolicy": true, "setIamPolicy": true, "testIamPermissions": true}

// iamTarget is the resource an IAM call names, "" when c is not one.
func iamTarget(c call) string {
	p := c.parts
	n := len(p)
	if n < 4 || !iamVerbs[p[n-1]] {
		return ""
	}
	switch {
	case n == 4 && p[0] == "global" && p[1] == "snapshots",
		n == 5 && p[0] == "regions" && (p[2] == "subnetworks" || p[2] == "disks"),
		n == 5 && p[0] == "zones" && (p[2] == "instances" || p[2] == "disks"):
		return "projects/" + c.project + "/" + strings.Join(p[:n-1], "/")
	}
	return ""
}

// iamCall serves one IAM method on rel; s.mu is held.
func (s *Server) iamCall(c call, rel string) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	verb := c.parts[len(c.parts)-1]
	switch {
	case verb == "getIamPolicy" && c.method == http.MethodGet:
		v := int64(0)
		if q := first(c.query, "optionsRequestedPolicyVersion"); q != "" {
			n, err := strconv.ParseInt(q, 10, 32)
			if err != nil {
				return nil, invalid("Invalid value for field 'optionsRequestedPolicyVersion': '%s'", q)
			}
			v = n
		}
		p, err := iampolicy.Get(storedPolicy(o), &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(v)})
		if err != nil {
			return nil, computeIAMError(err)
		}
		return policyObj(p)
	case verb == "setIamPolicy" && c.method == http.MethodPost:
		req, err := setRequest(c.body)
		if err != nil {
			return nil, err
		}
		next, err := iampolicy.SetConditional(storedPolicy(o), req)
		if err != nil {
			return nil, computeIAMError(err)
		}
		b, _ := json.Marshal(next)
		o[iamKey] = string(b)
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return policyObj(next.Proto())
	case verb == "testIamPermissions" && c.method == http.MethodPost:
		// Every requested permission: with enforcement on, the enforcer
		// keeps only those the caller holds.
		perms := strs(c.body, "permissions")
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

// setRequest is a {Zone,Region,Global}SetPolicyRequest as a SetIamPolicy
// request: its policy, or, without one, its bindings and etag.
func setRequest(body obj) (*iampb.SetIamPolicyRequest, error) {
	src, ok := body["policy"].(map[string]any)
	if !ok {
		src = obj{}
		if b, ok := body["bindings"]; ok {
			src["bindings"] = b
		}
		if e, ok := body["etag"]; ok {
			src["etag"] = e
		}
	}
	// Compute's Policy carries fields iam.v1's has not (rules, the
	// deprecated iamOwned); they are dropped, as nothing reads them.
	keep := obj{}
	for _, k := range []string{"version", "bindings", "etag", "auditConfigs"} {
		if v, ok := src[k]; ok {
			keep[k] = v
		}
	}
	b, _ := json.Marshal(keep)
	p := &iampb.Policy{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, p); err != nil {
		return nil, invalid("Invalid value for field 'policy': %v", err)
	}
	return &iampb.SetIamPolicyRequest{Policy: p}, nil
}

// storedPolicy is the policy kept on a record, nil for none.
func storedPolicy(o obj) *iampolicy.Stored {
	raw, _ := o[iamKey].(string)
	if raw == "" {
		return nil
	}
	var p iampolicy.Stored
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return &p
}

// policyObj is a policy in Compute's JSON shape.
func policyObj(p *iampb.Policy) (obj, error) {
	b, err := protojson.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out obj
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if _, ok := out["version"]; !ok {
		out["version"] = 1.0
	}
	return out, nil
}

// computeIAMError is an iampolicy refusal in Compute's error shape.
func computeIAMError(err error) error {
	var ae *apierror.Error
	if !errors.As(err, &ae) {
		return err
	}
	reason := ae.Reason
	if reason == "" {
		reason = "invalid"
	}
	return errf(ae.HTTPStatus(), reason, "%s", ae.Message)
}

// Policy is a served resource's allow policy (projects/P/zones/Z/instances/I,
// .../disks/D, projects/P/global/snapshots/S, projects/P/regions/R/subnetworks/N),
// nil when it has none or does not exist.
func (s *Server) Policy(rel string) *iampolicy.Stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.res[rel]
	if !ok {
		return nil
	}
	return storedPolicy(o)
}

// PolicyResourceNames are the project's instances, disks, snapshots and
// subnetworks, by relative name, sorted.
func (s *Server) PolicyResourceNames(project string) []string {
	prefix := "projects/" + project + "/"
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.res {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		p := strings.Split(rest, "/")
		switch {
		case len(p) == 3 && p[0] == "global" && p[1] == "snapshots",
			len(p) == 4 && p[0] == "regions" && (p[2] == "subnetworks" || p[2] == "disks"),
			len(p) == 4 && p[0] == "zones" && (p[2] == "instances" || p[2] == "disks"):
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
