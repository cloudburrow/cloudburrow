package vertexai

import (
	"context"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// google.cloud.aiplatform.v1beta1.PredictionService serves what v1 serves
// (#1937): each method converts its v1beta1 request to v1 through the JSON
// mapping, calls the v1 method, and converts the answer back. The
// conversion is strict, so a v1beta1-only field the v1 handler would never
// read is refused by name instead of dropped. CountTokens, which v1 has only
// on LlmUtilityService, is served here and there (tokens.go).

// toV1 converts src to dst through protojson, refusing fields dst lacks.
func toV1(src, dst proto.Message) error {
	b, err := protojson.Marshal(src)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	if err := protojson.Unmarshal(b, dst); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown field") {
			return apierror.Unimplemented("%s: this field is v1beta1-only and is not served; v1beta1 serves what v1 serves", msg)
		}
		return apierror.InvalidArgument("%v", err)
	}
	return nil
}

// fromV1 converts a v1 answer to its v1beta1 message. Every v1 field exists
// in v1beta1, so unknown fields are an internal fault, not the caller's.
func fromV1(src, dst proto.Message) error {
	b, err := protojson.Marshal(src)
	if err != nil {
		return apierror.Internal(err, "encoding the response: %v", err)
	}
	if err := lenient.Unmarshal(b, dst); err != nil {
		return apierror.Internal(err, "the answer does not fit its v1beta1 message: %v", err)
	}
	return nil
}

func (b *betaPrediction) v1() *predictionService { return &predictionService{s: b.s} }

// unary bridges one unary method.
func unary[BReq, BResp, VReq, VResp proto.Message](req BReq, vreq VReq, bresp BResp,
	call func(VReq) (VResp, error)) (BResp, error) {
	var zero BResp
	if err := toV1(req, vreq); err != nil {
		return zero, grpcError(err)
	}
	out, err := call(vreq)
	if err != nil {
		return zero, err
	}
	if err := fromV1(out, bresp); err != nil {
		return zero, grpcError(err)
	}
	return bresp, nil
}

func (b *betaPrediction) Predict(ctx context.Context, req *betapb.PredictRequest) (*betapb.PredictResponse, error) {
	return unary(req, &aiplatformpb.PredictRequest{}, &betapb.PredictResponse{},
		func(v *aiplatformpb.PredictRequest) (*aiplatformpb.PredictResponse, error) {
			return b.v1().Predict(ctx, v)
		})
}

func (b *betaPrediction) RawPredict(ctx context.Context, req *betapb.RawPredictRequest) (*httpbody.HttpBody, error) {
	var v aiplatformpb.RawPredictRequest
	if err := toV1(req, &v); err != nil {
		return nil, grpcError(err)
	}
	return b.v1().RawPredict(ctx, &v)
}

func (b *betaPrediction) DirectPredict(ctx context.Context, req *betapb.DirectPredictRequest) (*betapb.DirectPredictResponse, error) {
	return unary(req, &aiplatformpb.DirectPredictRequest{}, &betapb.DirectPredictResponse{},
		func(v *aiplatformpb.DirectPredictRequest) (*aiplatformpb.DirectPredictResponse, error) {
			return b.v1().DirectPredict(ctx, v)
		})
}

func (b *betaPrediction) DirectRawPredict(ctx context.Context, req *betapb.DirectRawPredictRequest) (*betapb.DirectRawPredictResponse, error) {
	return unary(req, &aiplatformpb.DirectRawPredictRequest{}, &betapb.DirectRawPredictResponse{},
		func(v *aiplatformpb.DirectRawPredictRequest) (*aiplatformpb.DirectRawPredictResponse, error) {
			return b.v1().DirectRawPredict(ctx, v)
		})
}

func (b *betaPrediction) Explain(ctx context.Context, req *betapb.ExplainRequest) (*betapb.ExplainResponse, error) {
	return unary(req, &aiplatformpb.ExplainRequest{}, &betapb.ExplainResponse{},
		func(v *aiplatformpb.ExplainRequest) (*aiplatformpb.ExplainResponse, error) {
			return b.v1().Explain(ctx, v)
		})
}

func (b *betaPrediction) GenerateContent(ctx context.Context, req *betapb.GenerateContentRequest) (*betapb.GenerateContentResponse, error) {
	return unary(req, &aiplatformpb.GenerateContentRequest{}, &betapb.GenerateContentResponse{},
		func(v *aiplatformpb.GenerateContentRequest) (*aiplatformpb.GenerateContentResponse, error) {
			return b.v1().GenerateContent(ctx, v)
		})
}

func (b *betaPrediction) EmbedContent(ctx context.Context, req *betapb.EmbedContentRequest) (*betapb.EmbedContentResponse, error) {
	return unary(req, &aiplatformpb.EmbedContentRequest{}, &betapb.EmbedContentResponse{},
		func(v *aiplatformpb.EmbedContentRequest) (*aiplatformpb.EmbedContentResponse, error) {
			return b.v1().EmbedContent(ctx, v)
		})
}

func (b *betaPrediction) CountTokens(ctx context.Context, req *betapb.CountTokensRequest) (*betapb.CountTokensResponse, error) {
	return unary(req, &aiplatformpb.CountTokensRequest{}, &betapb.CountTokensResponse{},
		func(v *aiplatformpb.CountTokensRequest) (*aiplatformpb.CountTokensResponse, error) {
			return (&llmUtility{s: b.s}).CountTokens(ctx, v)
		})
}

// ---- server streaming

// sendBridge is a v1 server stream that sends on a v1beta1 one.
type sendBridge[V proto.Message] struct {
	grpc.ServerStream
	send func(V) error
}

