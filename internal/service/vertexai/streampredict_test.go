package vertexai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

const embedEndpoint = "projects/p/locations/us-central1/publishers/google/models/" + localai.EmbeddingGemmaONNXID

func strT(s string) *aiplatformpb.Tensor {
	return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_STRING, StringVal: []string{s}}
}

func structT(kv map[string]*aiplatformpb.Tensor) *aiplatformpb.Tensor {
	return &aiplatformpb.Tensor{StructVal: kv}
}

func promptT(p string) []*aiplatformpb.Tensor {
	return []*aiplatformpb.Tensor{structT(map[string]*aiplatformpb.Tensor{"prompt": strT(p)})}
}

func contentOf(t *testing.T, outs []*aiplatformpb.Tensor) string {
	t.Helper()
	if len(outs) != 1 {
		t.Fatalf("outputs = %v, want one", outs)
	}
	return outs[0].GetStructVal()["content"].GetStringVal()[0]
}

func TestTensorValueRoundTrip(t *testing.T) {
	in := structT(map[string]*aiplatformpb.Tensor{
		"content": strT("hi"),
		"n":       {IntVal: []int32{3}},
		"xs":      {Shape: []int64{2}, FloatVal: []float32{1, 2}},
		"flag":    {BoolVal: []bool{true}},
	})
	v, err := tensorValue("inputs[0]", in)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(v.AsInterface())
	if string(j) != `{"content":"hi","flag":true,"n":3,"xs":[1,2]}` {
		t.Errorf("value = %s", j)
	}
	back := valueTensor(v.AsInterface())
	if got := back.GetStructVal()["xs"]; got.GetDtype() != aiplatformpb.Tensor_DOUBLE || len(got.GetDoubleVal()) != 2 || got.GetShape()[0] != 2 {
		t.Errorf("xs = %v", got)
	}
	for name, bad := range map[string]*aiplatformpb.Tensor{
		"inputs[0].tensor_val": {TensorVal: []byte{1}},
		"inputs[0].bytes_val":  {BytesVal: [][]byte{{1}}},
		"more than one type":   {StringVal: []string{"a"}, IntVal: []int32{1}},
	} {
		if _, err := tensorValue("inputs[0]", bad); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGRPCDirectPredictEmbedsLikePredict(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(fakeEmbedder{}, nil)
	c, _ := startMixed(t, s)
	inputs := []*aiplatformpb.Tensor{structT(map[string]*aiplatformpb.Tensor{"content": strT("hello"), "task_type": strT("RETRIEVAL_QUERY")})}
	params := structT(map[string]*aiplatformpb.Tensor{"outputDimensionality": {IntVal: []int32{128}}})
	resp, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{Endpoint: embedEndpoint, Inputs: inputs, Parameters: params})
	if err != nil {
		t.Fatalf("DirectPredict: %v", err)
	}
	vals := resp.GetOutputs()[0].GetStructVal()["embeddings"].GetStructVal()["values"]
	if len(resp.GetOutputs()) != 1 || len(vals.GetDoubleVal()) != 128 || vals.GetDtype() != aiplatformpb.Tensor_DOUBLE {
		t.Fatalf("outputs = %v", resp.GetOutputs())
	}
	// ServerStreamingPredict: an embedding is one message.
	st, err := c.ServerStreamingPredict(context.Background(), &aiplatformpb.StreamingPredictRequest{Endpoint: embedEndpoint, Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if len(m.GetOutputs()[0].GetStructVal()["embeddings"].GetStructVal()["values"].GetDoubleVal()) != 768 {
			t.Errorf("message %v", m)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d messages, want 1", n)
	}
	// The :predict subset's refusals hold.
	bad := []*aiplatformpb.Tensor{structT(map[string]*aiplatformpb.Tensor{"content": strT("x"), "nosuch": strT("y")})}
	if _, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{Endpoint: embedEndpoint, Inputs: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown field: %v", err)
	}
	// No runtime: the :predict 501.
	s.SetEmbedder(nil, errors.New("not compiled"))
	if _, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{Endpoint: embedEndpoint, Inputs: inputs}); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "not compiled") {
		t.Errorf("no runtime: %v", err)
	}
}

func TestGRPCServerStreamingPredictStreamsGeneration(t *testing.T) {
	c, _ := startMixed(t, NewServer(&EchoGenerator{ID: testModel}))
	st, err := c.ServerStreamingPredict(context.Background(), &aiplatformpb.StreamingPredictRequest{Endpoint: grpcModelName, Inputs: promptT("hello there friend")})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		parts = append(parts, contentOf(t, m.GetOutputs()))
	}
	if len(parts) < 2 || strings.Join(strings.Fields(strings.Join(parts, "")), " ") != "HELLO THERE FRIEND" {
		t.Errorf("parts = %q", parts)
	}
	// DirectPredict: the same answer in one response.
	resp, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{Endpoint: grpcModelName, Inputs: promptT("hello there")})
	if err != nil || contentOf(t, resp.GetOutputs()) != "HELLO THERE" {
		t.Errorf("DirectPredict: %v %v", resp, err)
	}
	// Refusals name the field.
	for field, req := range map[string]*aiplatformpb.DirectPredictRequest{
		"inputs[0].struct_val.temperature": {Endpoint: grpcModelName, Inputs: []*aiplatformpb.Tensor{structT(map[string]*aiplatformpb.Tensor{
			"prompt": strT("x"), "temperature": {DoubleVal: []float64{0.2}}})}},
		"parameters":                {Endpoint: grpcModelName, Inputs: promptT("x"), Parameters: structT(map[string]*aiplatformpb.Tensor{"max_tokens": {IntVal: []int32{8}}})},
		"inputs[0].struct_val.text": {Endpoint: grpcModelName, Inputs: []*aiplatformpb.Tensor{structT(map[string]*aiplatformpb.Tensor{"text": strT("x")})}},
		"inputs:":                   {Endpoint: grpcModelName},
	} {
		_, err := c.DirectPredict(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming it", field, err)
		}
	}
	if _, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{
		Endpoint: "projects/p/locations/l/publishers/google/models/gemini-2.0-flash", Inputs: promptT("x")}); status.Code(err) != codes.NotFound {
		t.Errorf("gemini: %v, want NOT_FOUND", err)
	}
}

