package vertexai

import (
	"context"
	"path/filepath"
	"testing"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSpeechCustomClasses: a custom class's lifecycle, refusals and
// persistence.
func TestSpeechCustomClasses(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "recognizers.json")
	v := newSpeech(t, file)
	items := []*speechpb.CustomClass_ClassItem{{Value: "Burrow"}, {Value: "Warren"}}
	op, err := v.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "places",
		CustomClass: &speechpb.CustomClass{DisplayName: "Places", Items: items}})
	if err != nil || !op.GetDone() || op.GetResponse() == nil {
		t.Fatalf("CreateCustomClass: %v %v", op, err)
	}
	name := speechParent + "/customClasses/places"
	long := string(make([]byte, 64))
	for _, c := range []struct {
		req  *speechpb.CreateCustomClassRequest
		want codes.Code
	}{
		{&speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "places", CustomClass: &speechpb.CustomClass{}}, codes.AlreadyExists},
		{&speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "Bad_ID", CustomClass: &speechpb.CustomClass{}}, codes.InvalidArgument},
		{&speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "abcd", CustomClass: &speechpb.CustomClass{DisplayName: long}}, codes.InvalidArgument},
		{&speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "abcd", CustomClass: &speechpb.CustomClass{Items: []*speechpb.CustomClass_ClassItem{{}}}}, codes.InvalidArgument},
	} {
		if _, err := v.CreateCustomClass(ctx, c.req); code(err) != c.want {
			t.Errorf("CreateCustomClass %s: %v, want %v", c.req.GetCustomClassId(), err, c.want)
		}
	}
	if _, err := v.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: speechParent, CustomClassId: "dry-run",
		CustomClass: &speechpb.CustomClass{}, ValidateOnly: true}); err != nil {
		t.Errorf("validate_only: %v", err)
	}
	if _, err := v.GetCustomClass(ctx, &speechpb.GetCustomClassRequest{Name: speechParent + "/customClasses/dry-run"}); code(err) != codes.NotFound {
		t.Errorf("validate_only created it: %v", err)
	}
	got, err := v.GetCustomClass(ctx, &speechpb.GetCustomClassRequest{Name: name})
	if err != nil || got.GetState() != speechpb.CustomClass_ACTIVE || len(got.GetItems()) != 2 || got.GetEtag() == "" {
		t.Fatalf("GetCustomClass: %v %v", got, err)
	}
	if _, err := v.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: name, Etag: "stale"}}); code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
	if _, err := v.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: name, DisplayName: "Homes"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("mask state: %v", err)
	}
	if _, err := v.DeleteCustomClass(ctx, &speechpb.DeleteCustomClassRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if l, _ := v.ListCustomClasses(ctx, &speechpb.ListCustomClassesRequest{Parent: speechParent}); len(l.GetCustomClasses()) != 0 {
		t.Errorf("deleted listed: %v", l)
	}
	if _, err := v.DeleteCustomClass(ctx, &speechpb.DeleteCustomClassRequest{Name: speechParent + "/customClasses/gone", AllowMissing: true}); err != nil {
		t.Errorf("allow_missing: %v", err)
	}

	// Persisted, deleted and listed with show_deleted.
	v2 := newSpeech(t, file)
	l, err := v2.ListCustomClasses(ctx, &speechpb.ListCustomClassesRequest{Parent: speechParent, ShowDeleted: true})
	if err != nil || len(l.GetCustomClasses()) != 1 || l.GetCustomClasses()[0].GetState() != speechpb.CustomClass_DELETED ||
		l.GetCustomClasses()[0].GetDisplayName() != "Homes" || l.GetCustomClasses()[0].GetExpireTime() == nil {
		t.Fatalf("after reload: %v %v", l, err)
	}
	if _, err := v2.UndeleteCustomClass(ctx, &speechpb.UndeleteCustomClassRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.UndeleteCustomClass(ctx, &speechpb.UndeleteCustomClassRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Errorf("undelete active: %v", err)
	}
}

// TestSpeechPhraseSets: a phrase set's lifecycle, boost limits, paging and
// persistence.
func TestSpeechPhraseSets(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "recognizers.json")
	v := newSpeech(t, file)
	for _, c := range []struct {
		ps   *speechpb.PhraseSet
		want codes.Code
	}{
		{&speechpb.PhraseSet{Boost: -1}, codes.InvalidArgument},
		{&speechpb.PhraseSet{Boost: 21}, codes.InvalidArgument},
		{&speechpb.PhraseSet{Phrases: []*speechpb.PhraseSet_Phrase{{Value: "x", Boost: 25}}}, codes.InvalidArgument},
		{&speechpb.PhraseSet{Phrases: []*speechpb.PhraseSet_Phrase{{Value: ""}}}, codes.InvalidArgument},
	} {
		if _, err := v.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: speechParent, PhraseSetId: "bad-one", PhraseSet: c.ps}); code(err) != c.want {
			t.Errorf("CreatePhraseSet %v: %v, want %v", c.ps, err, c.want)
		}
	}
	for _, id := range []string{"set-a", "set-b", "set-c", "set-d", "set-e", "set-f"} {
		if _, err := v.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: speechParent, PhraseSetId: id,
			PhraseSet: &speechpb.PhraseSet{Boost: 10, Phrases: []*speechpb.PhraseSet_Phrase{{Value: "cloud burrow", Boost: 15}}}}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := v.ListPhraseSets(ctx, &speechpb.ListPhraseSetsRequest{Parent: speechParent})
	if err != nil || len(l.GetPhraseSets()) != 5 || l.GetNextPageToken() == "" {
		t.Fatalf("default page: %v %v", l, err)
	}
	name := speechParent + "/phraseSets/set-a"
	if _, err := v.UpdatePhraseSet(ctx, &speechpb.UpdatePhraseSetRequest{PhraseSet: &speechpb.PhraseSet{Name: name, Boost: 5},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"boost"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.DeletePhraseSet(ctx, &speechpb.DeletePhraseSetRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.UpdatePhraseSet(ctx, &speechpb.UpdatePhraseSetRequest{PhraseSet: &speechpb.PhraseSet{Name: name}}); code(err) != codes.FailedPrecondition {
		t.Errorf("update deleted: %v", err)
	}
	v2 := newSpeech(t, file)
	got, err := v2.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: name})
	if err != nil || got.GetBoost() != 5 || got.GetState() != speechpb.PhraseSet_DELETED || got.GetPhrases()[0].GetValue() != "cloud burrow" {
		t.Fatalf("after reload: %v %v", got, err)
	}
	if _, err := v2.UndeletePhraseSet(ctx, &speechpb.UndeletePhraseSetRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
}
