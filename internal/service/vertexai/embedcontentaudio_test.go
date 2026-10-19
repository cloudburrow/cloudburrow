package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// audioEmbedder is multimodalEmbedder with its audio encoder (#1651).
type audioEmbedder struct{ multimodalEmbedder }

func (*audioEmbedder) EmbedsAudio() bool { return true }

// testWAV is n samples of 16-bit mono 16 kHz silence with one click.
func testWAV(n int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + 2*n))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(16000))
	w(uint32(32000))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(2 * n))
	for i := 0; i < n; i++ {
		v := int16(0)
		if i == n/2 {
			v = 16384
		}
		w(v)
	}
	return b.Bytes()
}

// fakeObjects is a Cloud Storage of objects by gs:// URI.
type fakeObjects map[string]struct {
	data []byte
	ct   string
}

func (f fakeObjects) Read(_ context.Context, bucket, object string, _ int64) ([]byte, string, error) {
	o, ok := f["gs://"+bucket+"/"+object]
	if !ok {
		return nil, "", apierror.NotFound("gs://%s/%s not found", bucket, object)
	}
	return o.data, o.ct, nil
}

func TestEmbedContentAudioAndFileDataOnEmbeddingGemma2(t *testing.T) {
	e := &audioEmbedder{}
	s := NewServer(nil)
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	wav := testWAV(16000)
	b64 := base64.StdEncoding.EncodeToString(wav)

	rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID,
		`{"content":{"parts":[{"text":"Speech:"},{"inlineData":{"mimeType":"audio/wav","data":"`+b64+`"}}]}}`)
	if rec.Code != http.StatusOK || len(e.in.Parts) != 2 || len(e.in.Parts[1].Audio) != 16000 {
		t.Fatalf("inline audio: status %d, parts %+v: %s", rec.Code, len(e.in.Parts), rec.Body)
	}

	// file_data before Cloud Storage is attached says so.
	body := `{"content":{"parts":[{"fileData":{"fileUri":"gs://b/clip.wav"}}]}}`
	if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Cloud Storage") {
		t.Fatalf("no storage: status %d: %s", rec.Code, rec.Body)
	}
	s.UseStorage(fakeObjects{
		"gs://b/clip.wav":  {wav, "audio/wav"},
		"gs://b/img.png":   {mustB64(t, testPNG(t, 32, 32)), "application/octet-stream"},
		"gs://b/movie.mp4": {[]byte{0}, "video/mp4"},
	})
	// The object's content type stands in for a missing mime_type.
	if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, body); rec.Code != http.StatusOK || len(e.in.Parts) != 1 || len(e.in.Parts[0].Audio) != 16000 {
		t.Fatalf("file_data audio: status %d: %s", rec.Code, rec.Body)
	}
	// The part's mime_type wins over the object's.
	if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID,
		`{"content":{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://b/img.png"}}]}}`); rec.Code != http.StatusOK || e.in.Parts[0].Image == nil {
		t.Fatalf("file_data image: status %d: %s", rec.Code, rec.Body)
	}

	long := base64.StdEncoding.EncodeToString(testWAV(100 * 16000))
	twoLong := `{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"` + long + `"}},{"inlineData":{"mimeType":"audio/wav","data":"` + long + `"}}]}}`
	for name, c := range map[string]struct {
		body, field string
		code        int
	}{
		"missing":    {`{"content":{"parts":[{"fileData":{"mimeType":"audio/wav","fileUri":"gs://b/none.wav"}}]}}`, "not found", http.StatusNotFound},
		"bad uri":    {`{"content":{"parts":[{"fileData":{"mimeType":"audio/wav","fileUri":"https://x/y.wav"}}]}}`, "file_data.file_uri", http.StatusBadRequest},
		"video":      {`{"content":{"parts":[{"fileData":{"fileUri":"gs://b/movie.mp4"}}]}}`, "file_data.mime_type", http.StatusBadRequest},
		"mp3":        {`{"content":{"parts":[{"inlineData":{"mimeType":"audio/mpeg","data":"AA=="}}]}}`, "audio/wav", http.StatusBadRequest},
		"over 180 s": {twoLong, "at most 180 seconds", http.StatusBadRequest},
		"bad wav":    {`{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"AA=="}}]}}`, "parts[0].inline_data", http.StatusBadRequest},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.field) {
			t.Errorf("%s: status %d, want %d naming %q: %s", name, rec.Code, c.code, c.field, rec.Body)
		}
	}

	// Without the audio encoder (a download from before it was pinned),
	// audio is refused as a type the model does not embed.
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, &multimodalEmbedder{}, nil)
	rec = postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"`+b64+`"}}]}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "video, audio and PDF") {
		t.Fatalf("no audio encoder: status %d: %s", rec.Code, rec.Body)
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
