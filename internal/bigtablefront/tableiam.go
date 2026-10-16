package bigtablefront

import (
	"context"
	"regexp"
	"strings"
	"sync"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Table IAM (#1486): BigtableTableAdmin's GetIamPolicy, SetIamPolicy and
// TestIamPermissions on tables, stored and never enforced, as ADR-0006
// decides for every service that serves them. The emulator has none of
// them; the front answers them through the shared internal/iampolicy (a set
// returns a new etag, a stale one is ABORTED, conditions and audit configs
// are UNIMPLEMENTED naming the field). The table must exist in the emulator,
// and its policy goes with it: DeleteTable through the front, and an
// instance's deletion, drop it, so a table made again starts with none.
// Policies are kept in memory, as the emulator keeps its tables.
//
// Authorized views and backups, which the emulator does not keep, answer
// the three methods UNIMPLEMENTED with the emulator's own reason.

var (
	tableNameRe      = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/tables/[^/]+$`)
	authorizedViewRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/tables/[^/]+/authorizedViews/[^/]+$`)
	backupNameRe     = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/clusters/[^/]+/backups/[^/]+$`)
)

// tableIAM is the table policies.
type tableIAM struct {
	upstream *grpc.ClientConn
	mu       sync.Mutex
	policies map[string]*iampolicy.Stored // by table name
}

func (t *tableIAM) methods() map[string]method {
	return map[string]method{
		admin + "GetIamPolicy":       unary(func() *iampb.GetIamPolicyRequest { return &iampb.GetIamPolicyRequest{} }, t.get),
		admin + "SetIamPolicy":       unary(func() *iampb.SetIamPolicyRequest { return &iampb.SetIamPolicyRequest{} }, t.set),
		admin + "TestIamPermissions": unary(func() *iampb.TestIamPermissionsRequest { return &iampb.TestIamPermissionsRequest{} }, t.test),
		admin + "DeleteTable":        unary(func() *btapb.DeleteTableRequest { return &btapb.DeleteTableRequest{} }, t.deleteTable),
	}
}

// target checks that resource is a table the emulator has.
func (t *tableIAM) target(ctx context.Context, resource string) error {
	switch {
	case tableNameRe.MatchString(resource):
		_, err := btapb.NewBigtableTableAdminClient(t.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: resource, View: btapb.Table_NAME_ONLY})
		return err
	case authorizedViewRe.MatchString(resource):
		return status.Error(codes.Unimplemented, "the emulator does not currently support authorized views")
	case backupNameRe.MatchString(resource):
		return status.Error(codes.Unimplemented, backups)
	}
	return status.Errorf(codes.InvalidArgument, "invalid resource %q: want projects/{project}/instances/{instance}/tables/{table}", resource)
}

func (t *tableIAM) get(ctx context.Context, req *iampb.GetIamPolicyRequest) (proto.Message, error) {
	if err := t.target(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p, err := iampolicy.Get(t.policies[req.GetResource()], req.GetOptions())
	return p, apierror.Wrap(err)
}

func (t *tableIAM) set(ctx context.Context, req *iampb.SetIamPolicyRequest) (proto.Message, error) {
	if err := t.target(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next, err := iampolicy.Set(t.policies[req.GetResource()], req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	t.policies[req.GetResource()] = &next
	return next.Proto(), nil
}

func (t *tableIAM) test(ctx context.Context, req *iampb.TestIamPermissionsRequest) (proto.Message, error) {
	if err := t.target(ctx, req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// deleteTable forwards DeleteTable to the emulator and drops the table's
// policy.
func (t *tableIAM) deleteTable(ctx context.Context, req *btapb.DeleteTableRequest) (proto.Message, error) {
	resp, err := btapb.NewBigtableTableAdminClient(t.upstream).DeleteTable(ctx, req)
	if err != nil {
		return nil, err
	}
	t.drop(req.GetName())
	return resp, nil
}

// drop forgets the policy of resource and of every table under it.
func (t *tableIAM) drop(resource string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.policies {
		if name == resource || strings.HasPrefix(name, resource+"/") {
			delete(t.policies, name)
		}
	}
}
