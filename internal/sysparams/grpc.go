package sysparams

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// FieldMaskMetadata is the gRPC metadata key of the response field mask,
// X-Goog-FieldMask's (https://docs.cloud.google.com/apis/docs/system-parameters).
const FieldMaskMetadata = "x-goog-fieldmask"

// incomingMask is the mask a call's metadata names, nil for none.
func incomingMask(ctx context.Context) (Mask, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, nil
	}
	vs := md.Get(FieldMaskMetadata)
	if len(vs) == 0 {
		return nil, nil
	}
	return Parse(strings.Join(vs, ","))
}

// UnaryServerInterceptor applies x-goog-fieldmask to a unary call's
// response: a mask naming a field the response has not is
// INVALID_ARGUMENT, as over REST, and every field it does not select is
// cleared.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		mask, err := incomingMask(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := handler(ctx, req)
		if err != nil || mask == nil {
			return resp, err
		}
		return pruneMessage(mask, resp)
	}
}

func pruneMessage(mask Mask, resp any) (any, error) {
	m, ok := resp.(proto.Message)
	if !ok {
		return resp, nil
	}
	if err := mask.Validate(m.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	// The handler may hold the message it returned (a cache, a store):
	// the clone is pruned, never it.
	c := proto.Clone(m)
	mask.PruneProto(c.ProtoReflect())
	return c, nil
}

// StreamServerInterceptor applies x-goog-fieldmask to each message a
// streaming call sends.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		mask, err := incomingMask(ss.Context())
		if err != nil {
			return err
		}
		if mask == nil {
			return handler(srv, ss)
		}
		return handler(srv, &maskedStream{ServerStream: ss, mask: mask})
	}
}

type maskedStream struct {
	grpc.ServerStream
	mask Mask
}

func (s *maskedStream) SendMsg(m any) error {
	pruned, err := pruneMessage(s.mask, m)
	if err != nil {
		return err
	}
	return s.ServerStream.SendMsg(pruned)
}
