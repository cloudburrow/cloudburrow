package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The Reservation API's gRPC transport (#2193):
// google.cloud.bigquery.reservation.v1.ReservationService, served on the
// front's gRPC port, each call transcoded to the REST method
// reservations.go serves, as the service's google.api.http annotations
// map it (reservation.proto), so both transports share one
// implementation and one state, as the Connection API's do
// (connectiongrpc.go). A REST error's status is the call's code.
const reservationService = "/google.cloud.bigquery.reservation.v1.ReservationService/"

// resvRoute is one method's REST call: its request and response
// messages, and how the request maps to the verb, path, query and body.
type resvRoute struct {
	req, resp func() proto.Message
	call      func(in proto.Message) (verb, path string, q url.Values, body proto.Message)
}

func pageQuery(size int32, token string) url.Values {
	q := url.Values{}
	if size != 0 {
		q.Set("pageSize", strconv.Itoa(int(size)))
	}
	if token != "" {
		q.Set("pageToken", token)
	}
	return q
}

func maskQuery(paths []string) url.Values {
	q := url.Values{}
	if len(paths) > 0 {
		q.Set("updateMask", strings.Join(paths, ","))
	}
	return q
}

var resvRoutes = map[string]resvRoute{
	"CreateReservation": {func() proto.Message { return &reservationpb.CreateReservationRequest{} }, func() proto.Message { return &reservationpb.Reservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.CreateReservationRequest)
			q := url.Values{}
			if r.GetReservationId() != "" {
				q.Set("reservationId", r.GetReservationId())
			}
			return http.MethodPost, r.GetParent() + "/reservations", q, r.GetReservation()
		}},
	"ListReservations": {func() proto.Message { return &reservationpb.ListReservationsRequest{} }, func() proto.Message { return &reservationpb.ListReservationsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.ListReservationsRequest)
			return http.MethodGet, r.GetParent() + "/reservations", pageQuery(r.GetPageSize(), r.GetPageToken()), nil
		}},
	"GetReservation": {func() proto.Message { return &reservationpb.GetReservationRequest{} }, func() proto.Message { return &reservationpb.Reservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodGet, m.(*reservationpb.GetReservationRequest).GetName(), nil, nil
		}},
	"DeleteReservation": {func() proto.Message { return &reservationpb.DeleteReservationRequest{} }, func() proto.Message { return &emptypb.Empty{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodDelete, m.(*reservationpb.DeleteReservationRequest).GetName(), nil, nil
		}},
	"UpdateReservation": {func() proto.Message { return &reservationpb.UpdateReservationRequest{} }, func() proto.Message { return &reservationpb.Reservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.UpdateReservationRequest)
			return http.MethodPatch, r.GetReservation().GetName(), maskQuery(r.GetUpdateMask().GetPaths()), r.GetReservation()
		}},
	"FailoverReservation": {func() proto.Message { return &reservationpb.FailoverReservationRequest{} }, func() proto.Message { return &reservationpb.Reservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.FailoverReservationRequest)
			return http.MethodPost, r.GetName() + ":failoverReservation", nil, &reservationpb.FailoverReservationRequest{FailoverMode: r.GetFailoverMode()}
		}},
	"CreateCapacityCommitment": {func() proto.Message { return &reservationpb.CreateCapacityCommitmentRequest{} }, func() proto.Message { return &reservationpb.CapacityCommitment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.CreateCapacityCommitmentRequest)
			q := url.Values{}
			if r.GetCapacityCommitmentId() != "" {
				q.Set("capacityCommitmentId", r.GetCapacityCommitmentId())
			}
			if r.GetEnforceSingleAdminProjectPerOrg() {
				q.Set("enforceSingleAdminProjectPerOrg", "true")
			}
			return http.MethodPost, r.GetParent() + "/capacityCommitments", q, r.GetCapacityCommitment()
		}},
	"ListCapacityCommitments": {func() proto.Message { return &reservationpb.ListCapacityCommitmentsRequest{} }, func() proto.Message { return &reservationpb.ListCapacityCommitmentsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.ListCapacityCommitmentsRequest)
			return http.MethodGet, r.GetParent() + "/capacityCommitments", pageQuery(r.GetPageSize(), r.GetPageToken()), nil
		}},
	"GetCapacityCommitment": {func() proto.Message { return &reservationpb.GetCapacityCommitmentRequest{} }, func() proto.Message { return &reservationpb.CapacityCommitment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodGet, m.(*reservationpb.GetCapacityCommitmentRequest).GetName(), nil, nil
		}},
	"DeleteCapacityCommitment": {func() proto.Message { return &reservationpb.DeleteCapacityCommitmentRequest{} }, func() proto.Message { return &emptypb.Empty{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.DeleteCapacityCommitmentRequest)
			q := url.Values{}
			if r.GetForce() {
				q.Set("force", "true")
			}
			return http.MethodDelete, r.GetName(), q, nil
		}},
	"UpdateCapacityCommitment": {func() proto.Message { return &reservationpb.UpdateCapacityCommitmentRequest{} }, func() proto.Message { return &reservationpb.CapacityCommitment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.UpdateCapacityCommitmentRequest)
			return http.MethodPatch, r.GetCapacityCommitment().GetName(), maskQuery(r.GetUpdateMask().GetPaths()), r.GetCapacityCommitment()
		}},
	"SplitCapacityCommitment": {func() proto.Message { return &reservationpb.SplitCapacityCommitmentRequest{} }, func() proto.Message { return &reservationpb.SplitCapacityCommitmentResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.SplitCapacityCommitmentRequest)
			return http.MethodPost, r.GetName() + ":split", nil, &reservationpb.SplitCapacityCommitmentRequest{SlotCount: r.GetSlotCount()}
		}},
	"MergeCapacityCommitments": {func() proto.Message { return &reservationpb.MergeCapacityCommitmentsRequest{} }, func() proto.Message { return &reservationpb.CapacityCommitment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.MergeCapacityCommitmentsRequest)
			return http.MethodPost, r.GetParent() + "/capacityCommitments:merge", nil,
				&reservationpb.MergeCapacityCommitmentsRequest{CapacityCommitmentIds: r.GetCapacityCommitmentIds(), CapacityCommitmentId: r.GetCapacityCommitmentId()}
		}},
	"CreateAssignment": {func() proto.Message { return &reservationpb.CreateAssignmentRequest{} }, func() proto.Message { return &reservationpb.Assignment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.CreateAssignmentRequest)
			q := url.Values{}
			if r.GetAssignmentId() != "" {
				q.Set("assignmentId", r.GetAssignmentId())
			}
			return http.MethodPost, r.GetParent() + "/assignments", q, r.GetAssignment()
		}},
	"ListAssignments": {func() proto.Message { return &reservationpb.ListAssignmentsRequest{} }, func() proto.Message { return &reservationpb.ListAssignmentsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.ListAssignmentsRequest)
			return http.MethodGet, r.GetParent() + "/assignments", pageQuery(r.GetPageSize(), r.GetPageToken()), nil
		}},
	"DeleteAssignment": {func() proto.Message { return &reservationpb.DeleteAssignmentRequest{} }, func() proto.Message { return &emptypb.Empty{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodDelete, m.(*reservationpb.DeleteAssignmentRequest).GetName(), nil, nil
		}},
	"SearchAssignments": {func() proto.Message { return &reservationpb.SearchAssignmentsRequest{} }, func() proto.Message { return &reservationpb.SearchAssignmentsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.SearchAssignmentsRequest)
			q := pageQuery(r.GetPageSize(), r.GetPageToken())
			q.Set("query", r.GetQuery())
			return http.MethodGet, r.GetParent() + ":searchAssignments", q, nil
		}},
	"SearchAllAssignments": {func() proto.Message { return &reservationpb.SearchAllAssignmentsRequest{} }, func() proto.Message { return &reservationpb.SearchAllAssignmentsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.SearchAllAssignmentsRequest)
			q := pageQuery(r.GetPageSize(), r.GetPageToken())
			q.Set("query", r.GetQuery())
			return http.MethodGet, r.GetParent() + ":searchAllAssignments", q, nil
		}},
	"MoveAssignment": {func() proto.Message { return &reservationpb.MoveAssignmentRequest{} }, func() proto.Message { return &reservationpb.Assignment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.MoveAssignmentRequest)
			return http.MethodPost, r.GetName() + ":move", nil,
				&reservationpb.MoveAssignmentRequest{DestinationId: r.GetDestinationId(), AssignmentId: r.GetAssignmentId()}
		}},
	"UpdateAssignment": {func() proto.Message { return &reservationpb.UpdateAssignmentRequest{} }, func() proto.Message { return &reservationpb.Assignment{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.UpdateAssignmentRequest)
			return http.MethodPatch, r.GetAssignment().GetName(), maskQuery(r.GetUpdateMask().GetPaths()), r.GetAssignment()
		}},
	"GetBiReservation": {func() proto.Message { return &reservationpb.GetBiReservationRequest{} }, func() proto.Message { return &reservationpb.BiReservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodGet, m.(*reservationpb.GetBiReservationRequest).GetName(), nil, nil
		}},
	"UpdateBiReservation": {func() proto.Message { return &reservationpb.UpdateBiReservationRequest{} }, func() proto.Message { return &reservationpb.BiReservation{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.UpdateBiReservationRequest)
			return http.MethodPatch, r.GetBiReservation().GetName(), maskQuery(r.GetUpdateMask().GetPaths()), r.GetBiReservation()
		}},
	"CreateReservationGroup": {func() proto.Message { return &reservationpb.CreateReservationGroupRequest{} }, func() proto.Message { return &reservationpb.ReservationGroup{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.CreateReservationGroupRequest)
			q := url.Values{}
			q.Set("reservationGroupId", r.GetReservationGroupId())
			return http.MethodPost, r.GetParent() + "/reservationGroups", q, r.GetReservationGroup()
		}},
	"GetReservationGroup": {func() proto.Message { return &reservationpb.GetReservationGroupRequest{} }, func() proto.Message { return &reservationpb.ReservationGroup{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodGet, m.(*reservationpb.GetReservationGroupRequest).GetName(), nil, nil
		}},
	"ListReservationGroups": {func() proto.Message { return &reservationpb.ListReservationGroupsRequest{} }, func() proto.Message { return &reservationpb.ListReservationGroupsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*reservationpb.ListReservationGroupsRequest)
			return http.MethodGet, r.GetParent() + "/reservationGroups", pageQuery(r.GetPageSize(), r.GetPageToken()), nil
		}},
	"DeleteReservationGroup": {func() proto.Message { return &reservationpb.DeleteReservationGroupRequest{} }, func() proto.Message { return &emptypb.Empty{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			return http.MethodDelete, m.(*reservationpb.DeleteReservationGroupRequest).GetName(), nil, nil
		}},
	"GetIamPolicy": {func() proto.Message { return &iampb.GetIamPolicyRequest{} }, func() proto.Message { return &iampb.Policy{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*iampb.GetIamPolicyRequest)
			return http.MethodPost, r.GetResource() + ":getIamPolicy", nil, &iampb.GetIamPolicyRequest{Options: r.GetOptions()}
		}},
	"SetIamPolicy": {func() proto.Message { return &iampb.SetIamPolicyRequest{} }, func() proto.Message { return &iampb.Policy{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*iampb.SetIamPolicyRequest)
			return http.MethodPost, r.GetResource() + ":setIamPolicy", nil, &iampb.SetIamPolicyRequest{Policy: r.GetPolicy(), UpdateMask: r.GetUpdateMask()}
		}},
	"TestIamPermissions": {func() proto.Message { return &iampb.TestIamPermissionsRequest{} }, func() proto.Message { return &iampb.TestIamPermissionsResponse{} },
		func(m proto.Message) (string, string, url.Values, proto.Message) {
			r := m.(*iampb.TestIamPermissionsRequest)
			return http.MethodPost, r.GetResource() + ":testIamPermissions", nil, &iampb.TestIamPermissionsRequest{Permissions: r.GetPermissions()}
		}},
}

