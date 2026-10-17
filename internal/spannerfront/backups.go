package spannerfront

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Backups (#1498). The emulator keeps no backups, so the front takes them
// itself: a backup is the database's schema (GetDatabaseDdl, with the roles
// and grants the emulator accepts and forgets, see roles.go) and every row
// of every table, read through the emulator in one read-only transaction at
// the backup's version time, and kept in memory as the emulator keeps its
// databases. A restore creates the database in the emulator with that
// schema and inserts the rows, so the restored database is the source as it
// was at the version time.
//
// Generated columns are not copied: the restored database computes them.
// A sequence restarts in the restored database, as the emulator keeps no
// sequence state to copy. Customer-managed encryption is refused: there is
// no Cloud KMS key an emulator can use.

const (
	maxBackupLife = 366 * 24 * time.Hour
	minBackupLife = 6 * time.Hour
	// versionRetention is Google's default version_retention_period: how
	// far back a backup's version_time may be.
	versionRetention = time.Hour
	// commitCells bounds the cells one restore commit inserts, under
	// Spanner's 80,000 mutations a commit.
	commitCells = 20000
)

var (
	backupIDRe   = regexp.MustCompile(`^[a-z][a-z0-9_\-]{0,58}[a-z0-9]$`)
	databaseIDRe = regexp.MustCompile(`^[a-z][a-z0-9_\-]{0,28}[a-z0-9]$`)
	backupNameRe = regexp.MustCompile(`^(projects/[^/]+/instances/[^/]+)/backups/([^/]+)$`)
	createTable  = regexp.MustCompile("(?is)^\\s*CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?([`\"]?[\\w.]+[`\"]?)")
)

type backup struct {
	pb   *databasepb.Backup
	snap *snapshot
}

type snapshot struct {
	dialect databasepb.DatabaseDialect
	ddl     []string
	tables  []tableRows
	size    int64
}

type tableRows struct {
	name    string // as a mutation names it: schema.table outside the default schema
	columns []string
	rows    []*structpb.ListValue
}

// expireBackups drops the backups past their expire time. s.mu is held.
func (s *Instances) expireBackups() {
	now := s.now()
	for name, b := range s.backups {
		if !now.Before(b.pb.GetExpireTime().AsTime()) {
			delete(s.backups, name)
		}
	}
}

func (s *Instances) backupCopy(name string) (*databasepb.Backup, *snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireBackups()
	b, ok := s.backups[name]
	if !ok {
		return nil, nil, false
	}
	return proto.Clone(b.pb).(*databasepb.Backup), b.snap, true
}

func backupNotFound(name string) error {
	return status.Errorf(codes.NotFound, "Backup not found: %s", name)
}

// checkExpire checks an expire time against Google's bounds, from now and
// under max.
func (s *Instances) checkExpire(method string, t *timestamppb.Timestamp, maxExpire time.Time) error {
	if t == nil {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: expire_time is required.", method)
	}
	e := t.AsTime()
	if e.Before(s.now().Add(minBackupLife - time.Minute)) {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: expire_time must be at least 6 hours from now.", method)
	}
	if e.After(maxExpire) {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: expire_time must be at most 366 days after the backup's version time (%s).", method, maxExpire.Add(-maxBackupLife).Format(time.RFC3339))
	}
	return nil
}

// refuseCMEK refuses customer-managed encryption, which encryption names
// (an encryption type) or key sets.
func refuseCMEK(method string, encryption fmt.Stringer, key string) error {
	if strings.Contains(encryption.String(), "CUSTOMER_MANAGED") || key != "" {
		return status.Errorf(codes.Unimplemented, "%s with customer-managed encryption is not served here: the emulator has no Cloud KMS key to encrypt with", method)
	}
	return nil
}

func (d databases) CreateBackup(ctx context.Context, req *databasepb.CreateBackupRequest) (*longrunningpb.Operation, error) {
	s := d.s
	if !instanceNameRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CreateBackup request: parent %q is not projects/{project}/instances/{instance}.", req.GetParent())
	}
	if !backupIDRe.MatchString(req.GetBackupId()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CreateBackup request: backup_id %q must match [a-z][a-z0-9_\\-]*[a-z0-9] and be 2 to 60 characters long.", req.GetBackupId())
	}
	in := req.GetBackup()
	if in == nil || in.GetDatabase() == "" {
		return nil, status.Error(codes.InvalidArgument, "Invalid CreateBackup request: backup.database is required.")
	}
	m := databaseNameRe.FindStringSubmatch(in.GetDatabase())
	if m == nil || m[1] != req.GetParent() {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CreateBackup request: backup.database %q must be a database of %s.", in.GetDatabase(), req.GetParent())
	}
	if c := req.GetEncryptionConfig(); c != nil {
		if err := refuseCMEK("CreateBackup", c.GetEncryptionType(), c.GetKmsKeyName()); err != nil {
			return nil, err
		}
	}
	return s.takeBackup(ctx, req.GetParent()+"/backups/"+req.GetBackupId(), in.GetDatabase(), in.GetVersionTime(), in.GetExpireTime(), nil)
}

