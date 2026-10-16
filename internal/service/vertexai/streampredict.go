package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Streaming and direct prediction (#1285).
//
// Vertex documents two families beside Predict and RawPredict:
//
//   - Tensor methods — DirectPredict (unary), ServerStreamingPredict (server
//     stream), StreamingPredict and StreamDirectPredict (bidirectional) — take
//     inputs and parameters as Tensors and answer outputs as Tensors. A Tensor
//     is the typed form of the JSON value an instance or prediction is, so
//     each input becomes the instance Predict would read, and each prediction
//     becomes an output, with the same code Predict runs behind it.
//   - StreamRawPredict streams HttpBody chunks of the model's native answer,
//     as RawPredict answers it whole.
//
// What each local model does with them:
//
//   - the embedding model answers once per request: an embedding is not
//     produced in pieces, so a stream carries one message;
//   - the generation model streams one message per chunk the runtime decodes,
//     as StreamGenerateContent does. Its tensor input is the Model Garden
//     text instance, {"prompt": "..."}, and each output is {"content": "..."};
//   - a custom endpoint is relayed as Predict and RawPredict relay it: the
//     container's HTTP contract has no streaming route, so a stream carries
//     its one answer.
//
// DirectRawPredict, StreamDirectRawPredict and StreamingRawPredict call
// method_name on a gRPC model server. Nothing here is one — the local models
// are served by this process and custom containers speak HTTP — so they are
// UNIMPLEMENTED, naming method_name.

// tensorValue converts an input Tensor to the JSON value it carries. path
// names the field in refusals.
func tensorValue(path string, t *aiplatformpb.Tensor) (*structpb.Value, error) {
	if t == nil {
		return structpb.NewNullValue(), nil
	}
	if len(t.GetTensorVal()) > 0 {
		return nil, apierror.InvalidArgument("%s.tensor_val: serialized tensor bytes are not read; "+
			"send the values in the typed fields", path)
	}
	if len(t.GetBytesVal()) > 0 {
		return nil, apierror.InvalidArgument("%s.bytes_val: no local model reads bytes", path)
	}
	if len(t.GetStructVal()) > 0 {
		fields := map[string]*structpb.Value{}
		for k, v := range t.GetStructVal() {
			x, err := tensorValue(path+".struct_val."+k, v)
			if err != nil {
				return nil, err
			}
			fields[k] = x
		}
		return structpb.NewStructValue(&structpb.Struct{Fields: fields}), nil
	}
	if len(t.GetListVal()) > 0 {
		vals := make([]*structpb.Value, len(t.GetListVal()))
		for i, v := range t.GetListVal() {
			x, err := tensorValue(fmt.Sprintf("%s.list_val[%d]", path, i), v)
			if err != nil {
				return nil, err
			}
			vals[i] = x
		}
		return structpb.NewListValue(&structpb.ListValue{Values: vals}), nil
	}
	var vals []*structpb.Value
	kinds := 0
	add := func(n int, f func(i int) *structpb.Value) {
		if n == 0 {
			return
		}
		kinds++
		for i := 0; i < n; i++ {
			vals = append(vals, f(i))
		}
	}
	add(len(t.GetBoolVal()), func(i int) *structpb.Value { return structpb.NewBoolValue(t.GetBoolVal()[i]) })
	add(len(t.GetStringVal()), func(i int) *structpb.Value { return structpb.NewStringValue(t.GetStringVal()[i]) })
	add(len(t.GetFloatVal()), func(i int) *structpb.Value { return structpb.NewNumberValue(float64(t.GetFloatVal()[i])) })
	add(len(t.GetDoubleVal()), func(i int) *structpb.Value { return structpb.NewNumberValue(t.GetDoubleVal()[i]) })
	add(len(t.GetIntVal()), func(i int) *structpb.Value { return structpb.NewNumberValue(float64(t.GetIntVal()[i])) })
	add(len(t.GetInt64Val()), func(i int) *structpb.Value { return structpb.NewNumberValue(float64(t.GetInt64Val()[i])) })
	add(len(t.GetUintVal()), func(i int) *structpb.Value { return structpb.NewNumberValue(float64(t.GetUintVal()[i])) })
	add(len(t.GetUint64Val()), func(i int) *structpb.Value { return structpb.NewNumberValue(float64(t.GetUint64Val()[i])) })
	switch {
	case kinds > 1:
		return nil, apierror.InvalidArgument("%s sets values of more than one type; a tensor holds one", path)
	case len(vals) == 0:
		return structpb.NewNullValue(), nil
	case len(vals) == 1 && len(t.GetShape()) == 0:
		return vals[0], nil
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vals}), nil
}

