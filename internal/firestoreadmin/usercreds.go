package firestoreadmin

// User credentials (#1500): the Admin API's userCreds resources, which on
// Google authenticate MongoDB drivers to a Firestore with MongoDB
// compatibility database, an Enterprise edition feature. They are stored
// and served as Google documents them, on an Enterprise edition database
// alone, and refused FAILED_PRECONDITION on a Standard one. The MongoDB
// wire protocol itself is not served, so nothing authenticates with them:
// they are records for the tools and Terraform configurations that manage
// them. A server-generated password is returned once, by CreateUserCreds
// and ResetUserPassword, as on Google, and never stored: only its SHA-256
// digest is kept.

import (
	"context"
	"crypto/sha256"
	"regexp"
	"sort"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	userCredsNameRe = regexp.MustCompile(`^(projects/([^/]+)/databases/([^/]+))/userCreds/([^/]+)$`)
	userCredsIDRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,61}[a-z0-9]$`)
)

// creds is a user's credentials and the digest of their password.
type creds struct {
	UserCreds *adminpb.UserCreds
	Digest    []byte
}

// enterprise is the live Enterprise edition database name. a.mu is held.
func (a *Admin) enterprise(name string) (*adminpb.Database, error) {
	db, err := a.database(name)
	if err != nil {
		return nil, err
	}
	if db.GetDatabaseEdition() != adminpb.Database_ENTERPRISE {
		return nil, status.Errorf(codes.FailedPrecondition, "User credentials are only available on an Enterprise edition database; %s is Standard edition.", name)
	}
	return db, nil
}

// newPassword is a server-generated password and its digest.
func newPassword() (string, []byte) {
	pw := randomID(24)
	d := sha256.Sum256([]byte(pw))
	return pw, d[:]
}

// principal is the identity Google names a user's credentials by.
func principal(name string) string {
	m := userCredsNameRe.FindStringSubmatch(name)
	return "principal://firestore.googleapis.com/projects/" + m[2] + "/name/databases/" + m[3] + "/userCreds/" + m[4]
}

// credsByName is the user credentials named name. a.mu is held.
func (a *Admin) credsByName(name string) (*creds, error) {
	m := userCredsNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user creds name %q: want projects/{project}/databases/{database}/userCreds/{user_creds}", name)
	}
	if _, err := a.enterprise(m[1]); err != nil {
		return nil, err
	}
	c, ok := a.userCreds[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "User creds %s not found.", name)
	}
	return c, nil
}

func (a *Admin) CreateUserCreds(_ context.Context, req *adminpb.CreateUserCredsRequest) (*adminpb.UserCreds, error) {
	id := req.GetUserCredsId()
	if !userCredsIDRe.MatchString(id) || uuidLikeRe.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "user_creds_id %q is invalid: it must be 4-63 characters of lowercase letters, numbers and hyphens, start with a letter, end with a letter or number, and not be UUID-like.", id)
	}
	if req.GetUserCreds() == nil {
		return nil, status.Error(codes.InvalidArgument, "user_creds is required.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.enterprise(req.GetParent()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/userCreds/" + id
	if _, ok := a.userCreds[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "User creds %s already exists.", name)
	}
	now := timestamppb.New(a.now())
	pw, digest := newPassword()
	uc := &adminpb.UserCreds{Name: name, CreateTime: now, UpdateTime: now, State: adminpb.UserCreds_ENABLED,
		UserCredsIdentity: &adminpb.UserCreds_ResourceIdentity_{ResourceIdentity: &adminpb.UserCreds_ResourceIdentity{Principal: principal(name)}}}
	a.userCreds[name] = &creds{UserCreds: uc, Digest: digest}
	out := proto.Clone(uc).(*adminpb.UserCreds)
	out.SecurePassword = pw
	return out, nil
}

func (a *Admin) GetUserCreds(_ context.Context, req *adminpb.GetUserCredsRequest) (*adminpb.UserCreds, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.credsByName(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(c.UserCreds).(*adminpb.UserCreds), nil
}

func (a *Admin) ListUserCreds(_ context.Context, req *adminpb.ListUserCredsRequest) (*adminpb.ListUserCredsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.enterprise(req.GetParent()); err != nil {
		return nil, err
	}
	resp := &adminpb.ListUserCredsResponse{}
	for name, c := range a.userCreds {
		if strings.HasPrefix(name, req.GetParent()+"/userCreds/") {
			resp.UserCreds = append(resp.UserCreds, proto.Clone(c.UserCreds).(*adminpb.UserCreds))
		}
	}
	sort.Slice(resp.UserCreds, func(i, j int) bool { return resp.UserCreds[i].Name < resp.UserCreds[j].Name })
	return resp, nil
}

func (a *Admin) setCredsState(name string, st adminpb.UserCreds_State) (*adminpb.UserCreds, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.credsByName(name)
	if err != nil {
		return nil, err
	}
	if c.UserCreds.State != st {
		c.UserCreds.State = st
		c.UserCreds.UpdateTime = timestamppb.New(a.now())
	}
	return proto.Clone(c.UserCreds).(*adminpb.UserCreds), nil
}

func (a *Admin) EnableUserCreds(_ context.Context, req *adminpb.EnableUserCredsRequest) (*adminpb.UserCreds, error) {
	return a.setCredsState(req.GetName(), adminpb.UserCreds_ENABLED)
}

func (a *Admin) DisableUserCreds(_ context.Context, req *adminpb.DisableUserCredsRequest) (*adminpb.UserCreds, error) {
	return a.setCredsState(req.GetName(), adminpb.UserCreds_DISABLED)
}

func (a *Admin) ResetUserPassword(_ context.Context, req *adminpb.ResetUserPasswordRequest) (*adminpb.UserCreds, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.credsByName(req.GetName())
	if err != nil {
		return nil, err
	}
	pw, digest := newPassword()
	c.Digest = digest
	c.UserCreds.UpdateTime = timestamppb.New(a.now())
	out := proto.Clone(c.UserCreds).(*adminpb.UserCreds)
	out.SecurePassword = pw
	return out, nil
}

func (a *Admin) DeleteUserCreds(_ context.Context, req *adminpb.DeleteUserCredsRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.credsByName(req.GetName()); err != nil {
		return nil, err
	}
	delete(a.userCreds, req.GetName())
	return &emptypb.Empty{}, nil
}
