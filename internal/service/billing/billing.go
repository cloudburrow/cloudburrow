// Package billing serves Cloud Billing locally (#2109): the Cloud Billing
// API v1 (cloudbilling.googleapis.com: billing accounts and subaccounts,
// projects' billing info, the billing account IAM policy, and the Cloud
// Catalog of services and SKUs) and the Budget API v1
// (billingbudgets.googleapis.com).
//
// A local billing account is a record and nothing more: it holds no
// payment details and no money, and CloudBurrow never contacts Google for
// it. Every instance starts with one open, top-level account under the
// instance's organization, as a new Google Cloud customer starts with the
// account they sign up with; the API creates subaccounts under it, as
// Google's does (billingAccounts.create creates subaccounts only).
//
// Spend is real, and it is zero: CloudBurrow measures usage but has no
// pricing model, so every SKU in the local catalog lists a price of 0 and
// every budget's actual spend is 0. No threshold is exceeded, so no budget
// notification is published (docs/billing.md).
//
// Read from Google's public docs on 2026-11-21:
// https://docs.cloud.google.com/billing/docs/reference/rest/v1/billingAccounts,
// .../projects/updateBillingInfo,
// https://docs.cloud.google.com/billing/docs/reference/budget/rest/v1/billingAccounts.budgets,
// https://docs.cloud.google.com/billing/docs/how-to/modify-project and
// https://docs.cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications.
package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/billing/apiv1/billingpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Projects resolves a project ID or number to the ID of a project the
// instance has, or fails with NOT_FOUND.
type Projects interface {
	Resolve(idOrNumber string) (string, error)
}

// APIs is what unlinking a project's billing does to its APIs: Google stops
// "all billable services within that project" when billing is disabled
// (modify-project), which CloudBurrow models by disabling the project's
// enabled billable APIs, and enables them again when billing is.
type APIs interface {
	// DisableBillable disables the project's enabled billable APIs and
	// returns their names.
	DisableBillable(project string) ([]string, error)
	// Enable enables the named APIs again.
	Enable(project string, apis []string) error
}

// Server is the Cloud Billing API's CloudBilling service.
type Server struct {
	pb.UnimplementedCloudBillingServer
	st       store.Store
	projects Projects
	org      string
	// APIs, when set, is told when a project's billing is unlinked and
	// linked again.
	APIs APIs
	now  func() time.Time

	mu sync.Mutex
}

// NewServer keeps accounts, links and budgets in st. org is the instance's
// organization ("organizations/N"), every top-level account's parent.
func NewServer(st store.Store, projects Projects, org string) *Server {
	return &Server{st: st, projects: projects, org: org, now: time.Now}
}

const (
	accountPrefix = "billing/accounts/"
	linkPrefix    = "billing/projects/"
	budgetPrefix  = "billing/budgets/"
	// DefaultDisplayName is the display name of the account an instance
	// starts with, the name Google's console gives a new account.
	DefaultDisplayName = "My Billing Account"
	// Currency is the local accounts' currency.
	Currency = "USD"
)

var accountID = regexp.MustCompile(`^[0-9A-F]{6}-[0-9A-F]{6}-[0-9A-F]{6}$`)

// account is one stored billing account.
type account struct {
	DisplayName string            `json:"displayName"`
	Open        bool              `json:"open"`
	Master      string            `json:"master,omitempty"`
	Parent      string            `json:"parent"`
	Currency    string            `json:"currency"`
	Created     time.Time         `json:"created"`
	Policy      *iampolicy.Stored `json:"policy,omitempty"`
}

// link is one project's billing.
type link struct {
	Account string `json:"account,omitempty"`
	// Stopped are the APIs unlinking disabled, enabled again on a link.
	Stopped []string `json:"stopped,omitempty"`
}

func (a *account) proto(id string) *pb.BillingAccount {
	return &pb.BillingAccount{Name: "billingAccounts/" + id, Open: a.Open, DisplayName: a.DisplayName,
		MasterBillingAccount: a.Master, Parent: a.Parent, CurrencyCode: a.Currency}
}

// DefaultAccountID is the ID of the account an instance starts with,
// derived from its organization so it is stable across restarts.
func DefaultAccountID(org string) string {
	sum := sha256.Sum256([]byte("cloudburrow-billing/" + org))
	h := strings.ToUpper(hex.EncodeToString(sum[:9]))
	return h[0:6] + "-" + h[6:12] + "-" + h[12:18]
}

