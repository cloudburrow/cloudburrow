package kms

import (
	"context"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// DeleteKeyRing (#1595), as the REST reference documents it
// (https://docs.cloud.google.com/kms/docs/reference/rest/v1/projects.locations.keyRings/delete,
// read 2026-10-17): DELETE /v1/{name=projects/*/locations/*/keyRings/*},
// an empty body, an Operation in answer; it "Permanently deletes the given
// KeyRing. All child resources of the KeyRing must have been previously
// deleted using their corresponding Delete operations." The how-to
// (https://docs.cloud.google.com/kms/docs/delete-kms-resources, read
// 2026-10-17) adds: "All keys in the key ring must be deleted", "All
// import jobs in the key ring must be expired", and "the key ring name
// becomes available for reuse within approximately 5 to 10 minutes".
//
// googleapis' service.proto has no RPC for it yet, so the Go client
// cannot call it; gcloud's GA `kms keyrings delete` does, over REST. The
// gRPC method is served at the name Google's would have,
// /google.cloud.kms.v1.KeyManagementService/DeleteKeyRing, with a request
// of one string field 1, name: GetKeyRingRequest's shape, which is the
// request type used here. The deletion is immediate, so the Operation is
// returned done. A ring that still has a key, or an import job that has
// not expired, is FAILED_PRECONDITION naming it, and a name deleted less
// than 5 minutes ago is ALREADY_EXISTS on CreateKeyRing; the pages give no
// codes (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/kms/docs/delete-kms-resources

// deleteKeyRingMethod is the gRPC method DeleteKeyRing is served at.
const deleteKeyRingMethod = "/google.cloud.kms.v1.KeyManagementService/DeleteKeyRing"

// ringHoldPrefix keeps a deleted ring's name from being reused for
// ringReuseAfter, the low end of the documented 5 to 10 minutes.
const ringHoldPrefix = "kms/ringhold/"

const ringReuseAfter = 5 * time.Minute

type ringHold struct {
	Name    string    `json:"name"`
	Deleted time.Time `json:"deleted"`
}

// DeleteKeyRing permanently deletes an empty key ring.
func (s *Server) DeleteKeyRing(_ context.Context, req *kmspb.GetKeyRingRequest) (*longrunningpb.Operation, error) {
	if err := parseKeyRing("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r keyRing
	if err := s.load(dbKey(ringPrefix, req.GetName()), &r, "KeyRing", req.GetName()); err != nil {
		return nil, err
	}
	keys, err := s.list(keyPrefix, r.Name)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if len(keys) > 0 {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"KeyRing %s still has %d CryptoKeys, such as %s: every key must be deleted with DeleteCryptoKey first", r.Name, len(keys), keys[0]))
	}
	jobs, err := s.list(importJobPrefix, r.Name)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	now := s.now()
	for _, name := range jobs {
		var j importJob
		if err := s.load(dbKey(importJobPrefix, name), &j, "ImportJob", name); err != nil {
			return nil, err
		}
		if st := j.state(now); st != kmspb.ImportJob_EXPIRED {
			return nil, apierror.Wrap(apierror.FailedPrecondition(
				"KeyRing %s has ImportJob %s in state %s: every import job must be expired before the key ring is deleted", r.Name, name, st))
		}
	}
	// The hold first, so a failure between never frees the name early.
	if err := s.put(dbKey(ringHoldPrefix, r.Name), ringHold{Name: r.Name, Deleted: now.UTC()}); err != nil {
		return nil, apierror.Wrap(err)
	}
	for _, name := range jobs {
		if err := s.db.Delete(dbKey(importJobPrefix, name)); err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "delete %s", name))
		}
	}
	if err := s.db.Delete(dbKey(ringPrefix, r.Name)); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "delete %s", r.Name))
	}
	return doneOperation(locationOf(r.Name), nil)
}

// ringHeld reports whether name was deleted too recently to be created
// again. Called with s.mu held.
func (s *Server) ringHeld(name string) (bool, error) {
	var h ringHold
	found, err := s.get(dbKey(ringHoldPrefix, name), &h)
	if err != nil || !found {
		return false, err
	}
	if s.now().Before(h.Deleted.Add(ringReuseAfter)) {
		return true, nil
	}
	return false, s.db.Delete(dbKey(ringHoldPrefix, name))
}

// keyManagementServiceDesc is kmspb's KeyManagementService with
// DeleteKeyRing added, so gRPC and the JSON transcoder both serve it.
func keyManagementServiceDesc() *grpc.ServiceDesc {
	d := kmspb.KeyManagementService_ServiceDesc
	d.Methods = append(append([]grpc.MethodDesc(nil), d.Methods...), grpc.MethodDesc{
		MethodName: "DeleteKeyRing",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(kmspb.GetKeyRingRequest)
			if err := dec(in); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, req any) (any, error) {
				return srv.(*Server).DeleteKeyRing(ctx, req.(*kmspb.GetKeyRingRequest))
			}
			if interceptor == nil {
				return call(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: deleteKeyRingMethod}, call)
		},
	})
	return &d
}