// takeBackup snapshots database into a new backup named name, and returns
// the done CreateBackup operation.
func (s *Instances) takeBackup(ctx context.Context, name, database string, version, expire *timestamppb.Timestamp, schedules []string) (*longrunningpb.Operation, error) {
	start := s.now()
	if version != nil {
		v := version.AsTime()
		if v.After(start) {
			return nil, status.Error(codes.InvalidArgument, "Invalid CreateBackup request: version_time must not be in the future.")
		}
		if v.Before(start.Add(-versionRetention)) {
			return nil, status.Error(codes.InvalidArgument, "Invalid CreateBackup request: version_time must be within the database's version retention period (1h).")
		}
	}
	vt := start
	if version != nil {
		vt = version.AsTime()
	}
	if err := s.checkExpire("CreateBackup", expire, vt.Add(maxBackupLife)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.expireBackups()
	_, exists := s.backups[name]
	s.mu.Unlock()
	if exists {
		return nil, status.Errorf(codes.AlreadyExists, "Backup already exists: %s", name)
	}
	snap, read, err := s.snapshot(ctx, database, version)
	if err != nil {
		return nil, err
	}
	now := s.now()
	b := &databasepb.Backup{
		Name: name, Database: database, VersionTime: read, ExpireTime: expire,
		CreateTime: timestamppb.New(now), SizeBytes: snap.size, ExclusiveSizeBytes: snap.size,
		State: databasepb.Backup_READY, DatabaseDialect: snap.dialect,
		EncryptionInfo:  &databasepb.EncryptionInfo{EncryptionType: databasepb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION},
		MaxExpireTime:   timestamppb.New(read.AsTime().Add(maxBackupLife)),
		BackupSchedules: schedules, OldestVersionTime: read,
	}
	s.mu.Lock()
	if _, exists := s.backups[name]; exists {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "Backup already exists: %s", name)
	}
	s.backups[name] = &backup{pb: b, snap: snap}
	s.mu.Unlock()
	meta := &databasepb.CreateBackupMetadata{Name: name, Database: database,
		Progress: &databasepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(name, meta, b)
}

// doneOp records a done operation under parent, with meta and resp.
func (s *Instances) doneOp(parent string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	m, err := anypb.New(meta)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	return s.record(&longrunningpb.Operation{Name: parent + "/operations/" + opID(), Metadata: m, Done: true,
		Result: &longrunningpb.Operation_Response{Response: r}}), nil
}

func (d databases) CopyBackup(ctx context.Context, req *databasepb.CopyBackupRequest) (*longrunningpb.Operation, error) {
	s := d.s
	if !instanceNameRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CopyBackup request: parent %q is not projects/{project}/instances/{instance}.", req.GetParent())
	}
	if !backupIDRe.MatchString(req.GetBackupId()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CopyBackup request: backup_id %q must match [a-z][a-z0-9_\\-]*[a-z0-9] and be 2 to 60 characters long.", req.GetBackupId())
	}
	if c := req.GetEncryptionConfig(); c != nil {
		if err := refuseCMEK("CopyBackup", c.GetEncryptionType(), c.GetKmsKeyName()); err != nil {
			return nil, err
		}
	}
	src, snap, ok := s.backupCopy(req.GetSourceBackup())
	if !ok {
		return nil, backupNotFound(req.GetSourceBackup())
	}
	if _, err := s.emu.GetInstance(ctx, instanceReq(req.GetParent())); err != nil {
		return nil, err
	}
	if err := s.checkExpire("CopyBackup", req.GetExpireTime(), src.GetMaxExpireTime().AsTime()); err != nil {
		return nil, err
	}
	start := s.now()
	name := req.GetParent() + "/backups/" + req.GetBackupId()
	b := proto.Clone(src).(*databasepb.Backup)
	b.Name, b.ExpireTime, b.CreateTime, b.BackupSchedules = name, req.GetExpireTime(), timestamppb.New(start), nil
	b.ReferencingDatabases, b.ReferencingBackups = nil, nil
	s.mu.Lock()
	if _, exists := s.backups[name]; exists {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "Backup already exists: %s", name)
	}
	s.backups[name] = &backup{pb: b, snap: snap}
	s.mu.Unlock()
	meta := &databasepb.CopyBackupMetadata{Name: name, SourceBackup: req.GetSourceBackup(),
		Progress: &databasepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(name, meta, b)
}

