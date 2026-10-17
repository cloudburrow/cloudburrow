package run

import (
	"net/http"
	"strconv"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Cloud Run v1's projects.locations.operations (#1533): the same
// long-running operations v2 creates, under the same
// projects/{project}/locations/{location}/operations collection, read,
// listed, waited on and deleted through v2's Operations server. v1 binds no
// cancel, as v2 does not.

func (s *Server) v1OperationName(r *http.Request) string {
	return "projects/" + r.PathValue("ns") + "/locations/" + r.PathValue("loc") + "/operations/" + r.PathValue("name")
}

func (s *Server) v1ListOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := &longrunningpb.ListOperationsRequest{Name: "projects/" + r.PathValue("ns") + "/locations/" + r.PathValue("loc"),
		Filter: q.Get("filter"), PageToken: q.Get("pageToken")}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			v1Write(w, apierror.InvalidArgument("pageSize %q is not a non-negative number", v), nil)
			return
		}
		req.PageSize = int32(n)
	}
	resp, err := NewOperationsServer(s).ListOperations(r.Context(), req)
	if resp != nil && resp.Operations == nil {
		resp.Operations = []*longrunningpb.Operation{}
	}
	v1Proto(w, resp, err)
}

func (s *Server) v1GetOperation(w http.ResponseWriter, r *http.Request) {
	if _, err := resourceName(r); err != nil {
		v1Write(w, err, nil)
		return
	}
	op, err := NewOperationsServer(s).GetOperation(r.Context(), &longrunningpb.GetOperationRequest{Name: s.v1OperationName(r)})
	v1Proto(w, op, err)
}

// v1DeleteOperation answers an empty body for an operation that exists, and
// NOT_FOUND for one that does not, which v1's Empty response leaves no room
// to say otherwise.
func (s *Server) v1DeleteOperation(w http.ResponseWriter, r *http.Request) {
	if _, err := resourceName(r); err != nil {
		v1Write(w, err, nil)
		return
	}
	name := s.v1OperationName(r)
	if _, err := s.toProtoOperation(name); err != nil {
		v1Write(w, err, nil)
		return
	}
	_, err := NewOperationsServer(s).DeleteOperation(r.Context(), &longrunningpb.DeleteOperationRequest{Name: name})
	v1Write(w, err, struct{}{})
}

// v1WaitOperation is POST {name}:wait, whose body is
// GoogleLongrunningWaitOperationRequest: a timeout, as a duration string.
func (s *Server) v1WaitOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("name"), ":wait")
	if !ok || strings.Contains(id, ":") {
		v1Write(w, apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path), nil)
		return
	}
	req := &longrunningpb.WaitOperationRequest{}
	if err := v1DecodeProto(r, req); err != nil {
		v1Write(w, err, nil)
		return
	}
	req.Name = "projects/" + r.PathValue("ns") + "/locations/" + r.PathValue("loc") + "/operations/" + id
	op, err := NewOperationsServer(s).WaitOperation(r.Context(), req)
	v1Proto(w, op, err)
}

// v1Proto writes a proto response as v1's JSON, which is proto3 JSON.
func v1Proto(w http.ResponseWriter, m proto.Message, err error) {
	if err != nil {
		rest.WriteError(w, err)
		return
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		rest.WriteError(w, apierror.Internal(err, "encode response"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}
