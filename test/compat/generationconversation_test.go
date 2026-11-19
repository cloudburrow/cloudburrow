//go:build compat

package compat

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/api/option"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The local model's conversations, function calling, image input and token
// accounting (#1937), through Google's own clients. Protocol behaviour is
// asserted; what the model says is asserted only where a constraint forces
// it (a call under mode ANY) or a test would be meaningless without it.

func requireUsage(t *testing.T, what string, u *genai.GenerateContentResponseUsageMetadata) {
	t.Helper()
	if u == nil {
		t.Fatalf("%s: no usageMetadata", what)
	}
	if u.PromptTokenCount <= 0 || u.CandidatesTokenCount <= 0 || u.TotalTokenCount != u.PromptTokenCount+u.CandidatesTokenCount {
		t.Errorf("%s: usageMetadata = prompt %d, candidates %d, total %d", what, u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent
//
// TestGenerateContentMultiTurnChatThroughTheOfficialSDK: the genai SDK's
// chat sends the whole conversation each turn, with a system instruction;
// the second turn's prompt is longer than the first's (the history is
// counted), and every response carries usageMetadata.
func TestGenerateContentMultiTurnChatThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	zero := float32(0)
	chat, err := cl.Chats.Create(ctx, localModel, &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "You answer in at most five words."}}},
		Temperature:       &zero,
	}, nil)
	if err != nil {
		t.Fatalf("Chats.Create: %v", err)
	}
	r1, err := chat.SendMessage(ctx, genai.Part{Text: "My name is Ada. Say hello."})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	requireUsage(t, "turn 1", r1.UsageMetadata)
	r2, err := chat.SendMessage(ctx, genai.Part{Text: "What is my name?"})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	requireUsage(t, "turn 2", r2.UsageMetadata)
	if r2.UsageMetadata.PromptTokenCount <= r1.UsageMetadata.PromptTokenCount {
		t.Errorf("turn 2's prompt (%d tokens) is not longer than turn 1's (%d): the history was not sent",
			r2.UsageMetadata.PromptTokenCount, r1.UsageMetadata.PromptTokenCount)
	}
	if r2.Candidates[0].FinishReason != genai.FinishReasonStop || r2.ModelVersion != localModel {
		t.Errorf("finish %q, model %q", r2.Candidates[0].FinishReason, r2.ModelVersion)
	}
	if !strings.Contains(strings.ToLower(r2.Text()), "ada") {
		t.Errorf("the model did not see the earlier turn: %q", r2.Text())
	}
	t.Logf("turn 2 (asserted to name Ada): %q", r2.Text())
}

// TestGenerateContentFunctionCallingRoundTripThroughTheOfficialSDK: under
// mode ANY the model must call the declared function, with arguments that
// fit its schema; the function's response is sent back and the model
// answers in text; mode NONE answers in text without a call.
func TestGenerateContentFunctionCallingRoundTripThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
		Name:        "get_weather",
		Description: "Returns the current weather for a city.",
		Parameters: &genai.Schema{Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}},
			Required:   []string{"city"}},
	}}}}
	cfg := &genai.GenerateContentConfig{Tools: tools, ToolConfig: &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}}
	chat, err := cl.Chats.Create(ctx, localModel, cfg, nil)
	if err != nil {
		t.Fatalf("Chats.Create: %v", err)
	}
	r1, err := chat.SendMessage(ctx, genai.Part{Text: "What is the weather in Paris?"})
	if err != nil {
		t.Fatalf("call turn: %v", err)
	}
	calls := r1.FunctionCalls()
	if len(calls) == 0 || calls[0].Name != "get_weather" {
		t.Fatalf("mode ANY produced no get_weather call: %+v", r1.Candidates[0].Content)
	}
	city, ok := calls[0].Args["city"].(string)
	if !ok || city == "" {
		t.Fatalf("the call's arguments do not fit the schema: %v", calls[0].Args)
	}
	requireUsage(t, "call turn", r1.UsageMetadata)
	t.Logf("call: get_weather(%v)", calls[0].Args)

	// The response turn: mode NONE so the model answers in text.
	cfg.ToolConfig.FunctionCallingConfig.Mode = genai.FunctionCallingConfigModeNone
	r2, err := cl.Models.GenerateContent(ctx, localModel, append(chat.History(false),
		genai.NewContentFromFunctionResponse("get_weather", map[string]any{"city": city, "sky": "sunny", "celsius": 21}, genai.RoleUser)), cfg)
	if err != nil {
		t.Fatalf("response turn: %v", err)
	}
	if len(r2.FunctionCalls()) != 0 || strings.TrimSpace(r2.Text()) == "" {
		t.Errorf("mode NONE: calls %v, text %q", r2.FunctionCalls(), r2.Text())
	}
	requireUsage(t, "response turn", r2.UsageMetadata)
	t.Logf("answer (not asserted): %q", r2.Text())

	// A function response with no call before it is refused as Vertex refuses it.
	_, err = cl.Models.GenerateContent(ctx, localModel, []*genai.Content{
		genai.NewContentFromText("hi", genai.RoleUser),
		genai.NewContentFromFunctionResponse("get_weather", map[string]any{}, genai.RoleUser),
	}, &genai.GenerateContentConfig{Tools: tools})
	if err == nil || !strings.Contains(err.Error(), "function response turn comes immediately after a function call turn") {
		t.Errorf("an unanswered function response: %v", err)
	}
}

func redPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestGenerateContentImagePartThroughTheOfficialSDK: an inline PNG reaches
// the model's vision encoder (its tokens are counted in the prompt), and a
// PDF is refused naming its type.
func TestGenerateContentImagePartThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	text, err := cl.Models.CountTokens(ctx, localModel, genai.Text("What colour is this image? One word."), nil)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	contents := []*genai.Content{genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromBytes(redPNG(t), "image/png"),
		genai.NewPartFromText("What colour is this image? One word."),
	}, genai.RoleUser)}
	resp, err := cl.Models.GenerateContent(ctx, localModel, contents, nil)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	requireUsage(t, "image", resp.UsageMetadata)
	if resp.UsageMetadata.PromptTokenCount <= text.TotalTokens {
		t.Errorf("the image added no tokens: %d with it, %d without", resp.UsageMetadata.PromptTokenCount, text.TotalTokens)
	}
	if !strings.Contains(strings.ToLower(resp.Text()), "red") {
		t.Errorf("the model did not see the image: %q", resp.Text())
	}
	_, err = cl.Models.GenerateContent(ctx, localModel, []*genai.Content{genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromBytes([]byte("%PDF-1.4"), "application/pdf"), genai.NewPartFromText("Summarise.")}, genai.RoleUser)}, nil)
	if err == nil || !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("a PDF part: %v", err)
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/CountTokens
//
// TestCountTokensAndComputeTokensThroughTheOfficialSDK: countTokens is the
// promptTokenCount generateContent reports for the same request, and grows
// with a system instruction and a tool; computeTokens returns the model's
// token IDs and their bytes, which join back to the text.
func TestCountTokensAndComputeTokensThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	prompt := genai.Text("Name three colours.")
	ct, err := cl.Models.CountTokens(ctx, localModel, prompt, nil)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	one := int32(1)
	gen, err := cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{MaxOutputTokens: one})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gen.UsageMetadata == nil || gen.UsageMetadata.PromptTokenCount != ct.TotalTokens || ct.TotalTokens <= 0 {
		t.Errorf("countTokens %d, promptTokenCount %v", ct.TotalTokens, gen.UsageMetadata)
	}
	if gen.Candidates[0].FinishReason != genai.FinishReasonMaxTokens || gen.UsageMetadata.CandidatesTokenCount != 1 {
		t.Errorf("maxOutputTokens 1: finish %q, candidates %d", gen.Candidates[0].FinishReason, gen.UsageMetadata.CandidatesTokenCount)
	}
	withSystem, err := cl.Models.CountTokens(ctx, localModel, prompt, &genai.CountTokensConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "Be brief."}}}})
	if err != nil || withSystem.TotalTokens <= ct.TotalTokens {
		t.Errorf("with a system instruction: %v, %v (without: %d)", withSystem, err, ct.TotalTokens)
	}

	comp, err := cl.Models.ComputeTokens(ctx, localModel, genai.Text("hello world"), nil)
	if err != nil {
		t.Fatalf("ComputeTokens: %v", err)
	}
	if len(comp.TokensInfo) != 1 || len(comp.TokensInfo[0].TokenIDs) == 0 || len(comp.TokensInfo[0].TokenIDs) != len(comp.TokensInfo[0].Tokens) {
		t.Fatalf("tokensInfo = %+v", comp.TokensInfo)
	}
	var joined []byte
	for _, b := range comp.TokensInfo[0].Tokens {
		joined = append(joined, b...)
	}
	if string(joined) != "hello world" || comp.TokensInfo[0].Role != "user" {
		t.Errorf("tokens join to %q, role %q", joined, comp.TokensInfo[0].Role)
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/StreamGenerateContent, google.cloud.aiplatform.v1beta1.PredictionService/Predict, google.cloud.aiplatform.v1.LlmUtilityService/CountTokens, google.cloud.aiplatform.v1.LlmUtilityService/ComputeTokens
//
// TestV1Beta1PredictionClientServesWhatV1Serves: the official v1beta1
// PredictionClient generates (unary and streaming) with usageMetadata and
// counts tokens over gRPC; v1's LlmUtilityClient counts and computes the
// same; and Predict on the generation model answers exactly as v1's does.
func TestV1Beta1PredictionClientServesWhatV1Serves(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvLocalAI)
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	beta, err := aiplatformbeta.NewPredictionClient(ctx, opts...)
	if err != nil {
		t.Fatalf("v1beta1 NewPredictionClient: %v", err)
	}
	defer beta.Close()
	llm, err := aiplatform.NewLlmUtilityClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewLlmUtilityClient: %v", err)
	}
	defer llm.Close()
	v1 := predictionClientAt(t, h, EnvLocalAI)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + localModel
	contents := []*betapb.Content{{Role: "user", Parts: []*betapb.Part{{Data: &betapb.Part_Text{Text: "Say ready."}}}}}

	resp, err := beta.GenerateContent(ctx, &betapb.GenerateContentRequest{Model: name, Contents: contents})
	if err != nil {
		t.Fatalf("v1beta1 GenerateContent: %v", err)
	}
	u := resp.GetUsageMetadata()
	if resp.GetModelVersion() != localModel || u.GetPromptTokenCount() <= 0 || u.GetTotalTokenCount() != u.GetPromptTokenCount()+u.GetCandidatesTokenCount() {
		t.Errorf("v1beta1 GenerateContent: %v", resp)
	}
	stream, err := beta.StreamGenerateContent(ctx, &betapb.GenerateContentRequest{Model: name, Contents: contents})
	if err != nil {
		t.Fatalf("v1beta1 StreamGenerateContent: %v", err)
	}
	var last *betapb.GenerateContentResponse
	for {
		r, err := stream.Recv()
		if err != nil {
			break
		}
		last = r
	}
	if last == nil || last.GetCandidates()[0].GetFinishReason() != betapb.Candidate_STOP || last.GetUsageMetadata().GetPromptTokenCount() != u.GetPromptTokenCount() {
		t.Errorf("the stream's last message: %v", last)
	}
	ct, err := beta.CountTokens(ctx, &betapb.CountTokensRequest{Endpoint: name, Model: name, Contents: contents})
	if err != nil || ct.GetTotalTokens() != u.GetPromptTokenCount() {
		t.Errorf("v1beta1 CountTokens = %v, %v; promptTokenCount %d", ct, err, u.GetPromptTokenCount())
	}
	v1Contents := []*aiplatformpb.Content{{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: "Say ready."}}}}}
	lct, err := llm.CountTokens(ctx, &aiplatformpb.CountTokensRequest{Endpoint: name, Model: name, Contents: v1Contents})
	if err != nil || lct.GetTotalTokens() != ct.GetTotalTokens() {
		t.Errorf("LlmUtility CountTokens = %v, %v", lct, err)
	}
	comp, err := llm.ComputeTokens(ctx, &aiplatformpb.ComputeTokensRequest{Endpoint: name, Model: name, Contents: v1Contents})
	if err != nil || len(comp.GetTokensInfo()) != 1 || len(comp.GetTokensInfo()[0].GetTokenIds()) == 0 {
		t.Errorf("LlmUtility ComputeTokens = %v, %v", comp, err)
	}

	_, betaErr := beta.Predict(ctx, &betapb.PredictRequest{Endpoint: name})
	_, v1Err := v1.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: name})
	if status.Code(betaErr) != status.Code(v1Err) || status.Convert(betaErr).Message() != status.Convert(v1Err).Message() {
		t.Errorf("Predict: v1beta1 %v, v1 %v", betaErr, v1Err)
	}
}
