package spannerfront

import (
	"context"
	"regexp"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Database drop protection (#1461). The emulator refuses UpdateDatabase,
// whose one updatable field an emulator database can carry is
// enable_drop_protection: the front keeps it, lays it over GetDatabase and
// ListDatabases, and enforces it as Google does, refusing DropDatabase of a
// protected database and DeleteInstance of an instance holding one
// FAILED_PRECONDITION.

// databaseClaimed are the DatabaseAdmin methods the front answers.
var databaseClaimed = map[string]bool{"GetDatabase": true, "ListDatabases": true, "UpdateDatabase": true, "DropDatabase": true,
	"CreateDatabase": true, "UpdateDatabaseDdl": true, "GetDatabaseDdl": true, "ListDatabaseRoles": true, "ListDatabaseOperations": true,
	"CreateBackup": true, "CopyBackup": true, "GetBackup": true, "UpdateBackup": true, "DeleteBackup": true, "ListBackups": true,
	"ListBackupOperations": true, "RestoreDatabase": true,
	"AddSplitPoints":       true,
	"CreateBackupSchedule": true, "GetBackupSchedule": true, "UpdateBackupSchedule": true, "DeleteBackupSchedule": true, "ListBackupSchedules": true}

var databaseNameRe = regexp.MustCompile(`^(projects/[^/]+/instances/[^/]+)/databases/[^/]+$`)

// databases is the DatabaseAdmin half of the front; it shares Instances'
// state.
type databases struct {
	databasepb.UnimplementedDatabaseAdminServer
	s *Instances
}

func (d databases) GetDatabase(ctx context.Context, req *databasepb.GetDatabaseRequest) (*databasepb.Database, error) {
	db, err := d.s.emuDB.GetDatabase(ctx, req)
	if err != nil {
		return nil, err
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	db.EnableDropProtection = d.s.protected[db.GetName()]
	d.s.overlayRestore(db)
	return db, nil
}

func (d databases) ListDatabases(ctx context.Context, req *databasepb.ListDatabasesRequest) (*databasepb.ListDatabasesResponse, error) {
	resp, err := d.s.emuDB.ListDatabases(ctx, req)
	if err != nil {
		return nil, err
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	for _, db := range resp.Databases {
		db.EnableDropProtection = d.s.protected[db.GetName()]
		d.s.overlayRestore(db)
	}
	return resp, nil
}

func (d databases) DropDatabase(ctx context.Context, req *databasepb.DropDatabaseRequest) (*emptypb.Empty, error) {
	d.s.mu.Lock()
	on := d.s.protected[req.GetDatabase()]
	d.s.mu.Unlock()
	if on {
		return nil, status.Errorf(codes.FailedPrecondition, "Database %s has drop protection enabled: disable it with UpdateDatabase (enable_drop_protection) before dropping it.", req.GetDatabase())
	}
	resp, err := d.s.emuDB.DropDatabase(ctx, req)
	if err != nil {
		return nil, err
	}
	d.s.mu.Lock()
	delete(d.s.protected, req.GetDatabase())
	delete(d.s.restored, req.GetDatabase())
	d.s.dropSplitPoints(req.GetDatabase())
	d.s.mu.Unlock()
	d.s.dropPolicies(req.GetDatabase())
	d.s.dropRoles(req.GetDatabase())
	d.s.dropSchedules(req.GetDatabase())
	return resp, nil
}

// UpdateDatabase sets or clears a database's drop protection, the one field
// it changes, and returns the done operation.
func (d databases) UpdateDatabase(ctx context.Context, req *databasepb.UpdateDatabaseRequest) (*longrunningpb.Operation, error) {
	in := req.GetDatabase()
	if in == nil || !databaseNameRe.MatchString(in.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateDatabase request: database.name %q is not projects/{project}/instances/{instance}/databases/{database}.", in.GetName())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Invalid UpdateDatabase request: update_mask must name at least one field.")
	}
	// The metadata carries the request, whose mask is written in JSON as
	// lowerCamel from proto names: a REST mask's camelCase path is made one.
	req = proto.Clone(req).(*databasepb.UpdateDatabaseRequest)
	for i, p := range paths {
		req.UpdateMask.Paths[i] = snake(p)
		if snake(p) != "enable_drop_protection" {
			return nil, status.Errorf(codes.Unimplemented, "UpdateDatabase of %s is not served here: the emulator carries a database's drop protection alone (no encryption or other settings)", p)
		}
	}
	cur, err := d.s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: in.GetName()})
	if err != nil {
		return nil, err
	}
	start := d.s.now()
	d.s.mu.Lock()
	if in.GetEnableDropProtection() {
		d.s.protected[in.GetName()] = true
	} else {
		delete(d.s.protected, in.GetName())
	}
	cur.EnableDropProtection = in.GetEnableDropProtection()
	d.s.mu.Unlock()
	meta, err := anypb.New(&databasepb.UpdateDatabaseMetadata{Request: req,
		Progress: &databasepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(d.s.now())}})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	resp, err := anypb.New(cur)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	return d.s.record(&longrunningpb.Operation{Name: in.GetName() + "/operations/" + opID(), Metadata: meta, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}), nil
}

// protectedIn is a protected database of the instance named instance, or
// "". s.mu is held.
func (s *Instances) protectedIn(instance string) string {
	for name := range s.protected {
		if strings.HasPrefix(name, instance+"/databases/") {
			return name
		}
	}
	return ""
}

// record keeps a done operation for GetOperation and returns a copy.
func (s *Instances) record(op *longrunningpb.Operation) *longrunningpb.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[op.Name] = op
	s.opOrder = append(s.opOrder, op.Name)
	// Keep the last 1000, so a long-lived instance does not grow forever.
	if len(s.opOrder) > 1000 {
		delete(s.ops, s.opOrder[0])
		s.opOrder = s.opOrder[1:]
	}
	return proto.Clone(op).(*longrunningpb.Operation)
}

// overlayRestore sets the restore information of a database restored from
// a backup. s.mu is held.
func (s *Instances) overlayRestore(db *databasepb.Database) {
	if info, ok := s.restored[db.GetName()]; ok {
		db.RestoreInfo = proto.Clone(info).(*databasepb.RestoreInfo)
	}
}
