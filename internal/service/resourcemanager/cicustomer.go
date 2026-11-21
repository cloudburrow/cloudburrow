package resourcemanager

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The customer's user invitations, policies and allowlisted domains
// (#2103), over Cloud Identity v1 (read 2026-11-21:
// https://docs.cloud.google.com/identity/docs/reference/rest/v1/customers.userinvitations,
// .../how-to/manage-user-invitations, .../v1/policies,
// .../concepts/overview-policies and .../v1/allowlistedDomains).
//
// User invitations: "an email that can be sent to an unmanaged user
// account inviting them to join the customer's Google Workspace or Cloud
// Identity account". An account is invitable when it is a consumer account
// whose domain "matches an existing verified Google Workspace or Cloud
// Identity domain": here an address of the customer's domain that is not a
// managed user (directoryusers.go). send creates the invitation when there
// is none and sends it (INVITED, mailsSentCount up; no mail leaves the
// instance); cancel takes a sent one back to NOT_YET_SENT ("User
// invitations can be canceled until they are either accepted or
// declined"); list filters on state ("state=='invited'", with || and !=)
// and orders by update_time or email, 100 a page up to 200. No consumer
// account signs up here, so none appears on its own; the invited user's
// answer is the instance's /cloudburrow/v1/{invitation}:accept, which
// makes the account a managed user, and :decline.
//
// Policies: an admin's policies, each a setting (type and value) applied
// to an organizational unit, optionally a group, with a CEL query;
// customer, policyQuery and setting.type immutable; sortOrder the policy's
// place among those of its setting type; type ADMIN. list's filter is
// evaluated as CEL over setting and customer ("setting.type.matches(...)",
// "customer == ..."), 50 a page up to 100 ("pageSize values greater than
// 100 default to 100"). They are stored configuration: no Workspace
// service here reads them.
//
// Allowlisted domains: a domain name each, required and immutable. Stored
// configuration too.

const (
	invitationPrefix = "resourcemanager/cloudidentity/userinvitations/"
	policyPrefix     = "resourcemanager/cloudidentity/policies/"
	allowlistPrefix  = "resourcemanager/cloudidentity/allowlisteddomains/"
	invitationType   = "type.googleapis.com/google.apps.cloudidentity.userinvitations.v1."
	policyType       = "type.googleapis.com/google.apps.cloudidentity.policy.v1.Policy"
	allowlistType    = "type.googleapis.com/google.apps.cloudidentity.allowlisteddomains.v1.AllowlistedDomain"
)

// Invitation states.
const (
	InvitationNotYetSent = "NOT_YET_SENT"
	InvitationInvited    = "INVITED"
	InvitationAccepted   = "ACCEPTED"
	InvitationDeclined   = "DECLINED"
)

// loaded reads one stored document into v; false when absent or bad.
func (r *Registry) loaded(key string, v any) bool {
	ok, err := r.loadJSONLocked(key, v)
	return ok && err == nil
}

// invitationEmail reads customers/{c}/userinvitations/{email}.
func invitationEmail(name string) (string, error) {
	rest, ok := strings.CutPrefix(name, "customers/")
	c, email, ok2 := strings.Cut(rest, "/userinvitations/")
	if !ok || !ok2 || email == "" || strings.Contains(email, "/") {
		return "", apierror.InvalidArgument("Invalid name %q: it must be customers/{customer}/userinvitations/{user_email_address}.", name)
	}
	if err := checkCustomer("customers/" + c); err != nil {
		return "", err
	}
	return normEmail(email), nil
}

func invitationName(email string) string { return CustomerName + "/userinvitations/" + email }

