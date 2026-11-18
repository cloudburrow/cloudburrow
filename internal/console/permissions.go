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
	// Condition is the binding's condition as text, empty for none.
	Condition string `json:"condition,omitempty"`
	// Conditional is the condition itself, for a service that stores IAM
	// Conditions (#1742: Resource Manager's projects), so the page can
	// write it back and edit it. A binding with Condition text and no
	// Conditional is shown and never written.
	Conditional *Condition `json:"conditional,omitempty"`
}

// Condition is an IAM Condition: a title, an optional description and a
// CEL expression, as Google's console edits one.
type Condition struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// Text is the condition as a Permissions tab lists it.
func (c *Condition) Text() string {
	if c == nil {
		return ""
	}
	if c.Title != "" {
		return c.Title + ": " + c.Expression
	}
	return c.Expression
}

// sameBinding is whether b is the binding of role under cond (nil for the
// unconditional one).
func sameBinding(b Binding, role string, cond *Condition) bool {
	if b.Role != role {
		return false
	}
	if cond == nil {
		return b.Condition == "" && b.Conditional == nil
	}
	return b.Conditional != nil && *b.Conditional == *cond
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
	// Conditions is whether the service stores IAM Conditions, so Grant
	// access offers one and a row's condition can be edited (#1742).
	Conditions bool `json:"conditions,omitempty"`
	// Enforced, when set, replaces PermissionsNote for a resource whose
	// policy the instance does check, in part (#1770: a BigQuery
	// connection's bigquery.connections.use, ADR-0006 as amended by
	// #1765). It must say which permission is checked and which identity
	// the caller is taken to be; PermissionsNote would read as "nothing is
	// enforced" there, which is wrong.
	Enforced string `json:"-"`
}

// PermissionsView is a KindPermissions section's content.
type PermissionsView struct {
	Policy
	PolicyTarget
	// Note is PermissionsNote, or the target's Enforced note.
	Note string `json:"note"`
	// Resource is the full resource name of the policy, such as
	// //cloudresourcemanager.googleapis.com/folders/123, which the condition
	// editor's Run Linter gives LintPolicy (#1756); empty when the provider
	// names none.
	Resource string `json:"resource,omitempty"`
}

// PolicyResourcer is a PolicyEditor that names the full resource name of
// the policy at a path, as the IAM API's LintPolicy takes it
// (full_resource_name, #1756).
type PolicyResourcer interface {
	PolicyResource(ctx context.Context, project string, path []string) string
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
	// TestPermissions is the service's TestIamPermissions for the resource
	// at a path: which of perms the caller holds, as the service answers
	// (#1242). Nothing is enforced (ADR-0006), so the services answer every
	// permission of a resource that exists.
	TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error)
}