func newAccountID() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	h := strings.ToUpper(hex.EncodeToString(b[:]))
	return h[0:6] + "-" + h[6:12] + "-" + h[12:18]
}

func (s *Server) get(key string, v any) (bool, error) {
	b, err := s.st.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, status.Errorf(codes.Internal, "billing: read %s: %v", key, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, status.Errorf(codes.Internal, "billing: decode %s: %v", key, err)
	}
	return true, nil
}

func (s *Server) put(key string, v any) error {
	if !store.SafeKey(key) {
		return status.Errorf(codes.InvalidArgument, "%q cannot be stored", key)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.st.Put(key, b)
}

// ensureDefault creates the instance's first account when none exists.
// Callers hold s.mu.
func (s *Server) ensureDefault() error {
	keys, err := s.st.List(accountPrefix)
	if err != nil {
		return status.Errorf(codes.Internal, "billing: list accounts: %v", err)
	}
	if len(keys) > 0 {
		return nil
	}
	return s.put(accountPrefix+DefaultAccountID(s.org), &account{DisplayName: DefaultDisplayName, Open: true,
		Parent: s.org, Currency: Currency, Created: s.now().UTC()})
}

// accountName parses "billingAccounts/ID".
func accountName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "billingAccounts/")
	if !ok || !accountID.MatchString(id) {
		return "", status.Errorf(codes.InvalidArgument, "Invalid billing account name %q: want billingAccounts/{billing_account_id}, such as billingAccounts/012345-567890-ABCDEF", name)
	}
	return id, nil
}

// load reads an account by name. Callers hold s.mu.
func (s *Server) load(name string) (string, *account, error) {
	id, err := accountName(name)
	if err != nil {
		return "", nil, err
	}
	if err := s.ensureDefault(); err != nil {
		return "", nil, err
	}
	a := &account{}
	ok, err := s.get(accountPrefix+id, a)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, status.Errorf(codes.NotFound, "Billing account %s not found.", name)
	}
	return id, a, nil
}

// Account is one billing account, for the console and the other services.
func (s *Server) Account(name string) (*pb.BillingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, a, err := s.load(name)
	if err != nil {
		return nil, err
	}
	return a.proto(id), nil
}

// Accounts are every billing account, sorted by name.
func (s *Server) Accounts() ([]*pb.BillingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts()
}

func (s *Server) accounts() ([]*pb.BillingAccount, error) {
	if err := s.ensureDefault(); err != nil {
		return nil, err
	}
	keys, err := s.st.List(accountPrefix)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "billing: list accounts: %v", err)
	}
	sort.Strings(keys)
	out := make([]*pb.BillingAccount, 0, len(keys))
	for _, k := range keys {
		a := &account{}
		if ok, err := s.get(k, a); err != nil {
			return nil, err
		} else if ok {
			out = append(out, a.proto(strings.TrimPrefix(k, accountPrefix)))
		}
	}
	return out, nil
}

// GetBillingAccount is billingAccounts.get.
func (s *Server) GetBillingAccount(_ context.Context, req *pb.GetBillingAccountRequest) (*pb.BillingAccount, error) {
	return s.Account(req.GetName())
}

// ListBillingAccounts is billingAccounts.list (and, with a billing account
// parent, billingAccounts.subAccounts.list; with an organization,
// organizations.billingAccounts.list). The only filter Google documents is
// the subaccounts of one account: masterBillingAccount=billingAccounts/ID.
func (s *Server) ListBillingAccounts(_ context.Context, req *pb.ListBillingAccountsRequest) (*pb.ListBillingAccountsResponse, error) {
	master := ""
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		k, v, ok := strings.Cut(f, "=")
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if !ok || (k != "masterBillingAccount" && k != "master_billing_account") {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid filter %q: only masterBillingAccount=billingAccounts/{billing_account_id} is supported", f)
		}
		if _, err := accountName(v); err != nil {
			return nil, err
		}
		master = v
	}
	parent := req.GetParent()
	if parent != "" && !strings.HasPrefix(parent, "organizations/") {
		if _, err := accountName(parent); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid parent %q: want organizations/{organization_id} or billingAccounts/{billing_account_id}", parent)
		}
	}
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	s.mu.Lock()
	all, err := s.accounts()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	byName := map[string]*pb.BillingAccount{}
	var keys []string
	for _, a := range all {
		if master != "" && a.GetMasterBillingAccount() != master {
			continue
		}
		if parent != "" && a.GetParent() != parent {
			continue
		}
		byName[a.GetName()] = a
		keys = append(keys, a.GetName())
	}
	size := int(req.GetPageSize())
	if size == 0 || size > 100 {
		size = 100
	}
	page, next, err := paging.Page("billingAccounts|"+parent+"|"+master, keys, req.GetPageToken(), size)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.ListBillingAccountsResponse{NextPageToken: next}
	for _, k := range page {
		out.BillingAccounts = append(out.BillingAccounts, byName[k])
	}
	return out, nil
}

