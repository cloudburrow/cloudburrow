package iamgate

import (
	"context"
	"encoding/binary"
	"net/http"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// testIAM is the service of a TestIamPermissions method of g's, which the
// gate answers for a caller with a token; nil for any other method.
func (g *Gate) testIAM(full string) *Service {
	for _, s := range g.Services {
		if s.TestIAM[full] {
			return s
		}
	}
	return nil
}

// answerTestIAM is a TestIamPermissions call's answer: the requested
// permissions the caller holds on the resource, as Google answers it
// (the method needs no permission of its own), by the host's evaluator
// with the resource's own policies. ok is false for the instance's own
// account (no token), whose call the front answers as before.
func (g *Gate) answerTestIAM(ctx context.Context, bearer, full string, m protoreflect.Message) (resp proto.Message, ok bool, err error) {
	svc := g.testIAM(full)
	if svc == nil || bearer == "" || m == nil {
		return nil, false, nil
	}
	req, isReq := m.Interface().(*iampb.TestIamPermissionsRequest)
	if !isReq || req.GetResource() == "" {
		return nil, false, nil
	}
	out := &iampb.TestIamPermissionsResponse{}
	if len(req.GetPermissions()) == 0 {
		return out, true, nil
	}
	res := req.GetResource()
	creq := iamcheck.Request{Bearer: bearer, Service: svc.Name, Permissions: req.GetPermissions(), Resource: res, Project: Project(res)}
	if svc.Type != nil {
		creq.Type = svc.Type(res)
	}
	if g.Own != nil {
		creq.Own = g.Own(res)
	}
	d, err := g.Client.Check(ctx, creq)
	if err != nil {
		return nil, true, status.Errorf(codes.Unavailable, "IAM check unavailable: %v", err)
	}
	if d.Unauthenticated {
		return nil, true, Unauthenticated()
	}
	for _, p := range req.GetPermissions() {
		if d.Holds(p) {
			out.Permissions = append(out.Permissions, p)
		}
	}
	return out, true, nil
}

// writeGRPCMessage answers a unary gRPC call with msg and an OK status.
func writeGRPCMessage(w http.ResponseWriter, msg proto.Message) {
	b, _ := proto.Marshal(msg)
	frame := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(b)))
	copy(frame[5:], b)
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
	h.Set("Grpc-Status", "0")
	h.Set("Grpc-Message", "")
}

// writeJSONMessage answers a REST call with msg.
func writeJSONMessage(w http.ResponseWriter, msg proto.Message) {
	b, _ := protojson.Marshal(msg)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
