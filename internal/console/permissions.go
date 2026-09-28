package console

// Permissions (#793): the IAM policy a service stores on a resource, shown on
// the resource's page as a Permissions tab, with Grant access and Remove
// principal, the way Google's console offers them.
//
// Cloud Tasks queues, Secret Manager secrets, Cloud KMS key rings and keys
// and Cloud Storage buckets all serve GetIamPolicy and SetIamPolicy, verified
// with the official clients, and none of them enforces a binding (ADR-0006).
// A page that listed bindings without saying so would read as protection, so
// the tab carries PermissionsNote whichever provider draws it: the server
// sets it, and a provider cannot leave it out.
//
// Every change is a read-modify-write through the provider's own
// GetIamPolicy and SetIamPolicy, sent with the etag the page read. A policy
// that changed since the page was drawn is refused by the service itself, with
// its own message, rather than overwritten: the console never writes a policy
// with a fresh etag it did not show.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// KindPermissions is an IAM policy's bindings, with Grant access and Remove
// principal. The server builds the section from the provider's PolicyEditor.
const KindPermissions SectionKind = "permissions"

// PermissionsNote is what every Permissions tab says above its bindings.
const PermissionsNote = "CloudBurrow stores IAM policies but does not enforce them (ADR-0006). " +
	"Every caller can still do everything to this resource, whatever the bindings below say: " +
	"a binding here grants nothing and denies nothing."

// Binding is one role and the principals that hold it.
type Binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
	// Condition is the binding's condition as text, empty for none. None of
	// the services with a Permissions tab stores one (they refuse IAM
	// Conditions UNIMPLEMENTED), so it is shown and never written.
	Condition string `json:"condition,omitempty"`
}

// Policy is an IAM policy as the console reads and writes it.
type Policy struct {
	Bindings []Binding `json:"bindings"`
	// Etag is opaque: the provider's own encoding of the service's etag, sent
	// back unchanged with a change.
	Etag string `json:"etag"`
}

// PolicyTarget describes the Permissions tab of one resource.
type PolicyTarget struct {
	// Link is the compatibility row that says what the service stores and
	// that it enforces nothing.
	Link string `json:"link,omitempty"`
	// RoleHelp and PrincipalHelp say what the service accepts for a role and
	// a principal, in its own terms.
	RoleHelp      string `json:"roleHelp,omitempty"`
	PrincipalHelp string `json:"principalHelp,omitempty"`
}

// PermissionsView is a KindPermissions section's content.
type PermissionsView struct {
	Policy
	PolicyTarget
	// Note is PermissionsNote, always.
	Note string `json:"note"`
}

// PolicyEditor is a provider whose resources carry an IAM policy.
type PolicyEditor interface {
	// PolicyOn returns the Permissions tab for the resource at a path, or nil
	// when that resource has no IAM policy in its service. It may ask the
	// service, as Cloud Storage does whether a folder is a managed folder
	// (#847): the page and the change route make the same call, so a tab is
	// drawn only where a change will be accepted.
	PolicyOn(ctx context.Context, path []string) *PolicyTarget
	// GetPolicy is the service's GetIamPolicy for the resource at a path.
	GetPolicy(ctx context.Context, project string, path []string) (Policy, error)
	// SetPolicy is the service's SetIamPolicy with p, whose etag is the one
	// the page read, so the service refuses it if the policy has changed.
	SetPolicy(ctx context.Context, project string, path []string, p Policy) error
}

// permissionsSection reads the policy for a resource page.
func permissionsSection(ctx context.Context, pe PolicyEditor, project string, path []string, target *PolicyTarget) Section {
	sec := Section{ID: "permissions", Label: "Permissions", Kind: KindPermissions}
	view := &PermissionsView{PolicyTarget: *target, Note: PermissionsNote}
	if view.PrincipalHelp == "" {
		view.PrincipalHelp = DefaultPrincipalHelp
	}
	sec.Permissions = view
	p, err := pe.GetPolicy(ctx, project, path)
	if err != nil {
		sec.Unavailable = "cannot read the IAM policy: " + userMessage(err)
		return sec
	}
	if p.Bindings == nil {
		p.Bindings = []Binding{}
	}
	view.Policy = p
	return sec
}

// DefaultPrincipalHelp is the principal field's help for a service that
// stores whatever principal it is given, which every service here does.
const DefaultPrincipalHelp = "One or more principals, separated by commas or new lines, written as IAM writes them: " +
	"user:ada@example.com, serviceAccount:name@project.iam.gserviceaccount.com, group:team@example.com, " +
	"domain:example.com, allUsers or allAuthenticatedUsers. This service stores a principal as it is typed " +
	"and checks no format."

