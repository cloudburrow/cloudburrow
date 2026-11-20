//go:build compat

package compat

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// speechClients are the official Speech-to-Text v2 clients, gRPC and REST,
// on the instance's local AI endpoint (#1799), never Google's.
func speechClients(t *testing.T, h *Harness) (*speech.Client, *speech.Client) {
	t.Helper()
	addr := h.Endpoint(EnvLocalAI)
	if addr == "" {
		t.Fatal("no local AI endpoint")
	}
	g, err := speech.NewClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("speech.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	r, err := speech.NewRESTClient(h.Context(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("speech.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return g, r
}

// createRecognizer makes a chirp recognizer with word offsets in parent and
// deletes it when the test ends.
func createRecognizer(t *testing.T, h *Harness, c *speech.Client, parent, id string) *speechpb.Recognizer {
	t.Helper()
	op, err := c.CreateRecognizer(h.Context(), &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: id,
		Recognizer: &speechpb.Recognizer{DisplayName: id, DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp",
			LanguageCodes: []string{"en-US"}, Features: &speechpb.RecognitionFeatures{EnableWordTimeOffsets: true},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatalf("CreateRecognizer: %v", err)
	}
	r, err := op.Wait(h.Context())
	if err != nil {
		t.Fatalf("CreateRecognizer wait: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(h.Context(), &speechpb.DeleteRecognizerRequest{Name: r.GetName(), AllowMissing: true})
	})
	return r
}

// TestSpeechRecognizers (#1799): Speech-to-Text v2's recognizers on the
// local AI endpoint, through the official cloud.google.com/go/speech
// client over gRPC and REST: a recognizer created (an operation waited
// on), read, listed, updated by field mask (a stale etag ABORTED), deleted
// (listed only with show_deleted, recognize on it FAILED_PRECONDITION) and
// undeleted; an invalid recognizer_id INVALID_ARGUMENT; Recognize on the
// recognizer transcribing whisper.cpp's JFK sample with no config of its
// own (the recognizer's chirp and word offsets apply) and with a config
// that overrides its language.
//
// covers: google.cloud.speech.v2.Speech/CreateRecognizer, google.cloud.speech.v2.Speech/GetRecognizer, google.cloud.speech.v2.Speech/ListRecognizers, google.cloud.speech.v2.Speech/UpdateRecognizer, google.cloud.speech.v2.Speech/DeleteRecognizer, google.cloud.speech.v2.Speech/UndeleteRecognizer, google.cloud.speech.v2.Speech/Recognize
func TestSpeechRecognizers(t *testing.T) {
	h := New(t)
	g, r := speechClients(t, h)
	// Several whisper recognitions with local whisper: more than h.Context()'s minute
	// on a 4-vCPU runner (main run 44709318191 timed out at 60s).
	// Ended by t.Cleanup, registered first so it runs last, after the
	// test's own cleanups that still use ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	parent := "projects/" + h.Project() + "/locations/us"
	wav, err := os.ReadFile("../../internal/speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: "Not_Valid",
		Recognizer: &speechpb.Recognizer{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid recognizer_id: %v", err)
	}
	rec := createRecognizer(t, h, g, parent, "jfk-rec")
	if rec.GetState() != speechpb.Recognizer_ACTIVE || rec.GetEtag() == "" || rec.GetUid() == "" {
		t.Errorf("created: %v", rec)
	}
	got, err := r.GetRecognizer(ctx, &speechpb.GetRecognizerRequest{Name: rec.GetName()})
	if err != nil || got.GetDefaultRecognitionConfig().GetModel() != "chirp" || got.GetDisplayName() != "jfk-rec" {
		t.Fatalf("GetRecognizer (REST): %v %v", got, err)
	}
	found := false
	it := r.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: parent})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListRecognizers: %v", err)
		}
		found = found || x.GetName() == rec.GetName()
	}
	if !found {
		t.Errorf("ListRecognizers lacks %s", rec.GetName())
	}
	if _, err := g.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: rec.GetName(), Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	uop, err := r.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: rec.GetName(), DisplayName: "kennedy"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateRecognizer (REST): %v", err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetDisplayName() != "kennedy" || up.GetDefaultRecognitionConfig().GetModel() != "chirp" {
		t.Errorf("updated: %v %v", up, err)
	}

	// Recognize on the recognizer: its config, then one overriding it.
	for _, c := range []struct {
		client *speech.Client
		config *speechpb.RecognitionConfig
	}{{g, nil}, {r, &speechpb.RecognitionConfig{LanguageCodes: []string{"en-GB"}}}} {
		res, err := c.client.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec.GetName(), Config: c.config,
			AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}})
		if err != nil {
			t.Fatalf("Recognize %v: %v", c.config, err)
		}
		var text []string
		for _, x := range res.GetResults() {
			text = append(text, x.GetAlternatives()[0].GetTranscript())
		}
		words := res.GetResults()[0].GetAlternatives()[0].GetWords()
		if !strings.Contains(strings.ToLower(strings.Join(text, " ")), "ask not what your country can do for you") || len(words) == 0 ||
			words[1].GetStartOffset() == nil || res.GetMetadata().GetTotalBilledDuration().GetSeconds() != 11 {
			t.Errorf("Recognize %v: %v", c.config, res)
		}
		if want := map[bool]string{true: "en-us", false: "en-gb"}[c.config == nil]; res.GetResults()[0].GetLanguageCode() != want {
			t.Errorf("Recognize %v: language %q, want %q", c.config, res.GetResults()[0].GetLanguageCode(), want)
		}
	}

	dop, err := g.DeleteRecognizer(ctx, &speechpb.DeleteRecognizerRequest{Name: rec.GetName()})
	if err != nil {
		t.Fatalf("DeleteRecognizer: %v", err)
	}
	if d, err := dop.Wait(ctx); err != nil || d.GetState() != speechpb.Recognizer_DELETED || d.GetExpireTime() == nil {
		t.Errorf("deleted: %v %v", d, err)
	}
	if _, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec.GetName(),
		AudioSource: &speechpb.RecognizeRequest_Content{Content: wav}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Recognize on a deleted recognizer: %v", err)
	}
	it = g.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: parent, ShowDeleted: true})
	if x, err := it.Next(); err != nil || x.GetName() != rec.GetName() {
		t.Errorf("ListRecognizers show_deleted: %v %v", x, err)
	}
	unop, err := r.UndeleteRecognizer(ctx, &speechpb.UndeleteRecognizerRequest{Name: rec.GetName()})
	if err != nil {
		t.Fatalf("UndeleteRecognizer (REST): %v", err)
	}
	if u, err := unop.Wait(ctx); err != nil || u.GetState() != speechpb.Recognizer_ACTIVE {
		t.Errorf("undeleted: %v %v", u, err)
	}
}

