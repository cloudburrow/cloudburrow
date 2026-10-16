//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func compatStr(s string) *aiplatformpb.Tensor {
	return &aiplatformpb.Tensor{Dtype: aiplatformpb.Tensor_STRING, StringVal: []string{s}}
}

func compatPrompt(p string) []*aiplatformpb.Tensor {
	return []*aiplatformpb.Tensor{{StructVal: map[string]*aiplatformpb.Tensor{"prompt": compatStr(p)}}}
}

func compatContent(t *testing.T, outs []*aiplatformpb.Tensor) string {
	t.Helper()
	if len(outs) != 1 || len(outs[0].GetStructVal()["content"].GetStringVal()) != 1 {
		t.Fatalf("outputs = %v, want one {content}", outs)
	}
	return outs[0].GetStructVal()["content"].GetStringVal()[0]
}

// covers: google.cloud.aiplatform.v1.PredictionService/ServerStreamingPredict, google.cloud.aiplatform.v1.PredictionService/StreamingPredict, google.cloud.aiplatform.v1.PredictionService/DirectPredict, google.cloud.aiplatform.v1.PredictionService/StreamDirectPredict, google.cloud.aiplatform.v1.PredictionService/StreamRawPredict
//
// TestStreamingPredictOnTheGenerationModel (#1285) drives every streaming
// and direct predict method of the official aiplatform PredictionClient,
// over gRPC, against the local generation publisher model. The tensor
// methods take the Model Garden text instance, {"prompt"}, and answer
// {"content"}: ServerStreamingPredict and StreamingPredict send more than
// one message, one per decoded chunk; DirectPredict and StreamDirectPredict
// one per request. StreamRawPredict streams GenerateContentResponse chunks
// with STOP on the last. A generation option is refused naming its field,
// and DirectRawPredict, which calls a gRPC model server, is UNIMPLEMENTED
// naming method_name. What the model says is not asserted.
func TestStreamingPredictOnTheGenerationModel(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvLocalAI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + localModel
	const prompt = "Count from one to five in words, one per line."

	// ServerStreamingPredict.
	ss, err := pc.ServerStreamingPredict(ctx, &aiplatformpb.StreamingPredictRequest{Endpoint: name, Inputs: compatPrompt(prompt)})
	if err != nil {
		t.Fatalf("ServerStreamingPredict: %v", err)
	}
	var parts []string
	for {
		m, err := ss.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ServerStreamingPredict stream: %v", err)
		}
		parts = append(parts, compatContent(t, m.GetOutputs()))
	}
	if len(parts) < 2 || strings.TrimSpace(strings.Join(parts, "")) == "" {
		t.Errorf("ServerStreamingPredict sent %d messages (%q); want a stream of text", len(parts), parts)
	}

	// StreamingPredict, bidirectional.
	bs, err := pc.StreamingPredict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Send(&aiplatformpb.StreamingPredictRequest{Endpoint: name, Inputs: compatPrompt(prompt)}); err != nil {
		t.Fatal(err)
	}
	_ = bs.CloseSend()
	n := 0
	for {
		m, err := bs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("StreamingPredict: %v", err)
		}
		compatContent(t, m.GetOutputs())
		n++
	}
	if n < 2 {
		t.Errorf("StreamingPredict sent %d messages, want a stream", n)
	}

	// DirectPredict: one answer.
	dp, err := pc.DirectPredict(ctx, &aiplatformpb.DirectPredictRequest{Endpoint: name, Inputs: compatPrompt("Say hello in one word.")})
	if err != nil {
		t.Fatalf("DirectPredict: %v", err)
	}
	if strings.TrimSpace(compatContent(t, dp.GetOutputs())) == "" {
		t.Error("DirectPredict: no text")
	}

	// StreamDirectPredict: one response per request, endpoint on the first.
	sd, err := pc.StreamDirectPredict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, ep := range []string{name, ""} {
		if err := sd.Send(&aiplatformpb.StreamDirectPredictRequest{Endpoint: ep, Inputs: compatPrompt("Say hello in one word.")}); err != nil {
			t.Fatal(err)
		}
		m, err := sd.Recv()
		if err != nil {
			t.Fatalf("StreamDirectPredict request %d: %v", i, err)
		}
		if strings.TrimSpace(compatContent(t, m.GetOutputs())) == "" {
			t.Errorf("StreamDirectPredict request %d: no text", i)
		}
	}
	_ = sd.CloseSend()

	// StreamRawPredict: GenerateContentResponse chunks, STOP on the last only.
	sr, err := pc.StreamRawPredict(ctx, &aiplatformpb.StreamRawPredictRequest{Endpoint: name, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json",
		Data:        []byte(`{"contents":[{"role":"user","parts":[{"text":"` + prompt + `"}]}]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var finishes []string
	for {
		m, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("StreamRawPredict: %v", err)
		}
		var r struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			ModelVersion string `json:"modelVersion"`
		}
		if err := json.Unmarshal(m.GetData(), &r); err != nil || len(r.Candidates) != 1 || r.ModelVersion != localModel {
			t.Fatalf("chunk %.200s: %v", m.GetData(), err)
		}
		finishes = append(finishes, r.Candidates[0].FinishReason)
	}
	if len(finishes) < 2 || finishes[len(finishes)-1] != "STOP" || finishes[0] != "" {
		t.Errorf("StreamRawPredict finish reasons %q: want a stream with STOP on the last only", finishes)
	}

	// Refusals.
	_, err = pc.DirectPredict(ctx, &aiplatformpb.DirectPredictRequest{Endpoint: name, Inputs: []*aiplatformpb.Tensor{{
		StructVal: map[string]*aiplatformpb.Tensor{"prompt": compatStr("x"), "max_tokens": {IntVal: []int32{8}}},
	}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "inputs[0].struct_val.max_tokens") {
		t.Errorf("max_tokens: %v, want INVALID_ARGUMENT naming it", err)
	}
	_, err = pc.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: name, MethodName: "/x.Y/Predict"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "method_name") {
		t.Errorf("DirectRawPredict: %v, want UNIMPLEMENTED naming method_name", err)
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/DirectPredict, google.cloud.aiplatform.v1.PredictionService/ServerStreamingPredict, google.cloud.aiplatform.v1.PredictionService/StreamRawPredict
//
// TestStreamingPredictOnTheEmbeddingModel (#1285) sends the text-embedding
// instance as a Tensor through DirectPredict and ServerStreamingPredict, and
// the :predict body through StreamRawPredict, to the local embedding model:
// each answers once, with the 768-value vector Predict gives.
func TestStreamingPredictOnTheEmbeddingModel(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel
	inputs := []*aiplatformpb.Tensor{{StructVal: map[string]*aiplatformpb.Tensor{
		"content": compatStr("The quick brown fox."), "task_type": compatStr("RETRIEVAL_QUERY"),
	}}}
	values := func(outs []*aiplatformpb.Tensor) int {
		if len(outs) != 1 {
			return -1
		}
		return len(outs[0].GetStructVal()["embeddings"].GetStructVal()["values"].GetDoubleVal())
	}

	dp, err := pc.DirectPredict(ctx, &aiplatformpb.DirectPredictRequest{Endpoint: name, Inputs: inputs})
	if err != nil {
		t.Fatalf("DirectPredict: %v", err)
	}
	if n := values(dp.GetOutputs()); n != 768 {
		t.Errorf("DirectPredict: %d values, want 768", n)
	}
	ss, err := pc.ServerStreamingPredict(ctx, &aiplatformpb.StreamingPredictRequest{Endpoint: name, Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ss.Recv()
	if err != nil || values(m.GetOutputs()) != 768 {
		t.Fatalf("ServerStreamingPredict: %v %v", m, err)
	}
	if _, err := ss.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("ServerStreamingPredict: want one message, then %v", err)
	}
	sr, err := pc.StreamRawPredict(ctx, &aiplatformpb.StreamRawPredictRequest{Endpoint: name, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"instances":[{"content":"The quick brown fox."}]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := sr.Recv()
	if err != nil || !strings.Contains(string(c.GetData()), `"predictions"`) {
		t.Fatalf("StreamRawPredict: %v %v", c, err)
	}
	// A size the model was not trained for is refused, as by :predict.
	_, err = pc.DirectPredict(ctx, &aiplatformpb.DirectPredictRequest{Endpoint: name, Inputs: inputs,
		Parameters: &aiplatformpb.Tensor{StructVal: map[string]*aiplatformpb.Tensor{"outputDimensionality": {IntVal: []int32{300}}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("outputDimensionality 300: %v, want INVALID_ARGUMENT", err)
	}
}
