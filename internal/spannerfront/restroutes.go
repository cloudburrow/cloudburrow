package spannerfront

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The REST bindings of the methods #1498 serves, as Google's admin REST API
// (spanner.googleapis.com/v1) binds them: gcloud and the Terraform provider
// call them. Each route reads its request from the path, the query and the
// JSON body, and answers the gRPC method's response as JSON.

type restReq struct {
	m    []string // the path's submatches
	q    url.Values
	body []byte
}

// into reads the body into msg.
func (r restReq) into(msg proto.Message) error {
	if len(r.body) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(r.body, msg); err != nil {
		return status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err)
	}
	return nil
}

func (r restReq) size() int32 {
	n, _ := strconv.Atoi(r.q.Get("pageSize"))
	return int32(n)
}

func (r restReq) mask(param string) *fieldmaskpb.FieldMask {
	v := r.q.Get(param)
	if v == "" {
		return nil
	}
	m := &fieldmaskpb.FieldMask{}
	for _, p := range strings.Split(v, ",") {
		m.Paths = append(m.Paths, snake(strings.TrimSpace(p)))
	}
	return m
}

type route struct {
	method string
	path   *regexp.Regexp
	call   func(ctx context.Context, s *Instances, r restReq) (proto.Message, error)
}

const (
	pInst   = `projects/[^/:]+/instances/[^/:]+`
	pDB     = pInst + `/databases/[^/:]+`
	pBackup = pInst + `/backups/[^/:]+`
)

func rx(s string) *regexp.Regexp { return regexp.MustCompile(`^/v1/` + s + `$`) }

