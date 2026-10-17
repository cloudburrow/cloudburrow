package firestoreadmin

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The REST paths of the Admin API (firestore.googleapis.com/v1), as the
// API's HTTP bindings name them.
var (
	restDatabases = regexp.MustCompile(`^projects/[^/]+$`)
	restDatabase  = regexp.MustCompile(`^projects/[^/]+/databases/[^/:]+$`)
	restIndexes   = regexp.MustCompile(`^projects/[^/]+/databases/[^/]+/collectionGroups/[^/]+$`)
)

// serveREST answers an admin call over REST.
func (a *Admin) serveREST(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	verb := ""
	if i := strings.LastIndex(p, ":"); i > strings.LastIndex(p, "/") {
		p, verb = p[:i], p[i+1:]
	}
	q := r.URL.Query()
	ctx := r.Context()
	body := func(m proto.Message) bool {
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err == nil && len(b) > 0 {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
		}
		if err != nil {
			writeError(w, status.Errorf(codes.InvalidArgument, "invalid JSON payload: %v", err))
			return false
		}
		return true
	}
	mask := func() *fieldmaskpb.FieldMask {
		v := q.Get("updateMask")
		if v == "" {
			v = q.Get("update_mask")
		}
		if v == "" {
			return nil
		}
		return &fieldmaskpb.FieldMask{Paths: strings.Split(v, ",")}
	}
	size := func() int32 {
		n, _ := strconv.Atoi(q.Get("pageSize"))
		return int32(n)
	}
	reply := func(m proto.Message, err error) { writeReply(w, m, err) }
	m, v := r.Method, verb

	switch {
	// databases
	case strings.HasSuffix(p, "/databases") && restDatabases.MatchString(strings.TrimSuffix(p, "/databases")) && v == "":
		parent := strings.TrimSuffix(p, "/databases")
		switch m {
		case http.MethodGet:
			reply(a.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: parent, ShowDeleted: q.Get("showDeleted") == "true"}))
			return
		case http.MethodPost:
			db := &adminpb.Database{}
			if !body(db) {
				return
			}
			reply(a.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: parent, Database: db, DatabaseId: q.Get("databaseId")}))
			return
		}
	case strings.HasSuffix(p, "/databases") && (v == "restore" || v == "clone"):
		parent := strings.TrimSuffix(p, "/databases")
		if v == "restore" {
			req := &adminpb.RestoreDatabaseRequest{}
			if !body(req) {
				return
			}
			req.Parent = parent
			reply(a.RestoreDatabase(ctx, req))
		} else {
			req := &adminpb.CloneDatabaseRequest{}
			if !body(req) {
				return
			}
			req.Parent = parent
			reply(a.CloneDatabase(ctx, req))
		}
		return
	case restDatabase.MatchString(p):
		switch {
		case v == "" && m == http.MethodGet:
			reply(a.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: p}))
			return
		case v == "" && m == http.MethodPatch:
			db := &adminpb.Database{}
			if !body(db) {
				return
			}
			db.Name = p
			reply(a.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{Database: db, UpdateMask: mask()}))
			return
		case v == "" && m == http.MethodDelete:
			reply(a.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: p, Etag: q.Get("etag")}))
			return
		case v == "exportDocuments" && m == http.MethodPost:
			req := &adminpb.ExportDocumentsRequest{}
			if !body(req) {
				return
			}
			req.Name = p
			reply(a.ExportDocuments(ctx, req))
			return
		case v == "importDocuments" && m == http.MethodPost:
			req := &adminpb.ImportDocumentsRequest{}
			if !body(req) {
				return
			}
			req.Name = p
			reply(a.ImportDocuments(ctx, req))
			return
		case v == "bulkDeleteDocuments" && m == http.MethodPost:
			req := &adminpb.BulkDeleteDocumentsRequest{}
			if !body(req) {
				return
			}
			req.Name = p
			reply(a.BulkDeleteDocuments(ctx, req))
			return
		}
	// indexes
	case strings.HasSuffix(p, "/indexes") && restIndexes.MatchString(strings.TrimSuffix(p, "/indexes")) && v == "":
		parent := strings.TrimSuffix(p, "/indexes")
		switch m {
		case http.MethodGet:
			reply(a.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: parent, Filter: q.Get("filter"), PageSize: size(), PageToken: q.Get("pageToken")}))
			return
		case http.MethodPost:
			x := &adminpb.Index{}
			if !body(x) {
				return
			}
			reply(a.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: parent, Index: x}))
			return
		}
	case indexNameRe.MatchString(p) && v == "":
		switch m {
		case http.MethodGet:
			reply(a.GetIndex(ctx, &adminpb.GetIndexRequest{Name: p}))
			return
		case http.MethodDelete:
			reply(a.DeleteIndex(ctx, &adminpb.DeleteIndexRequest{Name: p}))
			return
		}
	// fields
	case strings.HasSuffix(p, "/fields") && restIndexes.MatchString(strings.TrimSuffix(p, "/fields")) && v == "" && m == http.MethodGet:
		reply(a.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: strings.TrimSuffix(p, "/fields"), Filter: q.Get("filter"), PageSize: size(), PageToken: q.Get("pageToken")}))
		return
	case fieldNameRe.MatchString(p) && v == "":
		switch m {
		case http.MethodGet:
			reply(a.GetField(ctx, &adminpb.GetFieldRequest{Name: p}))
			return
		case http.MethodPatch:
			f := &adminpb.Field{}
			if !body(f) {
				return
			}
			f.Name = p
			reply(a.UpdateField(ctx, &adminpb.UpdateFieldRequest{Field: f, UpdateMask: mask()}))
			return
		}
	// operations
	case strings.HasSuffix(p, "/operations") && v == "" && m == http.MethodGet:
		reply(a.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: p, Filter: q.Get("filter"), PageSize: size(), PageToken: q.Get("pageToken")}))
		return
	case opNameRe.MatchString(p):
		switch {
		case v == "" && m == http.MethodGet:
			reply(a.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: p}))
			return
		case v == "" && m == http.MethodDelete:
			reply(a.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: p}))
			return
		case v == "cancel" && m == http.MethodPost:
			reply(a.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: p}))
			return
		}
	// backup schedules
	case strings.HasSuffix(p, "/backupSchedules") && restDatabase.MatchString(strings.TrimSuffix(p, "/backupSchedules")) && v == "":
		parent := strings.TrimSuffix(p, "/backupSchedules")
		switch m {
		case http.MethodGet:
			reply(a.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: parent}))
			return
		case http.MethodPost:
			s := &adminpb.BackupSchedule{}
			if !body(s) {
				return
			}
			reply(a.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: parent, BackupSchedule: s}))
			return
		}
	case scheduleNameRe.MatchString(p) && v == "":
		switch m {
		case http.MethodGet:
			reply(a.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: p}))
			return
		case http.MethodPatch:
			s := &adminpb.BackupSchedule{}
			if !body(s) {
				return
			}
			s.Name = p
			reply(a.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{BackupSchedule: s, UpdateMask: mask()}))
			return
		case http.MethodDelete:
			reply(a.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: p}))
			return
		}
	// backups
	case strings.HasSuffix(p, "/backups") && backupLocationRe.MatchString(strings.TrimSuffix(p, "/backups")) && v == "" && m == http.MethodGet:
		reply(a.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: strings.TrimSuffix(p, "/backups"), Filter: q.Get("filter")}))
		return
	case backupNameRe.MatchString(p) && v == "":
		switch m {
		case http.MethodGet:
			reply(a.GetBackup(ctx, &adminpb.GetBackupRequest{Name: p}))
			return
		case http.MethodDelete:
			reply(a.DeleteBackup(ctx, &adminpb.DeleteBackupRequest{Name: p}))
			return
		}
	// user credentials
	case strings.HasSuffix(p, "/userCreds") && restDatabase.MatchString(strings.TrimSuffix(p, "/userCreds")) && v == "":
		parent := strings.TrimSuffix(p, "/userCreds")
		switch m {
		case http.MethodGet:
			reply(a.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: parent}))
			return
		case http.MethodPost:
			uc := &adminpb.UserCreds{}
			if !body(uc) {
				return
			}
			id := q.Get("userCredsId")
			if id == "" {
				id = q.Get("user_creds_id")
			}
			reply(a.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: parent, UserCreds: uc, UserCredsId: id}))
			return
		}
	case userCredsNameRe.MatchString(p):
		switch {
		case v == "" && m == http.MethodGet:
			reply(a.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: p}))
			return
		case v == "" && m == http.MethodDelete:
			reply(a.DeleteUserCreds(ctx, &adminpb.DeleteUserCredsRequest{Name: p}))
			return
		case v == "enable" && m == http.MethodPost:
			reply(a.EnableUserCreds(ctx, &adminpb.EnableUserCredsRequest{Name: p}))
			return
		case v == "disable" && m == http.MethodPost:
			reply(a.DisableUserCreds(ctx, &adminpb.DisableUserCredsRequest{Name: p}))
			return
		case v == "resetPassword" && m == http.MethodPost:
			reply(a.ResetUserPassword(ctx, &adminpb.ResetUserPasswordRequest{Name: p}))
			return
		}
	}
	writeError(w, status.Errorf(codes.Unimplemented, "%s %s is not a Firestore Admin API method served here", r.Method, r.URL.Path))
}

func writeReply(w http.ResponseWriter, m proto.Message, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// writeError renders err as Firestore's REST API does:
// {"error": {"code", "message", "status"}}.
func writeError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := apierror.From(err).HTTPStatus()
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	body.Error.Code, body.Error.Message, body.Error.Status = code, st.Message(), rpccode.Code(st.Code()).String()
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
