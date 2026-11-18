package resourcemanager

// IAM service accounts (#1747): the IAM API's projects.serviceAccounts
// (google.iam.admin.v1.IAM's CreateServiceAccount, GetServiceAccount,
// ListServiceAccounts, UpdateServiceAccount, PatchServiceAccount,
// DeleteServiceAccount, UndeleteServiceAccount, EnableServiceAccount and
// DisableServiceAccount), served beside the custom roles (roles.go) on the
// Resource Manager port and kept in the registry's store. Behaviour follows
// Google's pages (docs.cloud.google.com/iam/docs/service-accounts-create,
// .../service-accounts-delete-undelete, .../iam/quotas and the
// projects.serviceAccounts REST reference, read 2026-11-17):
//
//   - an account ID is 6 to 30 lowercase letters, digits and dashes,
//     starting with a letter and not ending with a dash; the email is
//     ID@PROJECT_ID.iam.gserviceaccount.com and the name
//     projects/PROJECT_ID/serviceAccounts/EMAIL; a request may name the
//     account by email or by its 21-digit unique ID, and the project as "-";
//   - a display name is at most 100 bytes, a description at most 256;
//   - a project holds at most maxServiceAccounts accounts (Google's quota
//     "varies depending on the project"; 100 is its usual default);
//   - an account deleted less than 30 days ago can be undeleted, by its
//     unique ID only, unless an account with the same email was created
//     since; then it is gone;
//   - a disabled account is listed and read, and cannot authenticate
//     (credentials.go's checks).
//
// Keys are in serviceaccountkeys.go, the account's own IAM policy in iam.go.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const saPrefix = "resourcemanager/serviceaccounts/"

// Service account limits, from Google's IAM pages.
const (
	maxServiceAccounts     = 100
	maxSADisplayName       = 100
	maxSADescription       = 256
	saUndeletePeriod       = 30 * 24 * time.Hour
	saDefaultPageSize      = 20
	saMaxPageSize          = 100
	serviceAccountDomain   = ".iam.gserviceaccount.com"
	serviceAccountNotFound = "Unknown service account"
)

var saIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// serviceAccount is an account as the registry keeps it, keyed by its
// unique ID, so an account recreated under a deleted one's email is a
// separate record.
type serviceAccount struct {
	Project     string    `json:"project"`
	AccountID   string    `json:"accountId"`
	UniqueID    string    `json:"uniqueId"`
	DisplayName string    `json:"displayName,omitempty"`
	Description string    `json:"description,omitempty"`
	Etag        []byte    `json:"etag"`
	Disabled    bool      `json:"disabled,omitempty"`
	Deleted     bool      `json:"deleted,omitempty"`
	DeleteTime  time.Time `json:"deleteTime,omitempty"`
	CreateTime  time.Time `json:"createTime"`
	Keys        []saKey   `json:"keys,omitempty"`
}

func (a *serviceAccount) email() string { return a.AccountID + "@" + a.Project + serviceAccountDomain }

func (a *serviceAccount) name() string {
	return "projects/" + a.Project + "/serviceAccounts/" + a.email()
}

func (a *serviceAccount) proto() *adminpb.ServiceAccount {
	return &adminpb.ServiceAccount{Name: a.name(), ProjectId: a.Project, UniqueId: a.UniqueID, Email: a.email(),
		DisplayName: a.DisplayName, Description: a.Description, Etag: a.Etag, Oauth2ClientId: a.UniqueID, Disabled: a.Disabled}
}

// newUniqueID is a 21-digit numeric ID, starting with 1 as Google's do.
func newUniqueID() string {
	n, err := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return fmt.Sprintf("1%020s", n.String())
}

func newSAEtag() []byte {
	e := make([]byte, 8)
	_, _ = rand.Read(e[1:])
	e[0] = 0x07 // Google's etags start "Bw"
	return e
}

func saStoreKey(project, uniqueID string) string { return saPrefix + project + "/" + uniqueID }

// loadSALocked reads one record; nil when there is none or it was deleted
// longer ago than the undelete period (it is then purged).
func (r *Registry) loadSALocked(project, uniqueID string) (*serviceAccount, error) {
	b, err := r.st.Get(saStoreKey(project, uniqueID))
	if err != nil {
		return nil, nil
	}
	var a serviceAccount
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, apierror.Internal(err, "decode service account %s", uniqueID)
	}
	if a.Deleted && !r.now().Before(a.DeleteTime.Add(saUndeletePeriod)) {
		_ = r.st.Delete(saStoreKey(project, uniqueID))
		return nil, nil
	}
	return &a, nil
}