// valueTensor converts a JSON value to the Tensor that carries it: a string,
// number or bool as a scalar of that type, a list of numbers as a DOUBLE
// vector, any other list as list_val, an object as struct_val.
func valueTensor(v any) *aiplatformpb.Tensor {
	switch x := v.(type) {
	case nil:
		return &aiplatformpb.Tensor{}
	case bool:
		return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_BOOL, BoolVal: []bool{x}}
	case string:
		return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_STRING, StringVal: []string{x}}
	case float64:
		return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_DOUBLE, DoubleVal: []float64{x}}
	case map[string]any:
		out := &aiplatformpb.Tensor{StructVal: map[string]*aiplatformpb.Tensor{}}
		for k, e := range x {
			out.StructVal[k] = valueTensor(e)
		}
		return out
	case []any:
		nums := make([]float64, 0, len(x))
		for _, e := range x {
			f, ok := e.(float64)
			if !ok {
				nums = nil
				break
			}
			nums = append(nums, f)
		}
		if nums != nil && len(nums) == len(x) && len(x) > 0 {
			return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_DOUBLE, Shape: []int64{int64(len(x))}, DoubleVal: nums}
		}
		out := &aiplatformpb.Tensor{}
		for _, e := range x {
			out.ListVal = append(out.ListVal, valueTensor(e))
		}
		return out
	}
	return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_STRING, StringVal: []string{fmt.Sprint(v)}}
}

// predictionTensors converts a :predict answer's predictions to outputs.
func predictionTensors(answer []byte) ([]*aiplatformpb.Tensor, error) {
	var resp struct {
		Predictions []any `json:"predictions"`
	}
	if err := json.Unmarshal(answer, &resp); err != nil {
		return nil, apierror.Internal(err, "the answer is not a prediction response: %v", err)
	}
	out := make([]*aiplatformpb.Tensor, len(resp.Predictions))
	for i, p := range resp.Predictions {
		out[i] = valueTensor(p)
	}
	return out, nil
}

