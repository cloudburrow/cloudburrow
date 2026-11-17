package resourcemanager

// IAM custom roles (#1743): the IAM API's projects.roles and
// organizations.roles (google.iam.admin.v1.IAM's CreateRole, GetRole,
// ListRoles, UpdateRole, DeleteRole and UndeleteRole) and
// permissions.queryTestablePermissions, served on the Resource Manager port
// over gRPC and, transcoded, the v1 REST paths gcloud iam roles and
// Terraform's google_project_iam_custom_role and
// google_organization_iam_custom_role call. The roles are kept in the
// registry's store beside the policies that grant them, as Google
// documents them (docs.cloud.google.com/iam/docs/roles-overview and
// .../creating-custom-roles, read 2026-11-17):
//
//   - a role ID is 3 to 64 letters, digits, underscores and periods, and is
//     never reused within its project or organization while the role, or
//     its deletion, lasts;
//   - a title is at most 100 bytes, a description at most 300, a role at
//     most 3,000 permissions and 64 KB of title, description and permission
//     names; a project or the organization has at most 300 custom roles;
//   - a permission must be one Google's custom roles support page lists as
//     SUPPORTED or TESTING (internal/iamroles), and a project-level role may
//     not hold one on an organization or folder; any other is
//     INVALID_ARGUMENT "Permission P is not valid.";
//   - the stage is EAP, ALPHA (the default), BETA, GA, DEPRECATED or
//     DISABLED; a DISABLED role still appears in policies, grants nothing;
//   - an etag: an update or delete carrying a stale one is ABORTED;
//   - DeleteRole marks the role deleted: it is listed only with
//     show_deleted, its bindings stay in policies and grant nothing, and a
//     policy may not bind it anew. UndeleteRole restores it within 7 days.
//     44 days after the deletion it is gone, and its ID is free again.
//
// Every permission check CloudBurrow makes (Firestore with MongoDB
// compatibility's, Memorystore's IAM auth) evaluates a custom role through
// RolePermissions. Predefined roles (roles/...) are not served by GetRole or
// ListRoles: UNIMPLEMENTED.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

const rolePrefix = "resourcemanager/roles/"

// Custom role limits, from Google's IAM pages.
const (
	maxRolesPerParent   = 300
	maxRoleTitle        = 100
	maxRoleDescription  = 300
	maxRolePermissions  = 3000
	maxRoleBytes        = 64 << 10
	roleUndeletePeriod  = 7 * 24 * time.Hour
	roleDeletionProcess = 44 * 24 * time.Hour
)

var roleIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.]{3,64}$`)

// customRole is a custom role as the registry keeps it.
type customRole struct {
	Name        string    `json:"name"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Permissions []string  `json:"includedPermissions,omitempty"`
	Stage       string    `json:"stage,omitempty"`
	Etag        []byte    `json:"etag"`
	Deleted     bool      `json:"deleted,omitempty"`
	DeleteTime  time.Time `json:"deleteTime,omitempty"`
}

func (c *customRole) proto(full bool) *adminpb.Role {
	out := &adminpb.Role{Name: c.Name, Title: c.Title, Description: c.Description,
		Stage: adminpb.Role_RoleLaunchStage(adminpb.Role_RoleLaunchStage_value[c.Stage]), Etag: c.Etag, Deleted: c.Deleted}
	if full {
		out.IncludedPermissions = append([]string(nil), c.Permissions...)
	}
	return out
}

func newRoleEtag() []byte {
	e := make([]byte, 8)
	_, _ = rand.Read(e[1:])
	e[0] = 0x07 // Google's role etags start "Bw"
	return e
}

// roleParentLocked resolves a roles parent, "projects/{id or number}" or
// the organization, to its canonical name ("projects/{id}"), and whether it
// is a project. The caller holds r.mu.
func (r *Registry) roleParentLocked(parent string) (string, bool, error) {
	if x, ok := strings.CutPrefix(parent, "projects/"); ok {
		if x == "" || strings.Contains(x, "/") {
			return "", false, apierror.InvalidArgument("parent %q must be projects/{project} or organizations/{organization}", parent)
		}
		if _, err := r.load(x); err == nil {
			return "projects/" + x, true, nil
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			keys, _ := r.st.List(keyPrefix)
			for _, k := range keys {
				id := strings.TrimPrefix(k, keyPrefix)
				if r.projectNumber(id) == n {
					return "projects/" + id, true, nil
				}
			}
		}
		return "", false, apierror.NotFound("project %q not found", x)
	}
	if strings.HasPrefix(parent, "organizations/") {
		return parent, false, checkOrg(parent)
	}
	return "", false, apierror.InvalidArgument("parent %q must be projects/{project} or organizations/{organization}", parent)
}

