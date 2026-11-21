//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	speechv1 "cloud.google.com/go/speech/apiv1"
	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSpeechV1AdaptationResources (#2106): Speech-to-Text v1's Adaptation
// service through the official apiv1 AdaptationClient over gRPC: a custom
// class and a phrase set created, read, listed, updated by mask and
// deleted, with v1's refusals (an ID ending in a hyphen, a location other
// than global, us and eu, a negative boost); then recognition through the
// apiv1 Client: a phrase set reference whose phrase names the stored class
// as ${id} makes local whisper.cpp spell Nyhuis (adaptation.wav, which it
// misspells unaided, TestSpeechAdaptationBoost), and an unknown reference is
// INVALID_ARGUMENT.
//
// covers: google.cloud.speech.v1.Adaptation/CreatePhraseSet, google.cloud.speech.v1.Adaptation/GetPhraseSet, google.cloud.speech.v1.Adaptation/ListPhraseSet, google.cloud.speech.v1.Adaptation/UpdatePhraseSet, google.cloud.speech.v1.Adaptation/DeletePhraseSet, google.cloud.speech.v1.Adaptation/CreateCustomClass, google.cloud.speech.v1.Adaptation/GetCustomClass, google.cloud.speech.v1.Adaptation/ListCustomClasses, google.cloud.speech.v1.Adaptation/UpdateCustomClass, google.cloud.speech.v1.Adaptation/DeleteCustomClass
func TestSpeechV1AdaptationResources(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvLocalAI)
	if addr == "" {
		t.Fatal("no local AI endpoint")
	}
	// whisper.cpp takes about 25 s a transcription: longer than h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	ad, err := speechv1.NewAdaptationClient(ctx, opts...)
	if err != nil {
		t.Fatalf("speechv1.NewAdaptationClient: %v", err)
	}
	t.Cleanup(func() { _ = ad.Close() })
	c, err := speechv1.NewClient(ctx, opts...)
	if err != nil {
		t.Fatalf("speechv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	parent := "projects/" + h.Project() + "/locations/global"

	cc, err := ad.CreateCustomClass(ctx, &speechv1pb.CreateCustomClassRequest{Parent: parent, CustomClassId: "v1-surnames",
		CustomClass: &speechv1pb.CustomClass{Items: []*speechv1pb.CustomClass_ClassItem{{Value: "Smith"}}}})
	if err != nil {
		t.Fatalf("CreateCustomClass: %v", err)
	}
	t.Cleanup(func() {
		_ = ad.DeleteCustomClass(context.Background(), &speechv1pb.DeleteCustomClassRequest{Name: cc.GetName()})
	})
	if cc.GetName() != parent+"/customClasses/v1-surnames" || cc.GetCustomClassId() != "v1-surnames" {
		t.Errorf("CreateCustomClass: %v", cc)
	}
	cc, err = ad.UpdateCustomClass(ctx, &speechv1pb.UpdateCustomClassRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"items"}},
		CustomClass: &speechv1pb.CustomClass{Name: cc.GetName(), Items: []*speechv1pb.CustomClass_ClassItem{{Value: "Nyhuis"}}}})
	if err != nil || len(cc.GetItems()) != 1 || cc.GetItems()[0].GetValue() != "Nyhuis" {
		t.Fatalf("UpdateCustomClass: %v %v", cc, err)
	}
	if got, err := ad.GetCustomClass(ctx, &speechv1pb.GetCustomClassRequest{Name: cc.GetName()}); err != nil || got.GetItems()[0].GetValue() != "Nyhuis" {
		t.Errorf("GetCustomClass: %v %v", got, err)
	}
	found := false
	for it := ad.ListCustomClasses(ctx, &speechv1pb.ListCustomClassesRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListCustomClasses: %v", err)
		}
		found = found || x.GetName() == cc.GetName()
	}
	if !found {
		t.Errorf("ListCustomClasses does not list %s", cc.GetName())
	}

	ps, err := ad.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "v1-names",
		PhraseSet: &speechv1pb.PhraseSet{Boost: 5, Phrases: []*speechv1pb.PhraseSet_Phrase{{Value: "${v1-surnames}"}}}})
	if err != nil {
		t.Fatalf("CreatePhraseSet: %v", err)
	}
	t.Cleanup(func() {
		_ = ad.DeletePhraseSet(context.Background(), &speechv1pb.DeletePhraseSetRequest{Name: ps.GetName()})
	})
	ps, err = ad.UpdatePhraseSet(ctx, &speechv1pb.UpdatePhraseSetRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"boost"}},
		PhraseSet: &speechv1pb.PhraseSet{Name: ps.GetName(), Boost: 10}})
	if err != nil || ps.GetBoost() != 10 || len(ps.GetPhrases()) != 1 {
		t.Fatalf("UpdatePhraseSet: %v %v", ps, err)
	}
	if got, err := ad.GetPhraseSet(ctx, &speechv1pb.GetPhraseSetRequest{Name: ps.GetName()}); err != nil || got.GetBoost() != 10 {
		t.Errorf("GetPhraseSet: %v %v", got, err)
	}
	found = false
	for it := ad.ListPhraseSet(ctx, &speechv1pb.ListPhraseSetRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListPhraseSet: %v", err)
		}
		found = found || x.GetName() == ps.GetName()
	}
	if !found {
		t.Errorf("ListPhraseSet does not list %s", ps.GetName())
	}

	for name, err := range map[string]error{
		"id ending in a hyphen": func() error {
			_, err := ad.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "names-", PhraseSet: &speechv1pb.PhraseSet{}})
			return err
		}(),
		"location": func() error {
			_, err := ad.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: "projects/" + h.Project() + "/locations/asia-east1",
				PhraseSetId: "names", PhraseSet: &speechv1pb.PhraseSet{}})
			return err
		}(),
		"negative boost": func() error {
			_, err := ad.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "names",
				PhraseSet: &speechv1pb.PhraseSet{Boost: -1}})
			return err
		}(),
	} {
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}

	adapt := wavPCM(t, "../../internal/speech/testdata/adaptation.wav")
	recognize := func(a *speechv1pb.SpeechAdaptation) (string, error) {
		res, err := c.Recognize(ctx, &speechv1pb.RecognizeRequest{Config: &speechv1pb.RecognitionConfig{Encoding: speechv1pb.RecognitionConfig_LINEAR16,
			SampleRateHertz: 16000, LanguageCode: "en-US", Adaptation: a},
			Audio: &speechv1pb.RecognitionAudio{AudioSource: &speechv1pb.RecognitionAudio_Content{Content: adapt}}})
		if err != nil {
			return "", err
		}
		var out []string
		for _, r := range res.GetResults() {
			out = append(out, r.GetAlternatives()[0].GetTranscript())
		}
		return strings.Join(out, " "), nil
	}
	plain, err := recognize(nil)
	if err != nil {
		t.Fatalf("Recognize unaided: %v", err)
	}
	boosted, err := recognize(&speechv1pb.SpeechAdaptation{PhraseSetReferences: []string{ps.GetName()}})
	if err != nil {
		t.Fatalf("Recognize with phrase_set_references: %v", err)
	}
	if strings.Contains(plain, "Nyhuis") || !strings.Contains(boosted, "Nyhuis") {
		t.Errorf("unaided %q, with the stored phrase set %q: want Nyhuis only with it", plain, boosted)
	}
	if _, err := recognize(&speechv1pb.SpeechAdaptation{PhraseSetReferences: []string{parent + "/phraseSets/missing"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown phrase set reference: %v", err)
	}

	if err := ad.DeletePhraseSet(ctx, &speechv1pb.DeletePhraseSetRequest{Name: ps.GetName()}); err != nil {
		t.Errorf("DeletePhraseSet: %v", err)
	}
	if _, err := ad.GetPhraseSet(ctx, &speechv1pb.GetPhraseSetRequest{Name: ps.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted phrase set: %v", err)
	}
	if err := ad.DeleteCustomClass(ctx, &speechv1pb.DeleteCustomClassRequest{Name: cc.GetName()}); err != nil {
		t.Errorf("DeleteCustomClass: %v", err)
	}
}