func (d databases) GetBackup(_ context.Context, req *databasepb.GetBackupRequest) (*databasepb.Backup, error) {
	b, _, ok := d.s.backupCopy(req.GetName())
	if !ok {
		return nil, backupNotFound(req.GetName())
	}
	return b, nil
}

func (d databases) UpdateBackup(_ context.Context, req *databasepb.UpdateBackupRequest) (*databasepb.Backup, error) {
	s := d.s
	in := req.GetBackup()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Invalid UpdateBackup request: update_mask must name expire_time.")
	}
	for _, p := range paths {
		if snake(p) != "expire_time" {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateBackup request: field_mask path %q cannot be updated: expire_time is the one field UpdateBackup changes.", p)
		}
	}
	cur, _, ok := s.backupCopy(in.GetName())
	if !ok {
		return nil, backupNotFound(in.GetName())
	}
	if err := s.checkExpire("UpdateBackup", in.GetExpireTime(), cur.GetMaxExpireTime().AsTime()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.backups[in.GetName()]
	if !ok {
		return nil, backupNotFound(in.GetName())
	}
	b.pb.ExpireTime = in.GetExpireTime()
	return proto.Clone(b.pb).(*databasepb.Backup), nil
}

func (d databases) DeleteBackup(_ context.Context, req *databasepb.DeleteBackupRequest) (*emptypb.Empty, error) {
	s := d.s
	s.mu.Lock()
	s.expireBackups()
	_, ok := s.backups[req.GetName()]
	delete(s.backups, req.GetName())
	s.mu.Unlock()
	if !ok {
		return nil, backupNotFound(req.GetName())
	}
	s.dropPolicies(req.GetName())
	return &emptypb.Empty{}, nil
}

func (d databases) ListBackups(ctx context.Context, req *databasepb.ListBackupsRequest) (*databasepb.ListBackupsResponse, error) {
	s := d.s
	if _, err := s.emu.GetInstance(ctx, instanceReq(req.GetParent())); err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.expireBackups()
	byName := map[string]*databasepb.Backup{}
	for name, b := range s.backups {
		if strings.HasPrefix(name, req.GetParent()+"/backups/") && f.match(b.pb) {
			byName[name] = proto.Clone(b.pb).(*databasepb.Backup)
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("backups:"+req.GetParent()+":"+req.GetFilter(), byName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &databasepb.ListBackupsResponse{NextPageToken: next}
	for _, n := range page {
		resp.Backups = append(resp.Backups, byName[n])
	}
	return resp, nil
}

// pageOf pages the keys of byName.
func pageOf[T any](scope string, byName map[string]T, token string, size int32) ([]string, string, error) {
	keys := make([]string, 0, len(byName))
	for k := range byName {
		keys = append(keys, k)
	}
	page, next, err := paging.Page(scope, keys, token, int(size))
	if err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "Invalid page_token %q.", token)
	}
	return page, next, nil
}

// listOps lists the operations under parent+infix whose filter matches:
// the front's own and the emulator's, which it tracks by name.
func (s *Instances) listOps(ctx context.Context, scope, prefix, filter, token string, size int32) ([]*longrunningpb.Operation, string, error) {
	f, err := parseFilter(filter)
	if err != nil {
		return nil, "", err
	}
	byName := map[string]*longrunningpb.Operation{}
	s.mu.Lock()
	for name, op := range s.ops {
		if strings.HasPrefix(name, prefix) {
			byName[name] = proto.Clone(op).(*longrunningpb.Operation)
		}
	}
	var emu []string
	for _, name := range s.emuOpNames {
		if strings.HasPrefix(name, prefix) {
			emu = append(emu, name)
		}
	}
	s.mu.Unlock()
	for _, name := range emu {
		op, err := s.emuOps.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: name})
		if err != nil {
			continue // its database was dropped
		}
		byName[name] = op
	}
	for name, op := range byName {
		if !f.match(op) {
			delete(byName, name)
		}
	}
	page, next, err := pageOf(scope+":"+filter, byName, token, size)
	if err != nil {
		return nil, "", err
	}
	out := make([]*longrunningpb.Operation, 0, len(page))
	for _, n := range page {
		out = append(out, byName[n])
	}
	return out, next, nil
}