// UpdateBillingAccount is billingAccounts.patch: "Currently the only field
// that can be edited is display_name."
func (s *Server) UpdateBillingAccount(_ context.Context, req *pb.UpdateBillingAccountRequest) (*pb.BillingAccount, error) {
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "display_name" && p != "displayName" {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid update_mask path %q: only display_name can be updated", p)
		}
	}
	dn := req.GetAccount().GetDisplayName()
	if err := checkDisplayName(dn); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, a, err := s.load(req.GetName())
	if err != nil {
		return nil, err
	}
	a.DisplayName = dn
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	return a.proto(id), nil
}

func checkDisplayName(dn string) error {
	if strings.TrimSpace(dn) == "" {
		return status.Error(codes.InvalidArgument, "display_name is required")
	}
	return nil
}

// CreateBillingAccount is billingAccounts.create, which creates
// subaccounts: parent (or, in the older form, the account's
// master_billing_account) names the parent account. A subaccount takes its
// parent's currency, and cannot itself have subaccounts.
func (s *Server) CreateBillingAccount(_ context.Context, req *pb.CreateBillingAccountRequest) (*pb.BillingAccount, error) {
	in := req.GetBillingAccount()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "billing_account is required")
	}
	parent := req.GetParent()
	if parent == "" {
		parent = in.GetMasterBillingAccount()
	}
	if parent == "" {
		return nil, status.Error(codes.InvalidArgument, "parent is required: billingAccounts.create creates subaccounts of the billing account it names")
	}
	if strings.HasPrefix(parent, "organizations/") {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid parent %q: billingAccounts.create creates subaccounts; the parent must be billingAccounts/{billing_account_id}", parent)
	}
	if err := checkDisplayName(in.GetDisplayName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pid, p, err := s.load(parent)
	if err != nil {
		return nil, err
	}
	if p.Master != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "Billing account %s is a subaccount and cannot have subaccounts", parent)
	}
	if !p.Open {
		return nil, status.Errorf(codes.FailedPrecondition, "Billing account %s is closed", parent)
	}
	if c := in.GetCurrencyCode(); c != "" && c != p.Currency {
		return nil, status.Errorf(codes.InvalidArgument, "currency_code %q must match the parent account's currency %s", c, p.Currency)
	}
	id := newAccountID()
	a := &account{DisplayName: in.GetDisplayName(), Open: true, Master: "billingAccounts/" + pid,
		Parent: "billingAccounts/" + pid, Currency: p.Currency, Created: s.now().UTC()}
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	return a.proto(id), nil
}

// CreateTopLevel creates an open top-level account under the instance's
// organization: on Google that is the console's sign-up flow, with a
// payment profile; locally there is nothing to pay with, so it takes a
// display name only. The console's Create account uses it.
func (s *Server) CreateTopLevel(displayName string) (*pb.BillingAccount, error) {
	if err := checkDisplayName(displayName); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefault(); err != nil {
		return nil, err
	}
	id := newAccountID()
	a := &account{DisplayName: displayName, Open: true, Parent: s.org, Currency: Currency, Created: s.now().UTC()}
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	return a.proto(id), nil
}

