//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// covers: google.cloud.aiplatform.v1.PredictionService/Predict
//
// TestMultimodalEmbeddingPredictThroughThePredictionClient (#1651) loads
// EmbeddingGemma 2 on the console's Local models page and sends it, through
// the official aiplatform PredictionClient's Predict, multimodalembedding@001's
// request as Google documents it (get-multimodal-embeddings): one instance of
// text, image and video, each answered with its own embedding:
//   - text and a PNG image at dimension 256: textEmbedding and
//     imageEmbedding of 256 values;
//   - the model card's sea turtle video (8 seconds) with the default
//     videoSegmentConfig: one segment, 0 to 8 seconds, of 768 values
//     (video is not reduced), nearest the turtle query's textEmbedding;
//     and with intervalSec 4, two segments, 0 to 4 and 4 to 8;
//   - the image and the video as gcsUri objects in this instance's Cloud
//     Storage, the same vectors as inline;
//   - a VP9 WebM and an H.264 MKV (Google takes MKV and WEBM), each the
//     same vector as the MP4 it was remuxed from;
//   - AVI, FLV, MPEG and WMV (Google takes them too), decoded by FFmpeg in
//     the instance's video decoder container: the MP4's H.264 stream
//     copied into AVI and FLV gives the MP4's vector, and MPEG-4 Part 2
//     AVI, Sorenson Spark FLV, MPEG-2 and WMV 8 re-encodings of it a vector
//     within their encoders' loss of it;
//   - text over 32 tokens shortened to its first 32 (Google: "the model
//     internally shortens the input to this length"): 40 one-token words
//     embed as their first 32 do, and not as their first 31;
//
// and refuses what Google refuses, or this build cannot do, naming it: two
// instances, a dimension Google does not offer, an interval under 4 seconds
// (INVALID_ARGUMENT) and dimension 1408 (the local model's vectors have 768
// values; UNIMPLEMENTED); and multimodalembedding@001 itself is NOT_FOUND,
// nothing being substituted.
func TestMultimodalEmbeddingPredictThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	storageEndpoint := h.Endpoint(EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	turtle := fetchSeaTurtle(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	endpoint := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	predict := func(params map[string]any, instances ...map[string]any) (*aiplatformpb.PredictResponse, error) {
		req := &aiplatformpb.PredictRequest{Endpoint: endpoint}
		for _, in := range instances {
			v, err := structpb.NewValue(in)
			if err != nil {
				t.Fatal(err)
			}
			req.Instances = append(req.Instances, v)
		}
		if params != nil {
			v, err := structpb.NewValue(params)
			if err != nil {
				t.Fatal(err)
			}
			req.Parameters = v
		}
		return pc.Predict(ctx, req)
	}
	type segment struct {
		start, end int
		vec        []float32
	}
	type prediction struct {
		text, image []float32
		video       []segment
	}
	floats := func(v *structpb.Value) []float32 {
		var out []float32
		for _, x := range v.GetListValue().GetValues() {
			out = append(out, float32(x.GetNumberValue()))
		}
		return out
	}
	must := func(r *aiplatformpb.PredictResponse, err error) prediction {
		t.Helper()
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if len(r.GetPredictions()) != 1 {
			t.Fatalf("Predict: %d predictions, want 1", len(r.GetPredictions()))
		}
		f := r.GetPredictions()[0].GetStructValue().GetFields()
		p := prediction{text: floats(f["textEmbedding"]), image: floats(f["imageEmbedding"])}
		for _, s := range f["videoEmbeddings"].GetListValue().GetValues() {
			sf := s.GetStructValue().GetFields()
			p.video = append(p.video, segment{int(sf["startOffsetSec"].GetNumberValue()), int(sf["endOffsetSec"].GetNumberValue()), floats(sf["embedding"])})
		}
		return p
	}

	png := embeddingTestdata(t, "pattern.png")
	p := must(predict(map[string]any{"dimension": 256},
		map[string]any{"text": "a test pattern", "image": map[string]any{"bytesBase64Encoded": b64(png)}}))
	if len(p.text) != 256 || len(p.image) != 256 || p.video != nil {
		t.Fatalf("text and image at 256: %d and %d values, video %v", len(p.text), len(p.image), p.video)
	}

	queries := []string{"cats sleeping on a couch", "a president's speech about serving your country", "a turtle swimming in the ocean"}
	queryVecs := make([][]float32, len(queries))
	for i, q := range queries {
		queryVecs[i] = must(predict(nil, map[string]any{"text": q})).text
		if len(queryVecs[i]) != 768 {
			t.Fatalf("query %q: %d values", q, len(queryVecs[i]))
		}
	}
	video := func(m map[string]any) map[string]any { return map[string]any{"video": m} }
	whole := must(predict(map[string]any{"dimension": 128}, video(map[string]any{"bytesBase64Encoded": b64(turtle)})))
	if len(whole.video) != 1 || whole.video[0].start != 0 || whole.video[0].end != 8 || len(whole.video[0].vec) != 768 {
		t.Fatalf("default segments: %+v", whole.video)
	}
	var s [3]float64
	for i := range s {
		s[i] = cosine(whole.video[0].vec, queryVecs[i])
	}
	if s[2] <= s[0] || s[2] <= s[1] {
		t.Errorf("video segment against the queries: %.3f, want the turtle query first", s)
	}
	t.Logf("video segment against the queries %q: %.3f", queries, s)
	four := must(predict(nil, video(map[string]any{"bytesBase64Encoded": b64(turtle), "videoSegmentConfig": map[string]any{"intervalSec": 4}})))
	if len(four.video) != 2 || four.video[0].start != 0 || four.video[0].end != 4 || four.video[1].start != 4 || four.video[1].end != 8 {
		t.Fatalf("intervalSec 4: %+v", four.video)
	}
	if c := cosine(four.video[0].vec, four.video[1].vec); c > 0.9999 {
		t.Errorf("the two segments' embeddings are the same (cosine %.6f)", c)
	}

	// From this instance's Cloud Storage.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-mm-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	put := func(name string, data []byte) {
		w := sc.Bucket(bucket).Object(name).NewWriter(ctx)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		t.Cleanup(func() { _ = sc.Bucket(bucket).Object(name).Delete(context.Background()) })
	}
	put("pattern.png", png)
	put("sea-turtle.mp4", turtle)
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })
	gcs := must(predict(map[string]any{"dimension": 256}, map[string]any{
		"image": map[string]any{"gcsUri": "gs://" + bucket + "/pattern.png"},
		"video": map[string]any{"gcsUri": "gs://" + bucket + "/sea-turtle.mp4"},
	}))
	if c := cosine(gcs.image, p.image); c < 0.99999 {
		t.Errorf("gcsUri image: cosine with inline %.6f", c)
	}
	if len(gcs.video) != 1 || cosine(gcs.video[0].vec, whole.video[0].vec) < 0.99999 {
		t.Errorf("gcsUri video: %+v", gcs.video)
	}

	// WebM and MKV: the frames of the MP4 they were remuxed from.
	for _, c := range []struct{ remux, from string }{{"turtle-vp9.webm", "turtle-vp9.mp4"}, {"turtle-h264.mkv", "turtle-h264.mp4"}} {
		a := must(predict(nil, video(map[string]any{"bytesBase64Encoded": b64(embeddingTestdata(t, c.remux))})))
		b := must(predict(nil, video(map[string]any{"bytesBase64Encoded": b64(embeddingTestdata(t, c.from))})))
		if len(a.video) != 1 || len(b.video) != 1 || cosine(a.video[0].vec, b.video[0].vec) < 0.99999 {
			t.Errorf("%s against %s: %+v, %+v", c.remux, c.from, a.video, b.video)
		}
	}

	// AVI, FLV, MPEG and WMV, through the video decoder container.
	h264 := must(predict(nil, video(map[string]any{"bytesBase64Encoded": b64(embeddingTestdata(t, "turtle-h264.mp4"))})))
	for _, c := range []struct {
		file string
		min  float64
	}{
		{"turtle-h264.avi", 0.99999}, {"turtle-h264.flv", 0.99999},
		{"turtle-mpeg4.avi", 0.98}, {"turtle-flv1.flv", 0.98}, {"turtle-mpeg2.mpg", 0.98}, {"turtle-wmv2.wmv", 0.98},
	} {
		a := must(predict(nil, video(map[string]any{"bytesBase64Encoded": b64(embeddingTestdata(t, c.file))})))
		if len(a.video) != 1 || a.video[0].start != 0 || a.video[0].end != 3 || len(h264.video) != 1 {
			t.Errorf("%s: segments %+v, the MP4's %+v", c.file, a.video, h264.video)
			continue
		}
		cs := cosine(a.video[0].vec, h264.video[0].vec)
		if cs < c.min {
			t.Errorf("%s against turtle-h264.mp4: cosine %.6f, want at least %v", c.file, cs, c.min)
		}
		t.Logf("%s against turtle-h264.mp4: cosine %.6f", c.file, cs)
	}

	// Text over 32 tokens is shortened to 32.
	words := strings.Fields("the cat sat on the mat and the dog ran in the park with a red ball while the sun was high in the blue sky " +
		"over the green hill near the old town by the sea and the wind blew softly")
	if len(words) < 40 {
		t.Fatalf("%d words", len(words))
	}
	text := func(n int) string { return strings.Join(words[:n], " ") }
	long := must(predict(nil, map[string]any{"text": text(40)})).text
	first32 := must(predict(nil, map[string]any{"text": text(32)})).text
	first31 := must(predict(nil, map[string]any{"text": text(31)})).text
	if c := cosine(long, first32); c < 0.99999 {
		t.Errorf("40 words against their first 32: cosine %.6f, want the same vector", c)
	}
	if c := cosine(long, first31); c > 0.99999 {
		t.Errorf("40 words against their first 31: cosine %.6f, want a different vector", c)
	}

	for what, c := range map[string]struct {
		params    map[string]any
		instances []map[string]any
		code      codes.Code
		field     string
	}{
		"two instances":  {nil, []map[string]any{{"text": "a"}, {"text": "b"}}, codes.InvalidArgument, "one instance per request"},
		"dimension 768":  {map[string]any{"dimension": 768}, []map[string]any{{"text": "a"}}, codes.InvalidArgument, "128, 256, 512 or 1408"},
		"dimension 1408": {map[string]any{"dimension": 1408}, []map[string]any{{"text": "a"}}, codes.Unimplemented, "768 values"},
		"interval 3": {nil, []map[string]any{video(map[string]any{"bytesBase64Encoded": b64(turtle),
			"videoSegmentConfig": map[string]any{"intervalSec": 3}})}, codes.InvalidArgument, "minimum of 4"},
	} {
		_, err := predict(c.params, c.instances...)
		if status.Code(err) != c.code || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v, want %v naming %q", what, err, c.code, c.field)
		}
	}
	_, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{
		Endpoint:  "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/multimodalembedding@001",
		Instances: []*structpb.Value{structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"text": structpb.NewStringValue("a")}})},
	})
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), embeddingModel2) {
		t.Errorf("multimodalembedding@001: %v, want NOT_FOUND naming %s", err, embeddingModel2)
	}
}
