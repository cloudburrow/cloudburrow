//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// seaTurtleMP4 is the video EmbeddingGemma 2's model card embeds: 8 seconds
// of a sea turtle, 1280x720 H.264 High at 59.94 frames a second with
// B-frames, in a fragmented MP4, from Transformers.js's documentation
// dataset (the jfk.wav clip's), pinned by revision and SHA-256.
const (
	seaTurtleURL    = "https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/542437c29cc5273cfd29e25c77696f2b349badd7/sea-turtle.mp4"
	seaTurtleSHA256 = "7ff15892d7fe47454892f96af058909a8fd44dad98e89ad8c0a6993b3fde86fd"
)

func fetchSeaTurtle(t *testing.T, ctx context.Context) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, seaTurtleURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("fetching the test video: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("fetching the test video: HTTP %d %v", resp.StatusCode, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != seaTurtleSHA256 {
		t.Fatalf("the test video's SHA-256 is %x, want %s", sum, seaTurtleSHA256)
	}
	return b
}

// embeddingTestdata reads a fixture of internal/embedding/testdata.
func embeddingTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../internal/embedding/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsVideoAndPDFThroughThePredictionClient (#1722,
// #1723) loads EmbeddingGemma 2 on the console's Local models page and
// embeds, through the official aiplatform PredictionClient's EmbedContent
// as Google documents for gemini-embedding-2:
//   - the model card's video (MP4, H.264) as an inline_data part: its 8
//     frames at 1 frame a second, each 120 soft tokens between the image
//     markers (978 tokens with BOS and EOS), nearest the query that
//     describes it, as the model card's example is (0.731 there);
//   - the same video from this instance's Cloud Storage as file_data, the
//     same vector; and its video_metadata interval and fps, fewer frames;
//   - an H.265 MOV;
//   - a PDF, its page rendered as an image, nearest the query its text
//     describes, and with document_ocr its text too;
//
// and refuses, naming it, what Google does not take or this build cannot
// decode: a second video, a 10-bit VP9 (profile 2), an fps out of range, a PDF of 7 pages, and a
// scan with document_ocr. (audio_track_extraction is served since #1732,
// TestEmbedContentEmbedsAVideosAudioTrackThroughThePredictionClient.)
func TestEmbedContentEmbedsVideoAndPDFThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	storageEndpoint := h.Endpoint(EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	video := fetchSeaTurtle(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	textPart := func(s string) *aiplatformpb.Part { return &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: s}} }
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
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
	queries := []string{"cats sleeping on a couch", "a president's speech about serving your country", "a turtle swimming in the ocean"}
	queryVecs := make([][]float32, len(queries))
	for i, q := range queries {
		queryVecs[i] = must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, nil, textPart(q))).GetEmbedding().GetValues()
	}
	scoresOf := func(v []float32) []float64 {
		s := make([]float64, len(queryVecs))
		for i, q := range queryVecs {
			s[i] = cosine(v, q)
		}
		return s
	}

	r := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("video/mp4", video)))
	vec := r.GetEmbedding().GetValues()
	if len(vec) != 768 || math.Abs(norm(vec)-1) > 1e-3 {
		t.Fatalf("video: %d values, norm %.6f", len(vec), norm(vec))
	}
	if n := r.GetUsageMetadata().GetPromptTokenCount(); n != 978 {
		t.Errorf("video: prompt token count %d, want 978 (8 frames of 120 soft tokens and 2 markers, BOS, EOS)", n)
	}
	s := scoresOf(vec)
	if s[2] < 0.7 || s[2] <= s[0]+0.1 || s[2] <= s[1]+0.1 {
		t.Errorf("video against the model card's queries: %.3f, want the turtle query far ahead", s)
	}
	t.Logf("video against the model card's queries %q: %.3f", queries, s)

	// video_metadata: 2 s to 4 s at 2 frames a second is 4 frames.
	part := inline("video/mp4", video)
	part.Metadata = &aiplatformpb.Part_VideoMetadata{VideoMetadata: &aiplatformpb.VideoMetadata{
		StartOffset: durationpb.New(2 * time.Second), EndOffset: durationpb.New(4 * time.Second), Fps: 2}}
	clip := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, part))
	if n := clip.GetUsageMetadata().GetPromptTokenCount(); n != 4*122+2 || cosine(clip.GetEmbedding().GetValues(), vec) > 0.9999 {
		t.Errorf("video_metadata: %d tokens, want %d; cosine with the whole video %.4f", n, 4*122+2, cosine(clip.GetEmbedding().GetValues(), vec))
	}

	// An H.265 MOV of the same clip's first 3 seconds, scaled down.
	mov := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("video/quicktime", embeddingTestdata(t, "turtle-h265.mov"))))
	if s := scoresOf(mov.GetEmbedding().GetValues()); s[2] <= s[0] || s[2] <= s[1] {
		t.Errorf("H.265 MOV against the queries: %.3f, want the turtle query first", s)
	}

	// The video in this instance's Cloud Storage, as file_data.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-video-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := sc.Bucket(bucket).Object("sea-turtle.mp4").NewWriter(ctx)
	w.ContentType = "video/mp4"
	if _, err := w.Write(video); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("sea-turtle.mp4").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	fd := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil,
		&aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{FileUri: "gs://" + bucket + "/sea-turtle.mp4"}}}))
	if c := cosine(fd.GetEmbedding().GetValues(), vec); c < 0.99999 {
		t.Errorf("file_data video: cosine with inline_data %.6f", c)
	}

	// A PDF: its page is an image of its text, nearest the query it describes.
	pdf := embeddingTestdata(t, "turtle.pdf")
	page := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("application/pdf", pdf)))
	ps := scoresOf(page.GetEmbedding().GetValues())
	if ps[2] <= ps[0]+0.1 || ps[2] <= ps[1]+0.1 {
		t.Errorf("PDF against the queries: %.3f, want the turtle query far ahead", ps)
	}
	yes := true
	ocr := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT,
		&aiplatformpb.EmbedContentRequest_EmbedContentConfig{DocumentOcr: &yes}, inline("application/pdf", pdf)))
	if ocr.GetUsageMetadata().GetPromptTokenCount() <= page.GetUsageMetadata().GetPromptTokenCount() ||
		cosine(ocr.GetEmbedding().GetValues(), page.GetEmbedding().GetValues()) > 0.9999 {
		t.Errorf("document_ocr: %d tokens, %d without; want the page's text added",
			ocr.GetUsageMetadata().GetPromptTokenCount(), page.GetUsageMetadata().GetPromptTokenCount())
	}
	if s := scoresOf(ocr.GetEmbedding().GetValues()); s[2] <= s[0]+0.1 || s[2] <= s[1]+0.1 {
		t.Errorf("PDF with document_ocr against the queries: %.3f, want the turtle query far ahead", s)
	}
	t.Logf("PDF against the queries: %.3f", ps)

	// A scanned PDF has no text layer: document_ocr reads it with OCR
	// (#1733), adding the invoice's text (about 150 tokens) to the page.
	scan := embeddingTestdata(t, "scan.pdf")
	scanPage := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, nil, inline("application/pdf", scan)))
	scanOCR := must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT,
		&aiplatformpb.EmbedContentRequest_EmbedContentConfig{DocumentOcr: &yes}, inline("application/pdf", scan)))
	added := scanOCR.GetUsageMetadata().GetPromptTokenCount() - scanPage.GetUsageMetadata().GetPromptTokenCount()
	if added < 100 || cosine(scanOCR.GetEmbedding().GetValues(), scanPage.GetEmbedding().GetValues()) > 0.9999 {
		t.Errorf("scan with document_ocr: %d tokens added; want the OCR text (about 150 tokens)", added)
	}
	t.Logf("scan with document_ocr: %d tokens added", added)

	badFPS := inline("video/mp4", video)
	badFPS.Metadata = &aiplatformpb.Part_VideoMetadata{VideoMetadata: &aiplatformpb.VideoMetadata{Fps: 30}}
	for what, c := range map[string]struct {
		parts []*aiplatformpb.Part
		cfg   *aiplatformpb.EmbedContentRequest_EmbedContentConfig
		field string
	}{
		"two videos":  {[]*aiplatformpb.Part{inline("video/mp4", video), inline("video/mp4", video)}, nil, "at most 1 video"},
		"vp9 10-bit":  {[]*aiplatformpb.Part{inline("video/mp4", embeddingTestdata(t, "turtle-vp9-10bit.mp4"))}, nil, "VP9 profile 2"},
		"fps":         {[]*aiplatformpb.Part{badFPS}, nil, "video_metadata.fps"},
		"seven pages": {[]*aiplatformpb.Part{inline("application/pdf", embeddingTestdata(t, "seven-pages.pdf"))}, nil, "at most 6 pages"},
	} {
		_, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, c.cfg, c.parts...)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming %q", what, err, c.field)
		}
	}
}
