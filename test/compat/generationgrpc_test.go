//go:build compat

package compat

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.aiplatform.v1.PredictionService/StreamGenerateContent, google.cloud.aiplatform.v1.PredictionService/GenerateContent
//
// TestStreamGenerateContentThroughThePredictionClient (#1257) generates
// through the official aiplatform PredictionClient, over gRPC, with the same
// local model the genai SDK reaches over REST. StreamGenerateContent sends
// more than one GenerateContentResponse, each naming the model that ran, with
// STOP on the last and on no other; GenerateContent answers the same prompt
// in one response; and both refuse what the runtime cannot honour (a
// temperature) with the same INVALID_ARGUMENT, and another model with the
// same NOT_FOUND, never substituting. What the model says is not asserted.
func TestStreamGenerateContentThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvLocalAI)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + localModel
	user := func(text string) []*aiplatformpb.Content {
		return []*aiplatformpb.Content{{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: text}}}}}
	}

	// Streaming.
	stream, err := pc.StreamGenerateContent(ctx, &aiplatformpb.GenerateContentRequest{
		Model: name, Contents: user("Count from one to five in words, one per line."),
	})
	if err != nil {
		t.Fatalf("StreamGenerateContent: %v", err)
	}
	var (
		msgs   int
		text   strings.Builder
		finish []aiplatformpb.Candidate_FinishReason
	)
	start := time.Now()
	var first time.Duration
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if msgs == 0 {
			first = time.Since(start)
		}
		msgs++
		if resp.GetModelVersion() != localModel {
			t.Errorf("message %d modelVersion = %q, want %q", msgs, resp.GetModelVersion(), localModel)
		}
		c := resp.GetCandidates()
		if len(c) != 1 || c[0].GetContent().GetRole() != "model" {
			t.Fatalf("message %d = %v, want one model candidate", msgs, resp)
		}
		for _, p := range c[0].GetContent().GetParts() {
			text.WriteString(p.GetText())
		}
		finish = append(finish, c[0].GetFinishReason())
	}
	if msgs < 2 {
		t.Errorf("got %d messages; a stream that arrives in one piece is not streaming", msgs)
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("the stream carried no text")
	}
	for i, f := range finish {
		if last := i == len(finish)-1; (f == aiplatformpb.Candidate_STOP) != last {
			t.Errorf("message %d finishReason = %v; STOP belongs on the last message only", i+1, f)
		}
	}
	t.Logf("%d messages over %v (first after %v), %d bytes (content not asserted)",
		msgs, time.Since(start).Round(time.Millisecond), first.Round(time.Millisecond), text.Len())

	// Unary, on the same model.
	resp, err := pc.GenerateContent(ctx, &aiplatformpb.GenerateContentRequest{
		Model: name, Contents: user("Reply with the single word: ready."),
	})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	c := resp.GetCandidates()
	if len(c) != 1 || c[0].GetFinishReason() != aiplatformpb.Candidate_STOP || resp.GetModelVersion() != localModel {
		t.Fatalf("GenerateContent = %v", resp)
	}
	if strings.TrimSpace(c[0].GetContent().GetParts()[0].GetText()) == "" {
		t.Error("GenerateContent produced no text")
	}

	// The same refusals, unary and streaming.
	recvErr := func(req *aiplatformpb.GenerateContentRequest) error {
		st, err := pc.StreamGenerateContent(ctx, req)
		if err != nil {
			return err
		}
		if m, err := st.Recv(); err == nil {
			return errors.New("a message arrived: " + m.String())
		} else {
			return err
		}
	}
	temp := float32(0.2)
	refused := []struct {
		name string
		req  *aiplatformpb.GenerateContentRequest
		code codes.Code
		says string
	}{
		{"temperature", &aiplatformpb.GenerateContentRequest{Model: name, Contents: user("hi"),
			GenerationConfig: &aiplatformpb.GenerationConfig{Temperature: &temp}}, codes.InvalidArgument, "temperature"},
		{"another model", &aiplatformpb.GenerateContentRequest{
			Model: "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/gemini-2.0-flash", Contents: user("hi")},
			codes.NotFound, "gemini-2.0-flash"},
	}
	for _, r := range refused {
		_, uerr := pc.GenerateContent(ctx, r.req)
		serr := recvErr(r.req)
		for which, err := range map[string]error{"GenerateContent": uerr, "StreamGenerateContent": serr} {
			if status.Code(err) != r.code || !strings.Contains(err.Error(), r.says) {
				t.Errorf("%s, %s: %v, want %v naming %q", which, r.name, err, r.code, r.says)
			}
		}
	}
}
