// Package iampolicy stores IAM policies without enforcing them (ADR-0006).
//
// A service that serves GetIamPolicy, SetIamPolicy and TestIamPermissions
// keeps a Stored beside the resource and calls into this package, so every
// such service has the same semantics:
//
//   - SetIamPolicy stores the bindings and returns them with a new etag. A
//     request whose policy carries an etag that is not the stored one is
//     ABORTED, as on Google, so read-modify-write loops behave.
//   - IAM Conditions (#1742) are stored on the resources whose policies
//     Resource Manager keeps (projects, folders and the organization,
//     through SetWithAuditConfigs), with Google's policy version rules
//     (conditions.go), and (#1745) on the resources of the services Google
//     lists as allowing conditions (SetConditional); every other service
//     refuses them INVALID_ARGUMENT, with the field named, as Google refuses
//     a condition where it does not accept one. Audit configs are UNIMPLEMENTED too, except through
//     SetWithAuditConfigs, which the resources Google keeps them on (projects,
//     folders and the organization, #1486) call: there they are stored, as
//     update_mask says, and, as everything here, configure nothing. A version 3 policy without
//     conditions is accepted, as Google accepts it: Terraform's google
//     provider sets version 3 on every *_iam_member, conditions or not.
//   - TestIamPermissions returns every requested permission: nothing is
//     enforced, so nothing is denied.
//
// A few checks consult a stored project policy (Firestore with MongoDB
// compatibility's and Memorystore's IAM sign-ins); they evaluate a
// binding's condition with internal/iamcond.
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

// Binding is one role and its members, granted only where its condition,
// if it has one, evaluates to true.
type Binding struct {
	Role      string     `json:"role"`
	Members   []string   `json:"members"`
	Condition *Condition `json:"condition,omitempty"`
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
// A policy with a conditional binding is version 3, as Google returns it.
func (s *Stored) Proto() *iampb.Policy { return s.proto(3) }

// proto renders the policy for a reader that asked for version: below 3, a
// conditional binding's role carries "_withcond_" and its condition's hash,
// and the condition is left out, as Google's "Policy versions" documents.
func (s *Stored) proto(version int32) *iampb.Policy {
	p := &iampb.Policy{Version: 1, Etag: s.etag()}
	if s == nil {
		return p
	}
	for _, b := range s.Bindings {
		pb := &iampb.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)}
		if b.Condition != nil {
			if version < 3 {
				pb.Role = b.Role + withCond + b.Condition.hash(b.Role)
			} else {
				pb.Condition = b.Condition.proto()
				p.Version = 3
			}
		}
		p.Bindings = append(p.Bindings, pb)
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

// Get validates GetIamPolicy's options and returns the policy. A policy
// with no conditional binding is version 1 whatever was requested; one
// with a conditional binding is version 3 when 3 was requested, and
// otherwise version 1 with each conditional role renamed
// ROLE_withcond_HASH and its condition left out, as Google answers.
func Get(current *Stored, opts *iampb.GetPolicyOptions) (*iampb.Policy, error) {
	v := opts.GetRequestedPolicyVersion()
	switch v {
	case 0, 1, 3:
	default:
		return nil, apierror.InvalidArgument("options.requested_policy_version %d is not 0, 1 or 3", v)
	}
	return current.proto(v), nil
}

// Set applies a SetIamPolicy request to the current policy and returns the
// policy to store. The caller stores it only when this returns no error.
// Audit configs are UNIMPLEMENTED, naming the field. It is for a resource
// whose service Google does not list under "Services that allow
// conditions" (docs.cloud.google.com/iam/docs/resource-types-with-conditional-roles):
// a conditional binding is INVALID_ARGUMENT, naming it (#1745).
func Set(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	return apply(current, req, scopeNoConditions)
}

// SetConditional is Set for a resource whose service Google lists as
// allowing conditions in its resources' allow policies (#1745): Bigtable,
// BigQuery, Cloud KMS, Cloud Logging, Cloud Run, Secret Manager, Spanner,
// and Cloud Storage buckets with uniform bucket-level access. Conditional
// bindings are stored with the version rules of conditions.go, except the
// iam.googleapis.com/modifiedGrantsByRole API attribute, which Google's
// attribute reference accepts on projects, folders and organizations only.
func SetConditional(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	return apply(current, req, scopeResource)
}

// SetWithAuditConfigs also stores conditions (#1742): the resources that
// keep audit configs are the ones whose conditional bindings are served.

// SetWithAuditConfigs is Set for a resource whose policy carries audit
// configs on Google: a project, a folder or an organization (#1486). As
// iam_policy.proto documents update_mask, only the fields it names change,
// and with no mask "bindings, etag" is used, so a set without a mask, or
// whose mask does not name audit_configs, keeps the stored audit configs.
// They are stored, never acted on: no audit log is written or withheld.
func SetWithAuditConfigs(current *Stored, req *iampb.SetIamPolicyRequest) (Stored, error) {
	return apply(current, req, scopeHierarchy)
}

// scope is what a resource's policy accepts.
type scope int

const (
	scopeNoConditions scope = iota // no conditions, no audit configs
	scopeResource                  // conditions, no audit configs
	scopeHierarchy                 // conditions and audit configs
)

func apply(current *Stored, req *iampb.SetIamPolicyRequest, sc scope) (Stored, error) {
	audit := sc == scopeHierarchy
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
	// Google; a conditional binding needs version 3 (conditions.go).
	switch p.GetVersion() {
	case 0, 1, 3:
	default:
		return Stored{}, apierror.InvalidArgument("policy.version %d is not 1 or 3", p.GetVersion())
	}
	if !audit && len(p.GetAuditConfigs()) > 0 {
		return Stored{}, apierror.Unimplemented("policy.audit_configs: audit configs are not supported")
	}
	var bindings []Binding
	if err := checkConditions(p, sc); err != nil {
		return Stored{}, err
	}
	for i, b := range p.GetBindings() {
		if b.GetRole() == "" {
			return Stored{}, apierror.InvalidArgument("policy.bindings[%d].role is required", i)
		}
		// A binding without members grants nothing, and Google drops it.
		if len(b.GetMembers()) == 0 {
			continue
		}
		nb, err := binding(i, b, current, sc)
		if err != nil {
			return Stored{}, err
		}
		bindings = mergeBinding(bindings, nb)
	}
	if err := checkSameRoleAndPrincipal(bindings); err != nil {
		return Stored{}, err
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