// TestSpeechAdaptationResources (#1930): Speech-to-Text v2's custom classes
// and phrase sets on the local AI endpoint, through the official
// cloud.google.com/go/speech client over gRPC and REST: each created (an
// operation waited on), read, listed, updated by field mask (a stale etag
// ABORTED), deleted (listed only with show_deleted) and undeleted; an
// invalid ID and a boost over 20 INVALID_ARGUMENT; a recognition config
// naming a phrase set UNIMPLEMENTED, since local whisper.cpp applies no
// adaptation.
//
// covers: google.cloud.speech.v2.Speech/CreateCustomClass, google.cloud.speech.v2.Speech/GetCustomClass, google.cloud.speech.v2.Speech/ListCustomClasses, google.cloud.speech.v2.Speech/UpdateCustomClass, google.cloud.speech.v2.Speech/DeleteCustomClass, google.cloud.speech.v2.Speech/UndeleteCustomClass, google.cloud.speech.v2.Speech/CreatePhraseSet, google.cloud.speech.v2.Speech/GetPhraseSet, google.cloud.speech.v2.Speech/ListPhraseSets, google.cloud.speech.v2.Speech/UpdatePhraseSet, google.cloud.speech.v2.Speech/DeletePhraseSet, google.cloud.speech.v2.Speech/UndeletePhraseSet
func TestSpeechAdaptationResources(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	g, r := speechClients(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/global"

	if _, err := g.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: parent, CustomClassId: "Not_Valid",
		CustomClass: &speechpb.CustomClass{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid custom_class_id: %v", err)
	}
	cop, err := g.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: parent, CustomClassId: "pets",
		CustomClass: &speechpb.CustomClass{DisplayName: "Pets", Items: []*speechpb.CustomClass_ClassItem{{Value: "cat"}, {Value: "dog"}}}})
	if err != nil {
		t.Fatalf("CreateCustomClass: %v", err)
	}
	cc, err := cop.Wait(ctx)
	if err != nil || cc.GetState() != speechpb.CustomClass_ACTIVE || cc.GetEtag() == "" || cc.GetUid() == "" {
		t.Fatalf("created: %v %v", cc, err)
	}
	t.Cleanup(func() {
		_, _ = g.DeleteCustomClass(h.Context(), &speechpb.DeleteCustomClassRequest{Name: cc.GetName(), AllowMissing: true})
	})
	if got, err := r.GetCustomClass(ctx, &speechpb.GetCustomClassRequest{Name: cc.GetName()}); err != nil || len(got.GetItems()) != 2 {
		t.Errorf("GetCustomClass (REST): %v %v", got, err)
	}
	if _, err := g.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: cc.GetName(), Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	uop, err := r.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: cc.GetName(),
		Items: []*speechpb.CustomClass_ClassItem{{Value: "rabbit"}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"items"}}})
	if err != nil {
		t.Fatalf("UpdateCustomClass (REST): %v", err)
	}
	if up, err := uop.Wait(ctx); err != nil || len(up.GetItems()) != 1 || up.GetDisplayName() != "Pets" {
		t.Errorf("updated: %v %v", up, err)
	}
	dop, err := r.DeleteCustomClass(ctx, &speechpb.DeleteCustomClassRequest{Name: cc.GetName()})
	if err != nil {
		t.Fatalf("DeleteCustomClass (REST): %v", err)
	}
	if d, err := dop.Wait(ctx); err != nil || d.GetState() != speechpb.CustomClass_DELETED || d.GetExpireTime() == nil {
		t.Errorf("deleted: %v %v", d, err)
	}
	if x, err := g.ListCustomClasses(ctx, &speechpb.ListCustomClassesRequest{Parent: parent}).Next(); err != iterator.Done {
		t.Errorf("ListCustomClasses lists a deleted class: %v %v", x, err)
	}
	if x, err := r.ListCustomClasses(ctx, &speechpb.ListCustomClassesRequest{Parent: parent, ShowDeleted: true}).Next(); err != nil || x.GetName() != cc.GetName() {
		t.Errorf("ListCustomClasses show_deleted (REST): %v %v", x, err)
	}
	unop, err := g.UndeleteCustomClass(ctx, &speechpb.UndeleteCustomClassRequest{Name: cc.GetName()})
	if err != nil {
		t.Fatalf("UndeleteCustomClass: %v", err)
	}
	if u, err := unop.Wait(ctx); err != nil || u.GetState() != speechpb.CustomClass_ACTIVE {
		t.Errorf("undeleted: %v %v", u, err)
	}

	if _, err := g.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "too-much",
		PhraseSet: &speechpb.PhraseSet{Boost: 25}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("boost 25: %v", err)
	}
	pop, err := r.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "names",
		PhraseSet: &speechpb.PhraseSet{Boost: 5, Phrases: []*speechpb.PhraseSet_Phrase{{Value: "CloudBurrow", Boost: 15}, {Value: "SweetTech"}}}})
	if err != nil {
		t.Fatalf("CreatePhraseSet (REST): %v", err)
	}
	ps, err := pop.Wait(ctx)
	if err != nil || ps.GetBoost() != 5 || ps.GetPhrases()[0].GetBoost() != 15 {
		t.Fatalf("created: %v %v", ps, err)
	}
	t.Cleanup(func() {
		_, _ = g.DeletePhraseSet(h.Context(), &speechpb.DeletePhraseSetRequest{Name: ps.GetName(), AllowMissing: true})
	})
	if got, err := g.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: ps.GetName()}); err != nil || len(got.GetPhrases()) != 2 {
		t.Errorf("GetPhraseSet: %v %v", got, err)
	}
	if x, err := r.ListPhraseSets(ctx, &speechpb.ListPhraseSetsRequest{Parent: parent}).Next(); err != nil || x.GetName() != ps.GetName() {
		t.Errorf("ListPhraseSets (REST): %v %v", x, err)
	}
	pu, err := g.UpdatePhraseSet(ctx, &speechpb.UpdatePhraseSetRequest{PhraseSet: &speechpb.PhraseSet{Name: ps.GetName(), Boost: 8},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"boost"}}})
	if err != nil {
		t.Fatalf("UpdatePhraseSet: %v", err)
	}
	if up, err := pu.Wait(ctx); err != nil || up.GetBoost() != 8 || len(up.GetPhrases()) != 2 {
		t.Errorf("updated: %v %v", up, err)
	}
	if _, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: parent + "/recognizers/_",
		Config: &speechpb.RecognitionConfig{Model: "chirp_3", LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}},
			Adaptation: &speechpb.SpeechAdaptation{PhraseSets: []*speechpb.SpeechAdaptation_AdaptationPhraseSet{{
				Value: &speechpb.SpeechAdaptation_AdaptationPhraseSet_PhraseSet{PhraseSet: ps.GetName()}}}}},
		AudioSource: &speechpb.RecognizeRequest_Content{Content: []byte("RIFF")}}); status.Code(err) != codes.InvalidArgument ||
		strings.Contains(err.Error(), "adaptation") {
		// The phrase set is applied (TestSpeechAdaptationBoost); four bytes
		// are not a WAV file.
		t.Errorf("Recognize with adaptation: %v", err)
	}
	pd, err := g.DeletePhraseSet(ctx, &speechpb.DeletePhraseSetRequest{Name: ps.GetName()})
	if err != nil {
		t.Fatalf("DeletePhraseSet: %v", err)
	}
	if d, err := pd.Wait(ctx); err != nil || d.GetState() != speechpb.PhraseSet_DELETED {
		t.Errorf("deleted: %v %v", d, err)
	}
	pun, err := r.UndeletePhraseSet(ctx, &speechpb.UndeletePhraseSetRequest{Name: ps.GetName()})
	if err != nil {
		t.Fatalf("UndeletePhraseSet (REST): %v", err)
	}
	if u, err := pun.Wait(ctx); err != nil || u.GetState() != speechpb.PhraseSet_ACTIVE {
		t.Errorf("undeleted: %v %v", u, err)
	}
}

