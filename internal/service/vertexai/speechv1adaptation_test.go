package vertexai

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const v1Parent = "projects/p/locations/global"

// TestSpeechV1AdaptationResources (#2106): v1 Adaptation's phrase sets and
// custom classes: create, get, list (paged), update by mask, delete, the
// refusals, and their persistence.
func TestSpeechV1AdaptationResources(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "recognizers.json")
	a := &speechV1AdaptationService{s: newSpeech(t, file).s}

	ps, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: "Names-1",
		PhraseSet: &speechv1pb.PhraseSet{Boost: 10, Phrases: []*speechv1pb.PhraseSet_Phrase{{Value: "Nyhuis"}}}})
	if err != nil || ps.GetName() != v1Parent+"/phraseSets/Names-1" {
		t.Fatalf("CreatePhraseSet: %v %v", ps, err)
	}
	cc, err := a.CreateCustomClass(ctx, &speechv1pb.CreateCustomClassRequest{Parent: v1Parent + "/customClasses", CustomClassId: "towns",
		CustomClass: &speechv1pb.CustomClass{Items: []*speechv1pb.CustomClass_ClassItem{{Value: "Quoddy"}, {Value: "Vyborg"}}}})
	if err != nil || cc.GetName() != v1Parent+"/customClasses/towns" || cc.GetCustomClassId() != "towns" {
		t.Fatalf("CreateCustomClass: %v %v", cc, err)
	}
	for name, err := range map[string]error{
		"duplicate": func() error {
			_, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: "Names-1", PhraseSet: &speechv1pb.PhraseSet{}})
			return err
		}(),
		"bad id": func() error {
			_, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: "ab-", PhraseSet: &speechv1pb.PhraseSet{}})
			return err
		}(),
		"location": func() error {
			_, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: "projects/p/locations/asia", PhraseSetId: "abcd", PhraseSet: &speechv1pb.PhraseSet{}})
			return err
		}(),
		"negative boost": func() error {
			_, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: "abcd",
				PhraseSet: &speechv1pb.PhraseSet{Boost: -1}})
			return err
		}(),
		"empty item": func() error {
			_, err := a.CreateCustomClass(ctx, &speechv1pb.CreateCustomClassRequest{Parent: v1Parent, CustomClassId: "abcd",
				CustomClass: &speechv1pb.CustomClass{Items: []*speechv1pb.CustomClass_ClassItem{{}}}})
			return err
		}(),
		"missing": func() error {
			_, err := a.GetPhraseSet(ctx, &speechv1pb.GetPhraseSetRequest{Name: v1Parent + "/phraseSets/none"})
			return err
		}(),
	} {
		want := codes.InvalidArgument
		switch name {
		case "duplicate":
			want = codes.AlreadyExists
		case "missing":
			want = codes.NotFound
		}
		if code(grpcError(err)) != want {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}

	up, err := a.UpdatePhraseSet(ctx, &speechv1pb.UpdatePhraseSetRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"boost"}},
		PhraseSet: &speechv1pb.PhraseSet{Name: ps.GetName(), Boost: 15, Phrases: []*speechv1pb.PhraseSet_Phrase{{Value: "ignored"}}}})
	if err != nil || up.GetBoost() != 15 || len(up.GetPhrases()) != 1 || up.GetPhrases()[0].GetValue() != "Nyhuis" {
		t.Errorf("UpdatePhraseSet boost: %v %v", up, err)
	}
	if _, err := a.UpdatePhraseSet(ctx, &speechv1pb.UpdatePhraseSetRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
		PhraseSet: &speechv1pb.PhraseSet{Name: ps.GetName()}}); code(grpcError(err)) != codes.InvalidArgument {
		t.Errorf("mask name: %v", err)
	}

	for _, id := range []string{"set-b", "set-c"} {
		if _, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: id, PhraseSet: &speechv1pb.PhraseSet{}}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := a.ListPhraseSet(ctx, &speechv1pb.ListPhraseSetRequest{Parent: v1Parent, PageSize: 2})
	if err != nil || len(l.GetPhraseSets()) != 2 || l.GetNextPageToken() == "" {
		t.Fatalf("ListPhraseSet page 1: %v %v", l, err)
	}
	l2, err := a.ListPhraseSet(ctx, &speechv1pb.ListPhraseSetRequest{Parent: v1Parent, PageSize: 2, PageToken: l.GetNextPageToken()})
	if err != nil || len(l2.GetPhraseSets()) != 1 || l2.GetNextPageToken() != "" {
		t.Fatalf("ListPhraseSet page 2: %v %v", l2, err)
	}
	if l, _ := a.ListPhraseSet(ctx, &speechv1pb.ListPhraseSetRequest{Parent: "projects/p/locations/us"}); len(l.GetPhraseSets()) != 0 {
		t.Errorf("another location lists %v", l)
	}
	if _, err := a.DeletePhraseSet(ctx, &speechv1pb.DeletePhraseSetRequest{Name: v1Parent + "/phraseSets/set-c"}); err != nil {
		t.Fatal(err)
	}

	// A new server on the same files reads them back.
	b := &speechV1AdaptationService{s: newSpeech(t, file).s}
	all, err := b.ListPhraseSet(ctx, &speechv1pb.ListPhraseSetRequest{Parent: v1Parent})
	var names []string
	for _, p := range all.GetPhraseSets() {
		names = append(names, p.GetName())
	}
	sort.Strings(names)
	if err != nil || len(names) != 2 || names[0] != v1Parent+"/phraseSets/Names-1" {
		t.Errorf("after restart: %v %v", names, err)
	}
	got, err := b.GetCustomClass(ctx, &speechv1pb.GetCustomClassRequest{Name: cc.GetName()})
	if err != nil || len(got.GetItems()) != 2 {
		t.Errorf("class after restart: %v %v", got, err)
	}
	if _, err := b.DeleteCustomClass(ctx, &speechv1pb.DeleteCustomClassRequest{Name: cc.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetCustomClass(ctx, &speechv1pb.GetCustomClassRequest{Name: cc.GetName()}); code(grpcError(err)) != codes.NotFound {
		t.Errorf("deleted class: %v", err)
	}
}

// TestSpeechV1AdaptationBiasesRecognition (#2106): a v1 config's
// phrase_set_references and a phrase's ${id} naming a stored custom class
// become whisper's boosted phrases, as inline adaptation does.
func TestSpeechV1AdaptationBiasesRecognition(t *testing.T) {
	ctx := context.Background()
	v := newSpeech(t, filepath.Join(t.TempDir(), "recognizers.json"))
	a := &speechV1AdaptationService{s: v.s}
	if _, err := a.CreateCustomClass(ctx, &speechv1pb.CreateCustomClassRequest{Parent: v1Parent, CustomClassId: "towns",
		CustomClass: &speechv1pb.CustomClass{Items: []*speechv1pb.CustomClass_ClassItem{{Value: "Quoddy"}, {Value: "Vyborg"}}}}); err != nil {
		t.Fatal(err)
	}
	ps, err := a.CreatePhraseSet(ctx, &speechv1pb.CreatePhraseSetRequest{Parent: v1Parent, PhraseSetId: "names",
		PhraseSet: &speechv1pb.PhraseSet{Boost: 12, Phrases: []*speechv1pb.PhraseSet_Phrase{{Value: "Nyhuis"}, {Value: "to ${towns}", Boost: 4}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, opt, err := v.s.v1Checked(&speechv1pb.RecognitionConfig{Encoding: speechv1pb.RecognitionConfig_LINEAR16, SampleRateHertz: 16000,
		LanguageCode: "en-US", Adaptation: &speechv1pb.SpeechAdaptation{PhraseSetReferences: []string{ps.GetName()}}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, p := range opt.Phrases {
		got[p.Text] = p.Bias
	}
	want := map[string]float64{"Nyhuis": 12 * biasPerBoost, "to Quoddy": 4 * biasPerBoost, "to Vyborg": 4 * biasPerBoost}
	if len(got) != len(want) {
		t.Fatalf("phrases %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("phrase %q bias %v, want %v (all %v)", k, got[k], w, got)
		}
	}
	if opt.Prompt == "" {
		t.Errorf("no initial prompt")
	}

	// An inline phrase naming the stored class by its resource name.
	_, opt, err = v.s.v1Checked(&speechv1pb.RecognitionConfig{LanguageCode: "en-US",
		SpeechContexts: []*speechv1pb.SpeechContext{{Phrases: []string{"from ${" + v1Parent + "/customClasses/towns}"}, Boost: 8}}})
	if err != nil || len(opt.Phrases) != 2 {
		t.Errorf("resource-name class: %v %v", opt.Phrases, err)
	}
	// An unknown class or phrase set is refused.
	if _, _, err := v.s.v1Checked(&speechv1pb.RecognitionConfig{LanguageCode: "en-US",
		SpeechContexts: []*speechv1pb.SpeechContext{{Phrases: []string{"to ${nowhere}"}}}}); code(grpcError(err)) != codes.InvalidArgument {
		t.Errorf("unknown class: %v", err)
	}
}
