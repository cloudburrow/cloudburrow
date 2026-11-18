package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The BigQuery Connection API's gRPC transport (#1766):
// google.cloud.bigquery.connection.v1.ConnectionService, served on the
// front's gRPC port (the Storage Read and Write APIs'), each call
// transcoded to the REST method connections.go serves, as the service's
// google.api.http annotations map it (connection.proto), so both
// transports share one implementation and one state. A REST error is
// the gRPC status of its HTTP code.
const connectionService = "/google.cloud.bigquery.connection.v1.ConnectionService/"

// connection serves one ConnectionService call on ss.
func (s *storageRead) connection(ss grpc.ServerStream, method string) error {
	if s.write == nil {
		return status.Error(codes.Unimplemented, "the BigQuery Connection API is not served here")
	}
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var (
		resp     proto.Message
		verb     string
		path     string
		body     proto.Message
		query    = url.Values{}
		bodyJSON []byte
	)
	name := method[len(connectionService):]
	switch name {
	case "CreateConnection":
		r := &connectionpb.CreateConnectionRequest{}
		resp, verb = &connectionpb.Connection{}, http.MethodPost
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path, body = r.GetParent()+"/connections", r.GetConnection()
		if r.GetConnectionId() != "" {
			query.Set("connectionId", r.GetConnectionId())
		}
	case "GetConnection":
		r := &connectionpb.GetConnectionRequest{}
		resp, verb = &connectionpb.Connection{}, http.MethodGet
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetName()
	case "ListConnections":
		r := &connectionpb.ListConnectionsRequest{}
		resp, verb = &connectionpb.ListConnectionsResponse{}, http.MethodGet
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetParent() + "/connections"
		if r.GetPageSize() != 0 {
			query.Set("pageSize", strings.TrimSpace(protojsonInt(r.GetPageSize())))
		}
		if r.GetPageToken() != "" {
			query.Set("pageToken", r.GetPageToken())
		}
	case "UpdateConnection":
		r := &connectionpb.UpdateConnectionRequest{}
		resp, verb = &connectionpb.Connection{}, http.MethodPatch
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path, body = r.GetName(), r.GetConnection()
		if m := r.GetUpdateMask(); m != nil {
			query.Set("updateMask", strings.Join(m.GetPaths(), ","))
		}
	case "DeleteConnection":
		r := &connectionpb.DeleteConnectionRequest{}
		resp, verb = &emptypb.Empty{}, http.MethodDelete
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetName()
	case "GetIamPolicy":
		r := &iampb.GetIamPolicyRequest{}
		resp, verb = &iampb.Policy{}, http.MethodPost
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetResource() + ":getIamPolicy"
		b, _ := protojson.Marshal(&iampb.GetIamPolicyRequest{Options: r.GetOptions()})
		bodyJSON = b
	case "SetIamPolicy":
		r := &iampb.SetIamPolicyRequest{}
		resp, verb = &iampb.Policy{}, http.MethodPost
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetResource() + ":setIamPolicy"
		b, _ := protojson.Marshal(&iampb.SetIamPolicyRequest{Policy: r.GetPolicy(), UpdateMask: r.GetUpdateMask()})
		bodyJSON = b
	case "TestIamPermissions":
		r := &iampb.TestIamPermissionsRequest{}
		resp, verb = &iampb.TestIamPermissionsResponse{}, http.MethodPost
		if err := proto.Unmarshal(in, r); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		path = r.GetResource() + ":testIamPermissions"
		b, _ := protojson.Marshal(&iampb.TestIamPermissionsRequest{Permissions: r.GetPermissions()})
		bodyJSON = b
	default:
		return status.Errorf(codes.Unimplemented, "unknown method %s", method)
	}
	if !strings.HasPrefix(path, "projects/") {
		return status.Errorf(codes.InvalidArgument, "%s: the resource name %q is not projects/…/locations/…", name, path)
	}
	if body != nil {
		b, err := protojson.Marshal(body)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		bodyJSON = b
	}
	u := "http://bigquery/v1/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd *bytes.Reader
	if bodyJSON != nil {
		rd = bytes.NewReader(bodyJSON)
	} else {
		rd = bytes.NewReader(nil)
	}
	hr, err := http.NewRequestWithContext(ss.Context(), verb, u, rd)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if bodyJSON != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	rec := newRecorder()
	s.write.rest.ServeHTTP(rec, hr)
	code := rec.status
	if code == 0 {
		code = http.StatusOK
	}
	if code != http.StatusOK {
		return connectionStatus(code, rec.body.Bytes())
	}
	if out := bytes.TrimSpace(rec.body.Bytes()); len(out) > 0 {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, resp); err != nil {
			return status.Errorf(codes.Internal, "%s: the REST answer does not decode: %v", name, err)
		}
	}
	return ss.SendMsg(resp)
}

func protojsonInt(v int32) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// connectionStatus is the gRPC status of a REST error answer, as Google
// maps HTTP codes to canonical codes (google.rpc.Code).
func connectionStatus(code int, body []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	c := map[int]codes.Code{
		http.StatusBadRequest: codes.InvalidArgument, http.StatusUnauthorized: codes.Unauthenticated, http.StatusForbidden: codes.PermissionDenied,
		http.StatusNotFound: codes.NotFound, http.StatusConflict: codes.AlreadyExists, http.StatusPreconditionFailed: codes.FailedPrecondition,
		http.StatusTooManyRequests: codes.ResourceExhausted, http.StatusNotImplemented: codes.Unimplemented,
		http.StatusServiceUnavailable: codes.Unavailable, http.StatusInternalServerError: codes.Internal,
	}[code]
	if c == codes.OK {
		c = codes.Unknown
	}
	if code == http.StatusConflict && strings.Contains(msg, "etag") {
		// A stale policy is ABORTED over gRPC, as google.rpc.Code
		// documents for a read-modify-write conflict. UNVERIFIED
		// (re-read 2026-11-17): Google does not list the Connection
		// API's codes.
		c = codes.Aborted
	}
	return status.Error(c, msg)
}