// permissionsSection reads the policy for a resource page.
func permissionsSection(ctx context.Context, pe PolicyEditor, project string, path []string, target *PolicyTarget) Section {
	sec := Section{ID: "permissions", Label: "Permissions", Kind: KindPermissions}
	view := &PermissionsView{PolicyTarget: *target, Note: PermissionsNote}
	if target.Enforced != "" {
		view.Note = target.Enforced
	}
	if view.PrincipalHelp == "" {
		view.PrincipalHelp = DefaultPrincipalHelp
	}
	sec.Permissions = view
	if pr, ok := pe.(PolicyResourcer); ok {
		view.Resource = pr.PolicyResource(ctx, project, path)
	}
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
		Members   []string
		Role      string
		Condition *Condition
	}
	Remove *struct {
		Member, Role string
		Condition    *Condition
	}
	// Edit condition (#1742) moves Member's Role from the binding under
	// From to the one under To (nil: no condition), in one SetIamPolicy.
	EditCondition *struct {
		Member, Role string
		From, To     *Condition
	}
	// Test is Test permissions (#1242): it changes nothing, so it needs no
	// etag.
	Test *struct {
		Permissions []string
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
	if n := btoi(req.Grant != nil) + btoi(req.Remove != nil) + btoi(req.Test != nil) + btoi(req.EditCondition != nil); n != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send one of grant, remove, editCondition or test"})
		return
	}
	if req.Test != nil {
		s.testPermissions(w, r, pe, req.Path, req.Test.Permissions)
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
	switch {
	case req.Grant != nil:
		action, what = "grant", "granted "+strings.TrimSpace(req.Grant.Role)
	case req.EditCondition != nil:
		action, what = "edit condition", "changed the condition of "+req.EditCondition.Member+"'s "+req.EditCondition.Role
	default:
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
	switch {
	case req.Grant != nil:
		next, err = GrantConditional(current, req.Grant.Role, req.Grant.Members, req.Grant.Condition)
	case req.EditCondition != nil:
		e := req.EditCondition
		next, err = RemoveConditional(current, e.Role, e.Member, e.From)
		if err == nil {
			next, err = GrantConditional(next, e.Role, []string{e.Member}, e.To)
		}
		if err != nil && req.Etag != current.Etag {
			next, err = current, nil
		}
	default:
		next, err = RemoveConditional(current, req.Remove.Role, req.Remove.Member, req.Remove.Condition)
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

// testPermissions answers Test permissions: the service's
// TestIamPermissions with the permissions the page names, trimmed, each
// once, in the order given. It changes nothing, so it records no operation.
func (s *Server) testPermissions(w http.ResponseWriter, r *http.Request, pe PolicyEditor, path, perms []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if len(path) == 0 || pe.PolicyOn(ctx, path) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this resource has no IAM policy"})
		return
	}
	asked := splitPermissions(perms)
	if len(asked) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name at least one permission to test"})
		return
	}
	held, err := pe.TestPermissions(ctx, r.URL.Query().Get("project"), path, asked)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if held == nil {
		held = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"asked": asked, "permissions": held})
}

// splitPermissions is the permissions a page names, separated by commas,
// spaces or new lines, trimmed and each once.
func splitPermissions(in []string) []string {
	var out []string
	for _, p := range in {
		for _, f := range strings.FieldsFunc(p, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GrantAccess adds members to role's binding, making one if the policy has
// none. The role is passed on as given, so a role the service refuses is
// refused with its message; members are trimmed, and at least one is needed,
// because a binding with none grants nothing and a service drops it silently.
func GrantAccess(p Policy, role string, members []string) (Policy, error) {
	return GrantConditional(p, role, members, nil)
}

// GrantConditional is GrantAccess under a condition (nil for none): the
// members join role's binding with that condition (#1742). A condition
// without a title or an expression is refused here, as the service would.
func GrantConditional(p Policy, role string, members []string, cond *Condition) (Policy, error) {
	role = strings.TrimSpace(role)
	if cond != nil {
		c := Condition{Title: strings.TrimSpace(cond.Title), Description: strings.TrimSpace(cond.Description), Expression: strings.TrimSpace(cond.Expression)}
		if c == (Condition{}) {
			cond = nil
		} else {
			if c.Title == "" || c.Expression == "" {
				return Policy{}, fmt.Errorf("a condition needs a title and an expression")
			}
			cond = &c
		}
	}
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
		if sameBinding(b, role, cond) {
			for _, m := range add {
				if !slices.Contains(b.Members, m) {
					out.Bindings[i].Members = append(out.Bindings[i].Members, m)
				}
			}
			return out, nil
		}
	}
	out.Bindings = append(out.Bindings, Binding{Role: role, Members: add, Conditional: cond, Condition: cond.Text()})
	return out, nil
}

// RemovePrincipal takes member out of role's binding, and the binding out of
// the policy when it was the last member.
func RemovePrincipal(p Policy, role, member string) (Policy, error) {
	return RemoveConditional(p, role, member, nil)
}

// RemoveConditional is RemovePrincipal for role's binding under cond (nil
// for the unconditional one).
func RemoveConditional(p Policy, role, member string, cond *Condition) (Policy, error) {
	out := clonePolicy(p)
	for i, b := range out.Bindings {
		if !sameBinding(b, role, cond) {
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
		nb := Binding{Role: b.Role, Members: slices.Clone(b.Members), Condition: b.Condition}
		if b.Conditional != nil {
			c := *b.Conditional
			nb.Conditional = &c
		}
		out.Bindings = append(out.Bindings, nb)
	}
	return out
}
