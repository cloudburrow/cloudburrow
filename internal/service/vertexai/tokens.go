package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// countTokens and computeTokens (#1937), counted by the model's own
// tokenizer inside the runtime (cloudburrow_generate's count_only and
// compute modes), on the REST routes, LlmUtilityService (v1 and v1beta1) and
// v1beta1 PredictionService.
//
// countTokens' totalTokens is the same count generateContent reports as
// usageMetadata.promptTokenCount: LiteRT-LM's Conversation::CountTokens of
// the rendered conversation, so the chat template's turn markers, the
// declared tools and each image's soft tokens are included. UNVERIFIED
// (docs re-read 2026-11-19, still unanswered:
// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/get-token-count):
// whether Google's count includes template tokens, system instructions or
// tools; the page says only that multimodal counts "are an estimation".
// computeTokens tokenizes each content's text alone, with no template; the
// list-token page documents it as text only: "The prompt must contain only
// text. Multimodal prompts are not supported."

// CountTokensRequest is the countTokens body.
type CountTokensRequest struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	Instances         json.RawMessage   `json:"instances,omitempty"`
	Model             string            `json:"model,omitempty"`
	Endpoint          string            `json:"endpoint,omitempty"`
}

// CountTokensResponse is the countTokens answer.
type CountTokensResponse struct {
	TotalTokens         int32                `json:"totalTokens"`
	PromptTokensDetails []ModalityTokenCount `json:"promptTokensDetails,omitempty"`
}

// ComputeTokensRequest is the computeTokens body.
type ComputeTokensRequest struct {
	Contents  []Content       `json:"contents"`
	Instances json.RawMessage `json:"instances,omitempty"`
	Model     string          `json:"model,omitempty"`
	Endpoint  string          `json:"endpoint,omitempty"`
}

// TokensInfo is one content's tokens; tokenIds are int64, so strings in
// JSON, and tokens bytes, so base64.
type TokensInfo struct {
	Tokens   [][]byte `json:"tokens"`
	TokenIDs []string `json:"tokenIds"`
	Role     string   `json:"role,omitempty"`
}

// ComputeTokensResponse is the computeTokens answer.
type ComputeTokensResponse struct {
	TokensInfo []TokensInfo `json:"tokensInfo"`
}

func strictDecode(body io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apierror.InvalidArgument("malformed request body: %v", err)
	}
	return nil
}

// tokenCounter returns the generator's tokenizer, or why there is none.
func (s *Server) tokenCounter(requested string) (TokenCounter, error) {
	if _, err := s.resolveModel(requested); err != nil {
		return nil, err
	}
	tc, ok := s.gen.(TokenCounter)
	if !ok || !honoursOptions(s.gen) {
		return nil, apierror.Unimplemented(
			"token counting needs cloudburrow_generate, which exposes the model's tokenizer; this runtime does not, " +
				"and a count computed by any other means would not be the model's")
	}
	return tc, nil
}

// countTokens serves :countTokens for every transport.
func (s *Server) countTokens(ctx context.Context, requested string, body io.Reader) (*CountTokensResponse, error) {
	tc, err := s.tokenCounter(requested)
	if err != nil {
		return nil, err
	}
	var req CountTokensRequest
	if err := strictDecode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Instances) > 0 && string(req.Instances) != "[]" {
		return nil, apierror.Unimplemented("instances is not supported: send contents")
	}
	g := &GenerateContentRequest{Contents: req.Contents, SystemInstruction: req.SystemInstruction,
		Tools: req.Tools, GenerationConfig: req.GenerationConfig}
	if err := validateTools(g.Tools, nil); err != nil {
		return nil, err
	}
	if err := validateConversation(g, false); err != nil {
		return nil, err
	}
	rr, vision, err := toRunner(ctx, g, s.objects)
	if err != nil {
		return nil, err
	}
	n, err := tc.CountTokens(ctx, Request{Conversation: rr, Vision: vision})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apierror.Internal(err, "counting tokens failed: %v", err)
	}
	out := &CountTokensResponse{TotalTokens: int32(n)}
	if !vision {
		out.PromptTokensDetails = []ModalityTokenCount{{Modality: "TEXT", TokenCount: int32(n)}}
	}
	return out, nil
}

// computeTokens serves :computeTokens for every transport.
func (s *Server) computeTokens(ctx context.Context, requested string, body io.Reader) (*ComputeTokensResponse, error) {
	tc, err := s.tokenCounter(requested)
	if err != nil {
		return nil, err
	}
	var req ComputeTokensRequest
	if err := strictDecode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Instances) > 0 && string(req.Instances) != "[]" {
		return nil, apierror.Unimplemented("instances is not supported: send contents")
	}
	if len(req.Contents) == 0 {
		return nil, apierror.InvalidArgument("contents is required")
	}
	texts := make([]string, len(req.Contents))
	for i, c := range req.Contents {
		if c.Role != "" && c.Role != "user" && c.Role != "model" {
			return nil, apierror.InvalidArgument("contents[%d].role = %q. %s", i, c.Role, validRoles)
		}
		var b strings.Builder
		for j, p := range c.Parts {
			if p.kind() != "text" {
				// "The prompt must contain only text. Multimodal prompts are
				// not supported." (https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/list-token)
				return nil, apierror.InvalidArgument("contents[%d].parts[%d]: computeTokens tokenizes text parts only", i, j)
			}
			b.WriteString(p.Text)
		}
		texts[i] = b.String()
	}
	infos, err := tc.ComputeTokens(ctx, texts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apierror.Internal(err, "computing tokens failed: %v", err)
	}
	out := &ComputeTokensResponse{}
	for i, info := range infos {
		ti := TokensInfo{Tokens: info.Tokens, Role: req.Contents[i].Role}
		if ti.Tokens == nil {
			ti.Tokens = [][]byte{}
		}
		ti.TokenIDs = make([]string, len(info.IDs))
		for j, id := range info.IDs {
			ti.TokenIDs[j] = strconv.FormatInt(id, 10)
		}
		out.TokensInfo = append(out.TokensInfo, ti)
	}
	return out, nil
}

