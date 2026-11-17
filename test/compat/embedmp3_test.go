//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsMP3ThroughThePredictionClient (#1721) embeds MP3
// audio through the official aiplatform PredictionClient's EmbedContent, in
// the shape Google's gemini-embedding-2 samples use (mime_type audio/mpeg,
// inline or as a file_data gs:// URI). testdata/jfk.mp3 is the model card's
// jfk.wav (fetched and checked by SHA-256 as the WAV test does) encoded by
// LAME 4.0 at 64 kbit/s, 44.1 kHz stereo; its vector is within cosine 0.99
// of the WAV's, of the same 279 tokens, and nearest the speech query. The
// same file uploaded with the official Cloud Storage client and sent as
// file_data, with no mime_type, is the inline vector, as is the console's
// Embeddings API's. A file that is not an MP3, and more than 180 seconds of
// MP3, are INVALID_ARGUMENT naming why.
func TestEmbedContentEmbedsMP3ThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	storageEndpoint := h.Endpoint(EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	mp3, err := os.ReadFile("testdata/jfk.mp3")
	if err != nil {
		t.Fatal(err)
	}
	wav := fetchJFK(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}
	embed := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, parts ...*aiplatformpb.Part) (*aiplatformpb.EmbedContentResponse, error) {
		return pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task},
		})
	}
	must := func(r *aiplatformpb.EmbedContentResponse, err error) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return r
	}
	doc := aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT

	fromWAV := must(embed(doc, inline("audio/wav", wav))).GetEmbedding().GetValues()
	r := must(embed(doc, inline("audio/mpeg", mp3)))
	fromMP3 := r.GetEmbedding().GetValues()
	if len(fromMP3) != 768 || math.Abs(norm(fromMP3)-1) > 1e-3 {
		t.Fatalf("MP3: %d values, norm %.6f", len(fromMP3), norm(fromMP3))
	}
	if n := r.GetUsageMetadata().GetPromptTokenCount(); n != 279 {
		t.Errorf("MP3: prompt token count %d, want the WAV's 279", n)
	}
	c := cosine(fromMP3, fromWAV)
	if c < 0.99 {
		t.Errorf("MP3 against the WAV it was made from: cosine %.4f, want at least 0.99", c)
	}
	t.Logf("MP3 against the WAV it was made from: cosine %.6f", c)
	queries := []string{"a president's speech about serving your country", "cats sleeping on a couch", "a turtle swimming in the ocean"}
	scores := make([]float64, len(queries))
	for i, q := range queries {
		scores[i] = cosine(must(embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY,
			&aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: q}})).GetEmbedding().GetValues(), fromMP3)
	}
	if scores[0] < 0.7 || scores[0] <= scores[1]+0.1 || scores[0] <= scores[2]+0.1 {
		t.Errorf("MP3 against the model card's queries: %.3f, want the speech query far ahead", scores)
	}
	t.Logf("MP3 against the model card's queries %q: %.3f", queries, scores)
	// audio/mp3, the other name, is the same.
	if v := must(embed(doc, inline("audio/mp3", mp3))).GetEmbedding().GetValues(); cosine(v, fromMP3) < 0.99999 {
		t.Errorf("audio/mp3: cosine with audio/mpeg %.6f", cosine(v, fromMP3))
	}

	// The same file in this instance's Cloud Storage, as file_data.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-mp3-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := sc.Bucket(bucket).Object("speech/jfk.mp3").NewWriter(ctx)
	w.ContentType = "audio/mpeg"
	if _, err := w.Write(mp3); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("speech/jfk.mp3").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	uri := "gs://" + bucket + "/speech/jfk.mp3"
	fd := must(embed(doc, &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{FileUri: uri}}}))
	if c := cosine(fd.GetEmbedding().GetValues(), fromMP3); c < 0.99999 || fd.GetUsageMetadata().GetPromptTokenCount() != 279 {
		t.Errorf("file_data MP3: cosine with inline_data %.6f, %d tokens", c, fd.GetUsageMetadata().GetPromptTokenCount())
	}

	// The console's Embeddings API, which its Audio picker sends, gives the same vector.
	code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=embedContent&model="+embeddingModel2,
		`{"content":{"role":"user","parts":[{"inlineData":{"mimeType":"audio/mpeg","data":"`+base64.StdEncoding.EncodeToString(mp3)+`"}}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT"}}`)
	var got struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || cosine(got.Embedding.Values, fromMP3) < 0.99999 {
		t.Errorf("console inline MP3 = %d %.200s", code, body)
	}

	// 17 copies of the 11-second file are 187 seconds.
	for what, c := range map[string]struct {
		data []byte
		want string
	}{
		"not an MP3": {wav, "parts[0].inline_data: the data is not a valid MP3 file"},
		"truncated":  {mp3[:3], "the data is not a valid MP3 file"},
		"too long":   {bytes.Repeat(mp3, 17), "at most 180 seconds are embedded"},
	} {
		_, err := embed(doc, inline("audio/mpeg", c.data))
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want InvalidArgument naming %q", what, err, c.want)
		}
	}
}
