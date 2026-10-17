package bigtablefront

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backups and snapshots (#1499). The emulator keeps neither, so the front
// does: a backup, or a snapshot, is a copy of its table, the column
// families and every cell (timestamps kept) read out of the emulator when it
// is made, held in memory as the emulator holds its tables. RestoreTable and
// CreateTableFromSnapshot write the copy back into a new table of the
// emulator. CopyBackup copies the copy. The long-running operations Google
// answers with are done when they are returned (instances.go).
//
// A cluster of an instance the front created must exist; one of an instance
// it never created is taken as named, as instances.go explains for tables.
// Deleting an instance deletes its backups and snapshots; deleting a table
// leaves its backups, as Google does.

var (
	backupRe   = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/clusters/([^/]+)/backups/([^/]+)$`)
	snapshotRe = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/clusters/([^/]+)/snapshots/([^/]+)$`)
	tableRe    = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/tables/([^/]+)$`)
	backupIDRe = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]{0,49}$`)
	tableIDRe  = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]{0,49}$`)
	// clusterOrAllRe is a cluster, or every cluster of an instance ("-").
	clusterOrAllRe = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/clusters/([^/]+)$`)
)

// Google's bounds on a backup's expire_time, from the time of the request.
const (
	minBackupTTL = 6 * time.Hour
	maxBackupTTL = 90 * 24 * time.Hour
	// snapshotTTL is a snapshot's default and longest life.
	snapshotTTL = 30 * 24 * time.Hour
)

// tableCopy is a table as it was when copied.
type tableCopy struct {
	families map[string]*btapb.ColumnFamily
	rows     []*btpb.MutateRowsRequest_Entry
	size     int64
}

type backupEntry struct {
	pb   *btapb.Backup
	data *tableCopy
}

type snapshotEntry struct {
	pb   *btapb.Snapshot
	data *tableCopy
}

// copies is the backups and snapshots.
type copies struct {
	upstream  *grpc.ClientConn
	instances *instances
	mu        sync.Mutex
	backups   map[string]*backupEntry
	snapshots map[string]*snapshotEntry
}

func newCopies(upstream *grpc.ClientConn, in *instances) *copies {
	return &copies{upstream: upstream, instances: in, backups: map[string]*backupEntry{}, snapshots: map[string]*snapshotEntry{}}
}

func (c *copies) methods() map[string]method {
	return map[string]method{
		admin + "CreateBackup":            unary(func() *btapb.CreateBackupRequest { return &btapb.CreateBackupRequest{} }, c.createBackup),
		admin + "GetBackup":               unary(func() *btapb.GetBackupRequest { return &btapb.GetBackupRequest{} }, c.getBackup),
		admin + "ListBackups":             unary(func() *btapb.ListBackupsRequest { return &btapb.ListBackupsRequest{} }, c.listBackups),
		admin + "UpdateBackup":            unary(func() *btapb.UpdateBackupRequest { return &btapb.UpdateBackupRequest{} }, c.updateBackup),
		admin + "DeleteBackup":            unary(func() *btapb.DeleteBackupRequest { return &btapb.DeleteBackupRequest{} }, c.deleteBackup),
		admin + "CopyBackup":              unary(func() *btapb.CopyBackupRequest { return &btapb.CopyBackupRequest{} }, c.copyBackup),
		admin + "RestoreTable":            unary(func() *btapb.RestoreTableRequest { return &btapb.RestoreTableRequest{} }, c.restoreTable),
		admin + "SnapshotTable":           unary(func() *btapb.SnapshotTableRequest { return &btapb.SnapshotTableRequest{} }, c.snapshotTable),
		admin + "GetSnapshot":             unary(func() *btapb.GetSnapshotRequest { return &btapb.GetSnapshotRequest{} }, c.getSnapshot),
		admin + "ListSnapshots":           unary(func() *btapb.ListSnapshotsRequest { return &btapb.ListSnapshotsRequest{} }, c.listSnapshots),
		admin + "DeleteSnapshot":          unary(func() *btapb.DeleteSnapshotRequest { return &btapb.DeleteSnapshotRequest{} }, c.deleteSnapshot),
		admin + "CreateTableFromSnapshot": unary(func() *btapb.CreateTableFromSnapshotRequest { return &btapb.CreateTableFromSnapshotRequest{} }, c.createTableFromSnapshot),
	}
}