func (r *Registry) putSALocked(a *serviceAccount) error {
	b, err := json.Marshal(a)
	if err != nil {
		return apierror.Internal(err, "encode service account")
	}
	if err := r.st.Put(saStoreKey(a.Project, a.UniqueID), b); err != nil {
		return apierror.Internal(err, "store service account")
	}
	return nil
}

// accountsLocked are every record under prefix (a project's, or all),
// deleted ones included, sorted by email.
func (r *Registry) accountsLocked(project string) ([]*serviceAccount, error) {
	prefix := saPrefix
	if project != "" {
		prefix += project + "/"
	}
	keys, err := r.st.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "list service accounts")
	}
	var out []*serviceAccount
	for _, k := range keys {
		p, id, ok := strings.Cut(strings.TrimPrefix(k, saPrefix), "/")
		if !ok {
			continue
		}
		a, err := r.loadSALocked(p, id)
		if err != nil {
			return nil, err
		}
		if a != nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].email() < out[j].email() })
	return out, nil
}

// saProjectLocked resolves a project named in a request, by ID or number,
// to its ID.
func (r *Registry) saProjectLocked(x string) (string, error) {
	p, _, err := r.roleParentLocked("projects/" + x)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(p, "projects/"), nil
}

// findSALocked resolves "projects/{p}/serviceAccounts/{email or unique ID}"
// to its live account; with deleted, a deleted one too (the unique ID
// alone names it then).
func (r *Registry) findSALocked(name string, deleted bool) (*serviceAccount, error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	p, acct, ok2 := strings.Cut(rest, "/serviceAccounts/")
	if !ok || !ok2 || p == "" || acct == "" || strings.Contains(acct, "/") {
		return nil, apierror.InvalidArgument("Invalid service account name %q: it must be projects/{project}/serviceAccounts/{email or unique ID}.", name)
	}
	project := ""
	if p != "-" {
		var err error
		if project, err = r.saProjectLocked(p); err != nil {
			return nil, err
		}
	}
	byEmail := strings.Contains(acct, "@")
	if byEmail && project == "" {
		_, domain, _ := strings.Cut(acct, "@")
		project = strings.TrimSuffix(domain, serviceAccountDomain)
	}
	all, err := r.accountsLocked(project)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.Deleted && (!deleted || byEmail) {
			continue
		}
		if (byEmail && a.email() == acct) || (!byEmail && a.UniqueID == acct) {
			return a, nil
		}
	}
	return nil, apierror.NotFound(serviceAccountNotFound)
}

// CreateServiceAccount creates projects/{project}/serviceAccounts/{id}.
func (r *Registry) CreateServiceAccount(req *adminpb.CreateServiceAccountRequest) (*adminpb.ServiceAccount, error) {
	p, ok := strings.CutPrefix(req.GetName(), "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	id := req.GetAccountId()
	if !saIDPattern.MatchString(id) {
		return nil, apierror.InvalidArgument("Invalid account ID %q: it must be 6 to 30 lowercase letters, digits and dashes, start with a letter and not end with a dash.", id)
	}
	sa := req.GetServiceAccount()
	if len(sa.GetDisplayName()) > maxSADisplayName {
		return nil, apierror.InvalidArgument("The display name can be at most %d bytes long; it is %d.", maxSADisplayName, len(sa.GetDisplayName()))
	}
	if len(sa.GetDescription()) > maxSADescription {
		return nil, apierror.InvalidArgument("The description can be at most %d bytes long; it is %d.", maxSADescription, len(sa.GetDescription()))
	}
	r.mu.Lock()
	defer r.unlock()
	project, err := r.saProjectLocked(p)
	if err != nil {
		return nil, err
	}
	if pr, err := r.load(project); err == nil && pr.State == StateDeleteRequested {
		return nil, apierror.FailedPrecondition("project %s is pending deletion", project)
	}
	all, err := r.accountsLocked(project)
	if err != nil {
		return nil, err
	}
	live := 0
	for _, a := range all {
		if a.Deleted {
			continue
		}
		live++
		if a.AccountID == id {
			return nil, apierror.AlreadyExists("Service account %s already exists within project projects/%s.", id, project)
		}
	}
	if live >= maxServiceAccounts {
		return nil, &apierror.Error{Code: codes.ResourceExhausted, Reason: "rateLimitExceeded",
			Message: fmt.Sprintf("Maximum number of service accounts on project reached (%d).", maxServiceAccounts)}
	}
	a := &serviceAccount{Project: project, AccountID: id, UniqueID: newUniqueID(), DisplayName: sa.GetDisplayName(),
		Description: sa.GetDescription(), Etag: newSAEtag(), CreateTime: r.now().UTC()}
	// A new account under a deleted one's email is a new identity: the
	// deleted one can no longer be undeleted, and its policy is not the
	// new one's.
	for _, old := range all {
		if old.Deleted && old.AccountID == id {
			_ = r.st.Delete(saStoreKey(project, old.UniqueID))
		}
	}
	r.dropIAM(a.name())
	if err := a.rotateSystemKey(r.now()); err != nil {
		return nil, err
	}
	if err := r.putSALocked(a); err != nil {
		return nil, err
	}
	return a.proto(), nil
}

// GetServiceAccount reads one live account.
func (r *Registry) GetServiceAccount(name string) (*adminpb.ServiceAccount, error) {
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return nil, err
	}
	return a.proto(), nil
}