// TestSpeechBatchRecognize (#1930): BatchRecognize through the official
// client, gRPC and REST, on whisper.cpp's JFK clip uploaded to the
// instance's Cloud Storage with the official storage client: the
// transcript written under a gs:// prefix as BatchRecognizeResults JSON with
// WebVTT captions beside it, the operation's metadata naming the output;
// inline for one file; a missing object failing its file alone; 16 files
// and inline output for two INVALID_ARGUMENT; Recognize reading the clip's
// gs:// uri; and Google's other general models (long, short, telephony,
// chirp_2, chirp_3) transcribing it, medical_dictation UNIMPLEMENTED.
//
// covers: google.cloud.speech.v2.Speech/BatchRecognize, google.cloud.speech.v2.Speech/Recognize
func TestSpeechBatchRecognize(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" || h.Endpoint(EnvStorage) == "" {
		t.Skip("this instance serves no local AI endpoint or Cloud Storage")
	}
	g, r := speechClients(t, h)
	sc := storageClient(t, h)
	// Seven transcriptions with local whisper: more than h.Context()'s minute.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	bh := bucket(t, h, sc)
	b := "gs://" + bh.BucketName()
	clip, err := os.ReadFile("../../internal/speech/testdata/jfk.flac")
	if err != nil {
		t.Fatal(err)
	}
	w := bh.Object("audio/jfk.flac").NewWriter(ctx)
	w.ContentType = "audio/flac"
	if _, err := w.Write(clip); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	rec := "projects/" + h.Project() + "/locations/global/recognizers/_"
	cfg := func(model string) *speechpb.RecognitionConfig {
		return &speechpb.RecognitionConfig{Model: model, LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}
	}
	file := func(uris ...string) []*speechpb.BatchRecognizeFileMetadata {
		var out []*speechpb.BatchRecognizeFileMetadata
		for _, u := range uris {
			out = append(out, &speechpb.BatchRecognizeFileMetadata{AudioSource: &speechpb.BatchRecognizeFileMetadata_Uri{Uri: u}})
		}
		return out
	}
	inline := &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_InlineResponseConfig{InlineResponseConfig: &speechpb.InlineOutputConfig{}}}
	many := make([]string, 16)
	for i := range many {
		many[i] = b + "/audio/jfk.flac?" + strconv.Itoa(i)
	}
	if _, err := g.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: cfg("chirp_3"), Files: file(many...),
		RecognitionOutputConfig: inline}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("16 files: %v", err)
	}
	if _, err := g.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: cfg("chirp_3"),
		Files: file(b+"/audio/jfk.flac", b+"/audio/other.flac"), RecognitionOutputConfig: inline}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("inline output for two files: %v", err)
	}

	gcs := &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_GcsOutputConfig{GcsOutputConfig: &speechpb.GcsOutputConfig{Uri: b + "/out"}},
		OutputFormatConfig: &speechpb.OutputFormatConfig{Native: &speechpb.NativeOutputFileFormatConfig{}, Vtt: &speechpb.VttOutputFileFormatConfig{}}}
	op, err := g.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: cfg("chirp_3"),
		Files: file(b+"/audio/jfk.flac", b+"/audio/missing.flac"), RecognitionOutputConfig: gcs})
	if err != nil {
		t.Fatalf("BatchRecognize: %v", err)
	}
	res, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("BatchRecognize wait: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetBatchRecognizeMetadata().GetTranscriptionMetadata()[b+"/audio/jfk.flac"].GetUri() == "" {
		t.Errorf("metadata: %v %v", md, err)
	}
	if e := res.GetResults()[b+"/audio/missing.flac"].GetError(); codes.Code(e.GetCode()) != codes.NotFound {
		t.Errorf("missing file's result: %v", res.GetResults()[b+"/audio/missing.flac"])
	}
	csr := res.GetResults()[b+"/audio/jfk.flac"].GetCloudStorageResult()
	if !strings.HasPrefix(csr.GetUri(), b+"/out/") || !strings.HasSuffix(csr.GetVttFormatUri(), ".vtt") {
		t.Fatalf("result: %v", res)
	}
	read := func(uri string) []byte {
		rd, err := bh.Object(strings.TrimPrefix(uri, b+"/")).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		defer rd.Close()
		data, _ := io.ReadAll(rd)
		return data
	}
	var results speechpb.BatchRecognizeResults
	if err := protojson.Unmarshal(read(csr.GetUri()), &results); err != nil || !strings.Contains(strings.ToLower(protojson.Format(&results)), "your country") {
		t.Errorf("BatchRecognizeResults: %v %v", err, &results)
	}
	if vtt := string(read(csr.GetVttFormatUri())); !strings.HasPrefix(vtt, "WEBVTT") || !strings.Contains(strings.ToLower(vtt), "your country") {
		t.Errorf("VTT: %q", vtt)
	}

	// Inline, through REST, with each served general model.
	for _, model := range []string{"long", "short", "telephony", "chirp_2", "chirp_3"} {
		op, err := r.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: cfg(model), Files: file(b + "/audio/jfk.flac"),
			RecognitionOutputConfig: inline})
		if err != nil {
			t.Fatalf("BatchRecognize %s (REST): %v", model, err)
		}
		res, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("BatchRecognize %s wait: %v", model, err)
		}
		tr := res.GetResults()[b+"/audio/jfk.flac"].GetInlineResult().GetTranscript()
		if !strings.Contains(strings.ToLower(protojson.Format(tr)), "your country") {
			t.Errorf("BatchRecognize %s inline: %v", model, res)
		}
	}
	if _, err := g.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: rec, Config: cfg("medical_dictation"),
		Files: file(b + "/audio/jfk.flac"), RecognitionOutputConfig: inline}); status.Code(err) != codes.Unimplemented {
		t.Errorf("medical_dictation: %v", err)
	}
	rr, err := g.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: rec, Config: cfg("chirp_3"),
		AudioSource: &speechpb.RecognizeRequest_Uri{Uri: b + "/audio/jfk.flac"}})
	if err != nil || !strings.Contains(strings.ToLower(protojson.Format(rr)), "your country") {
		t.Errorf("Recognize uri: %v %v", rr, err)
	}
}