// op records a done operation through the instance admin's store.
func (c *copies) op(target string, meta, resp proto.Message) (proto.Message, error) {
	c.instances.mu.Lock()
	defer c.instances.mu.Unlock()
	return c.instances.done(target, meta, resp)
}

// checkCluster checks that cluster names a cluster, which must exist if
// its instance is one the front created. all allows the "-" wildcard.
func (c *copies) checkCluster(cluster string, all bool) error {
	m := clusterOrAllRe.FindStringSubmatch(cluster)
	if m == nil {
		return status.Errorf(codes.InvalidArgument, "invalid cluster %q: want projects/{project}/instances/{instance}/clusters/{cluster}", cluster)
	}
	if m[3] == "-" {
		if all {
			return nil
		}
		return status.Errorf(codes.InvalidArgument, "invalid cluster %q: name one cluster", cluster)
	}
	c.instances.mu.Lock()
	defer c.instances.mu.Unlock()
	in, ok := c.instances.byNm["projects/"+m[1]+"/instances/"+m[2]]
	if !ok {
		return nil
	}
	if _, ok := in.clusters[m[3]]; !ok {
		return notFound("Cluster", cluster)
	}
	return nil
}

// instanceOf is the instance of a cluster, table, backup or snapshot name.
func instanceOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[:4], "/")
}

// read copies the table name out of the emulator.
func (c *copies) read(ctx context.Context, name string) (*tableCopy, error) {
	t, err := btapb.NewBigtableTableAdminClient(c.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_FULL})
	if err != nil {
		return nil, err
	}
	out := &tableCopy{families: map[string]*btapb.ColumnFamily{}}
	for id, f := range t.GetColumnFamilies() {
		out.families[id] = proto.Clone(f).(*btapb.ColumnFamily)
	}
	rows, err := readAll(ctx, btpb.NewBigtableClient(c.upstream), &btpb.ReadRowsRequest{TableName: name})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		e := &btpb.MutateRowsRequest_Entry{RowKey: r.key}
		for _, cl := range r.cells {
			e.Mutations = append(e.Mutations, &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
				FamilyName: cl.family, ColumnQualifier: cl.qualifier, TimestampMicros: cl.ts, Value: cl.value,
			}}})
			out.size += int64(len(r.key) + len(cl.family) + len(cl.qualifier) + len(cl.value) + 8)
		}
		out.rows = append(out.rows, e)
	}
	return out, nil
}

// write creates table tableID in instance from data.
func (c *copies) write(ctx context.Context, instance, tableID string, data *tableCopy) (*btapb.Table, error) {
	if !tableIDRe.MatchString(tableID) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid table_id %q", tableID)
	}
	fams := map[string]*btapb.ColumnFamily{}
	for id, f := range data.families {
		fams[id] = proto.Clone(f).(*btapb.ColumnFamily)
	}
	t, err := btapb.NewBigtableTableAdminClient(c.upstream).CreateTable(ctx, &btapb.CreateTableRequest{
		Parent: instance, TableId: tableID, Table: &btapb.Table{ColumnFamilies: fams},
	})
	if err != nil {
		return nil, err
	}
	d := btpb.NewBigtableClient(c.upstream)
	for start := 0; start < len(data.rows); start += 1000 {
		end := min(start+1000, len(data.rows))
		stream, err := d.MutateRows(ctx, &btpb.MutateRowsRequest{TableName: t.GetName(), Entries: data.rows[start:end]})
		if err != nil {
			return nil, err
		}
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			for _, e := range resp.GetEntries() {
				if s := e.GetStatus(); s.GetCode() != 0 {
					return nil, status.Errorf(codes.Internal, "restoring row %q: %s", data.rows[start+int(e.GetIndex())].GetRowKey(), s.GetMessage())
				}
			}
		}
	}
	return t, nil
}

