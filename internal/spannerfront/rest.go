package spannerfront

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var (
	restInstances = regexp.MustCompile(`^/v1/(projects/[^/:]+)/instances$`)
	restInstance  = regexp.MustCompile(`^/v1/(projects/[^/:]+/instances/[^/:]+)$`)
	restDatabases = regexp.MustCompile(`^/v1/(projects/[^/:]+/instances/[^/:]+)/databases$`)
	restDatabase  = regexp.MustCompile(`^/v1/(projects/[^/:]+/instances/[^/:]+/databases/[^/:]+)$`)
)

// restCall is the method a REST request is, and whether the front answers it.
func restCall(r *http.Request) (string, bool) {
	p := r.URL.Path
	switch {
	case restInstances.MatchString(p) && r.Method == http.MethodGet:
		return "ListInstances", true
	case restDatabases.MatchString(p) && r.Method == http.MethodGet:
		return "ListDatabases", true
	case restDatabase.MatchString(p):
		switch r.Method {
		case http.MethodGet:
			return "GetDatabase", true
		case http.MethodPatch:
			return "UpdateDatabase", true
		case http.MethodDelete:
			return "DropDatabase", true
		}
	case restInstance.MatchString(p):
		switch r.Method {
		case http.MethodGet:
			return "GetInstance", true
		case http.MethodPatch:
			return "UpdateInstance", true
		case http.MethodDelete:
			return "DeleteInstance", true
		}
	}
	return "", false
}

func (s *Instances) serveREST(w http.ResponseWriter, r *http.Request) {
	method, _ := restCall(r)
	if name := strings.TrimPrefix(r.URL.Path, "/v1/"); r.Method == http.MethodGet && s.ownOperation(name) {
		method = "GetOperation"
	}
	ctx, q := r.Context(), r.URL.Query()
	reply := func(m proto.Message, err error) {
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
	switch method {
	case "GetOperation":
		reply(s.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: strings.TrimPrefix(r.URL.Path, "/v1/")}))
	case "ListDatabases":
		n, _ := strconv.Atoi(q.Get("pageSize"))
		reply(databases{s: s}.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: restDatabases.FindStringSubmatch(r.URL.Path)[1],
			PageSize: int32(n), PageToken: q.Get("pageToken")}))
	case "GetDatabase":
		reply(databases{s: s}.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: restDatabase.FindStringSubmatch(r.URL.Path)[1]}))
	case "DropDatabase":
		reply(databases{s: s}.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: restDatabase.FindStringSubmatch(r.URL.Path)[1]}))
	case "UpdateDatabase":
		db := &databasepb.Database{}
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(b) > 0 {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, db)
		}
		if err != nil {
			writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
			return
		}
		db.Name = restDatabase.FindStringSubmatch(r.URL.Path)[1]
		var mask *fieldmaskpb.FieldMask
		if v := q.Get("updateMask"); v != "" {
			mask = &fieldmaskpb.FieldMask{}
			for _, p := range strings.Split(v, ",") {
				mask.Paths = append(mask.Paths, snake(p))
			}
		}
		reply(databases{s: s}.UpdateDatabase(ctx, &databasepb.UpdateDatabaseRequest{Database: db, UpdateMask: mask}))
	case "ListInstances":
		n, _ := strconv.Atoi(q.Get("pageSize"))
		reply(s.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: restInstances.FindStringSubmatch(r.URL.Path)[1],
			PageSize: int32(n), PageToken: q.Get("pageToken"), Filter: q.Get("filter")}))
	case "GetInstance":
		reply(s.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: restInstance.FindStringSubmatch(r.URL.Path)[1]}))
	case "DeleteInstance":
		reply(s.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: restInstance.FindStringSubmatch(r.URL.Path)[1]}))
	case "UpdateInstance":
		req := &instancepb.UpdateInstanceRequest{}
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, req)
		}
		if err != nil {
			writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
			return
		}
		name := restInstance.FindStringSubmatch(r.URL.Path)[1]
		if req.Instance == nil {
			req.Instance = &instancepb.Instance{}
		}
		// The URL names the instance: the Terraform provider's body names
		// it with a formatting error ("instances/%!s(<nil>)").
		req.Instance.Name = name
		reply(s.UpdateInstance(ctx, req))
	}
}

// writeError renders err as Google's REST APIs do:
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
