//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestRawPredictOnTheEmbeddingModel (#1284) sends RawPredict through the
// official aiplatform PredictionClient, over gRPC, to the local embedding
// publisher model: its native body is the text-embedding :predict body, and
// the answer is :predict's, unwrapped, with a 768-value vector. A content
// type the model cannot read is INVALID_ARGUMENT naming
// http_body.content_type.
func TestRawPredictOnTheEmbeddingModel(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel

	resp, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: name, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json",
		Data:        []byte(`{"instances":[{"content":"The quick brown fox.","task_type":"RETRIEVAL_QUERY"}]}`),
	}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var got struct {
		Predictions []struct {
			Embeddings struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(resp.GetData(), &got); err != nil || len(got.Predictions) != 1 {
		t.Fatalf("body %.200s: %v", resp.GetData(), err)
	}
	if n := len(got.Predictions[0].Embeddings.Values); n != 768 || got.Model != embeddingModel {
		t.Errorf("%d values from %q, want 768 from %q", n, got.Model, embeddingModel)
	}
	_, err = pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: name, HttpBody: &httpbody.HttpBody{
		ContentType: "text/plain", Data: []byte("hello"),
	}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "http_body.content_type") {
		t.Errorf("text/plain: %v, want INVALID_ARGUMENT naming http_body.content_type", err)
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestRawPredictOnTheGenerationModel (#1284) sends RawPredict to the local
// generation publisher model: its native body is the generateContent body,
// and the answer is a GenerateContentResponse naming the model that ran, with
// STOP. What the runtime cannot honour (a temperature) is the same
// INVALID_ARGUMENT generateContent gives, and another model is NOT_FOUND.
// What the model says is not asserted.
func TestRawPredictOnTheGenerationModel(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvLocalAI)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + localModel
	raw := func(model, body string) (*httpbody.HttpBody, error) {
		return pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: model, HttpBody: &httpbody.HttpBody{
			ContentType: "application/json", Data: []byte(body),
		}})
	}

	resp, err := raw(name, `{"contents":[{"role":"user","parts":[{"text":"Say hello in one word."}]}]}`)
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var got struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		ModelVersion string `json:"modelVersion"`
	}
	if err := json.Unmarshal(resp.GetData(), &got); err != nil || len(got.Candidates) != 1 {
		t.Fatalf("body %.300s: %v", resp.GetData(), err)
	}
	c := got.Candidates[0]
	if c.Content.Role != "model" || c.FinishReason != "STOP" || got.ModelVersion != localModel {
		t.Errorf("response = %s", resp.GetData())
	}
	if len(c.Content.Parts) == 0 || strings.TrimSpace(c.Content.Parts[0].Text) == "" {
		t.Errorf("no text in %s", resp.GetData())
	}

	_, err = raw(name, `{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"temperature":0.2}}`)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "temperature") {
		t.Errorf("temperature: %v, want INVALID_ARGUMENT naming it", err)
	}
	_, err = raw("projects/"+h.Project()+"/locations/us-central1/publishers/google/models/gemini-2.0-flash",
		`{"contents":[{"parts":[{"text":"x"}]}]}`)
	if status.Code(err) != codes.NotFound {
		t.Errorf("gemini: %v, want NOT_FOUND (no substitution)", err)
	}
}