// opPrefix is the name prefix of the operations on parent's collection,
// as a filter's "name" would see it.
func opPrefix(parent, collection string) string { return parent + "/" + collection + "/" }

func (d databases) ListBackupOperations(ctx context.Context, req *databasepb.ListBackupOperationsRequest) (*databasepb.ListBackupOperationsResponse, error) {
	if _, err := d.s.emu.GetInstance(ctx, instanceReq(req.GetParent())); err != nil {
		return nil, err
	}
	ops, next, err := d.s.listOps(ctx, "backupOps:"+req.GetParent(), opPrefix(req.GetParent(), "backups"), req.GetFilter(), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &databasepb.ListBackupOperationsResponse{Operations: ops, NextPageToken: next}, nil
}

func (d databases) ListDatabaseOperations(ctx context.Context, req *databasepb.ListDatabaseOperationsRequest) (*databasepb.ListDatabaseOperationsResponse, error) {
	if _, err := d.s.emu.GetInstance(ctx, instanceReq(req.GetParent())); err != nil {
		return nil, err
	}
	ops, next, err := d.s.listOps(ctx, "databaseOps:"+req.GetParent(), opPrefix(req.GetParent(), "databases"), req.GetFilter(), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &databasepb.ListDatabaseOperationsResponse{Operations: ops, NextPageToken: next}, nil
}

func (d databases) RestoreDatabase(ctx context.Context, req *databasepb.RestoreDatabaseRequest) (*longrunningpb.Operation, error) {
	s := d.s
	if !instanceNameRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid RestoreDatabase request: parent %q is not projects/{project}/instances/{instance}.", req.GetParent())
	}
	if !databaseIDRe.MatchString(req.GetDatabaseId()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid RestoreDatabase request: database_id %q must match [a-z][a-z0-9_\\-]*[a-z0-9] and be 2 to 30 characters long.", req.GetDatabaseId())
	}
	if c := req.GetEncryptionConfig(); c != nil {
		if err := refuseCMEK("RestoreDatabase", c.GetEncryptionType(), c.GetKmsKeyName()); err != nil {
			return nil, err
		}
	}
	src := req.GetBackup()
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "Invalid RestoreDatabase request: backup is required.")
	}
	b, snap, ok := s.backupCopy(src)
	if !ok {
		return nil, backupNotFound(src)
	}
	start := s.now()
	name := req.GetParent() + "/databases/" + req.GetDatabaseId()
	if err := s.restore(ctx, req.GetParent(), req.GetDatabaseId(), snap); err != nil {
		return nil, err
	}
	info := &databasepb.RestoreInfo{SourceType: databasepb.RestoreSourceType_BACKUP,
		SourceInfo: &databasepb.RestoreInfo_BackupInfo{BackupInfo: &databasepb.BackupInfo{
			Backup: src, VersionTime: b.GetVersionTime(), CreateTime: b.GetCreateTime(), SourceDatabase: b.GetDatabase()}}}
	s.mu.Lock()
	s.restored[name] = info
	s.mu.Unlock()
	db, err := databases{s: s}.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: name})
	if err != nil {
		return nil, err
	}
	meta := &databasepb.RestoreDatabaseMetadata{Name: name, SourceType: databasepb.RestoreSourceType_BACKUP,
		SourceInfo: &databasepb.RestoreDatabaseMetadata_BackupInfo{BackupInfo: info.GetBackupInfo()},
		Progress:   &databasepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(name, meta, db)
}

func instanceReq(name string) *instancepb.GetInstanceRequest {
	return &instancepb.GetInstanceRequest{Name: name}
}