var routes = []route{
	{http.MethodPost, rx(`(` + pInst + `)/backups`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		b := &databasepb.Backup{}
		if err := r.into(b); err != nil {
			return nil, err
		}
		req := &databasepb.CreateBackupRequest{Parent: r.m[1], BackupId: r.q.Get("backupId"), Backup: b}
		if t := r.q.Get("encryptionConfig.encryptionType"); t != "" || r.q.Get("encryptionConfig.kmsKeyName") != "" {
			req.EncryptionConfig = &databasepb.CreateBackupEncryptionConfig{KmsKeyName: r.q.Get("encryptionConfig.kmsKeyName"),
				EncryptionType: databasepb.CreateBackupEncryptionConfig_EncryptionType(databasepb.CreateBackupEncryptionConfig_EncryptionType_value[t])}
		}
		return databases{s: s}.CreateBackup(ctx, req)
	}},
	{http.MethodPost, rx(`(` + pInst + `)/backups:copy`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &databasepb.CopyBackupRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Parent = r.m[1]
		return databases{s: s}.CopyBackup(ctx, req)
	}},
	{http.MethodGet, rx(`(` + pBackup + `)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.GetBackup(ctx, &databasepb.GetBackupRequest{Name: r.m[1]})
	}},
	{http.MethodPatch, rx(`(` + pBackup + `)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		b := &databasepb.Backup{}
		if err := r.into(b); err != nil {
			return nil, err
		}
		b.Name = r.m[1]
		return databases{s: s}.UpdateBackup(ctx, &databasepb.UpdateBackupRequest{Backup: b, UpdateMask: r.mask("updateMask")})
	}},
	{http.MethodDelete, rx(`(` + pBackup + `)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.DeleteBackup(ctx, &databasepb.DeleteBackupRequest{Name: r.m[1]})
	}},
	{http.MethodGet, rx(`(` + pInst + `)/backups`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.ListBackups(ctx, &databasepb.ListBackupsRequest{Parent: r.m[1], Filter: r.q.Get("filter"), PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodGet, rx(`(` + pInst + `)/backupOperations`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.ListBackupOperations(ctx, &databasepb.ListBackupOperationsRequest{Parent: r.m[1], Filter: r.q.Get("filter"), PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodGet, rx(`(` + pInst + `)/databaseOperations`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.ListDatabaseOperations(ctx, &databasepb.ListDatabaseOperationsRequest{Parent: r.m[1], Filter: r.q.Get("filter"), PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodPost, rx(`(` + pInst + `)/databases:restore`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &databasepb.RestoreDatabaseRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Parent = r.m[1]
		return databases{s: s}.RestoreDatabase(ctx, req)
	}},
	{http.MethodPost, rx(`(` + pInst + `)/databases`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &databasepb.CreateDatabaseRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Parent = r.m[1]
		return databases{s: s}.CreateDatabase(ctx, req)
	}},
	{http.MethodPatch, rx(`(` + pDB + `)/ddl`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &databasepb.UpdateDatabaseDdlRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Database = r.m[1]
		return databases{s: s}.UpdateDatabaseDdl(ctx, req)
	}},
	{http.MethodGet, rx(`(` + pDB + `)/ddl`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: r.m[1]})
	}},
	{http.MethodGet, rx(`(` + pDB + `)/databaseRoles`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.ListDatabaseRoles(ctx, &databasepb.ListDatabaseRolesRequest{Parent: r.m[1], PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodPost, rx(`(` + pDB + `)/backupSchedules`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		b := &databasepb.BackupSchedule{}
		if err := r.into(b); err != nil {
			return nil, err
		}
		return databases{s: s}.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: r.m[1], BackupScheduleId: r.q.Get("backupScheduleId"), BackupSchedule: b})
	}},
	{http.MethodGet, rx(`(` + pDB + `)/backupSchedules`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: r.m[1], PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodGet, rx(`(` + pDB + `/backupSchedules/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: r.m[1]})
	}},
	{http.MethodPatch, rx(`(` + pDB + `/backupSchedules/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		b := &databasepb.BackupSchedule{}
		if err := r.into(b); err != nil {
			return nil, err
		}
		b.Name = r.m[1]
		return databases{s: s}.UpdateBackupSchedule(ctx, &databasepb.UpdateBackupScheduleRequest{BackupSchedule: b, UpdateMask: r.mask("updateMask")})
	}},
	{http.MethodDelete, rx(`(` + pDB + `/backupSchedules/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return databases{s: s}.DeleteBackupSchedule(ctx, &databasepb.DeleteBackupScheduleRequest{Name: r.m[1]})
	}},
	{http.MethodGet, rx(`(projects/[^/:]+)/instanceConfigs`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: r.m[1], PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodPost, rx(`(projects/[^/:]+)/instanceConfigs`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &instancepb.CreateInstanceConfigRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Parent = r.m[1]
		return s.CreateInstanceConfig(ctx, req)
	}},
	{http.MethodGet, rx(`(projects/[^/:]+/instanceConfigs/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: r.m[1]})
	}},
	{http.MethodPatch, rx(`(projects/[^/:]+/instanceConfigs/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &instancepb.UpdateInstanceConfigRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		if req.InstanceConfig == nil {
			req.InstanceConfig = &instancepb.InstanceConfig{}
		}
		req.InstanceConfig.Name = r.m[1]
		return s.UpdateInstanceConfig(ctx, req)
	}},
	{http.MethodDelete, rx(`(projects/[^/:]+/instanceConfigs/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.DeleteInstanceConfig(ctx, &instancepb.DeleteInstanceConfigRequest{Name: r.m[1], Etag: r.q.Get("etag"), ValidateOnly: r.q.Get("validateOnly") == "true"})
	}},
	{http.MethodGet, rx(`(projects/[^/:]+)/instanceConfigOperations`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.ListInstanceConfigOperations(ctx, &instancepb.ListInstanceConfigOperationsRequest{Parent: r.m[1], Filter: r.q.Get("filter"), PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodPost, rx(`(` + pInst + `):move`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &instancepb.MoveInstanceRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		req.Name = r.m[1]
		return s.MoveInstance(ctx, req)
	}},
	{http.MethodGet, rx(`(` + pInst + `)/instancePartitions`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.ListInstancePartitions(ctx, &instancepb.ListInstancePartitionsRequest{Parent: r.m[1], PageSize: r.size(), PageToken: r.q.Get("pageToken")})
	}},
	{http.MethodGet, rx(`(` + pInst + `/instancePartitions/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: r.m[1]})
	}},
	{http.MethodPatch, rx(`(` + pInst + `/instancePartitions/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		req := &instancepb.UpdateInstancePartitionRequest{}
		if err := r.into(req); err != nil {
			return nil, err
		}
		if req.InstancePartition == nil {
			req.InstancePartition = &instancepb.InstancePartition{}
		}
		req.InstancePartition.Name = r.m[1]
		return s.UpdateInstancePartition(ctx, req)
	}},
	{http.MethodDelete, rx(`(` + pInst + `/instancePartitions/[^/:]+)`), func(ctx context.Context, s *Instances, r restReq) (proto.Message, error) {
		return s.DeleteInstancePartition(ctx, &instancepb.DeleteInstancePartitionRequest{Name: r.m[1], Etag: r.q.Get("etag")})
	}},
}

// matchRoute is the route r takes, and its path's submatches.
func matchRoute(r *http.Request) (*route, []string) {
	for i := range routes {
		if routes[i].method == r.Method {
			if m := routes[i].path.FindStringSubmatch(r.URL.Path); m != nil {
				return &routes[i], m
			}
		}
	}
	return nil, nil
}

func (s *Instances) serveRoute(w http.ResponseWriter, r *http.Request, rt *route, m []string) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "reading the request: %v", err))
		return
	}
	resp, err := rt.call(r.Context(), s, restReq{m: m, q: r.URL.Query(), body: b})
	if err != nil {
		writeError(w, err)
		return
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(out)
}
