//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// jfkWAV is the clip EmbeddingGemma 2's model card embeds: 11 seconds of
// President Kennedy's inaugural address (a US government work), a 16-bit
// stereo 44.1 kHz WAV in Transformers.js's documentation dataset, pinned by
// revision and SHA-256.
const (
	jfkWAVURL    = "https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/542437c29cc5273cfd29e25c77696f2b349badd7/jfk.wav"
	jfkWAVSHA256 = "aa81c2552465568567e670f3823117e633900d16bd6202346a72f3c8464c74c8"
)

func fetchJFK(t *testing.T, ctx context.Context) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, jfkWAVURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("fetching the test clip: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("fetching the test clip: HTTP %d %v", resp.StatusCode, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != jfkWAVSHA256 {
		t.Fatalf("the test clip's SHA-256 is %x, want %s", sum, jfkWAVSHA256)
	}
	return b
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsAudioThroughThePredictionClient (#1651) loads
// EmbeddingGemma 2 on the console's Local models page and embeds the model
// card's speech clip through the official aiplatform PredictionClient's
// EmbedContent, as Google documents for gemini-embedding-2: a WAV
// inline_data part (stereo 44.1 kHz, downmixed and resampled to 16 kHz) is
// one 768-value unit vector of 279 tokens (275 soft tokens, 25 a second, the
// audio markers, BOS and EOS), nearest the query that describes it, as the
// model card's example is. The same clip uploaded with the official Cloud
// Storage client and sent as a file_data gs:// part, with no mime_type (the
// object's content type stands in), is the same vector, as is a PNG image
// there; text with the clip is one vector; the console's Embeddings API
// gives the same vector for the same file_data part; and what is not served
// (MP3, video, PDF, audio_track_extraction, a missing object) is refused
// naming it.
func TestEmbedContentEmbedsAudioThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	storageEndpoint := h.Endpoint(EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	clip := fetchJFK(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	textPart := func(s string) *aiplatformpb.Part { return &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: s}} }
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}
	fileData := func(mt, uri string) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{MimeType: mt, FileUri: uri}}}
	}
	embed := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, cfg *aiplatformpb.EmbedContentRequest_EmbedContentConfig, parts ...*aiplatformpb.Part) (*aiplatformpb.EmbedContentResponse, error) {
		if cfg == nil {
			cfg = &aiplatformpb.EmbedContentRequest_EmbedContentConfig{}
		}
		cfg.TaskType = &task
		return pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts}, EmbedContentConfig: cfg,
		})
	}
	must := func(r *aiplatformpb.EmbedContentResponse, err error) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return r
	}

	r := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("audio/wav", clip)))
	audio := r.GetEmbedding().GetValues()
	if len(audio) != 768 || math.Abs(norm(audio)-1) > 1e-3 {
		t.Fatalf("audio: %d values, norm %.6f", len(audio), norm(audio))
	}
	if n := r.GetUsageMetadata().GetPromptTokenCount(); n != 279 {
		t.Errorf("audio: prompt token count %d, want 279 (275 soft tokens, 2 audio markers, BOS, EOS)", n)
	}
	// The model card's queries; its clip scores 0.763 with the speech one.
	queries := []string{"a president's speech about serving your country", "cats sleeping on a couch", "a turtle swimming in the ocean"}
	scores := make([]float64, len(queries))
	for i, q := range queries {
		scores[i] = cosine(must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, nil, textPart(q))).GetEmbedding().GetValues(), audio)
	}
	if scores[0] < 0.7 || scores[0] <= scores[1]+0.1 || scores[0] <= scores[2]+0.1 {
		t.Errorf("audio against the model card's queries: %.3f, want the speech query far ahead", scores)
	}
	t.Logf("audio against the model card's queries %q: %.3f", queries, scores)

	// Text with the clip is one vector, near the clip.
	mixed := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, textPart("Speech:"), inline("audio/wav", clip)))
	if mv := mixed.GetEmbedding().GetValues(); len(mv) != 768 || mixed.GetUsageMetadata().GetPromptTokenCount() <= 279 || cosine(mv, audio) > 0.9999 || cosine(mv, audio) < 0.8 {
		t.Errorf("text and audio: %d values, %d tokens, cosine with the clip %.4f", len(mv), mixed.GetUsageMetadata().GetPromptTokenCount(), cosine(mv, audio))
	}

	// Two clips are one vector of both, as several images are.
	two := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("audio/wav", clip), inline("audio/wav", clip)))
	if n := two.GetUsageMetadata().GetPromptTokenCount(); n != 2*277+3 || cosine(two.GetEmbedding().GetValues(), audio) < 0.9 {
		t.Errorf("two clips: %d tokens, want %d (BOS, each clip with its markers, the space between them, EOS); cosine with one %.4f", n, 2*277+3, cosine(two.GetEmbedding().GetValues(), audio))
	}

	// The same clip, and an image, in this instance's Cloud Storage.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-audio-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	png := shapeImage(t, "circle", color.RGBA{220, 20, 30, 255})
	for object, o := range map[string]struct {
		data []byte
		ct   string
	}{"speech/jfk.wav": {clip, "audio/wav"}, "circle.png": {png, "image/png"}} {
		w := sc.Bucket(bucket).Object(object).NewWriter(ctx)
		w.ContentType = o.ct
		if _, err := w.Write(o.data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", object, err)
		}
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("speech/jfk.wav").Delete(context.Background())
		_ = sc.Bucket(bucket).Object("circle.png").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	uri := "gs://" + bucket + "/speech/jfk.wav"
	fd := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, fileData("", uri)))
	if c := cosine(fd.GetEmbedding().GetValues(), audio); c < 0.99999 || fd.GetUsageMetadata().GetPromptTokenCount() != 279 {
		t.Errorf("file_data audio: cosine with inline_data %.6f, %d tokens", c, fd.GetUsageMetadata().GetPromptTokenCount())
	}
	img := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("image/png", png))).GetEmbedding().GetValues()
	fdImg := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, fileData("image/png", "gs://"+bucket+"/circle.png")))
	if c := cosine(fdImg.GetEmbedding().GetValues(), img); c < 0.99999 {
		t.Errorf("file_data image: cosine with inline_data %.6f", c)
	}

	// The console's Embeddings screen sends the same file_data part and gets the same vector.
	code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=embedContent&model="+embeddingModel2,
		`{"content":{"role":"user","parts":[{"fileData":{"fileUri":"`+uri+`"}}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT"}}`)
	var got struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil {
		t.Fatalf("console :embedContent = %d %s", code, body)
	}
	if c := cosine(got.Embedding.Values, audio); c < 0.99999 {
		t.Errorf("console file_data: cosine with the SDK's %.6f", c)
	}
	// And the same inline clip.
	code, body = consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=embedContent&model="+embeddingModel2,
		`{"content":{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"`+base64.StdEncoding.EncodeToString(clip)+`"}}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT"}}`)
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || cosine(got.Embedding.Values, audio) < 0.99999 {
		t.Errorf("console inline audio = %d %.200s", code, body)
	}

	yes := true
	for what, c := range map[string]struct {
		parts []*aiplatformpb.Part
		cfg   *aiplatformpb.EmbedContentRequest_EmbedContentConfig
		code  codes.Code
		field string
	}{
		"mp3":         {[]*aiplatformpb.Part{inline("audio/mpeg", []byte{0xff, 0xfb, 0x90, 0})}, nil, codes.InvalidArgument, "audio/mpeg"},
		"video":       {[]*aiplatformpb.Part{inline("video/mp4", []byte{0})}, nil, codes.InvalidArgument, "inline_data.mime_type"},
		"pdf":         {[]*aiplatformpb.Part{fileData("application/pdf", uri)}, nil, codes.InvalidArgument, "file_data.mime_type"},
		"audio track": {[]*aiplatformpb.Part{textPart("x")}, &aiplatformpb.EmbedContentRequest_EmbedContentConfig{AudioTrackExtraction: &yes}, codes.InvalidArgument, "audio_track_extraction"},
		"ocr":         {[]*aiplatformpb.Part{textPart("x")}, &aiplatformpb.EmbedContentRequest_EmbedContentConfig{DocumentOcr: &yes}, codes.InvalidArgument, "document_ocr"},
		"missing":     {[]*aiplatformpb.Part{fileData("audio/wav", "gs://"+bucket+"/none.wav")}, nil, codes.NotFound, "none.wav"},
	} {
		_, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, c.cfg, c.parts...)
		if status.Code(err) != c.code || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v, want %v naming %q", what, err, c.code, c.field)
		}
	}
}