// SetOpen closes or reopens an account, as the console's Close billing
// account and Reopen billing account do. A closed account's projects keep
// their link but have billing disabled, as on Google; closing an account
// closes its subaccounts too.
func (s *Server) SetOpen(name string, open bool) (*pb.BillingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, a, err := s.load(name)
	if err != nil {
		return nil, err
	}
	if open && a.Master != "" {
		if _, p, err := s.load(a.Master); err == nil && !p.Open {
			return nil, status.Errorf(codes.FailedPrecondition, "Reopen the parent account %s first", a.Master)
		}
	}
	a.Open = open
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	if !open {
		all, err := s.accounts()
		if err != nil {
			return nil, err
		}
		for _, sub := range all {
			if sub.GetMasterBillingAccount() == "billingAccounts/"+id && sub.GetOpen() {
				sid := strings.TrimPrefix(sub.GetName(), "billingAccounts/")
				sa := &account{}
				if ok, err := s.get(accountPrefix+sid, sa); err == nil && ok {
					sa.Open = false
					if err := s.put(accountPrefix+sid, sa); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return a.proto(id), nil
}

// MoveBillingAccount is billingAccounts.move: it changes the organization a
// top-level account belongs to. A subaccount "always belongs to the same
// organization as its parent account", so moving one is refused.
func (s *Server) MoveBillingAccount(_ context.Context, req *pb.MoveBillingAccountRequest) (*pb.BillingAccount, error) {
	dest := req.GetDestinationParent()
	if dest == "" {
		return nil, status.Error(codes.InvalidArgument, "destination_parent is required")
	}
	if !strings.HasPrefix(dest, "organizations/") {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid destination_parent %q: want organizations/{organization_id}", dest)
	}
	if dest != s.org {
		return nil, status.Errorf(codes.NotFound, "Organization %s not found: this instance has one organization, %s", dest, s.org)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, a, err := s.load(req.GetName())
	if err != nil {
		return nil, err
	}
	if a.Master != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "Billing account %s is a subaccount: a subaccount always belongs to the same organization as its parent account", req.GetName())
	}
	a.Parent = dest
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	return a.proto(id), nil
}

// projectName parses "projects/{id or number}" to the project's ID.
func (s *Server) projectName(name string) (string, error) {
	p, ok := strings.CutPrefix(name, "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return "", status.Errorf(codes.InvalidArgument, "Invalid project name %q: want projects/{project_id}", name)
	}
	if s.projects == nil {
		return p, nil
	}
	id, err := s.projects.Resolve(p)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "Project %s not found.", name)
	}
	return id, nil
}

func (s *Server) info(project string, l link) *pb.ProjectBillingInfo {
	out := &pb.ProjectBillingInfo{Name: "projects/" + project + "/billingInfo", ProjectId: project, BillingAccountName: l.Account}
	if l.Account != "" {
		a := &account{}
		if ok, _ := s.get(accountPrefix+strings.TrimPrefix(l.Account, "billingAccounts/"), a); ok {
			out.BillingEnabled = a.Open
		}
	}
	return out
}

// GetProjectBillingInfo is projects.getBillingInfo.
func (s *Server) GetProjectBillingInfo(_ context.Context, req *pb.GetProjectBillingInfoRequest) (*pb.ProjectBillingInfo, error) {
	name := strings.TrimSuffix(req.GetName(), "/billingInfo")
	id, err := s.projectName(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var l link
	if _, err := s.get(linkPrefix+id, &l); err != nil {
		return nil, err
	}
	return s.info(id, l), nil
}

// UpdateProjectBillingInfo is projects.updateBillingInfo: a billing account
// name links the project to it, an empty one disables billing ("Any
// billable activity of your in-use services will stop"), which disables the
// project's billable APIs through APIs; linking again enables them.
func (s *Server) UpdateProjectBillingInfo(_ context.Context, req *pb.UpdateProjectBillingInfoRequest) (*pb.ProjectBillingInfo, error) {
	id, err := s.projectName(strings.TrimSuffix(req.GetName(), "/billingInfo"))
	if err != nil {
		return nil, err
	}
	target := req.GetProjectBillingInfo().GetBillingAccountName()
	s.mu.Lock()
	defer s.mu.Unlock()
	var l link
	if _, err := s.get(linkPrefix+id, &l); err != nil {
		return nil, err
	}
	wasEnabled := s.info(id, l).GetBillingEnabled()
	if target != "" {
		aid, a, err := s.load(target)
		if err != nil {
			return nil, err
		}
		l.Account = "billingAccounts/" + aid
		if a.Open && len(l.Stopped) > 0 && s.APIs != nil {
			if err := s.APIs.Enable(id, l.Stopped); err != nil {
				return nil, status.Errorf(codes.Internal, "enable the APIs unlinking stopped: %v", err)
			}
			l.Stopped = nil
		}
	} else {
		l.Account = ""
	}
	enabled := s.info(id, l).GetBillingEnabled()
	if wasEnabled && !enabled && s.APIs != nil {
		stopped, err := s.APIs.DisableBillable(id)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "disable the project's billable APIs: %v", err)
		}
		l.Stopped = mergeNames(l.Stopped, stopped)
	}
	if l.Account == "" && len(l.Stopped) == 0 {
		if err := s.st.Delete(linkPrefix + id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	} else if err := s.put(linkPrefix+id, &l); err != nil {
		return nil, err
	}
	return s.info(id, l), nil
}

func mergeNames(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string(nil), a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// Linked are the projects linked to the account name, sorted.
func (s *Server) Linked(name string) ([]*pb.ProjectBillingInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _, err := s.load(name)
	if err != nil {
		return nil, err
	}
	keys, err := s.st.List(linkPrefix)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "billing: list links: %v", err)
	}
	sort.Strings(keys)
	var out []*pb.ProjectBillingInfo
	for _, k := range keys {
		var l link
		if ok, err := s.get(k, &l); err != nil {
			return nil, err
		} else if ok && l.Account == "billingAccounts/"+id {
			out = append(out, s.info(strings.TrimPrefix(k, linkPrefix), l))
		}
	}
	return out, nil
}