// snapshot reads database's schema and rows at version (now when nil), and
// returns them with the read timestamp.
func (s *Instances) snapshot(ctx context.Context, database string, version *timestamppb.Timestamp) (*snapshot, *timestamppb.Timestamp, error) {
	db, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: database})
	if err != nil {
		return nil, nil, err
	}
	ddl, err := s.emuDB.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: database})
	if err != nil {
		return nil, nil, err
	}
	snap := &snapshot{dialect: db.GetDatabaseDialect(), ddl: append(ddl.GetStatements(), s.roleDDL(database)...)}
	if snap.dialect == databasepb.DatabaseDialect_DATABASE_DIALECT_UNSPECIFIED {
		snap.dialect = databasepb.DatabaseDialect_GOOGLE_STANDARD_SQL
	}
	pg := snap.dialect == databasepb.DatabaseDialect_POSTGRESQL

	sess, err := s.data.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: database})
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_, _ = s.data.DeleteSession(context.WithoutCancel(ctx), &spannerpb.DeleteSessionRequest{Name: sess.GetName()})
	}()
	ro := &spannerpb.TransactionOptions_ReadOnly{ReturnReadTimestamp: true,
		TimestampBound: &spannerpb.TransactionOptions_ReadOnly_Strong{Strong: true}}
	if version != nil {
		ro.TimestampBound = &spannerpb.TransactionOptions_ReadOnly_ReadTimestamp{ReadTimestamp: version}
	}
	tx, err := s.data.BeginTransaction(ctx, &spannerpb.BeginTransactionRequest{Session: sess.GetName(),
		Options: &spannerpb.TransactionOptions{Mode: &spannerpb.TransactionOptions_ReadOnly_{ReadOnly: ro}}})
	if err != nil {
		return nil, nil, err
	}
	query := func(sql string) (*spannerpb.ResultSet, error) {
		return s.data.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: sess.GetName(), Sql: sql,
			Transaction: &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Id{Id: tx.GetId()}}})
	}
	colsSQL := "SELECT c.TABLE_SCHEMA, c.TABLE_NAME, c.COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS c " +
		"JOIN INFORMATION_SCHEMA.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME " +
		"WHERE t.TABLE_TYPE = 'BASE TABLE' AND c.IS_GENERATED = 'NEVER' AND c.TABLE_SCHEMA NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS') " +
		"ORDER BY c.TABLE_SCHEMA, c.TABLE_NAME, c.ORDINAL_POSITION"
	if pg {
		colsSQL = "SELECT c.table_schema, c.table_name, c.column_name FROM information_schema.columns c " +
			"JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name " +
			"WHERE t.table_type = 'BASE TABLE' AND c.is_generated = 'NEVER' AND c.table_schema NOT IN ('information_schema', 'spanner_sys', 'pg_catalog') " +
			"ORDER BY c.table_schema, c.table_name, c.ordinal_position"
	}
	cols, err := query(colsSQL)
	if err != nil {
		return nil, nil, status.Errorf(status.Code(err), "reading the schema of %s: %v", database, status.Convert(err).Message())
	}
	byTable := map[string]*tableRows{}
	var order []string
	for _, r := range cols.GetRows() {
		v := r.GetValues()
		schema, table, col := v[0].GetStringValue(), v[1].GetStringValue(), v[2].GetStringValue()
		name := table
		if schema != "" && schema != "public" {
			name = schema + "." + table
		}
		t, ok := byTable[name]
		if !ok {
			t = &tableRows{name: name}
			byTable[name] = t
			order = append(order, name)
		}
		t.columns = append(t.columns, col)
	}
	// Parents and referenced tables are created first: the schema's order
	// is the order rows can be inserted in.
	pos := map[string]int{}
	for i, st := range snap.ddl {
		if m := createTable.FindStringSubmatch(st); m != nil {
			pos[strings.ToLower(strings.Trim(m[1], "`\""))] = i + 1
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		pi, pj := pos[strings.ToLower(order[i])], pos[strings.ToLower(order[j])]
		if pi == 0 {
			pi = len(snap.ddl) + 1
		}
		if pj == 0 {
			pj = len(snap.ddl) + 1
		}
		return pi < pj
	})
	for _, name := range order {
		t := byTable[name]
		quoted := make([]string, len(t.columns))
		for i, c := range t.columns {
			quoted[i] = quoteIdent(c, pg)
		}
		rs, err := query("SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteTable(name, pg))
		if err != nil {
			return nil, nil, status.Errorf(status.Code(err), "reading %s of %s: %v", name, database, status.Convert(err).Message())
		}
		t.rows = rs.GetRows()
		for _, r := range t.rows {
			snap.size += int64(proto.Size(r))
		}
		snap.tables = append(snap.tables, *t)
	}
	read := tx.GetReadTimestamp()
	if read == nil {
		read = timestamppb.New(s.now())
	}
	return snap, read, nil
}