func TestGRPCBidiPredictAnswersEachRequest(t *testing.T) {
	c, _ := startMixed(t, NewServer(&EchoGenerator{ID: testModel}))
	st, err := c.StreamDirectPredict(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"one two", "three"} {
		ep := grpcModelName
		if i > 0 {
			ep = "" // later requests may omit it
		}
		if err := st.Send(&aiplatformpb.StreamDirectPredictRequest{Endpoint: ep, Inputs: promptT(p)}); err != nil {
			t.Fatal(err)
		}
		m, err := st.Recv()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if got := contentOf(t, m.GetOutputs()); got != strings.ToUpper(p) {
			t.Errorf("request %d: %q", i, got)
		}
	}
	_ = st.CloseSend()
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("end: %v", err)
	}

	bs, err := c.StreamingPredict(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = bs.Send(&aiplatformpb.StreamingPredictRequest{Inputs: promptT("x")})
	if _, err := bs.Recv(); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("no endpoint: %v", err)
	}
}

func TestGRPCTensorPredictOnAnEndpoint(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n }))
	resp, err := c.DirectPredict(context.Background(), &aiplatformpb.DirectPredictRequest{Endpoint: endpointName,
		Inputs: []*aiplatformpb.Tensor{{DoubleVal: []float64{5}}, {IntVal: []int32{2}}}})
	if err != nil {
		t.Fatalf("DirectPredict: %v", err)
	}
	if len(resp.GetOutputs()) != 2 || resp.GetOutputs()[0].GetDoubleVal()[0] != 10 || resp.GetOutputs()[1].GetDoubleVal()[0] != 4 {
		t.Errorf("outputs = %v", resp.GetOutputs())
	}
	st, err := c.StreamRawPredict(context.Background(), &aiplatformpb.StreamRawPredictRequest{Endpoint: endpointName,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"instances":[7]}`)}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.Recv()
	if err != nil || !strings.Contains(string(m.GetData()), `"predictions":[14]`) {
		t.Fatalf("StreamRawPredict: %v %v", m, err)
	}
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("want one chunk, then: %v", err)
	}
}

func TestGRPCStreamRawPredictStreamsGeneration(t *testing.T) {
	s := NewServer(&EchoGenerator{ID: testModel})
	s.SetEmbedder(fakeEmbedder{}, nil)
	c, _ := startMixed(t, s)
	st, err := c.StreamRawPredict(context.Background(), &aiplatformpb.StreamRawPredictRequest{Endpoint: grpcModelName,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello there"}]}]}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var finishes []string
	var text strings.Builder
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		var r GenerateContentResponse
		if err := json.Unmarshal(m.GetData(), &r); err != nil || m.GetContentType() != "application/json" {
			t.Fatalf("chunk %s: %v", m.GetData(), err)
		}
		text.WriteString(r.Candidates[0].Content.Parts[0].Text)
		finishes = append(finishes, r.Candidates[0].FinishReason)
	}
	if len(finishes) < 3 || finishes[len(finishes)-1] != FinishReasonStop || finishes[0] != "" || !strings.Contains(text.String(), "HELLO") {
		t.Errorf("finishes %q, text %q", finishes, text.String())
	}
	// Refused before the first chunk, as generateContent refuses it.
	st, _ = c.StreamRawPredict(context.Background(), &aiplatformpb.StreamRawPredictRequest{Endpoint: grpcModelName,
		HttpBody: &httpbody.HttpBody{Data: []byte(`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"topK":3}}`)}})
	if _, err := st.Recv(); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "topK") {
		t.Errorf("topK: %v", err)
	}
	// The embedding model: one chunk.
	st, _ = c.StreamRawPredict(context.Background(), &aiplatformpb.StreamRawPredictRequest{Endpoint: embedEndpoint,
		HttpBody: &httpbody.HttpBody{Data: []byte(`{"instances":[{"content":"hi"}]}`)}})
	if m, err := st.Recv(); err != nil || !strings.Contains(string(m.GetData()), `"predictions"`) {
		t.Errorf("embedding: %v %v", m, err)
	}
}

func TestGRPCDirectRawMethodsNameMethodName(t *testing.T) {
	c, _ := startMixed(t, NewServer(&EchoGenerator{ID: testModel}))
	_, err := c.DirectRawPredict(context.Background(), &aiplatformpb.DirectRawPredictRequest{Endpoint: endpointName, MethodName: "/tensorflow.serving.PredictionService/Predict"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "method_name") {
		t.Errorf("DirectRawPredict: %v", err)
	}
	st, _ := c.StreamingRawPredict(context.Background())
	_ = st.Send(&aiplatformpb.StreamingRawPredictRequest{Endpoint: endpointName, MethodName: "/m"})
	if _, err := st.Recv(); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "method_name") {
		t.Errorf("StreamingRawPredict: %v", err)
	}
	sd, _ := c.StreamDirectRawPredict(context.Background())
	_ = sd.Send(&aiplatformpb.StreamDirectRawPredictRequest{Endpoint: endpointName, MethodName: "/m"})
	if _, err := sd.Recv(); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "method_name") {
		t.Errorf("StreamDirectRawPredict: %v", err)
	}
}

func TestStreamRawPredictREST(t *testing.T) {
	s := NewServer(&EchoGenerator{ID: testModel})
	s.SetEmbedder(fakeEmbedder{}, nil)
	post := func(model, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/projects/p/locations/us-central1/publishers/google/models/"+model+":streamRawPredict", strings.NewReader(body))
		req.Host = "127.0.0.1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := post(testModel, `{"contents":[{"role":"user","parts":[{"text":"hello there"}]}]}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || strings.Count(rec.Body.String(), "data: ") < 3 ||
		!strings.Contains(rec.Body.String(), `"finishReason":"STOP"`) {
		t.Errorf("generation: %d %s", rec.Code, rec.Body)
	}
	rec = post(localai.EmbeddingGemmaONNXID, `{"instances":[{"content":"hi"}]}`)
	if rec.Code != 200 || strings.Count(rec.Body.String(), "data: ") != 1 || rec.Header().Get(ProvenanceHeader) == "" {
		t.Errorf("embedding: %d %s", rec.Code, rec.Body)
	}
	rec = post(testModel, `{"contents":[{"parts":[{"text":"x"}]}],"tools":[{}]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "tools") {
		t.Errorf("tools: %d %s", rec.Code, rec.Body)
	}
}