// generationPrompt reads the Model Garden text instance, {"prompt": "..."},
// from the inputs. Every other field is refused by name: the runtime applies
// no generation option, so a max_tokens or temperature would be ignored.
func generationPrompt(inputs []*aiplatformpb.Tensor, params *aiplatformpb.Tensor) (string, error) {
	if params != nil && (len(params.GetStructVal()) > 0 || len(params.GetListVal()) > 0 || params.GetDtype() != aiplatformpb.Tensor_DATA_TYPE_UNSPECIFIED) {
		return "", apierror.InvalidArgument("parameters cannot be honoured: the LiteRT-LM runtime applies no generation option, " +
			"and accepting one would report a setting that was never applied")
	}
	if len(inputs) != 1 {
		return "", apierror.InvalidArgument("inputs: the generation model takes exactly one input, {\"prompt\": \"...\"}; got %d", len(inputs))
	}
	fields := inputs[0].GetStructVal()
	var extra []string
	for k := range fields {
		if k != "prompt" {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return "", apierror.InvalidArgument("inputs[0].struct_val.%s cannot be honoured: the generation model reads only prompt "+
			"and applies no generation option", strings.Join(extra, ", inputs[0].struct_val."))
	}
	p := fields["prompt"]
	if p == nil || len(p.GetStringVal()) != 1 || strings.TrimSpace(p.GetStringVal()[0]) == "" {
		return "", apierror.InvalidArgument("inputs[0].struct_val.prompt is required, as one non-empty string_val")
	}
	return p.GetStringVal()[0], nil
}

// textOutput is one generation output: {"content": text}.
func textOutput(text string) []*aiplatformpb.Tensor {
	return []*aiplatformpb.Tensor{{StructVal: map[string]*aiplatformpb.Tensor{
		"content": {Dtype: aiplatformpb.Tensor_STRING, StringVal: []string{text}},
	}}}
}

// tensorPredict serves one tensor request on endpoint, calling emit with the
// outputs of each message to send. stream asks the generation model for one
// message per chunk; otherwise every method sends one message.
func (p *predictionService) tensorPredict(ctx context.Context, endpoint string, inputs []*aiplatformpb.Tensor,
	params *aiplatformpb.Tensor, stream bool, emit func([]*aiplatformpb.Tensor) error) error {

	t, err := parseTarget("endpoint", endpoint)
	if err != nil {
		return err
	}
	if t.model != "" {
		if err := t.checkPublisher(); err != nil {
			return err
		}
	}
	if t.model != "" && checkEmbeddingModel(t.model, "predict") != nil {
		return p.tensorGenerate(ctx, t.model, inputs, params, stream, emit)
	}

	instances := make([]*structpb.Value, len(inputs))
	for i, in := range inputs {
		if instances[i], err = tensorValue(fmt.Sprintf("inputs[%d]", i), in); err != nil {
			return err
		}
	}
	var pv *structpb.Value
	if params != nil {
		if pv, err = tensorValue("parameters", params); err != nil {
			return err
		}
		if _, null := pv.GetKind().(*structpb.Value_NullValue); null {
			pv = nil
		}
	}
	body, err := predictBody(instances, pv)
	if err != nil {
		return err
	}
	var answer []byte
	if t.model != "" {
		resp, err := p.s.embedPredict(ctx, t.model, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if answer, err = json.Marshal(resp); err != nil {
			return apierror.Internal(err, "encoding the response: %v", err)
		}
		_ = grpc.SetHeader(ctx, metadata.Pairs(strings.ToLower(ProvenanceHeader), provenanceValue))
	} else if answer, err = p.s.endpointPredict(ctx, t.project, t.location, t.endpoint, body); err != nil {
		return err
	}
	outputs, err := predictionTensors(answer)
	if err != nil {
		return err
	}
	return emit(outputs)
}

// tensorGenerate serves a tensor request on the generation model.
func (p *predictionService) tensorGenerate(ctx context.Context, model string, inputs []*aiplatformpb.Tensor,
	params *aiplatformpb.Tensor, stream bool, emit func([]*aiplatformpb.Tensor) error) error {

	if _, err := p.s.resolveModel(model); err != nil {
		return err
	}
	prompt, err := generationPrompt(inputs, params)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"contents": []map[string]any{{
		"role": "user", "parts": []map[string]string{{"text": prompt}},
	}}})
	_, chunks, errFn, err := p.s.startGeneration(ctx, model, bytes.NewReader(body))
	if err != nil {
		return err
	}
	var all strings.Builder
	for c := range chunks {
		if c.Last || c.Text == "" {
			continue
		}
		if !stream {
			all.WriteString(c.Text)
			continue
		}
		if err := emit(textOutput(c.Text)); err != nil {
			drain(chunks)
			return err
		}
	}
	if err := generationError(ctx, errFn); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !stream {
		return emit(textOutput(strings.TrimRight(all.String(), "\n")))
	}
	return nil
}