// cell is one cell read back from ReadRows.
type cell struct {
	family    string
	qualifier []byte
	ts        int64
	value     []byte
}

type row struct {
	key   []byte
	cells []cell
}

// readAll reads every row req selects, merging ReadRows' chunks.
func readAll(ctx context.Context, d btpb.BigtableClient, req *btpb.ReadRowsRequest) ([]row, error) {
	stream, err := d.ReadRows(ctx, req)
	if err != nil {
		return nil, err
	}
	var (
		out   []row
		cur   row
		cl    cell
		buf   []byte
		split bool
	)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, ch := range resp.GetChunks() {
			if ch.GetResetRow() {
				cur, buf, split = row{}, nil, false
				continue
			}
			if !split {
				if len(ch.GetRowKey()) > 0 {
					cur.key = append([]byte(nil), ch.GetRowKey()...)
				}
				if ch.GetFamilyName() != nil {
					cl.family = ch.GetFamilyName().GetValue()
				}
				if ch.GetQualifier() != nil {
					cl.qualifier = append([]byte(nil), ch.GetQualifier().GetValue()...)
				}
				cl.ts = ch.GetTimestampMicros()
			}
			buf = append(buf, ch.GetValue()...)
			if ch.GetValueSize() > 0 {
				split = true
			} else {
				split = false
				cl.value = buf
				buf = nil
				cur.cells = append(cur.cells, cl)
			}
			if ch.GetCommitRow() {
				out = append(out, cur)
				cur = row{}
			}
		}
	}
}

// --- backups ---

func (c *copies) checkExpiry(field string, expire *timestamppb.Timestamp, now time.Time) error {
	if expire == nil {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if d := expire.AsTime().Sub(now); d < minBackupTTL || d > maxBackupTTL {
		return status.Errorf(codes.InvalidArgument, "%s must be at least 6 hours and at most 90 days from now", field)
	}
	return nil
}

func (c *copies) createBackup(ctx context.Context, req *btapb.CreateBackupRequest) (proto.Message, error) {
	start := time.Now().UTC()
	if err := c.checkCluster(req.GetParent(), false); err != nil {
		return nil, err
	}
	if !backupIDRe.MatchString(req.GetBackupId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup_id %q", req.GetBackupId())
	}
	b := req.GetBackup()
	if b == nil {
		return nil, status.Error(codes.InvalidArgument, "backup is required")
	}
	src := b.GetSourceTable()
	if !tableRe.MatchString(src) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup.source_table %q: want projects/{project}/instances/{instance}/tables/{table}", src)
	}
	if instanceOf(src) != instanceOf(req.GetParent()) {
		return nil, status.Error(codes.InvalidArgument, "the source table must be in the instance of the backup's cluster")
	}
	if err := c.checkExpiry("backup.expire_time", b.GetExpireTime(), start); err != nil {
		return nil, err
	}
	typ := b.GetBackupType()
	if typ == btapb.Backup_BACKUP_TYPE_UNSPECIFIED {
		typ = btapb.Backup_STANDARD
	}
	if b.GetHotToStandardTime() != nil && typ != btapb.Backup_HOT {
		return nil, status.Error(codes.InvalidArgument, "hot_to_standard_time is for HOT backups only")
	}
	name := req.GetParent() + "/backups/" + req.GetBackupId()
	c.mu.Lock()
	_, exists := c.backups[name]
	c.mu.Unlock()
	if exists {
		return nil, status.Errorf(codes.AlreadyExists, "Backup %s already exists.", name)
	}
	data, err := c.read(ctx, src)
	if err != nil {
		return nil, err
	}
	end := time.Now().UTC()
	pb := &btapb.Backup{
		Name: name, SourceTable: src, ExpireTime: b.GetExpireTime(), StartTime: timestamppb.New(start), EndTime: timestamppb.New(end),
		SizeBytes: data.size, State: btapb.Backup_READY, BackupType: typ, HotToStandardTime: b.GetHotToStandardTime(),
		EncryptionInfo: &btapb.EncryptionInfo{EncryptionType: btapb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION},
	}
	c.mu.Lock()
	if _, exists := c.backups[name]; exists {
		c.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "Backup %s already exists.", name)
	}
	c.backups[name] = &backupEntry{pb: pb, data: data}
	c.mu.Unlock()
	return c.op(name, &btapb.CreateBackupMetadata{Name: name, SourceTable: src, StartTime: pb.StartTime, EndTime: pb.EndTime}, proto.Clone(pb))
}

