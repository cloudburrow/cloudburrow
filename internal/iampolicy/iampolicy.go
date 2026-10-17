// Package iampolicy stores IAM policies without enforcing them (ADR-0006).
//
// A service that serves GetIamPolicy, SetIamPolicy and TestIamPermissions
// keeps a Stored beside the resource and calls into this package, so every
// such service has the same semantics:
//
//   - SetIamPolicy stores the bindings and returns them with a new etag. A
//     request whose policy carries an etag that is not the stored one is
//     ABORTED, as on Google, so read-modify-write loops behave.
//   - IAM Conditions are UNIMPLEMENTED, with the field named. They are never
//     stored and ignored. Audit configs are UNIMPLEMENTED too, except through
//     SetWithAuditConfigs, which the resources Google keeps them on (projects,
//     folders and the organization, #1486) call: there they are stored, as
//     update_mask says, and, as everything here, configure nothing. A version 3 policy without
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
	Bindings     []Binding     `json:"bindings,omitempty"`
	AuditConfigs []AuditConfig `json:"auditConfigs,omitempty"`
	Etag         []byte        `json:"etag,omitempty"`
}

// AuditConfig is a policy's audit logging configuration for one service
// ("allServices" for every one). It is stored and returned; no audit log is
// written or withheld because of it (#1486).
type AuditConfig struct {
	Service    string           `json:"service"`
	LogConfigs []AuditLogConfig `json:"auditLogConfigs,omitempty"`
}

// AuditLogConfig is one log type of an AuditConfig, and the principals
// exempted from it. LogType is the enum's name, such as "DATA_READ".
type AuditLogConfig struct {
	LogType         string   `json:"logType"`
	ExemptedMembers []string `json:"exemptedMembers,omitempty"`
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
	for _, a := range s.AuditConfigs {
		ac := &iampb.AuditConfig{Service: a.Service}
		for _, l := range a.LogConfigs {
			ac.AuditLogConfigs = append(ac.AuditLogConfigs, &iampb.AuditLogConfig{
				LogType:         iampb.AuditLogConfig_LogType(iampb.AuditLogConfig_LogType_value[l.LogType]),
				ExemptedMembers: append([]string(nil), l.ExemptedMembers...),
			})
		}
		p.AuditConfigs = append(p.AuditConfigs, ac)
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
// Audit configs are UNIMPLEMENTED, naming the field.
func Set(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	return apply(current, req, false)
}

// SetWithAuditConfigs is Set for a resource whose policy carries audit
// configs on Google: a project, a folder or an organization (#1486). As
// iam_policy.proto documents update_mask, only the fields it names change,
// and with no mask "bindings, etag" is used, so a set without a mask, or
// whose mask does not name audit_configs, keeps the stored audit configs.
// They are stored, never acted on: no audit log is written or withheld.
func SetWithAuditConfigs(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	return apply(current, req, true)
}

func apply(current *Stored, req *iampb.SetIamPolicyRequest, audit bool) (Stored, error) {
	masked := map[string]bool{}
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		// version is a Policy field too: gcloud kms ekm-config
		// set-iam-policy and add-iam-policy-binding send
		// "bindings,etag,version" (#1469).
		case "bindings", "etag", "version":
			masked[path] = true
		case "audit_configs", "auditConfigs":
			if !audit {
				return Stored{}, apierror.Unimplemented("update_mask %q: audit configs are not supported", path)
			}
			masked["audit_configs"] = true
		default:
			return Stored{}, apierror.InvalidArgument("update_mask path %q is not a Policy field", path)
		}
	}
	if len(masked) == 0 {
		masked["bindings"], masked["etag"] = true, true
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
	if !audit && len(p.GetAuditConfigs()) > 0 {
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
	audits, err := auditConfigs(p.GetAuditConfigs())
	if err != nil {
		return Stored{}, err
	}
	if e := p.GetEtag(); len(e) > 0 && !bytes.Equal(e, current.etag()) {
		return Stored{}, apierror.Aborted("the policy's etag does not match the current policy; read it again and retry")
	}
	next := Stored{Bindings: bindings}
	if audit {
		// Without "bindings" in the mask the stored bindings stay; without
		// "audit_configs", the stored audit configs.
		if !masked["bindings"] && current != nil {
			next.Bindings = current.Bindings
		}
		if masked["audit_configs"] {
			next.AuditConfigs = audits
		} else if current != nil {
			next.AuditConfigs = current.AuditConfigs
		}
	}
	next.Etag = make([]byte, 8)
	_, _ = rand.Read(next.Etag)
	return next, nil
}

// auditConfigs validates a policy's audit configs and returns them to store.
func auditConfigs(in []*iampb.AuditConfig) ([]AuditConfig, error) {
	var out []AuditConfig
	for i, a := range in {
		if a.GetService() == "" {
			return nil, apierror.InvalidArgument("policy.audit_configs[%d].service is required: a service name, or allServices", i)
		}
		ac := AuditConfig{Service: a.GetService()}
		for j, l := range a.GetAuditLogConfigs() {
			if l.GetLogType() == iampb.AuditLogConfig_LOG_TYPE_UNSPECIFIED {
				return nil, apierror.InvalidArgument("policy.audit_configs[%d].audit_log_configs[%d].log_type is required: ADMIN_READ, DATA_WRITE or DATA_READ", i, j)
			}
			if _, ok := iampb.AuditLogConfig_LogType_name[int32(l.GetLogType())]; !ok {
				return nil, apierror.InvalidArgument("policy.audit_configs[%d].audit_log_configs[%d].log_type %d is not ADMIN_READ, DATA_WRITE or DATA_READ", i, j, l.GetLogType())
			}
			ac.LogConfigs = append(ac.LogConfigs, AuditLogConfig{LogType: l.GetLogType().String(),
				ExemptedMembers: append([]string(nil), l.GetExemptedMembers()...)})
		}
		out = append(out, ac)
	}
	return out, nil
}

// Permissions answers TestIamPermissions: every requested permission, since
// nothing is enforced and so nothing is denied.
func Permissions(req *iampb.TestIamPermissionsRequest) *iampb.TestIamPermissionsResponse {
	return &iampb.TestIamPermissionsResponse{Permissions: append([]string(nil), req.GetPermissions()...)}
}