// reservation serves one ReservationService call on ss.
func (s *storageRead) reservation(ss grpc.ServerStream, method string) error {
	if s.write == nil {
		return status.Error(codes.Unimplemented, "the BigQuery Reservation API is not served here")
	}
	name := method[len(reservationService):]
	rt, ok := resvRoutes[name]
	if !ok {
		return status.Errorf(codes.Unimplemented, "unknown method %s", method)
	}
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	req := rt.req()
	if err := proto.Unmarshal(in, req); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	verb, path, q, body := rt.call(req)
	if !strings.HasPrefix(path, "projects/") {
		return status.Errorf(codes.InvalidArgument, "%s: the resource name %q is not projects/…/locations/…", name, path)
	}
	var bodyJSON []byte
	if body != nil {
		b, err := protojson.Marshal(body)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		bodyJSON = b
	}
	u := "http://bigquery/v1/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	hr, err := http.NewRequestWithContext(ss.Context(), verb, u, bytes.NewReader(bodyJSON))
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
		return reservationStatus(code, rec.body.Bytes())
	}
	resp := rt.resp()
	if out := bytes.TrimSpace(rec.body.Bytes()); len(out) > 0 {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, resp); err != nil {
			return status.Errorf(codes.Internal, "%s: the REST answer does not decode: %v", name, err)
		}
	}
	return ss.SendMsg(resp)
}

// reservationStatus is the gRPC status of a REST error answer: its
// status name, else its HTTP code's.
func reservationStatus(code int, body []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Status != "" {
		if c, ok := codeByName[e.Error.Status]; ok {
			return status.Error(c, e.Error.Message)
		}
	}
	return connectionStatus(code, body)
}

var codeByName = func() map[string]codes.Code {
	m := map[string]codes.Code{}
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		m[strings.ToUpper(snake(c.String()))] = c
	}
	m["CANCELLED"] = codes.Canceled
	return m
}()

// snake turns a code's Go name (InvalidArgument) into its canonical name
// (INVALID_ARGUMENT), upper-cased by the caller.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return b.String()
}