// splitRoleNameLocked resolves "{parent}/roles/{id}" to its canonical
// parent and ID.
func (r *Registry) splitRoleNameLocked(name string) (parent, id string, project bool, err error) {
	if strings.HasPrefix(name, "roles/") {
		return "", "", false, apierror.Unimplemented("predefined role %s: only custom roles (projects/*/roles/*, organizations/*/roles/*) are served", name)
	}
	p, id, ok := strings.Cut(name, "/roles/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", "", false, apierror.InvalidArgument("role name %q must be projects/{project}/roles/{role} or organizations/{organization}/roles/{role}", name)
	}
	parent, project, err = r.roleParentLocked(p)
	return parent, id, project, err
}

// loadRoleLocked reads a role, purging it once its deletion has run its
// course. nil when there is none.
func (r *Registry) loadRoleLocked(parent, id string) (*customRole, error) {
	b, err := r.st.Get(rolePrefix + parent + "/" + id)
	if err != nil {
		return nil, nil
	}
	var c customRole
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, apierror.Internal(err, "decode role %s/roles/%s", parent, id)
	}
	if c.Deleted && !r.now().Before(c.DeleteTime.Add(roleDeletionProcess)) {
		_ = r.st.Delete(rolePrefix + parent + "/" + id)
		return nil, nil
	}
	return &c, nil
}

func (r *Registry) putRoleLocked(parent, id string, c *customRole) error {
	b, err := json.Marshal(c)
	if err != nil {
		return apierror.Internal(err, "encode role")
	}
	if err := r.st.Put(rolePrefix+parent+"/"+id, b); err != nil {
		return apierror.Internal(err, "store role")
	}
	return nil
}

