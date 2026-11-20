package vertexai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/speech"
)

// speechObjects is a writable Cloud Storage of objects by gs:// URI.
type speechObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (f *speechObjects) Read(_ context.Context, bucket, object string, _ int64) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs["gs://"+bucket+"/"+object]
	if !ok {
		return nil, "", apierror.NotFound("gs://%s/%s not found", bucket, object)
	}
	return o, "", nil
}

func (f *speechObjects) Write(_ context.Context, bucket, object, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs["gs://"+bucket+"/"+object] = data
	return nil
}

var autoChirp = &speechpb.RecognitionConfig{Model: "chirp_3", LanguageCodes: []string{"en-US"},
	DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}

func gcsOut(uri string) *speechpb.RecognitionOutputConfig {
	return &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_GcsOutputConfig{
		GcsOutputConfig: &speechpb.GcsOutputConfig{Uri: uri}}}
}

func files(uris ...string) []*speechpb.BatchRecognizeFileMetadata {
	var out []*speechpb.BatchRecognizeFileMetadata
	for _, u := range uris {
		out = append(out, &speechpb.BatchRecognizeFileMetadata{AudioSource: &speechpb.BatchRecognizeFileMetadata_Uri{Uri: u}})
	}
	return out
}

// TestSpeechBatchRecognizeRefusals: what BatchRecognize refuses when called.
func TestSpeechBatchRecognizeRefusals(t *testing.T) {
	ctx := context.Background()
	v := newSpeech(t, "")
	rec := speechParent + "/recognizers/_"
	inline := &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_InlineResponseConfig{
		InlineResponseConfig: &speechpb.InlineOutputConfig{}}}
	sixteen := make([]string, 16)
	for i := range sixteen {
		sixteen[i] = "gs://b/a" + string(rune('a'+i)) + ".wav"
	}
	for _, c := range []struct {
		name string
		req  *speechpb.BatchRecognizeRequest
		want codes.Code
	}{
		{"no storage", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.FailedPrecondition},
	} {
		if _, err := v.BatchRecognize(ctx, c.req); code(err) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	v.s.UseStorage(&speechObjects{objs: map[string][]byte{}})
	for _, c := range []struct {
		name string
		req  *speechpb.BatchRecognizeRequest
		want codes.Code
	}{
		{"no files", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"16 files", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files(sixteen...), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"no output", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files("gs://b/a.wav")}, codes.InvalidArgument},
		{"inline two", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files("gs://b/a.wav", "gs://b/b.wav"), RecognitionOutputConfig: inline}, codes.InvalidArgument},
		{"bad uri", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files("https://x/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"bad output", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("b/out")}, codes.InvalidArgument},
		{"no config", &speechpb.BatchRecognizeRequest{Recognizer: rec, Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"medical", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: &speechpb.RecognitionConfig{Model: "medical_dictation", LanguageCodes: []string{"en-US"},
			DecodingConfig: autoChirp.DecodingConfig}, Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.Unimplemented},
		// On the default recognizer a mask keeps only its fields of config:
		// model alone has no language.
		{"mask", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, ConfigMask: &fieldmaskpb.FieldMask{Paths: []string{"model"}},
			Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"bad mask", &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: autoChirp, ConfigMask: &fieldmaskpb.FieldMask{Paths: []string{"nope"}},
			Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.InvalidArgument},
		{"missing recognizer", &speechpb.BatchRecognizeRequest{Recognizer: speechParent + "/recognizers/nope", Files: files("gs://b/a.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")}, codes.NotFound},
	} {
		if _, err := v.BatchRecognize(ctx, c.req); code(err) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// TestSpeechBatchRecognizeFileError: a missing object fails its file, not
// the operation.
func TestSpeechBatchRecognizeFileError(t *testing.T) {
	ctx := context.Background()
	v := newSpeech(t, "")
	v.s.UseStorage(&speechObjects{objs: map[string][]byte{}})
	op, err := v.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: speechParent + "/recognizers/_", Config: autoChirp,
		Files: files("gs://b/missing.wav"), RecognitionOutputConfig: gcsOut("gs://b/out")})
	if err != nil {
		t.Fatal(err)
	}
	resp := waitBatch(t, v, op.GetName())
	fr := resp.GetResults()["gs://b/missing.wav"]
	if fr.GetError().GetCode() != int32(codes.NotFound) {
		t.Errorf("file result: %v", fr)
	}
}

func waitBatch(t *testing.T, v *speechService, name string) *speechpb.BatchRecognizeResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		got, err := v.s.ops.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Done() {
			op, err := opProto(got)
			if err != nil || op.GetError() != nil {
				t.Fatalf("operation: %v %v", op, err)
			}
			resp := &speechpb.BatchRecognizeResponse{}
			if err := op.GetResponse().UnmarshalTo(resp); err != nil {
				t.Fatal(err)
			}
			md := &speechpb.OperationMetadata{}
			if err := op.GetMetadata().UnmarshalTo(md); err != nil || md.GetBatchRecognizeMetadata() == nil || md.GetProgressPercent() != 100 {
				t.Errorf("metadata: %v %v", md, err)
			}
			return resp
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("BatchRecognize did not finish")
	return nil
}

// TestSpeechBatchRecognize transcribes whisper.cpp's sample from Cloud
// Storage, to Cloud Storage with captions and inline.
func TestSpeechBatchRecognize(t *testing.T) {
	if testing.Short() {
		t.Skip("fetches the whisper model and transcribes")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	cache := localai.Cache{Dir: filepath.Join(dir, "cloudburrow-test", "models")}
	speech.SetModelSource(func(ctx context.Context) (string, error) {
		p, err := cache.FetchPinnedFile(ctx, localai.Whisper, localai.Whisper.Pin.Files[0])
		if err != nil {
			t.Skipf("fetching the whisper model: %v", err)
		}
		return p, nil
	})
	t.Cleanup(func() { speech.SetModelSource(nil) })
	wav, err := os.ReadFile("../../speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v := newSpeech(t, "")
	objs := &speechObjects{objs: map[string][]byte{"gs://in/jfk.wav": wav}}
	v.s.UseStorage(objs)
	out := gcsOut("gs://out/results")
	out.OutputFormatConfig = &speechpb.OutputFormatConfig{Native: &speechpb.NativeOutputFileFormatConfig{}, Vtt: &speechpb.VttOutputFileFormatConfig{}}
	op, err := v.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: speechParent + "/recognizers/_", Config: autoChirp,
		Files: files("gs://in/jfk.wav"), RecognitionOutputConfig: out})
	if err != nil {
		t.Fatal(err)
	}
	resp := waitBatch(t, v, op.GetName())
	csr := resp.GetResults()["gs://in/jfk.wav"].GetCloudStorageResult()
	if !strings.HasPrefix(csr.GetUri(), "gs://out/results/jfk_transcript_") || !strings.HasSuffix(csr.GetVttFormatUri(), ".vtt") {
		t.Fatalf("result: %v", resp)
	}
	objs.mu.Lock()
	native, vtt := objs.objs[csr.GetUri()], objs.objs[csr.GetVttFormatUri()]
	objs.mu.Unlock()
	var results speechpb.BatchRecognizeResults
	if err := protojson.Unmarshal(native, &results); err != nil || !strings.Contains(strings.ToLower(protojsonString(&results)), "ask not what your country") {
		t.Errorf("native output %v: %s", err, native)
	}
	if !strings.HasPrefix(string(vtt), "WEBVTT\n\n00:00:00.000 --> ") {
		t.Errorf("vtt output: %q", vtt)
	}
	if resp.GetTotalBilledDuration().AsDuration() != 11*time.Second {
		t.Errorf("billed %v", resp.GetTotalBilledDuration())
	}

	op, err = v.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: speechParent + "/recognizers/_", Config: autoChirp,
		Files: files("gs://in/jfk.wav"), RecognitionOutputConfig: &speechpb.RecognitionOutputConfig{
			Output: &speechpb.RecognitionOutputConfig_InlineResponseConfig{InlineResponseConfig: &speechpb.InlineOutputConfig{}}}})
	if err != nil {
		t.Fatal(err)
	}
	resp = waitBatch(t, v, op.GetName())
	tr := resp.GetResults()["gs://in/jfk.wav"].GetInlineResult().GetTranscript()
	if len(tr.GetResults()) == 0 || !strings.Contains(protojsonString(tr), "country") {
		t.Errorf("inline: %v", resp)
	}

	// Recognize reads a uri from Cloud Storage too.
	rr, err := v.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: speechParent + "/recognizers/_", Config: autoChirp,
		AudioSource: &speechpb.RecognizeRequest_Uri{Uri: "gs://in/jfk.wav"}})
	if err != nil || !strings.Contains(protojsonString(rr), "country") {
		t.Errorf("Recognize uri: %v %v", rr, err)
	}
}

func protojsonString(m proto.Message) string {
	b, _ := protojson.Marshal(m)
	return string(b)
}

// TestCaptions: WebVTT and SubRip timestamps.
func TestCaptions(t *testing.T) {
	res := speech.Result{Segments: []speech.Segment{{Text: " Hello ", Start: 0, End: 1500 * time.Millisecond},
		{Text: "world", Start: 1500 * time.Millisecond, End: 3723004 * time.Millisecond}}}
	if got, want := captions(res, true), "WEBVTT\n\n00:00:00.000 --> 00:00:01.500\nHello\n\n00:00:01.500 --> 01:02:03.004\nworld\n\n"; got != want {
		t.Errorf("vtt %q, want %q", got, want)
	}
	if got, want := captions(res, false), "1\n00:00:00,000 --> 00:00:01,500\nHello\n\n2\n00:00:01,500 --> 01:02:03,004\nworld\n\n"; got != want {
		t.Errorf("srt %q, want %q", got, want)
	}
}