// backup returns the backup name names. c.mu is held.
func (c *copies) backup(name string) (*backupEntry, error) {
	if !backupRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup name %q: want projects/{project}/instances/{instance}/clusters/{cluster}/backups/{backup}", name)
	}
	b, ok := c.backups[name]
	if !ok {
		return nil, notFound("Backup", name)
	}
	return b, nil
}

func (c *copies) getBackup(_ context.Context, req *btapb.GetBackupRequest) (proto.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.backup(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(b.pb), nil
}

// page cuts names to the page a token and size select.
func page(n int, token string, size int32) (start, end int, next string, err error) {
	if size < 0 {
		return 0, 0, "", status.Error(codes.InvalidArgument, "page_size cannot be negative")
	}
	if token != "" {
		s, err := strconv.Atoi(token)
		if err != nil || s < 0 || s > n {
			return 0, 0, "", status.Errorf(codes.InvalidArgument, "invalid page_token %q", token)
		}
		start = s
	}
	end = n
	if size > 0 && start+int(size) < end {
		end = start + int(size)
	}
	if end < n {
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

// under lists the keys of m in the cluster parent ("-" for every cluster
// of its instance), sorted.
func under[V any](m map[string]V, parent, kind string) []string {
	prefix, all := parent+"/"+kind+"/", strings.HasSuffix(parent, "/clusters/-")
	if all {
		prefix = strings.TrimSuffix(parent, "-")
	}
	var names []string
	for n := range m {
		rest, ok := strings.CutPrefix(n, prefix)
		if ok && (!all || strings.Count(rest, "/") == 2) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func (c *copies) listBackups(_ context.Context, req *btapb.ListBackupsRequest) (proto.Message, error) {
	if err := c.checkCluster(req.GetParent(), true); err != nil {
		return nil, err
	}
	if req.GetFilter() != "" {
		return nil, status.Error(codes.Unimplemented, "the emulator does not currently support filter in ListBackups")
	}
	if ob := req.GetOrderBy(); ob != "" && ob != "name" && ob != "name asc" {
		return nil, status.Error(codes.Unimplemented, "the emulator does not currently support order_by in ListBackups other than name")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	names := under(c.backups, req.GetParent(), "backups")
	start, end, next, err := page(len(names), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	out := &btapb.ListBackupsResponse{NextPageToken: next}
	for _, n := range names[start:end] {
		out.Backups = append(out.Backups, proto.Clone(c.backups[n].pb).(*btapb.Backup))
	}
	return out, nil
}

func (c *copies) updateBackup(_ context.Context, req *btapb.UpdateBackupRequest) (proto.Message, error) {
	upd := req.GetBackup()
	if upd == nil {
		return nil, status.Error(codes.InvalidArgument, "backup is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.backup(upd.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(b.pb).(*btapb.Backup)
	for _, p := range paths {
		switch p {
		case "expire_time":
			if err := c.checkExpiry("backup.expire_time", upd.GetExpireTime(), next.GetStartTime().AsTime()); err != nil {
				return nil, err
			}
			next.ExpireTime = upd.GetExpireTime()
		case "hot_to_standard_time":
			if next.GetBackupType() != btapb.Backup_HOT {
				return nil, status.Error(codes.InvalidArgument, "hot_to_standard_time is for HOT backups only")
			}
			next.HotToStandardTime = upd.GetHotToStandardTime()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not updatable: want expire_time or hot_to_standard_time", p)
		}
	}
	b.pb = next
	return proto.Clone(next), nil
}

func (c *copies) deleteBackup(_ context.Context, req *btapb.DeleteBackupRequest) (proto.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.backup(req.GetName()); err != nil {
		return nil, err
	}
	delete(c.backups, req.GetName())
	return &emptypb.Empty{}, nil
}

func (c *copies) copyBackup(_ context.Context, req *btapb.CopyBackupRequest) (proto.Message, error) {
	start := time.Now().UTC()
	if err := c.checkCluster(req.GetParent(), false); err != nil {
		return nil, err
	}
	if !backupIDRe.MatchString(req.GetBackupId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup_id %q", req.GetBackupId())
	}
	if err := c.checkExpiry("expire_time", req.GetExpireTime(), start); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/backups/" + req.GetBackupId()
	c.mu.Lock()
	src, err := c.backup(req.GetSourceBackup())
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if _, exists := c.backups[name]; exists {
		c.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "Backup %s already exists.", name)
	}
	end := time.Now().UTC()
	// A copy of a copy names the original as its source.
	origin := src.pb.GetName()
	if s := src.pb.GetSourceBackup(); s != "" {
		origin = s
	}
	pb := proto.Clone(src.pb).(*btapb.Backup)
	pb.Name, pb.SourceBackup, pb.ExpireTime = name, origin, req.GetExpireTime()
	pb.StartTime, pb.EndTime = timestamppb.New(start), timestamppb.New(end)
	c.backups[name] = &backupEntry{pb: pb, data: src.data}
	meta := &btapb.CopyBackupMetadata{Name: name, SourceBackupInfo: &btapb.BackupInfo{
		Backup: src.pb.GetName(), StartTime: src.pb.GetStartTime(), EndTime: src.pb.GetEndTime(), SourceTable: src.pb.GetSourceTable(), SourceBackup: src.pb.GetSourceBackup(),
	}}
	c.mu.Unlock()
	return c.op(name, meta, proto.Clone(pb))
}

func (c *copies) restoreTable(ctx context.Context, req *btapb.RestoreTableRequest) (proto.Message, error) {
	if !instanceRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}", req.GetParent())
	}
	c.mu.Lock()
	b, err := c.backup(req.GetBackup())
	var pb *btapb.Backup
	if err == nil {
		pb = proto.Clone(b.pb).(*btapb.Backup)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	t, err := c.write(ctx, req.GetParent(), req.GetTableId(), b.data)
	if err != nil {
		return nil, err
	}
	t.RestoreInfo = &btapb.RestoreInfo{SourceType: btapb.RestoreSourceType_BACKUP, SourceInfo: &btapb.RestoreInfo_BackupInfo{BackupInfo: backupInfo(pb)}}
	meta := &btapb.RestoreTableMetadata{Name: t.GetName(), SourceType: btapb.RestoreSourceType_BACKUP,
		SourceInfo: &btapb.RestoreTableMetadata_BackupInfo{BackupInfo: backupInfo(pb)},
		Progress:   &btapb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.Now(), EndTime: timestamppb.Now()}}
	return c.op(t.GetName(), meta, t)
}

func backupInfo(b *btapb.Backup) *btapb.BackupInfo {
	return &btapb.BackupInfo{Backup: b.GetName(), StartTime: b.GetStartTime(), EndTime: b.GetEndTime(), SourceTable: b.GetSourceTable(), SourceBackup: b.GetSourceBackup()}
}

// --- snapshots ---

func (c *copies) snapshotTable(ctx context.Context, req *btapb.SnapshotTableRequest) (proto.Message, error) {
	start := time.Now().UTC()
	if !tableRe.MatchString(req.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}/instances/{instance}/tables/{table}", req.GetName())
	}
	if err := c.checkCluster(req.GetCluster(), false); err != nil {
		return nil, err
	}
	if instanceOf(req.GetName()) != instanceOf(req.GetCluster()) {
		return nil, status.Error(codes.InvalidArgument, "the table must be in the instance of the cluster")
	}
	if !backupIDRe.MatchString(req.GetSnapshotId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid snapshot_id %q", req.GetSnapshotId())
	}
	ttl := snapshotTTL
	if d := req.GetTtl(); d != nil {
		ttl = d.AsDuration()
		if ttl < 24*time.Hour || ttl > snapshotTTL {
			return nil, status.Error(codes.InvalidArgument, "ttl must be at least 24 hours and at most 30 days")
		}
	}
	name := req.GetCluster() + "/snapshots/" + req.GetSnapshotId()
	data, err := c.read(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	t, err := btapb.NewBigtableTableAdminClient(c.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: req.GetName(), View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		return nil, err
	}
	pb := &btapb.Snapshot{Name: name, SourceTable: t, DataSizeBytes: data.size, CreateTime: timestamppb.New(start),
		DeleteTime: timestamppb.New(start.Add(ttl)), State: btapb.Snapshot_READY, Description: req.GetDescription()}
	c.mu.Lock()
	if _, exists := c.snapshots[name]; exists {
		c.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "Snapshot %s already exists.", name)
	}
	c.snapshots[name] = &snapshotEntry{pb: pb, data: data}
	c.mu.Unlock()
	return c.op(name, &btapb.SnapshotTableMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start), FinishTime: timestamppb.Now()}, proto.Clone(pb))
}

// snapshot returns the snapshot name names. c.mu is held.
func (c *copies) snapshot(name string) (*snapshotEntry, error) {
	if !snapshotRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid snapshot name %q: want projects/{project}/instances/{instance}/clusters/{cluster}/snapshots/{snapshot}", name)
	}
	s, ok := c.snapshots[name]
	if !ok {
		return nil, notFound("Snapshot", name)
	}
	return s, nil
}

func (c *copies) getSnapshot(_ context.Context, req *btapb.GetSnapshotRequest) (proto.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := c.snapshot(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(s.pb), nil
}

func (c *copies) listSnapshots(_ context.Context, req *btapb.ListSnapshotsRequest) (proto.Message, error) {
	if err := c.checkCluster(req.GetParent(), true); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	names := under(c.snapshots, req.GetParent(), "snapshots")
	start, end, next, err := page(len(names), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	out := &btapb.ListSnapshotsResponse{NextPageToken: next}
	for _, n := range names[start:end] {
		out.Snapshots = append(out.Snapshots, proto.Clone(c.snapshots[n].pb).(*btapb.Snapshot))
	}
	return out, nil
}

func (c *copies) deleteSnapshot(_ context.Context, req *btapb.DeleteSnapshotRequest) (proto.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.snapshot(req.GetName()); err != nil {
		return nil, err
	}
	delete(c.snapshots, req.GetName())
	return &emptypb.Empty{}, nil
}

func (c *copies) createTableFromSnapshot(ctx context.Context, req *btapb.CreateTableFromSnapshotRequest) (proto.Message, error) {
	start := time.Now().UTC()
	if !instanceRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}", req.GetParent())
	}
	c.mu.Lock()
	s, err := c.snapshot(req.GetSourceSnapshot())
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if instanceOf(req.GetSourceSnapshot()) != req.GetParent() {
		return nil, status.Error(codes.InvalidArgument, "the snapshot must be in the instance the table is created in")
	}
	t, err := c.write(ctx, req.GetParent(), req.GetTableId(), s.data)
	if err != nil {
		return nil, err
	}
	return c.op(t.GetName(), &btapb.CreateTableFromSnapshotMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start), FinishTime: timestamppb.Now()}, t)
}

// drop forgets the backups and snapshots of instance.
func (c *copies) drop(instance string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := range c.backups {
		if strings.HasPrefix(n, instance+"/") {
			delete(c.backups, n)
		}
	}
	for n := range c.snapshots {
		if strings.HasPrefix(n, instance+"/") {
			delete(c.snapshots, n)
		}
	}
}