// ProjectBilling is the project's billing info, for the console.
func (s *Server) ProjectBilling(project string) (*pb.ProjectBillingInfo, error) {
	return s.GetProjectBillingInfo(context.Background(), &pb.GetProjectBillingInfoRequest{Name: "projects/" + project + "/billingInfo"})
}

// ListProjectBillingInfo is billingAccounts.projects.list.
func (s *Server) ListProjectBillingInfo(_ context.Context, req *pb.ListProjectBillingInfoRequest) (*pb.ListProjectBillingInfoResponse, error) {
	all, err := s.Linked(req.GetName())
	if err != nil {
		return nil, err
	}
	byName := map[string]*pb.ProjectBillingInfo{}
	keys := make([]string, 0, len(all))
	for _, p := range all {
		byName[p.GetName()] = p
		keys = append(keys, p.GetName())
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 100 {
		size = 100
	}
	page, next, err := paging.Page("billingProjects|"+req.GetName(), keys, req.GetPageToken(), size)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.ListProjectBillingInfoResponse{NextPageToken: next}
	for _, k := range page {
		out.ProjectBillingInfo = append(out.ProjectBillingInfo, byName[k])
	}
	return out, nil
}

// GetIamPolicy is billingAccounts.getIamPolicy.
func (s *Server) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, a, err := s.load(req.GetResource())
	if err != nil {
		return nil, err
	}
	return iampolicy.Get(a.Policy, req.GetOptions())
}

// SetIamPolicy is billingAccounts.setIamPolicy.
func (s *Server) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, a, err := s.load(req.GetResource())
	if err != nil {
		return nil, err
	}
	for _, b := range req.GetPolicy().GetBindings() {
		if r := b.GetRole(); !strings.HasPrefix(r, "roles/") && !strings.Contains(r, "/roles/") {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid role %q", r)
		}
	}
	next, err := iampolicy.SetConditional(a.Policy, req)
	if err != nil {
		return nil, err
	}
	a.Policy = &next
	if err := s.put(accountPrefix+id, a); err != nil {
		return nil, err
	}
	return next.Proto(), nil
}

// TestIamPermissions is billingAccounts.testIamPermissions: every requested
// billing permission, since this API's policy is stored and not enforced.
func (s *Server) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	s.mu.Lock()
	_, _, err := s.load(req.GetResource())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, p := range req.GetPermissions() {
		if strings.ContainsAny(p, "*") || strings.Count(p, ".") < 2 {
			return nil, status.Errorf(codes.InvalidArgument, "Permission %s is not valid.", p)
		}
	}
	return iampolicy.Permissions(req), nil
}

// Reset forgets every account, link and budget; the next read creates the
// instance's first account again.
func (s *Server) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, prefix := range []string{accountPrefix, linkPrefix, budgetPrefix} {
		keys, err := s.st.List(prefix)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := s.st.Delete(k); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// ResetProject forgets the project's billing link.
func (s *Server) ResetProject(project string) error {
	if project == "" {
		return s.Reset()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.st.Delete(linkPrefix + project); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("billing: forget %s: %w", project, err)
	}
	return nil
}
