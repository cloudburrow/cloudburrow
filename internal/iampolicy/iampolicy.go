// Package iampolicy stores IAM policies without enforcing them (ADR-0006).
//
// A service that serves GetIamPolicy, SetIamPolicy and TestIamPermissions
// keeps a Stored beside the resource and calls into this package, so every
// such service has the same semantics:
//
//   - SetIamPolicy stores the bindings and returns them with a new etag. A
//     request whose policy carries an etag that is not the stored one is
//     ABORTED, as on Google, so read-modify-write loops behave.
//   - IAM Conditions and audit configs are UNIMPLEMENTED, with the field
//     named. They are never stored and ignored. A version 3 policy without
//     conditions is accepted, as Google accepts it: Terraform's google
//     provider sets version 3 on every *_iam_member, conditions or not.
//   - TestIamPermissions returns every requested permission: nothing is
//     enforced, so nothing is denied.
//
// Nothing here, or anywhere in CloudBurrow, consults a stored policy.
package iampolicy

import (
	"bytes"
	"crypto/rand"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Stored is a policy as a service keeps it, beside its resource.
type Stored struct {
	Bindings []Binding `json:"bindings,omitempty"`
	Etag     []byte    `json:"etag,omitempty"`
}

// Binding is one role and its members.
type Binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

// emptyEtag is the etag of a resource that has never had a policy set: the
// "ACAB" Google shows for an empty policy. A SetIamPolicy carrying it
// succeeds against a resource with no policy.
var emptyEtag = []byte{0x00, 0x20, 0x01} // base64 "ACAB"

// etag returns the policy's etag, or the empty policy's.
func (s *Stored) etag() []byte {
	if s == nil || len(s.Etag) == 0 {
		return emptyEtag
	}
	return s.Etag
}

// Proto renders the policy. A nil Stored is the empty policy.
func (s *Stored) Proto() *iampb.Policy {
	p := &iampb.Policy{Version: 1, Etag: s.etag()}
	if s == nil {
		return p
	}
	for _, b := range s.Bindings {
		p.Bindings = append(p.Bindings, &iampb.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)})
	}
	return p
}

// Get validates GetIamPolicy's options and returns the policy. Version 3 may
// be requested, since a client asks for it without using conditions; the
// answer is version 1 because nothing stored has a condition.
func Get(current *Stored, opts *iampb.GetPolicyOptions) (*iampb.Policy, error) {
	switch v := opts.GetRequestedPolicyVersion(); v {
	case 0, 1, 3:
	default:
		return nil, apierror.InvalidArgument("options.requested_policy_version %d is not 0, 1 or 3", v)
	}
	return current.Proto(), nil
}

// Set applies a SetIamPolicy request to the current policy and returns the
// policy to store. The caller stores it only when this returns no error.
func Set(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "bindings", "etag":
		case "audit_configs", "auditConfigs":
			return Stored{}, apierror.Unimplemented("update_mask %q: audit configs are not supported", path)
		default:
			return Stored{}, apierror.InvalidArgument("update_mask path %q is not a Policy field", path)
		}
	}
	p := req.GetPolicy()
	if p == nil {
		return Stored{}, apierror.InvalidArgument("policy is required")
	}
	// Any valid version is accepted for a policy without conditions, as on
	// Google. Conditions themselves are refused below, so a version 3 policy
	// stored here never holds one.
	switch p.GetVersion() {
	case 0, 1, 3:
	default:
		return Stored{}, apierror.InvalidArgument("policy.version %d is not 1 or 3", p.GetVersion())
	}
	if len(p.GetAuditConfigs()) > 0 {
		return Stored{}, apierror.Unimplemented("policy.audit_configs: audit configs are not supported")
	}
	var bindings []Binding
	for i, b := range p.GetBindings() {
		if b.GetCondition() != nil {
			return Stored{}, apierror.Unimplemented("policy.bindings[%d].condition: IAM Conditions are not supported", i)
		}
		if b.GetRole() == "" {
			return Stored{}, apierror.InvalidArgument("policy.bindings[%d].role is required", i)
		}
		// A binding without members grants nothing, and Google drops it.
		if len(b.GetMembers()) == 0 {
			continue
		}
		bindings = append(bindings, Binding{Role: b.GetRole(), Members: append([]string(nil), b.GetMembers()...)})
	}
	if e := p.GetEtag(); len(e) > 0 && !bytes.Equal(e, current.etag()) {
		return Stored{}, apierror.Aborted("the policy's etag does not match the current policy; read it again and retry")
	}
	etag := make([]byte, 8)
	_, _ = rand.Read(etag)
	return Stored{Bindings: bindings, Etag: etag}, nil
}

// Permissions answers TestIamPermissions: every requested permission, since
// nothing is enforced and so nothing is denied.
func Permissions(req *iampb.TestIamPermissionsRequest) *iampb.TestIamPermissionsResponse {
	return &iampb.TestIamPermissionsResponse{Permissions: append([]string(nil), req.GetPermissions()...)}
}