func (s sendBridge[V]) Send(v V) error { return s.send(v) }

func (b *betaPrediction) StreamGenerateContent(req *betapb.GenerateContentRequest, stream betapb.PredictionService_StreamGenerateContentServer) error {
	var v aiplatformpb.GenerateContentRequest
	if err := toV1(req, &v); err != nil {
		return grpcError(err)
	}
	return b.v1().StreamGenerateContent(&v, sendBridge[*aiplatformpb.GenerateContentResponse]{stream,
		func(r *aiplatformpb.GenerateContentResponse) error {
			var out betapb.GenerateContentResponse
			if err := fromV1(r, &out); err != nil {
				return grpcError(err)
			}
			return stream.Send(&out)
		}})
}

func (b *betaPrediction) StreamRawPredict(req *betapb.StreamRawPredictRequest, stream betapb.PredictionService_StreamRawPredictServer) error {
	var v aiplatformpb.StreamRawPredictRequest
	if err := toV1(req, &v); err != nil {
		return grpcError(err)
	}
	return b.v1().StreamRawPredict(&v, sendBridge[*httpbody.HttpBody]{stream, stream.Send})
}

func (b *betaPrediction) ServerStreamingPredict(req *betapb.StreamingPredictRequest, stream betapb.PredictionService_ServerStreamingPredictServer) error {
	var v aiplatformpb.StreamingPredictRequest
	if err := toV1(req, &v); err != nil {
		return grpcError(err)
	}
	return b.v1().ServerStreamingPredict(&v, sendBridge[*aiplatformpb.StreamingPredictResponse]{stream,
		func(r *aiplatformpb.StreamingPredictResponse) error {
			var out betapb.StreamingPredictResponse
			if err := fromV1(r, &out); err != nil {
				return grpcError(err)
			}
			return stream.Send(&out)
		}})
}

// ---- bidirectional streaming

// bidiBridge is a v1 bidirectional stream over a v1beta1 one.
type bidiBridge[VReq, VResp proto.Message] struct {
	grpc.ServerStream
	send func(VResp) error
	recv func() (VReq, error)
}

func (s bidiBridge[VReq, VResp]) Send(v VResp) error  { return s.send(v) }
func (s bidiBridge[VReq, VResp]) Recv() (VReq, error) { return s.recv() }

// bridgeRecv reads a v1beta1 message and converts it.
func bridgeRecv[B, V proto.Message](recv func() (B, error), v func() V) func() (V, error) {
	return func() (V, error) {
		var zero V
		in, err := recv()
		if err != nil {
			return zero, err
		}
		out := v()
		if err := toV1(in, out); err != nil {
			return zero, grpcError(err)
		}
		return out, nil
	}
}

// bridgeSend converts a v1 message and sends it.
func bridgeSend[V, B proto.Message](send func(B) error, b func() B) func(V) error {
	return func(r V) error {
		out := b()
		if err := fromV1(r, out); err != nil {
			return grpcError(err)
		}
		return send(out)
	}
}

func (b *betaPrediction) StreamingPredict(stream betapb.PredictionService_StreamingPredictServer) error {
	return b.v1().StreamingPredict(bidiBridge[*aiplatformpb.StreamingPredictRequest, *aiplatformpb.StreamingPredictResponse]{stream,
		bridgeSend[*aiplatformpb.StreamingPredictResponse](stream.Send, func() *betapb.StreamingPredictResponse { return &betapb.StreamingPredictResponse{} }),
		bridgeRecv(stream.Recv, func() *aiplatformpb.StreamingPredictRequest { return &aiplatformpb.StreamingPredictRequest{} })})
}

func (b *betaPrediction) StreamDirectPredict(stream betapb.PredictionService_StreamDirectPredictServer) error {
	return b.v1().StreamDirectPredict(bidiBridge[*aiplatformpb.StreamDirectPredictRequest, *aiplatformpb.StreamDirectPredictResponse]{stream,
		bridgeSend[*aiplatformpb.StreamDirectPredictResponse](stream.Send, func() *betapb.StreamDirectPredictResponse { return &betapb.StreamDirectPredictResponse{} }),
		bridgeRecv(stream.Recv, func() *aiplatformpb.StreamDirectPredictRequest { return &aiplatformpb.StreamDirectPredictRequest{} })})
}

func (b *betaPrediction) StreamDirectRawPredict(stream betapb.PredictionService_StreamDirectRawPredictServer) error {
	return b.v1().StreamDirectRawPredict(bidiBridge[*aiplatformpb.StreamDirectRawPredictRequest, *aiplatformpb.StreamDirectRawPredictResponse]{stream,
		bridgeSend[*aiplatformpb.StreamDirectRawPredictResponse](stream.Send, func() *betapb.StreamDirectRawPredictResponse { return &betapb.StreamDirectRawPredictResponse{} }),
		bridgeRecv(stream.Recv, func() *aiplatformpb.StreamDirectRawPredictRequest {
			return &aiplatformpb.StreamDirectRawPredictRequest{}
		})})
}

func (b *betaPrediction) StreamingRawPredict(stream betapb.PredictionService_StreamingRawPredictServer) error {
	return b.v1().StreamingRawPredict(bidiBridge[*aiplatformpb.StreamingRawPredictRequest, *aiplatformpb.StreamingRawPredictResponse]{stream,
		bridgeSend[*aiplatformpb.StreamingRawPredictResponse](stream.Send, func() *betapb.StreamingRawPredictResponse { return &betapb.StreamingRawPredictResponse{} }),
		bridgeRecv(stream.Recv, func() *aiplatformpb.StreamingRawPredictRequest { return &aiplatformpb.StreamingRawPredictRequest{} })})
}