// tokensREST serves the two verbs' JSON routes.
func (s *Server) tokensREST(w http.ResponseWriter, r *http.Request, requested, verb string) {
	var resp any
	var err error
	if verb == "countTokens" {
		resp, err = s.countTokens(r.Context(), requested, r.Body)
	} else {
		resp, err = s.computeTokens(r.Context(), requested, r.Body)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// llmUtility is LlmUtilityService, v1.
type llmUtility struct {
	aiplatformpb.UnimplementedLlmUtilityServiceServer
	s *Server
}

// tokensBody is a token request's JSON body and its publisher model.
func tokensBody(endpoint, model string, req any) (string, []byte, error) {
	name := endpoint
	if name == "" {
		name = model
	}
	t, err := parseTarget("endpoint", name)
	if err != nil {
		return "", nil, err
	}
	if t.model == "" {
		return "", nil, apierror.Unimplemented("token counting is served only on publisher models (projects/{p}/locations/{l}/publishers/google/models/{m})")
	}
	if err := t.checkPublisher(); err != nil {
		return "", nil, err
	}
	var j []byte
	switch m := req.(type) {
	case *aiplatformpb.CountTokensRequest:
		j, err = protojson.Marshal(m)
	case *aiplatformpb.ComputeTokensRequest:
		j, err = protojson.Marshal(m)
	}
	if err != nil {
		return "", nil, apierror.InvalidArgument("%v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(j, &fields); err != nil {
		return "", nil, apierror.Internal(err, "re-encoding the request: %v", err)
	}
	delete(fields, "model")
	delete(fields, "endpoint")
	body, err := json.Marshal(fields)
	if err != nil {
		return "", nil, apierror.Internal(err, "re-encoding the request: %v", err)
	}
	return t.model, body, nil
}

func (l *llmUtility) CountTokens(ctx context.Context, req *aiplatformpb.CountTokensRequest) (*aiplatformpb.CountTokensResponse, error) {
	model, body, err := tokensBody(req.GetEndpoint(), req.GetModel(), req)
	if err != nil {
		return nil, grpcError(err)
	}
	resp, err := l.s.countTokens(ctx, model, bytes.NewReader(body))
	if err != nil {
		return nil, grpcError(err)
	}
	out := &aiplatformpb.CountTokensResponse{}
	return out, grpcError(jsonToProto(resp, out))
}

func (l *llmUtility) ComputeTokens(ctx context.Context, req *aiplatformpb.ComputeTokensRequest) (*aiplatformpb.ComputeTokensResponse, error) {
	model, body, err := tokensBody(req.GetEndpoint(), req.GetModel(), req)
	if err != nil {
		return nil, grpcError(err)
	}
	resp, err := l.s.computeTokens(ctx, model, bytes.NewReader(body))
	if err != nil {
		return nil, grpcError(err)
	}
	out := &aiplatformpb.ComputeTokensResponse{}
	return out, grpcError(jsonToProto(resp, out))
}

// jsonToProto converts a JSON-route answer to its proto.
func jsonToProto(v any, out proto.Message) error {
	b, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encoding the response: %v", err)
	}
	if err := lenient.Unmarshal(b, out); err != nil {
		return apierror.Internal(err, "the answer does not fit its message: %v", err)
	}
	return nil
}

// betaLlmUtility is LlmUtilityService, v1beta1 (ComputeTokens only; its
// CountTokens is on v1beta1 PredictionService), bridged to v1.
type betaLlmUtility struct {
	betapb.UnimplementedLlmUtilityServiceServer
	s *Server
}

func (b *betaLlmUtility) ComputeTokens(ctx context.Context, req *betapb.ComputeTokensRequest) (*betapb.ComputeTokensResponse, error) {
	return unary(req, &aiplatformpb.ComputeTokensRequest{}, &betapb.ComputeTokensResponse{},
		func(v *aiplatformpb.ComputeTokensRequest) (*aiplatformpb.ComputeTokensResponse, error) {
			return (&llmUtility{s: b.s}).ComputeTokens(ctx, v)
		})
}

// registerTokens registers LlmUtilityService, v1 and v1beta1, on srv.
func (s *Server) registerTokens(srv *grpc.Server) {
	aiplatformpb.RegisterLlmUtilityServiceServer(srv, &llmUtility{s: s})
	betapb.RegisterLlmUtilityServiceServer(srv, &betaLlmUtility{s: s})
}
