package vertexai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// recordingEmbedder is fakeEmbedder that remembers its last call.
type recordingEmbedder struct {
	fakeEmbedder
	mu   sync.Mutex
	in   embedding.Input
	dim  int
	auto bool
}

func (r *recordingEmbedder) Embed(ctx context.Context, in []embedding.Input, dim int, auto bool) ([]embedding.Output, error) {
	r.mu.Lock()
	r.in, r.dim, r.auto = in[0], dim, auto
	r.mu.Unlock()
	return r.fakeEmbedder.Embed(ctx, in, dim, auto)
}

const embedName = "projects/p/locations/us-central1/publishers/google/models/" + localai.EmbeddingGemmaONNXID

func postEmbed(t *testing.T, s *Server, version, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/"+version+"/projects/p/locations/us-central1/publishers/google/models/"+model+":embedContent", strings.NewReader(body))
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEmbedContentRESTWithoutRuntimeIs501(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(nil, errors.New("not compiled"))
	rec := postEmbed(t, s, "v1", localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]}}`)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not compiled") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// The same 501 :predict gives.
	if p := postPredict(t, s, localai.EmbeddingGemmaONNXID, `{"instances":[{"content":"x"}]}`); p.Code != rec.Code {
		t.Fatalf(":predict %d, :embedContent %d", p.Code, rec.Code)
	}
}

func TestEmbedContentREST(t *testing.T) {
	e := &recordingEmbedder{}
	s := NewServer(nil)
	s.SetEmbedder(e, nil)
	for _, v := range []string{"v1", APIVersion} {
		rec := postEmbed(t, s, v, localai.EmbeddingGemmaONNXID,
			`{"content":{"role":"user","parts":[{"text":"hello"}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT","title":"T","outputDimensionality":256}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", v, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Header().Get(ProvenanceHeader), "not an official Google artifact") {
			t.Fatal("provenance header missing")
		}
		resp := &aiplatformpb.EmbedContentResponse{}
		if err := protojson.Unmarshal(rec.Body.Bytes(), resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.GetEmbedding().GetValues()) != 256 || resp.GetUsageMetadata().GetPromptTokenCount() != 3 {
			t.Fatalf("response %s", rec.Body)
		}
		if e.in.TaskType != "RETRIEVAL_DOCUMENT" || e.in.Title != "T" || e.in.Content != "hello" || e.dim != 256 || !e.auto {
			t.Fatalf("embedder got %+v dim %d auto %v", e.in, e.dim, e.auto)
		}
	}

	// The deprecated top-level fields, in snake_case, are honoured too.
	rec := postEmbed(t, s, "v1", localai.EmbeddingGemmaONNXID,
		`{"content":{"parts":[{"text":"a"},{"text":"b"}]},"task_type":"CLUSTERING","output_dimensionality":128,"auto_truncate":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if e.in.TaskType != "CLUSTERING" || e.in.Content != "a\nb" || e.dim != 128 || e.auto {
		t.Fatalf("embedder got %+v dim %d auto %v", e.in, e.dim, e.auto)
	}

	refused := map[string]struct {
		model, body string
		code        int
		field       string
	}{
		"other model":     {"text-embedding-005", `{"content":{"parts":[{"text":"x"}]}}`, 404, "No substitution"},
		"bad dimension":   {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"outputDimensionality":300}}`, 400, "output_dimensionality"},
		"no content":      {localai.EmbeddingGemmaONNXID, `{}`, 400, "content"},
		"image part":      {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}`, 400, "content.parts[0]"},
		"document ocr":    {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"documentOcr":true}}`, 400, "document_ocr"},
		"audio":           {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"audioTrackExtraction":true}}`, 400, "audio_track_extraction"},
		"conflict":        {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]},"title":"a","embedContentConfig":{"title":"b"}}`, 400, "title"},
		"unknown field":   {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":"x"}]},"bogus":1}`, 400, "bogus"},
		"body model":      {localai.EmbeddingGemmaONNXID, `{"model":"projects/p/locations/l/publishers/google/models/other","content":{"parts":[{"text":"x"}]}}`, 400, "does not match"},
		"empty text":      {localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"text":" "}]}}`, 400, "empty"},
		"other publisher": {localai.EmbeddingGemmaONNXID, `{"model":"projects/p/locations/l/publishers/acme/models/x","content":{"parts":[{"text":"x"}]}}`, 404, "acme"},
	}
	for name, c := range refused {
		rec := postEmbed(t, s, "v1", c.model, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.field) {
			t.Errorf("%s: status %d, want %d naming %q: %s", name, rec.Code, c.code, c.field, rec.Body)
		}
	}
}

func TestGRPCEmbedContent(t *testing.T) {
	e := &recordingEmbedder{}
	s := NewServer(nil)
	s.SetEmbedder(e, nil)
	c, _ := startMixed(t, s)
	ctx := context.Background()
	text := &aiplatformpb.Content{Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: "hi"}}}}

	task := aiplatformpb.EmbedContentRequest_QUESTION_ANSWERING
	resp, err := c.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
		Model: proto.String(embedName), Content: text,
		EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{
			TaskType: &task, OutputDimensionality: proto.Int32(512),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetEmbedding().GetValues()) != 512 || e.in.TaskType != "QUESTION_ANSWERING" {
		t.Fatalf("got %d values, task %q", len(resp.GetEmbedding().GetValues()), e.in.TaskType)
	}
	// UNSPECIFIED is the model's default prefix.
	unspec := aiplatformpb.EmbedContentRequest_UNSPECIFIED
	if _, err := c.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{Model: proto.String(embedName), Content: text, TaskType: &unspec}); err != nil || e.in.TaskType != "" {
		t.Fatalf("UNSPECIFIED: %v, task %q", err, e.in.TaskType)
	}

	for name, c2 := range map[string]struct {
		req   *aiplatformpb.EmbedContentRequest
		code  codes.Code
		field string
	}{
		"bad dimension": {&aiplatformpb.EmbedContentRequest{Model: proto.String(embedName), Content: text, OutputDimensionality: proto.Int32(100)}, codes.InvalidArgument, "output_dimensionality"},
		"other model":   {&aiplatformpb.EmbedContentRequest{Model: proto.String("projects/p/locations/l/publishers/google/models/text-embedding-005"), Content: text}, codes.NotFound, "text-embedding-005"},
		"endpoint":      {&aiplatformpb.EmbedContentRequest{Model: proto.String("projects/p/locations/l/endpoints/e"), Content: text}, codes.InvalidArgument, "publisher models"},
		"no model":      {&aiplatformpb.EmbedContentRequest{Content: text}, codes.InvalidArgument, "model"},
	} {
		if _, err := c.EmbedContent(ctx, c2.req); status.Code(err) != c2.code || !strings.Contains(err.Error(), c2.field) {
			t.Errorf("%s: %v, want %v naming %q", name, err, c2.code, c2.field)
		}
	}

	s.SetEmbedder(nil, errors.New("not compiled"))
	if _, err := c.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{Model: proto.String(embedName), Content: text}); status.Code(err) != codes.Unimplemented {
		t.Errorf("no runtime: %v, want UNIMPLEMENTED", err)
	}
}