// Invitations are the customer's user invitations, by email.
func (r *Registry) Invitations() ([]*ci.UserInvitation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := r.st.List(invitationPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list invitations")
	}
	var out []*ci.UserInvitation
	for _, k := range keys {
		var inv ci.UserInvitation
		if r.loaded(k, &inv) {
			out = append(out, &inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Invitation is one user invitation.
func (r *Registry) Invitation(name string) (*ci.UserInvitation, error) {
	email, err := invitationEmail(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var inv ci.UserInvitation
	if !r.loaded(invitationPrefix+email, &inv) {
		return nil, apierror.NotFound("User invitation %s not found.", name)
	}
	return &inv, nil
}

// invitableLocked is whether email is an unmanaged account of the
// customer's verified domain.
func (r *Registry) invitableLocked(email string) bool {
	if !validEmail(email) || !InDomain(email) {
		return false
	}
	if _, err := r.userLocked(email); err == nil {
		return false
	}
	if g, _ := r.groupByEmailLocked(email); g != nil {
		return false
	}
	return true
}

// IsInvitable is isInvitableUser.
func (r *Registry) IsInvitable(name string) (bool, error) {
	email, err := invitationEmail(name)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.invitableLocked(email), nil
}

// InvitationAction sends, cancels, accepts or declines an invitation.
func (r *Registry) InvitationAction(name, action string, accept AcceptInput) (*ci.UserInvitation, error) {
	email, err := invitationEmail(name)
	if err != nil {
		return nil, err
	}
	if action == "accept" {
		// The account becomes managed: a user of the customer.
		if _, err := r.invitationState(email, InvitationInvited); err != nil {
			return nil, err
		}
		local, _, _ := strings.Cut(email, "@")
		given, family := orDefault(accept.GivenName, local), orDefault(accept.FamilyName, "User")
		pw := orDefault(accept.Password, newIdentityID()+newIdentityID())
		if _, err := r.InsertUser(map[string]any{"primaryEmail": email, "name": map[string]any{"givenName": given, "familyName": family}, "password": pw}); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var inv ci.UserInvitation
	exists := r.loaded(invitationPrefix+email, &inv)
	switch action {
	case "send":
		if exists && inv.State == InvitationAccepted {
			return nil, apierror.FailedPrecondition("The user %s has already accepted the invitation.", email)
		}
		if !r.invitableLocked(email) {
			return nil, apierror.FailedPrecondition("%s is not an invitable user: it must be an unmanaged account of a verified domain of the customer (%s).", email, OrganizationDomain)
		}
		if !exists {
			inv = ci.UserInvitation{Name: invitationName(email)}
		}
		inv.State = InvitationInvited
		inv.MailsSentCount++
	case "cancel":
		if !exists {
			return nil, apierror.NotFound("User invitation %s not found.", name)
		}
		if inv.State != InvitationInvited {
			return nil, apierror.FailedPrecondition("Only a sent invitation can be cancelled; %s is %s.", email, inv.State)
		}
		inv.State = InvitationNotYetSent
	case "accept":
		inv.State = InvitationAccepted
	case "decline":
		if !exists || inv.State != InvitationInvited {
			return nil, apierror.FailedPrecondition("The user %s has no invitation to decline.", email)
		}
		inv.State = InvitationDeclined
	default:
		return nil, apierror.NotFound("no such method: %s", action)
	}
	inv.UpdateTime = ciTime(r.now())
	return &inv, r.putJSONLocked(invitationPrefix+email, &inv)
}

func (r *Registry) invitationState(email, want string) (*ci.UserInvitation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var inv ci.UserInvitation
	if !r.loaded(invitationPrefix+email, &inv) || inv.State != want {
		return nil, apierror.FailedPrecondition("The user %s has no invitation to accept.", email)
	}
	return &inv, nil
}

// AcceptInput is the invited user's names and password.
type AcceptInput struct {
	GivenName  string `json:"givenName"`
	FamilyName string `json:"familyName"`
	Password   string `json:"password"`
}

var reCustomerTerm = regexp.MustCompile(`\bcustomer\s*(==|!=)`)

var reStateTerm = regexp.MustCompile(`^state\s*(==|!=)\s*'([a-z_]+)'$`)

// invitationFilter reads list's filter: state terms joined by ||.
func invitationFilter(f string) (func(string) bool, error) {
	if strings.TrimSpace(f) == "" {
		return func(string) bool { return true }, nil
	}
	var tests []func(string) bool
	for _, t := range strings.Split(f, "||") {
		m := reStateTerm.FindStringSubmatch(strings.Trim(strings.TrimSpace(t), "()"))
		if m == nil {
			return nil, apierror.InvalidArgument("Invalid filter %q: it filters on state, as state=='invited', joined by ||.", f)
		}
		want := strings.ToUpper(m[2])
		switch want {
		case InvitationNotYetSent, InvitationInvited, InvitationAccepted, InvitationDeclined:
		default:
			return nil, apierror.InvalidArgument("Invalid filter %q: unknown state %s.", f, m[2])
		}
		neq := m[1] == "!="
		tests = append(tests, func(s string) bool { return (s == want) != neq })
	}
	return func(s string) bool {
		for _, t := range tests {
			if t(s) {
				return true
			}
		}
		return false
	}, nil
}

// Policy is a stored policy.
type storedPolicy struct {
	Policy  *ci.Policy `json:"policy"`
	Created time.Time  `json:"created"`
}

var reSettingType = regexp.MustCompile(`^settings/[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

func (r *Registry) policiesLocked() ([]*storedPolicy, error) {
	keys, err := r.st.List(policyPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list policies")
	}
	var out []*storedPolicy
	for _, k := range keys {
		var p storedPolicy
		if r.loaded(k, &p) {
			out = append(out, &p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Policy.Name < out[j].Policy.Name
	})
	// sortOrder: the policy's place among its setting type's.
	n := map[string]float64{}
	for _, p := range out {
		n[p.Policy.Setting.Type]++
		p.Policy.PolicyQuery.SortOrder = n[p.Policy.Setting.Type]
	}
	return out, nil
}

// Policies are the customer's policies.
func (r *Registry) Policies() ([]*ci.Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps, err := r.policiesLocked()
	if err != nil {
		return nil, err
	}
	out := make([]*ci.Policy, len(ps))
	for i, p := range ps {
		out[i] = p.Policy
	}
	return out, nil
}

func policyID(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "policies/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("Invalid policy name %q: it must be policies/{policy}.", name)
	}
	return id, nil
}

// GetPolicy reads a policy.
func (r *Registry) GetPolicy(name string) (*ci.Policy, error) {
	id, err := policyID(name)
	if err != nil {
		return nil, err
	}
	ps, err := r.Policies()
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.Name == "policies/"+id {
			return p, nil
		}
	}
	return nil, apierror.NotFound("Policy %s not found.", name)
}

// knownOrgUnitLocked is whether orgUnits/{id} names the root
// organizational unit or one of the customer's users'.
func (r *Registry) knownOrgUnitLocked(ou string) bool {
	id, ok := strings.CutPrefix(ou, "orgUnits/")
	if !ok {
		return false
	}
	if id == RootOrgUnitID {
		return true
	}
	us, _ := r.usersLocked()
	for _, u := range us {
		for _, x := range queryUser(u, func(string) string { return "" })["org_units"].([]any) {
			if x.(map[string]any)["org_unit_id"] == id {
				return true
			}
		}
	}
	return false
}

// CreatePolicy creates an admin policy.
func (r *Registry) CreatePolicy(in *ci.Policy) (*ci.Policy, error) {
	if in == nil || in.PolicyQuery == nil || in.PolicyQuery.OrgUnit == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: policyQuery.orgUnit is required.")
	}
	if in.Setting == nil || in.Setting.Type == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: setting.type is required.")
	}
	if !reSettingType.MatchString(in.Setting.Type) {
		return nil, apierror.InvalidArgument("Invalid setting.type %q: it must be settings/{service}.{setting}.", in.Setting.Type)
	}
	if len(in.Setting.Value) == 0 {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: setting.value is required.")
	}
	var v map[string]any
	if err := json.Unmarshal(in.Setting.Value, &v); err != nil || v == nil {
		return nil, apierror.InvalidArgument("Invalid setting.value: it must be a JSON object (a Struct).")
	}
	switch in.Customer {
	case "", "customers/my_customer", CustomerName:
	default:
		if !strings.HasPrefix(in.Customer, "customers/C") {
			return nil, apierror.InvalidArgument("Invalid customer %q: it must be customers/{customerId}, the ID beginning with C.", in.Customer)
		}
		return nil, permissionDenied(in.Customer)
	}
	if strings.Contains(in.PolicyQuery.Query, "licenses") {
		return nil, apierror.InvalidArgument("The licenses clause is not supported in mutate endpoints.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.knownOrgUnitLocked(in.PolicyQuery.OrgUnit) {
		return nil, apierror.InvalidArgument("Invalid policyQuery.orgUnit %q: it must be orgUnits/{id} of an organizational unit of the customer (the root is orgUnits/%s).", in.PolicyQuery.OrgUnit, RootOrgUnitID)
	}
	if in.PolicyQuery.Group != "" {
		if _, err := r.groupFromNameLocked(in.PolicyQuery.Group); err != nil {
			return nil, err
		}
	}
	p := &ci.Policy{Name: "policies/" + newIdentityID(), Customer: CustomerName, Type: "ADMIN",
		PolicyQuery: &ci.PolicyQuery{OrgUnit: in.PolicyQuery.OrgUnit, Group: in.PolicyQuery.Group, Query: in.PolicyQuery.Query},
		Setting:     &ci.Setting{Type: in.Setting.Type, Value: in.Setting.Value}}
	sp := &storedPolicy{Policy: p, Created: r.now()}
	if err := r.putJSONLocked(policyPrefix+strings.TrimPrefix(p.Name, "policies/"), sp); err != nil {
		return nil, err
	}
	ps, err := r.policiesLocked()
	if err != nil {
		return nil, err
	}
	for _, x := range ps {
		if x.Policy.Name == p.Name {
			return x.Policy, nil
		}
	}
	return p, nil
}

// PatchPolicy changes a policy's setting value, its one mutable field.
func (r *Registry) PatchPolicy(name string, in *ci.Policy) (*ci.Policy, error) {
	id, err := policyID(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var sp storedPolicy
	if !r.loaded(policyPrefix+id, &sp) {
		return nil, apierror.NotFound("Policy %s not found.", name)
	}
	p := sp.Policy
	if in.Customer != "" && in.Customer != p.Customer && in.Customer != "customers/my_customer" {
		return nil, apierror.InvalidArgument("customer is immutable.")
	}
	if q := in.PolicyQuery; q != nil && ((q.OrgUnit != "" && q.OrgUnit != p.PolicyQuery.OrgUnit) || (q.Group != "" && q.Group != p.PolicyQuery.Group) ||
		(q.Query != "" && q.Query != p.PolicyQuery.Query)) {
		return nil, apierror.InvalidArgument("policyQuery is immutable.")
	}
	if in.Setting == nil {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: setting is required.")
	}
	if in.Setting.Type != "" && in.Setting.Type != p.Setting.Type {
		return nil, apierror.InvalidArgument("setting.type is immutable.")
	}
	var v map[string]any
	if err := json.Unmarshal(in.Setting.Value, &v); err != nil || v == nil {
		return nil, apierror.InvalidArgument("Invalid setting.value: it must be a JSON object (a Struct).")
	}
	p.Setting.Value = in.Setting.Value
	if err := r.putJSONLocked(policyPrefix+id, &sp); err != nil {
		return nil, err
	}
	ps, err := r.policiesLocked()
	if err != nil {
		return nil, err
	}
	for _, x := range ps {
		if x.Policy.Name == p.Name {
			return x.Policy, nil
		}
	}
	return p, nil
}

// DeletePolicy deletes a policy.
func (r *Registry) DeletePolicy(name string) error {
	id, err := policyID(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var sp storedPolicy
	if !r.loaded(policyPrefix+id, &sp) {
		return apierror.NotFound("Policy %s not found.", name)
	}
	if err := r.st.Delete(policyPrefix + id); err != nil {
		return apierror.Internal(err, "delete policy")
	}
	return nil
}

var (
	polEnvOnce sync.Once
	polEnv     *cel.Env
	polEnvErr  error
)

// allowlistRe is allowlistedDomains.list's filter: "Currently, only exact
// matches on the domain are supported, such as \"domain = 'google.com'\",
// with no composite conditions" (cloudidentity v1 discovery document).
var allowlistRe = regexp.MustCompile(`^\s*domain\s*==?\s*(?:'([^']*)'|"([^"]*)")\s*$`)

// allowlistFilter reads that filter: the one domain it matches, or "".
func allowlistFilter(f string) (string, error) {
	if strings.TrimSpace(f) == "" {
		return "", nil
	}
	m := allowlistRe.FindStringSubmatch(f)
	if m == nil {
		return "", apierror.InvalidArgument("Invalid filter %q: only an exact match on the domain is supported, as domain = 'example.com', with no composite conditions.", f)
	}
	return m[1] + m[2], nil
}

// policyFilter compiles list's CEL filter over setting and customer.
func policyFilter(f string) (func(*ci.Policy) bool, error) {
	if strings.TrimSpace(f) == "" {
		return func(p *ci.Policy) bool { return true }, nil
	}
	polEnvOnce.Do(func() {
		polEnv, polEnvErr = cel.NewEnv(cel.Variable("setting", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("customer", cel.StringType))
	})
	if polEnvErr != nil {
		return nil, apierror.Internal(polEnvErr, "policy filter")
	}
	a, iss := polEnv.Compile(f)
	if iss.Err() != nil {
		return nil, apierror.InvalidArgument("Invalid filter %q: %v", f, iss.Err())
	}
	if a.OutputType() != cel.BoolType {
		return nil, apierror.InvalidArgument("Invalid filter %q: it must be a condition.", f)
	}
	prg, err := polEnv.Program(a)
	if err != nil {
		return nil, apierror.InvalidArgument("Invalid filter %q: %v", f, err)
	}
	// "You may only filter on policies for a single customer at a time."
	if len(reCustomerTerm.FindAllString(f, -1)) > 1 {
		return nil, apierror.InvalidArgument("Invalid filter %q: you may only filter on policies for a single customer at a time.", f)
	}
	return func(p *ci.Policy) bool {
		var val any
		_ = json.Unmarshal(p.Setting.Value, &val)
		out, _, err := prg.Eval(map[string]any{"setting": map[string]any{"type": p.Setting.Type, "value": val}, "customer": p.Customer})
		if err != nil {
			return false
		}
		b, _ := out.Value().(bool)
		return b
	}, nil
}

// Allowlisted domains.

func (r *Registry) allowlistLocked() ([]*ci.AllowlistedDomain, error) {
	keys, err := r.st.List(allowlistPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list allowlisted domains")
	}
	var out []*ci.AllowlistedDomain
	for _, k := range keys {
		var d ci.AllowlistedDomain
		if r.loaded(k, &d) {
			out = append(out, &d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// AllowlistedDomains are the allowlisted domains, by domain.
func (r *Registry) AllowlistedDomains() ([]*ci.AllowlistedDomain, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.allowlistLocked()
}

var reDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// CreateAllowlistedDomain adds a domain to the allowlist.
func (r *Registry) CreateAllowlistedDomain(domain string) (*ci.AllowlistedDomain, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: domain is required.")
	}
	if !reDomain.MatchString(domain) {
		return nil, apierror.InvalidArgument("Invalid domain %q.", domain)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.allowlistLocked()
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Domain == domain {
			return nil, apierror.AlreadyExists("The domain %s is already allowlisted: %s.", domain, d.Name)
		}
	}
	d := &ci.AllowlistedDomain{Name: "allowlistedDomains/" + newIdentityID(), Domain: domain}
	return d, r.putJSONLocked(allowlistPrefix+strings.TrimPrefix(d.Name, "allowlistedDomains/"), d)
}

func allowlistID(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "allowlistedDomains/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("Invalid name %q: it must be allowlistedDomains/{id}.", name)
	}
	return id, nil
}

// AllowlistedDomain reads one.
func (r *Registry) AllowlistedDomain(name string) (*ci.AllowlistedDomain, error) {
	id, err := allowlistID(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var d ci.AllowlistedDomain
	if !r.loaded(allowlistPrefix+id, &d) {
		return nil, apierror.NotFound("Allowlisted domain %s not found.", name)
	}
	return &d, nil
}

// DeleteAllowlistedDomain removes a domain from the allowlist.
func (r *Registry) DeleteAllowlistedDomain(name string) error {
	if _, err := r.AllowlistedDomain(name); err != nil {
		return err
	}
	id, _ := allowlistID(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.st.Delete(allowlistPrefix + id); err != nil {
		return apierror.Internal(err, "delete allowlisted domain")
	}
	return nil
}

// customerRoutes registers invitations, policies and allowlisted domains.
func (r *Registry) customerRoutes(rt *rest.Router) {
	rt.Handle("GET /v1/customers/{customer}/userinvitations", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		if err := checkCustomer("customers/" + req.PathValue("customer")); err != nil {
			return err
		}
		keep, err := invitationFilter(q.Get("filter"))
		if err != nil {
			return err
		}
		ob := strings.Join(strings.Fields(strings.ToLower(q.Get("orderBy"))), " ")
		switch ob {
		case "", "email", "email asc", "email desc", "update_time", "update_time asc", "update_time desc":
		default:
			return apierror.InvalidArgument("Invalid orderBy %q: it must be one of update_time asc, update_time desc, email asc and email desc.", q.Get("orderBy"))
		}
		size, err := listSize(req, 100, 200)
		if err != nil {
			return err
		}
		all, err := r.Invitations()
		if err != nil {
			return err
		}
		var list []*ci.UserInvitation
		for _, inv := range all {
			if keep(inv.State) {
				list = append(list, inv)
			}
		}
		if strings.HasPrefix(ob, "update_time") {
			sort.SliceStable(list, func(i, j int) bool { return list[i].UpdateTime < list[j].UpdateTime })
		}
		if strings.HasSuffix(ob, "desc") {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
		keys := make([]string, len(list))
		byKey := map[string]*ci.UserInvitation{}
		for i, inv := range list {
			keys[i], byKey[inv.Name] = inv.Name, inv
		}
		page, next, err := pageOfSize(req, "invitations:"+q.Get("filter")+ob, keys, size)
		if err != nil {
			return err
		}
		resp := ci.ListUserInvitationsResponse{NextPageToken: next, UserInvitations: []*ci.UserInvitation{}}
		for _, k := range page {
			resp.UserInvitations = append(resp.UserInvitations, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/customers/{customer}/userinvitations/{inv}", func(w http.ResponseWriter, req *http.Request) error {
		email, verb, _ := strings.Cut(req.PathValue("inv"), ":")
		name := "customers/" + req.PathValue("customer") + "/userinvitations/" + email
		switch verb {
		case "":
			inv, err := r.Invitation(name)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, inv)
		case "isInvitableUser":
			ok, err := r.IsInvitable(name)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, ci.IsInvitableUserResponse{IsInvitableUser: ok, ForceSendFields: []string{"IsInvitableUser"}})
		}
		return apierror.NotFound("no such method: GET %s", req.URL.Path)
	})
	rt.Handle("POST /v1/customers/{customer}/userinvitations/{inv}", func(w http.ResponseWriter, req *http.Request) error {
		email, verb, _ := strings.Cut(req.PathValue("inv"), ":")
		if verb != "send" && verb != "cancel" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		inv, err := r.InvitationAction("customers/"+req.PathValue("customer")+"/userinvitations/"+email, verb, AcceptInput{})
		if err != nil {
			return err
		}
		return doneOp(w, invitationType+map[string]string{"send": "SendUserInvitationResponse", "cancel": "CancelUserInvitationResponse"}[verb],
			map[string]any{"userInvitation": inv})
	})
	rt.Handle("POST /cloudburrow/v1/customers/{customer}/userinvitations/{inv}", func(w http.ResponseWriter, req *http.Request) error {
		email, verb, _ := strings.Cut(req.PathValue("inv"), ":")
		if verb != "accept" && verb != "decline" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		var in AcceptInput
		if req.ContentLength != 0 {
			if err := rest.DecodeJSON(req, &in); err != nil {
				return err
			}
		}
		inv, err := r.InvitationAction("customers/"+req.PathValue("customer")+"/userinvitations/"+email, verb, in)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, inv)
	})

	rt.Handle("GET /v1/policies", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		keep, err := policyFilter(q.Get("filter"))
		if err != nil {
			return err
		}
		size, err := listSize(req, 50, 100)
		if err != nil {
			return err
		}
		all, err := r.Policies()
		if err != nil {
			return err
		}
		keys := []string{}
		byKey := map[string]*ci.Policy{}
		for _, p := range all {
			if keep(p) {
				keys = append(keys, p.Name)
				byKey[p.Name] = p
			}
		}
		page, next, err := pageOfSize(req, "policies:"+q.Get("filter"), keys, size)
		if err != nil {
			return err
		}
		resp := ci.ListPoliciesResponse{NextPageToken: next, Policies: []*ci.Policy{}}
		for _, k := range page {
			resp.Policies = append(resp.Policies, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("POST /v1/policies", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.Policy
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.CreatePolicy(&in)
		if err != nil {
			return err
		}
		return doneOp(w, policyType, p)
	})
	rt.Handle("GET /v1/policies/{policy}", func(w http.ResponseWriter, req *http.Request) error {
		p, err := r.GetPolicy("policies/" + req.PathValue("policy"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, p)
	})
	rt.Handle("PATCH /v1/policies/{policy}", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.Policy
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.PatchPolicy("policies/"+req.PathValue("policy"), &in)
		if err != nil {
			return err
		}
		return doneOp(w, policyType, p)
	})
	rt.Handle("DELETE /v1/policies/{policy}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeletePolicy("policies/" + req.PathValue("policy")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})

	rt.Handle("GET /v1/allowlistedDomains", func(w http.ResponseWriter, req *http.Request) error {
		only, err := allowlistFilter(req.URL.Query().Get("filter"))
		if err != nil {
			return err
		}
		size, err := listSize(req, 50, 100)
		if err != nil {
			return err
		}
		all, err := r.AllowlistedDomains()
		if err != nil {
			return err
		}
		keys := []string{}
		byKey := map[string]*ci.AllowlistedDomain{}
		for _, d := range all {
			if only != "" && !strings.EqualFold(d.Domain, only) {
				continue
			}
			keys = append(keys, d.Name)
			byKey[d.Name] = d
		}
		page, next, err := pageOfSize(req, "allowlist"+only, keys, size)
		if err != nil {
			return err
		}
		resp := ci.ListAllowlistedDomainsResponse{NextPageToken: next, AllowlistedDomains: []*ci.AllowlistedDomain{}}
		for _, k := range page {
			resp.AllowlistedDomains = append(resp.AllowlistedDomains, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("POST /v1/allowlistedDomains", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.AllowlistedDomain
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		d, err := r.CreateAllowlistedDomain(in.Domain)
		if err != nil {
			return err
		}
		return doneOp(w, allowlistType, d)
	})
	rt.Handle("GET /v1/allowlistedDomains/{id}", func(w http.ResponseWriter, req *http.Request) error {
		d, err := r.AllowlistedDomain("allowlistedDomains/" + req.PathValue("id"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, d)
	})
	rt.Handle("DELETE /v1/allowlistedDomains/{id}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteAllowlistedDomain("allowlistedDomains/" + req.PathValue("id")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
}