func quoteIdent(name string, pg bool) string {
	if pg {
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	return "`" + name + "`"
}

func quoteTable(name string, pg bool) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p, pg)
	}
	return strings.Join(parts, ".")
}

// restore creates the database id under instance from snap, and inserts
// its rows.
func (s *Instances) restore(ctx context.Context, instance, id string, snap *snapshot) error {
	pg := snap.dialect == databasepb.DatabaseDialect_POSTGRESQL
	create := "CREATE DATABASE " + quoteIdent(id, pg)
	op, err := s.emuDB.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: instance, CreateStatement: create, DatabaseDialect: snap.dialect})
	if err != nil {
		return err
	}
	if err := s.waitEmu(ctx, op); err != nil {
		return err
	}
	name := instance + "/databases/" + id
	s.trackEmuOp(op.GetName())
	if len(snap.ddl) > 0 {
		op, err := s.emuDB.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: name, Statements: snap.ddl})
		if err != nil {
			return s.undoRestore(ctx, name, err)
		}
		if err := s.waitEmu(ctx, op); err != nil {
			return s.undoRestore(ctx, name, err)
		}
		s.trackRoles(name, snap.ddl)
	}
	sess, err := s.data.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: name})
	if err != nil {
		return s.undoRestore(ctx, name, err)
	}
	defer func() {
		_, _ = s.data.DeleteSession(context.WithoutCancel(ctx), &spannerpb.DeleteSessionRequest{Name: sess.GetName()})
	}()
	var muts []*spannerpb.Mutation
	cells := 0
	flush := func() error {
		if len(muts) == 0 {
			return nil
		}
		_, err := s.data.Commit(ctx, &spannerpb.CommitRequest{Session: sess.GetName(), Mutations: muts,
			Transaction: &spannerpb.CommitRequest_SingleUseTransaction{SingleUseTransaction: &spannerpb.TransactionOptions{
				Mode: &spannerpb.TransactionOptions_ReadWrite_{ReadWrite: &spannerpb.TransactionOptions_ReadWrite{}}}}})
		muts, cells = nil, 0
		return err
	}
	for _, t := range snap.tables {
		for _, r := range t.rows {
			if n := len(muts); n == 0 || muts[n-1].GetInsert().GetTable() != t.name {
				muts = append(muts, &spannerpb.Mutation{Operation: &spannerpb.Mutation_Insert{Insert: &spannerpb.Mutation_Write{Table: t.name, Columns: t.columns}}})
			}
			w := muts[len(muts)-1].GetInsert()
			w.Values = append(w.Values, r)
			if cells += len(t.columns); cells >= commitCells {
				if err := flush(); err != nil {
					return s.undoRestore(ctx, name, err)
				}
			}
		}
	}
	if err := flush(); err != nil {
		return s.undoRestore(ctx, name, err)
	}
	return nil
}

// undoRestore drops a database a restore could not finish, and returns err.
func (s *Instances) undoRestore(ctx context.Context, name string, err error) error {
	_, _ = s.emuDB.DropDatabase(context.WithoutCancel(ctx), &databasepb.DropDatabaseRequest{Database: name})
	s.dropRoles(name)
	return status.Errorf(status.Code(err), "restoring %s: %v", name, status.Convert(err).Message())
}

// waitEmu waits for an emulator operation, and returns its error.
func (s *Instances) waitEmu(ctx context.Context, op *longrunningpb.Operation) error {
	for !op.GetDone() {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(20 * time.Millisecond):
		}
		var err error
		if op, err = s.emuOps.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
			return err
		}
	}
	if e := op.GetError(); e != nil {
		return status.ErrorProto(e)
	}
	return nil
}

// trackEmuOp keeps the name of an emulator operation, for the listings.
func (s *Instances) trackEmuOp(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emuOpNames = append(s.emuOpNames, name)
	if len(s.emuOpNames) > 1000 {
		s.emuOpNames = s.emuOpNames[1:]
	}
}