// ListServiceAccounts lists a project's live accounts, by email.
func (r *Registry) ListServiceAccounts(req *adminpb.ListServiceAccountsRequest) (*adminpb.ListServiceAccountsResponse, error) {
	p, ok := strings.CutPrefix(req.GetName(), "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = saDefaultPageSize
	}
	if size > saMaxPageSize {
		size = saMaxPageSize
	}
	r.mu.Lock()
	defer r.unlock()
	project, err := r.saProjectLocked(p)
	if err != nil {
		return nil, err
	}
	all, err := r.accountsLocked(project)
	if err != nil {
		return nil, err
	}
	out := &adminpb.ListServiceAccountsResponse{}
	for _, a := range all {
		if a.Deleted || (req.GetPageToken() != "" && a.email() <= req.GetPageToken()) {
			continue
		}
		if len(out.Accounts) == size {
			out.NextPageToken = out.Accounts[len(out.Accounts)-1].GetEmail()
			break
		}
		out.Accounts = append(out.Accounts, a.proto())
	}
	return out, nil
}

// PatchServiceAccount changes an account's display name and description,
// as the mask names them; UpdateServiceAccount (mask nil) changes both.
func (r *Registry) PatchServiceAccount(sa *adminpb.ServiceAccount, mask []string) (*adminpb.ServiceAccount, error) {
	if sa == nil {
		return nil, apierror.InvalidArgument("service_account is required")
	}
	if mask == nil {
		mask = []string{"display_name", "description"}
	}
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(sa.GetName(), false)
	if err != nil {
		return nil, err
	}
	if len(sa.GetEtag()) > 0 && string(sa.GetEtag()) != string(a.Etag) {
		return nil, apierror.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
	}
	for _, f := range mask {
		switch f {
		case "display_name", "displayName":
			if len(sa.GetDisplayName()) > maxSADisplayName {
				return nil, apierror.InvalidArgument("The display name can be at most %d bytes long; it is %d.", maxSADisplayName, len(sa.GetDisplayName()))
			}
			a.DisplayName = sa.GetDisplayName()
		case "description":
			if len(sa.GetDescription()) > maxSADescription {
				return nil, apierror.InvalidArgument("The description can be at most %d bytes long; it is %d.", maxSADescription, len(sa.GetDescription()))
			}
			a.Description = sa.GetDescription()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q: only display_name and description can be changed", f)
		}
	}
	a.Etag = newSAEtag()
	if err := r.putSALocked(a); err != nil {
		return nil, err
	}
	return a.proto(), nil
}

// DeleteServiceAccount marks an account deleted: hidden from reads and
// lists, its keys refused, undeletable by unique ID for 30 days.
func (r *Registry) DeleteServiceAccount(name string) error {
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return err
	}
	a.Deleted, a.DeleteTime = true, r.now().UTC()
	return r.putSALocked(a)
}