// DirectPredict answers one tensor request with one response.
func (p *predictionService) DirectPredict(ctx context.Context, req *aiplatformpb.DirectPredictRequest) (*aiplatformpb.DirectPredictResponse, error) {
	var out *aiplatformpb.DirectPredictResponse
	err := p.tensorPredict(ctx, req.GetEndpoint(), req.GetInputs(), req.GetParameters(), false, func(o []*aiplatformpb.Tensor) error {
		out = &aiplatformpb.DirectPredictResponse{Outputs: o}
		return nil
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

// ServerStreamingPredict streams the answer to one tensor request.
func (p *predictionService) ServerStreamingPredict(req *aiplatformpb.StreamingPredictRequest, stream aiplatformpb.PredictionService_ServerStreamingPredictServer) error {
	return streamError(p.tensorPredict(stream.Context(), req.GetEndpoint(), req.GetInputs(), req.GetParameters(), true,
		func(o []*aiplatformpb.Tensor) error {
			return sent(stream.Send(&aiplatformpb.StreamingPredictResponse{Outputs: o}))
		}))
}

// StreamingPredict answers each request on a bidirectional stream as
// ServerStreamingPredict answers it, in order.
func (p *predictionService) StreamingPredict(stream aiplatformpb.PredictionService_StreamingPredictServer) error {
	ep := ""
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ep, err = streamEndpoint(ep, req.GetEndpoint()); err != nil {
			return grpcError(err)
		}
		if err := p.tensorPredict(stream.Context(), ep, req.GetInputs(), req.GetParameters(), true,
			func(o []*aiplatformpb.Tensor) error {
				return sent(stream.Send(&aiplatformpb.StreamingPredictResponse{Outputs: o}))
			}); err != nil {
			return streamError(err)
		}
	}
}

// StreamDirectPredict answers each request on a bidirectional stream with
// one response, as DirectPredict does.
func (p *predictionService) StreamDirectPredict(stream aiplatformpb.PredictionService_StreamDirectPredictServer) error {
	ep := ""
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ep, err = streamEndpoint(ep, req.GetEndpoint()); err != nil {
			return grpcError(err)
		}
		if err := p.tensorPredict(stream.Context(), ep, req.GetInputs(), req.GetParameters(), false,
			func(o []*aiplatformpb.Tensor) error {
				return sent(stream.Send(&aiplatformpb.StreamDirectPredictResponse{Outputs: o}))
			}); err != nil {
			return streamError(err)
		}
	}
}

// streamEndpoint is the endpoint a bidirectional request addresses: Vertex
// requires it on the first request, and a later one may repeat it or omit
// it, but not change it.
func streamEndpoint(first, got string) (string, error) {
	switch {
	case first == "" && got == "":
		return "", apierror.InvalidArgument("endpoint is required on the first request of the stream")
	case first == "":
		return got, nil
	case got != "" && got != first:
		return "", apierror.InvalidArgument("endpoint %q differs from the stream's first request, %q", got, first)
	}
	return first, nil
}

// errSent marks a failed Send, which is the client going away: its error is
// returned as it is, not wrapped as a refusal.
type errSent struct{ error }

// streamError is grpcError, except that a failed Send is returned as it was.
func streamError(err error) error {
	var e errSent
	if errors.As(err, &e) {
		return e.error
	}
	return grpcError(err)
}

func sent(err error) error {
	if err != nil {
		return errSent{err}
	}
	return nil
}

// rawStream serves StreamRawPredict on t, calling emit with each chunk.
// The generation model sends one GenerateContentResponse per chunk, the
// finish reason on a final one, as StreamGenerateContent frames them; the
// embedding model and a custom endpoint send their one answer.
func (s *Server) rawStream(ctx context.Context, t target, body []byte, contentType string, emit func(ct string, data []byte) error) error {
	if t.endpoint != "" {
		a, err := s.relay(ctx, t.project, t.location, t.endpoint, body, contentType)
		if err != nil {
			return err
		}
		if a.status < 200 || a.status > 299 {
			return containerError(t.endpoint, a)
		}
		return emit(a.contentType, a.body)
	}
	if err := t.checkPublisher(); err != nil {
		return err
	}
	if checkEmbeddingModel(t.model, "streamRawPredict") == nil {
		out, _, err := s.publisherRawPredict(ctx, t.model, body, contentType)
		if err != nil {
			return err
		}
		return emit("application/json", out)
	}
	if err := checkRawContentType(contentType); err != nil {
		return err
	}
	actual, chunks, errFn, err := s.startGeneration(ctx, t.model, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for c := range chunks {
		resp := textResponse(actual, c.Text, "")
		if c.Last {
			resp = textResponse(actual, "", c.FinishReason)
		}
		b, err := json.Marshal(resp)
		if err == nil {
			err = emit("application/json", b)
		}
		if err != nil {
			drain(chunks)
			return err
		}
	}
	if err := generationError(ctx, errFn); err != nil {
		return err
	}
	return ctx.Err()
}

// StreamRawPredict streams the model's native answer as HttpBody chunks.
func (p *predictionService) StreamRawPredict(req *aiplatformpb.StreamRawPredictRequest, stream aiplatformpb.PredictionService_StreamRawPredictServer) error {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return grpcError(err)
	}
	hb := req.GetHttpBody()
	err = p.s.rawStream(stream.Context(), t, hb.GetData(), hb.GetContentType(), func(ct string, data []byte) error {
		return sent(stream.Send(&httpbody.HttpBody{ContentType: ct, Data: data}))
	})
	return streamError(err)
}

// gRPC model server methods: nothing here is one.
func noModelServer(method, name string) error {
	return apierror.Unimplemented("%s calls method_name %q on a gRPC model server, and none is served here: "+
		"the local models answer Predict, RawPredict and their streaming forms, and a custom endpoint is a Cloud Run service, "+
		"whose container is reached over HTTP/1.1 at its predict route (a port named h2c is not honoured, so no gRPC server in it can be called); "+
		"see docs/compatibility.md", method, name)
}

// DirectRawPredict is UNIMPLEMENTED, naming method_name.
func (p *predictionService) DirectRawPredict(_ context.Context, req *aiplatformpb.DirectRawPredictRequest) (*aiplatformpb.DirectRawPredictResponse, error) {
	return nil, grpcError(noModelServer("DirectRawPredict", req.GetMethodName()))
}

// StreamDirectRawPredict is UNIMPLEMENTED, naming method_name.
func (p *predictionService) StreamDirectRawPredict(stream aiplatformpb.PredictionService_StreamDirectRawPredictServer) error {
	req, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return grpcError(noModelServer("StreamDirectRawPredict", req.GetMethodName()))
}

// StreamingRawPredict is UNIMPLEMENTED, naming method_name.
func (p *predictionService) StreamingRawPredict(stream aiplatformpb.PredictionService_StreamingRawPredictServer) error {
	req, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return grpcError(noModelServer("StreamingRawPredict", req.GetMethodName()))
}

// streamRawPredictREST serves POST .../{model}:streamRawPredict and
// .../endpoints/{e}:streamRawPredict. A publisher model's chunks are
// server-sent events, as :streamGenerateContent sends them; a custom
// endpoint's one answer is relayed as it came.
func (s *Server) streamRawPredictREST(w http.ResponseWriter, r *http.Request, t target, body []byte) {
	flusher, _ := w.(http.Flusher)
	wrote := false
	err := s.rawStream(r.Context(), t, body, r.Header.Get("Content-Type"), func(ct string, data []byte) error {
		if t.endpoint != "" {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			wrote = true
			_, err := w.Write(data)
			return err
		}
		if !wrote {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if checkEmbeddingModel(t.model, "") == nil {
				w.Header().Set(ProvenanceHeader, provenanceValue)
			}
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err == nil || r.Context().Err() != nil {
		return
	}
	if !wrote {
		apierror.WriteJSON(w, err)
		return
	}
	// After the first event, a failure is a final event, as writeStream sends.
	fmt.Fprintf(w, "data: %s\n\n", apierror.MarshalJSON(err))
	if flusher != nil {
		flusher.Flush()
	}
}

// readBody reads a bounded request body.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return nil, apierror.InvalidArgument("reading the request: %v", err)
	}
	if len(body) > maxRequestBytes {
		return nil, apierror.InvalidArgument("the request is larger than %d bytes", maxRequestBytes)
	}
	return body, nil
}