// rolesLocked are parent's roles, sorted by name, purged ones dropped.
func (r *Registry) rolesLocked(parent string) ([]*customRole, error) {
	keys, err := r.st.List(rolePrefix + parent + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list roles")
	}
	var out []*customRole
	for _, k := range keys {
		id := strings.TrimPrefix(k, rolePrefix+parent+"/")
		c, err := r.loadRoleLocked(parent, id)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// validateRole checks a role's title, description, permissions and stage,
// and returns its permissions sorted, without duplicates.
func validateRole(role *adminpb.Role, project bool) ([]string, error) {
	if len(role.GetTitle()) > maxRoleTitle {
		return nil, apierror.InvalidArgument("The role title can be at most %d bytes long; it is %d.", maxRoleTitle, len(role.GetTitle()))
	}
	if len(role.GetDescription()) > maxRoleDescription {
		return nil, apierror.InvalidArgument("The role description can be at most %d bytes long; it is %d.", maxRoleDescription, len(role.GetDescription()))
	}
	if _, ok := adminpb.Role_RoleLaunchStage_name[int32(role.GetStage())]; !ok {
		return nil, apierror.InvalidArgument("stage %d is not EAP, ALPHA, BETA, GA, DEPRECATED or DISABLED", role.GetStage())
	}
	seen := map[string]bool{}
	var perms []string
	size := len(role.GetTitle()) + len(role.GetDescription())
	for _, p := range role.GetIncludedPermissions() {
		if seen[p] {
			continue
		}
		seen[p] = true
		switch iamroles.Level(p) {
		case iamroles.Supported, iamroles.Testing:
		default:
			return nil, apierror.InvalidArgument("Permission %s is not valid.", p)
		}
		if project && !iamroles.ProjectApplicable(p) {
			return nil, apierror.InvalidArgument("Permission %s is not valid.", p)
		}
		perms = append(perms, p)
		size += len(p)
	}
	if len(perms) > maxRolePermissions {
		return nil, apierror.InvalidArgument("A custom role can contain at most %d permissions; this one has %d.", maxRolePermissions, len(perms))
	}
	if size > maxRoleBytes {
		return nil, apierror.InvalidArgument("The role's title, description and permission names total %d bytes; at most %d are allowed.", size, maxRoleBytes)
	}
	sort.Strings(perms)
	return perms, nil
}

// CreateRole creates a custom role under parent.
func (r *Registry) CreateRole(parent, id string, role *adminpb.Role) (*adminpb.Role, error) {
	r.mu.Lock()
	defer r.unlock()
	canon, project, err := r.roleParentLocked(parent)
	if err != nil {
		return nil, err
	}
	if !roleIDPattern.MatchString(id) {
		return nil, apierror.InvalidArgument("The role_id %q is invalid: a role ID is 3 to 64 characters of letters, digits, underscores (_) and periods (.).", id)
	}
	if role == nil {
		role = &adminpb.Role{}
	}
	perms, err := validateRole(role, project)
	if err != nil {
		return nil, err
	}
	existing, err := r.loadRoleLocked(canon, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Deleted {
			return nil, apierror.AlreadyExists("A role named %s in %s was deleted and cannot be created again until its deletion completes, 44 days after it was deleted; undelete it instead.", id, canon)
		}
		return nil, apierror.AlreadyExists("A role named %s in %s already exists.", id, canon)
	}
	all, err := r.rolesLocked(canon)
	if err != nil {
		return nil, err
	}
	if len(all) >= maxRolesPerParent {
		return nil, apierror.FailedPrecondition("%s already has %d custom roles, the most a project or organization can have.", canon, maxRolesPerParent)
	}
	c := &customRole{Name: canon + "/roles/" + id, Title: role.GetTitle(), Description: role.GetDescription(),
		Permissions: perms, Stage: role.GetStage().String(), Etag: newRoleEtag()}
	if err := r.putRoleLocked(canon, id, c); err != nil {
		return nil, err
	}
	return c.proto(true), nil
}

// GetRole reads a custom role, a deleted one included.
func (r *Registry) GetRole(name string) (*adminpb.Role, error) {
	r.mu.Lock()
	defer r.unlock()
	c, err := r.roleLocked(name)
	if err != nil {
		return nil, err
	}
	return c.proto(true), nil
}

func (r *Registry) roleLocked(name string) (*customRole, error) {
	parent, id, _, err := r.splitRoleNameLocked(name)
	if err != nil {
		return nil, err
	}
	c, err := r.loadRoleLocked(parent, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, apierror.NotFound("The role named %s was not found.", name)
	}
	return c, nil
}

// ListRoles lists parent's custom roles.
func (r *Registry) ListRoles(req *adminpb.ListRolesRequest) (*adminpb.ListRolesResponse, error) {
	if req.GetParent() == "" || strings.HasPrefix(req.GetParent(), "roles") {
		return nil, apierror.Unimplemented("listing predefined roles is not served: give a parent, projects/{project} or organizations/{organization}")
	}
	if req.GetPageSize() < 0 || req.GetPageSize() > 1000 {
		return nil, apierror.InvalidArgument("page_size %d must be between 0 and 1000", req.GetPageSize())
	}
	r.mu.Lock()
	defer r.unlock()
	canon, _, err := r.roleParentLocked(req.GetParent())
	if err != nil {
		return nil, err
	}
	all, err := r.rolesLocked(canon)
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = 300
	}
	out := &adminpb.ListRolesResponse{}
	for _, c := range all {
		if c.Deleted && !req.GetShowDeleted() {
			continue
		}
		if req.GetPageToken() != "" && c.Name <= req.GetPageToken() {
			continue
		}
		if len(out.Roles) == size {
			out.NextPageToken = out.Roles[len(out.Roles)-1].GetName()
			break
		}
		out.Roles = append(out.Roles, c.proto(req.GetView() == adminpb.RoleView_FULL))
	}
	return out, nil
}

// UpdateRole changes the fields of a custom role that mask names (every
// one of title, description, included_permissions and stage without one).
func (r *Registry) UpdateRole(name string, role *adminpb.Role, mask []string) (*adminpb.Role, error) {
	r.mu.Lock()
	defer r.unlock()
	parent, id, project, err := r.splitRoleNameLocked(name)
	if err != nil {
		return nil, err
	}
	c, err := r.roleLocked(name)
	if err != nil {
		return nil, err
	}
	if role == nil {
		role = &adminpb.Role{}
	}
	if c.Deleted {
		return nil, apierror.FailedPrecondition("The role %s is deleted; undelete it before updating it.", c.Name)
	}
	if e := role.GetEtag(); len(e) > 0 && string(e) != string(c.Etag) {
		return nil, apierror.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
	}
	fields := map[string]bool{}
	if len(mask) == 0 {
		mask = []string{"title", "description", "included_permissions", "stage"}
	}
	for _, p := range mask {
		switch p {
		case "title", "description", "stage":
			fields[p] = true
		case "included_permissions", "includedPermissions":
			fields["included_permissions"] = true
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not title, description, included_permissions or stage", p)
		}
	}
	next := c.proto(true)
	if fields["title"] {
		next.Title = role.GetTitle()
	}
	if fields["description"] {
		next.Description = role.GetDescription()
	}
	if fields["included_permissions"] {
		next.IncludedPermissions = role.GetIncludedPermissions()
	}
	if fields["stage"] {
		next.Stage = role.GetStage()
	}
	perms, err := validateRole(next, project)
	if err != nil {
		return nil, err
	}
	c.Title, c.Description, c.Permissions, c.Stage = next.Title, next.Description, perms, next.Stage.String()
	c.Etag = newRoleEtag()
	if err := r.putRoleLocked(parent, id, c); err != nil {
		return nil, err
	}
	return c.proto(true), nil
}

// DeleteRole marks a custom role deleted.
func (r *Registry) DeleteRole(name string, etag []byte) (*adminpb.Role, error) {
	return r.setRoleDeleted(name, etag, true)
}

// UndeleteRole restores a custom role deleted within the last 7 days.
func (r *Registry) UndeleteRole(name string, etag []byte) (*adminpb.Role, error) {
	return r.setRoleDeleted(name, etag, false)
}

func (r *Registry) setRoleDeleted(name string, etag []byte, deleted bool) (*adminpb.Role, error) {
	r.mu.Lock()
	defer r.unlock()
	parent, id, _, err := r.splitRoleNameLocked(name)
	if err != nil {
		return nil, err
	}
	c, err := r.roleLocked(name)
	if err != nil {
		return nil, err
	}
	if len(etag) > 0 && string(etag) != string(c.Etag) {
		return nil, apierror.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
	}
	switch {
	case deleted && c.Deleted:
		return nil, apierror.FailedPrecondition("The role %s is already deleted.", c.Name)
	case !deleted && !c.Deleted:
		return nil, apierror.FailedPrecondition("The role %s is not deleted.", c.Name)
	case !deleted && !r.now().Before(c.DeleteTime.Add(roleUndeletePeriod)):
		return nil, apierror.FailedPrecondition("The role %s was deleted more than 7 days ago and can no longer be undeleted.", c.Name)
	}
	c.Deleted = deleted
	c.DeleteTime = time.Time{}
	if deleted {
		c.DeleteTime = r.now()
	}
	c.Etag = newRoleEtag()
	if err := r.putRoleLocked(parent, id, c); err != nil {
		return nil, err
	}
	return c.proto(true), nil
}

// RolePermissions are the permissions a custom role grants: none when it
// is deleted, DISABLED or absent. ok is whether role names a custom role
// at all (projects/*/roles/* or organizations/*/roles/*).
func (r *Registry) RolePermissions(role string) (perms []string, ok bool) {
	if !IsCustomRole(role) {
		return nil, false
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.roleLocked(role)
	if err != nil || c.Deleted || c.Stage == adminpb.Role_DISABLED.String() {
		return nil, true
	}
	return append([]string(nil), c.Permissions...), true
}

// IsCustomRole reports whether role names a custom role.
func IsCustomRole(role string) bool {
	return (strings.HasPrefix(role, "projects/") || strings.HasPrefix(role, "organizations/")) && strings.Contains(role, "/roles/")
}

// checkCustomRoleBindingsLocked refuses a policy for resource that binds a
// custom role anew which does not exist, is deleted, or is not of
// resource's project or the organization: Google documents that a custom
// role is grantable only within the project or organization it was
// created in, and that a deleted role cannot be used in new bindings. A
// binding the current policy already has is kept as it is. The caller
// holds r.mu.
func (r *Registry) checkCustomRoleBindingsLocked(resource string, current, next []string) error {
	had := map[string]bool{}
	for _, role := range current {
		had[role] = true
	}
	for _, role := range next {
		if !IsCustomRole(role) || had[role] {
			continue
		}
		refuse := apierror.InvalidArgument("Role (%s) does not exist in the resource's hierarchy.", role)
		c, err := r.roleLocked(role)
		if err != nil || c.Deleted {
			return refuse
		}
		parent := c.Name[:strings.Index(c.Name, "/roles/")]
		switch {
		case strings.HasPrefix(parent, "projects/"):
			if resource != parent {
				return refuse
			}
		case strings.HasPrefix(resource, "projects/"):
			p, err := r.load(strings.TrimPrefix(resource, "projects/"))
			if err != nil || p.Parent == "" {
				return refuse
			}
		}
	}
	return nil
}

// dropRolesLocked deletes parent's custom roles, when its project is
// purged. The caller holds r.mu.
func (r *Registry) dropRolesLocked(parent string) error {
	return r.deletePrefixLocked(rolePrefix + parent + "/")
}

// QueryTestablePermissions lists the permissions a custom role on the
// project or organization a full resource name names may hold.
func (r *Registry) QueryTestablePermissions(req *adminpb.QueryTestablePermissionsRequest) (*adminpb.QueryTestablePermissionsResponse, error) {
	if req.GetFullResourceName() == "" {
		return nil, apierror.InvalidArgument("full_resource_name is required, such as //cloudresourcemanager.googleapis.com/projects/PROJECT")
	}
	// gcloud names the resource by the host its cloudresourcemanager
	// override gives (//cloudresourcemanager.localhost:PORT/projects/P), so
	// any host is taken: the path says what the resource is.
	_, rest, ok := strings.Cut(strings.TrimPrefix(req.GetFullResourceName(), "//"), "/")
	if !ok || !strings.HasPrefix(req.GetFullResourceName(), "//") || (!strings.HasPrefix(rest, "projects/") && !strings.HasPrefix(rest, "organizations/")) {
		return nil, apierror.Unimplemented("full_resource_name %q: only a project's or the organization's (//cloudresourcemanager.googleapis.com/projects/P, .../organizations/O) are served", req.GetFullResourceName())
	}
	if req.GetPageSize() < 0 || req.GetPageSize() > 1000 {
		return nil, apierror.InvalidArgument("page_size %d must be between 0 and 1000", req.GetPageSize())
	}
	r.mu.Lock()
	_, project, err := r.roleParentLocked(rest)
	r.unlock()
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = 100
	}
	out := &adminpb.QueryTestablePermissionsResponse{}
	for _, p := range iamroles.Names() {
		if req.GetPageToken() != "" && p <= req.GetPageToken() {
			continue
		}
		if project && !iamroles.ProjectApplicable(p) {
			continue
		}
		if len(out.Permissions) == size {
			out.NextPageToken = out.Permissions[len(out.Permissions)-1].GetName()
			break
		}
		perm := &adminpb.Permission{Name: p}
		switch iamroles.Level(p) {
		case iamroles.Testing:
			perm.CustomRolesSupportLevel = adminpb.Permission_TESTING
		case iamroles.NotSupported:
			perm.CustomRolesSupportLevel = adminpb.Permission_NOT_SUPPORTED
		}
		out.Permissions = append(out.Permissions, perm)
	}
	return out, nil
}

// IAMAdminServer serves google.iam.admin.v1.IAM's role and permission
// methods; the rest (service accounts, keys, grantable roles, lint) are
// UNIMPLEMENTED.
type IAMAdminServer struct {
	adminpb.UnimplementedIAMServer
	reg *Registry
}

// NewIAMAdminServer returns the IAM admin service over reg.
func NewIAMAdminServer(reg *Registry) *IAMAdminServer { return &IAMAdminServer{reg: reg} }

func (s *IAMAdminServer) CreateRole(_ context.Context, req *adminpb.CreateRoleRequest) (*adminpb.Role, error) {
	return s.reg.CreateRole(req.GetParent(), req.GetRoleId(), req.GetRole())
}

func (s *IAMAdminServer) GetRole(_ context.Context, req *adminpb.GetRoleRequest) (*adminpb.Role, error) {
	return s.reg.GetRole(req.GetName())
}

func (s *IAMAdminServer) ListRoles(_ context.Context, req *adminpb.ListRolesRequest) (*adminpb.ListRolesResponse, error) {
	return s.reg.ListRoles(req)
}

func (s *IAMAdminServer) UpdateRole(_ context.Context, req *adminpb.UpdateRoleRequest) (*adminpb.Role, error) {
	return s.reg.UpdateRole(req.GetName(), req.GetRole(), req.GetUpdateMask().GetPaths())
}

func (s *IAMAdminServer) DeleteRole(_ context.Context, req *adminpb.DeleteRoleRequest) (*adminpb.Role, error) {
	return s.reg.DeleteRole(req.GetName(), req.GetEtag())
}

func (s *IAMAdminServer) UndeleteRole(_ context.Context, req *adminpb.UndeleteRoleRequest) (*adminpb.Role, error) {
	return s.reg.UndeleteRole(req.GetName(), req.GetEtag())
}

func (s *IAMAdminServer) QueryTestablePermissions(_ context.Context, req *adminpb.QueryTestablePermissionsRequest) (*adminpb.QueryTestablePermissionsResponse, error) {
	return s.reg.QueryTestablePermissions(req)
}

// bindingRoles are the roles a policy binds.
func bindingRoles(p *iampolicy.Stored) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Bindings))
	for _, b := range p.Bindings {
		out = append(out, b.Role)
	}
	return out
}