// UndeleteServiceAccount restores an account deleted less than 30 days
// ago, named by its unique ID.
func (r *Registry) UndeleteServiceAccount(name string) (*adminpb.UndeleteServiceAccountResponse, error) {
	r.mu.Lock()
	defer r.unlock()
	_, acct, _ := strings.Cut(name, "/serviceAccounts/")
	if strings.Contains(acct, "@") {
		return nil, apierror.InvalidArgument("Undelete names a service account by its unique ID: projects/-/serviceAccounts/{unique ID}.")
	}
	a, err := r.findSALocked(name, true)
	if err != nil {
		return nil, err
	}
	if a.Deleted {
		a.Deleted, a.DeleteTime = false, time.Time{}
		a.Etag = newSAEtag()
		if err := r.putSALocked(a); err != nil {
			return nil, err
		}
	}
	return &adminpb.UndeleteServiceAccountResponse{RestoredAccount: a.proto()}, nil
}

// SetServiceAccountDisabled enables or disables an account.
func (r *Registry) SetServiceAccountDisabled(name string, disabled bool) error {
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return err
	}
	if a.Disabled == disabled {
		return nil
	}
	a.Disabled = disabled
	a.Etag = newSAEtag()
	return r.putSALocked(a)
}

// AccountState is what the IAM Credentials API needs of an account it is
// asked to act as.
type AccountState struct {
	Email    string
	UniqueID string
	Disabled bool
}

// ServiceAccountByName resolves an account the way IAM Credentials names
// one, "projects/-/serviceAccounts/{email or unique ID}" or the bare email
// or ID; ok false when it is not a live account.
func (r *Registry) ServiceAccountByName(account string) (AccountState, bool) {
	name := account
	if !strings.HasPrefix(name, "projects/") {
		name = "projects/-/serviceAccounts/" + account
	}
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return AccountState{}, false
	}
	return AccountState{Email: a.email(), UniqueID: a.UniqueID, Disabled: a.Disabled}, true
}

// ServiceAccountSigner returns the private key of a live, enabled
// account's current system-managed key and its ID, rotating it when it has
// expired: what signBlob and signJwt sign with.
func (r *Registry) ServiceAccountSigner(account string) (*rsa.PrivateKey, string, error) {
	name := account
	if !strings.HasPrefix(name, "projects/") {
		name = "projects/-/serviceAccounts/" + account
	}
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return nil, "", err
	}
	k := a.systemKey(r.now())
	if k == nil {
		if err := a.rotateSystemKey(r.now()); err != nil {
			return nil, "", err
		}
		if err := r.putSALocked(a); err != nil {
			return nil, "", err
		}
		k = a.systemKey(r.now())
	}
	priv, err := k.privateKey()
	if err != nil {
		return nil, "", err
	}
	return priv, k.ID, nil
}

// dropServiceAccountsLocked deletes a purged project's accounts.
func (r *Registry) dropServiceAccountsLocked(project string) error {
	if err := r.deletePrefixLocked(iamPrefix + "projects/" + project + "/serviceAccounts/"); err != nil {
		return err
	}
	return r.deletePrefixLocked(saPrefix + project + "/")
}

// isServiceAccountResource reports whether a policy's resource is a
// service account's.
func isServiceAccountResource(resource string) bool {
	return strings.HasPrefix(resource, "projects/") && strings.Contains(resource, "/serviceAccounts/")
}

// --- the gRPC methods ----------------------------------------------------

func (s *IAMAdminServer) CreateServiceAccount(_ context.Context, req *adminpb.CreateServiceAccountRequest) (*adminpb.ServiceAccount, error) {
	return wrap(s.reg.CreateServiceAccount(req))
}

func (s *IAMAdminServer) GetServiceAccount(_ context.Context, req *adminpb.GetServiceAccountRequest) (*adminpb.ServiceAccount, error) {
	return wrap(s.reg.GetServiceAccount(req.GetName()))
}

func (s *IAMAdminServer) ListServiceAccounts(_ context.Context, req *adminpb.ListServiceAccountsRequest) (*adminpb.ListServiceAccountsResponse, error) {
	return wrap(s.reg.ListServiceAccounts(req))
}

func (s *IAMAdminServer) UpdateServiceAccount(_ context.Context, req *adminpb.ServiceAccount) (*adminpb.ServiceAccount, error) {
	return wrap(s.reg.PatchServiceAccount(req, nil))
}