// permissionsRequest is one change from the Permissions tab.
type permissionsRequest struct {
	Path []string
	// Etag is the one the page read.
	Etag  string
	Grant *struct {
		Members []string
		Role    string
	}
	Remove *struct {
		Member, Role string
	}
}

// handlePermissions applies Grant access or Remove principal: the service's
// GetIamPolicy, the change, and its SetIamPolicy with the page's etag.
func (s *Server) handlePermissions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	pe, ok := p.(PolicyEditor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": p.Title() + " has no IAM policies"})
		return
	}
	var req permissionsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	if (req.Grant == nil) == (req.Remove == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send one of grant or remove"})
		return
	}
	// Without the etag the page read, a change would overwrite whatever the
	// policy became since: the lost update the etag exists to prevent.
	if req.Etag == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "etag is required: send the one the policy was read with"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if len(req.Path) == 0 || pe.PolicyOn(ctx, req.Path) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this resource has no IAM policy"})
		return
	}
	project := r.URL.Query().Get("project")
	name := strings.Join(req.Path, "/")
	var action, what string
	if req.Grant != nil {
		action, what = "grant", "granted "+strings.TrimSpace(req.Grant.Role)
	} else {
		action, what = "remove principal", "removed "+req.Remove.Member+" from "+req.Remove.Role
	}
	opID := s.logs.StartOperation(action, name, project)
	fail := func(err error) {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: action + " failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err), "operation": opID})
	}

	current, err := pe.GetPolicy(ctx, project, req.Path)
	if err != nil {
		fail(err)
		return
	}
	var next Policy
	if req.Grant != nil {
		next, err = GrantAccess(current, req.Grant.Role, req.Grant.Members)
	} else {
		next, err = RemovePrincipal(current, req.Remove.Role, req.Remove.Member)
		// A binding already gone from a policy that has changed since the
		// page read it is the service's to refuse, by the etag: sent
		// unchanged, the stale etag gets its own message.
		if err != nil && req.Etag != current.Etag {
			next, err = current, nil
		}
	}
	if err != nil {
		fail(err)
		return
	}
	next.Etag = req.Etag
	if err := pe.SetPolicy(ctx, project, req.Path, next); err != nil {
		fail(err)
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: what + " on " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"applied": action, "operation": opID})
}

// GrantAccess adds members to role's binding, making one if the policy has
// none. The role is passed on as given, so a role the service refuses is
// refused with its message; members are trimmed, and at least one is needed,
// because a binding with none grants nothing and a service drops it silently.
func GrantAccess(p Policy, role string, members []string) (Policy, error) {
	role = strings.TrimSpace(role)
	var add []string
	for _, m := range members {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(add, m) {
			add = append(add, m)
		}
	}
	if len(add) == 0 {
		return Policy{}, fmt.Errorf("name at least one principal to grant %s to", orRole(role))
	}
	out := clonePolicy(p)
	for i, b := range out.Bindings {
		if b.Role == role && b.Condition == "" {
			for _, m := range add {
				if !slices.Contains(b.Members, m) {
					out.Bindings[i].Members = append(out.Bindings[i].Members, m)
				}
			}
			return out, nil
		}
	}
	out.Bindings = append(out.Bindings, Binding{Role: role, Members: add})
	return out, nil
}

// RemovePrincipal takes member out of role's binding, and the binding out of
// the policy when it was the last member.
func RemovePrincipal(p Policy, role, member string) (Policy, error) {
	out := clonePolicy(p)
	for i, b := range out.Bindings {
		if b.Role != role || b.Condition != "" {
			continue
		}
		j := slices.Index(b.Members, member)
		if j < 0 {
			break
		}
		out.Bindings[i].Members = slices.Delete(out.Bindings[i].Members, j, j+1)
		if len(out.Bindings[i].Members) == 0 {
			out.Bindings = slices.Delete(out.Bindings, i, i+1)
		}
		return out, nil
	}
	return Policy{}, fmt.Errorf("%s does not hold %s on this resource", member, orRole(role))
}

func orRole(role string) string {
	if role == "" {
		return "a role"
	}
	return role
}

func clonePolicy(p Policy) Policy {
	out := Policy{Etag: p.Etag}
	for _, b := range p.Bindings {
		out.Bindings = append(out.Bindings, Binding{Role: b.Role, Members: slices.Clone(b.Members), Condition: b.Condition})
	}
	return out
}