// iamAdminPath reports whether a JSON request is the IAM API's
// (iam.googleapis.com v1), served here beside Resource Manager's v1: its
// roles, permissions and service accounts, the last answered UNIMPLEMENTED.
func isIAMAdminPath(path string) bool {
	if strings.HasPrefix(path, "/v1/roles") || strings.HasPrefix(path, "/v1/permissions:") || strings.HasPrefix(path, "/v1/iamPolicies:") {
		return true
	}
	for _, c := range []string{"/v1/projects/", "/v1/organizations/"} {
		rest, ok := strings.CutPrefix(path, c)
		if !ok {
			continue
		}
		_, sub, _ := strings.Cut(rest, "/")
		if strings.HasPrefix(sub, "roles") || strings.HasPrefix(sub, "serviceAccounts") {
			return true
		}
	}
	return false
}

// iamAdminDesc describes the IAM methods served, for the JSON transcoder:
// adminpb's generated descriptor is unexported and its registrar takes a
// *grpc.Server only. A method not named here answers 501 over JSON.
var iamAdminDesc = &grpc.ServiceDesc{
	ServiceName: "google.iam.admin.v1.IAM",
	HandlerType: (*adminpb.IAMServer)(nil),
	Methods: []grpc.MethodDesc{
		unaryMethod("CreateRole", (*IAMAdminServer).CreateRole),
		unaryMethod("GetRole", (*IAMAdminServer).GetRole),
		unaryMethod("ListRoles", (*IAMAdminServer).ListRoles),
		unaryMethod("UpdateRole", (*IAMAdminServer).UpdateRole),
		unaryMethod("DeleteRole", (*IAMAdminServer).DeleteRole),
		unaryMethod("UndeleteRole", (*IAMAdminServer).UndeleteRole),
		unaryMethod("QueryTestablePermissions", (*IAMAdminServer).QueryTestablePermissions),
	},
}

func unaryMethod[Req any, PReq interface {
	*Req
	proto.Message
}, Resp proto.Message](name string, fn func(*IAMAdminServer, context.Context, PReq) (Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := PReq(new(Req))
		if err := dec(in); err != nil {
			return nil, err
		}
		call := func(ctx context.Context, req any) (any, error) { return fn(srv.(*IAMAdminServer), ctx, req.(PReq)) }
		if interceptor == nil {
			return call(ctx, in)
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/google.iam.admin.v1.IAM/" + name}, call)
	}}
}