func (s *IAMAdminServer) PatchServiceAccount(_ context.Context, req *adminpb.PatchServiceAccountRequest) (*adminpb.ServiceAccount, error) {
	mask := req.GetUpdateMask().GetPaths()
	if len(mask) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required: display_name, description"))
	}
	return wrap(s.reg.PatchServiceAccount(req.GetServiceAccount(), mask))
}

func (s *IAMAdminServer) DeleteServiceAccount(_ context.Context, req *adminpb.DeleteServiceAccountRequest) (*emptypb.Empty, error) {
	return empty(s.reg.DeleteServiceAccount(req.GetName()))
}

func (s *IAMAdminServer) UndeleteServiceAccount(_ context.Context, req *adminpb.UndeleteServiceAccountRequest) (*adminpb.UndeleteServiceAccountResponse, error) {
	return wrap(s.reg.UndeleteServiceAccount(req.GetName()))
}

func (s *IAMAdminServer) EnableServiceAccount(_ context.Context, req *adminpb.EnableServiceAccountRequest) (*emptypb.Empty, error) {
	return empty(s.reg.SetServiceAccountDisabled(req.GetName(), false))
}

func (s *IAMAdminServer) DisableServiceAccount(_ context.Context, req *adminpb.DisableServiceAccountRequest) (*emptypb.Empty, error) {
	return empty(s.reg.SetServiceAccountDisabled(req.GetName(), true))
}

func (s *IAMAdminServer) CreateServiceAccountKey(_ context.Context, req *adminpb.CreateServiceAccountKeyRequest) (*adminpb.ServiceAccountKey, error) {
	return wrap(s.reg.CreateServiceAccountKey(req))
}

func (s *IAMAdminServer) UploadServiceAccountKey(_ context.Context, req *adminpb.UploadServiceAccountKeyRequest) (*adminpb.ServiceAccountKey, error) {
	return wrap(s.reg.UploadServiceAccountKey(req.GetName(), req.GetPublicKeyData()))
}

func (s *IAMAdminServer) ListServiceAccountKeys(_ context.Context, req *adminpb.ListServiceAccountKeysRequest) (*adminpb.ListServiceAccountKeysResponse, error) {
	return wrap(s.reg.ListServiceAccountKeys(req))
}

func (s *IAMAdminServer) GetServiceAccountKey(_ context.Context, req *adminpb.GetServiceAccountKeyRequest) (*adminpb.ServiceAccountKey, error) {
	return wrap(s.reg.GetServiceAccountKey(req.GetName(), req.GetPublicKeyType()))
}

func (s *IAMAdminServer) DeleteServiceAccountKey(_ context.Context, req *adminpb.DeleteServiceAccountKeyRequest) (*emptypb.Empty, error) {
	return empty(s.reg.DeleteServiceAccountKey(req.GetName()))
}

func (s *IAMAdminServer) DisableServiceAccountKey(_ context.Context, req *adminpb.DisableServiceAccountKeyRequest) (*emptypb.Empty, error) {
	return empty(s.reg.SetServiceAccountKeyDisabled(req.GetName(), true))
}

func (s *IAMAdminServer) EnableServiceAccountKey(_ context.Context, req *adminpb.EnableServiceAccountKeyRequest) (*emptypb.Empty, error) {
	return empty(s.reg.SetServiceAccountKeyDisabled(req.GetName(), false))
}

// saResource is the canonical policy resource of the account a request
// names: projects/{project ID}/serviceAccounts/{email}.
func (s *IAMAdminServer) saResource(name string) (string, error) {
	if !isServiceAccountResource(name) {
		return "", apierror.InvalidArgument("resource %q must be projects/{project}/serviceAccounts/{account}", name)
	}
	s.reg.mu.Lock()
	defer s.reg.unlock()
	a, err := s.reg.findSALocked(name, false)
	if err != nil {
		return "", err
	}
	return a.name(), nil
}

// GetIamPolicy is a service account's own policy: who may act as it,
// impersonate it or manage it.
func (s *IAMAdminServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := s.saResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(s.reg, res, req)
}

func (s *IAMAdminServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := s.saResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return wrap(s.reg.SetIAMPolicy(res, req))
}

func (s *IAMAdminServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	res, err := s.saResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(s.reg, res, req)
}

func wrap[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, apierror.Wrap(err)
	}
	return v, nil
}

func empty(err error) (*emptypb.Empty, error) {
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}
