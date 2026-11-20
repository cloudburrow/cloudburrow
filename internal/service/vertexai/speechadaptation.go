package vertexai

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Speech-to-Text v2's model adaptation resources (#1930): custom classes
// and phrase sets, on the local AI endpoint, gRPC and JSON, as
// https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.customClasses
// and .../projects.locations.phraseSets document them (read 2026-11-19):
// Create, Get, List, Update (patch), Delete and Undelete, each returning a
// done operation with OperationMetadata, as the recognizers do.
//
//   - IDs are 4 to 63 characters of [a-z0-9-] starting with a letter (the
//     reference: "4-63 characters, and valid characters are /[a-z][0-9]-/");
//     display_name is at most 63 characters; at most 100 annotations, each
//     key and value at most 63 characters.
//   - A phrase set's boost is in (0, 20] when set, a phrase's in [0, 20]:
//     "negative boost values will return an error". A phrase or class item
//     needs a value.
//   - validate_only changes nothing; etag is checked when sent (ABORTED when
//     stale); Delete marks a resource DELETED with delete_time and an
//     expire_time 30 days on, allow_missing answers a missing one; Undelete
//     makes it ACTIVE; List hides deleted ones unless show_deleted, its
//     page_size 5 when unset and at most 100. kms_key_name is output only:
//     nothing is encrypted with a key, so it stays empty.
//   - Kept in customclasses.json and phrasesets.json beside recognizers.json.
//
// A RecognitionConfig's adaptation (naming these, or inline) stays
// UNIMPLEMENTED in Recognize and BatchRecognize: local whisper.cpp has no
// phrase boosting, and a prompt would only approximate it.
//
// UNVERIFIED (re-read 2026-11-19: the customClasses and phraseSets references above, which define
// the fields, not these): the ID rule's
// leading letter, uid and etag formats, the 30-day expire_time, the texts
// of the errors.

const (
	adaptMaxAnnotations = 100
	adaptMaxLen         = 63
	adaptMaxBoost       = 20
)

// adaptFiles are the custom classes' and phrase sets' files beside the
// recognizers' file.
func adaptFiles(recognizers string) (string, string) {
	if recognizers == "" {
		return "", ""
	}
	d := filepath.Dir(recognizers)
	return filepath.Join(d, "customclasses.json"), filepath.Join(d, "phrasesets.json")
}

// loadAdaptation reads the custom classes and phrase sets; the caller holds
// s.mu and has set s.speech.
func (st *speechStore) loadAdaptation() {
	st.classes = map[string]*speechpb.CustomClass{}
	st.phraseSets = map[string]*speechpb.PhraseSet{}
	cf, pf := adaptFiles(st.file)
	if cf == "" {
		return
	}
	if b, err := os.ReadFile(cf); err == nil {
		var l speechpb.ListCustomClassesResponse
		if protojson.Unmarshal(b, &l) == nil {
			for _, x := range l.GetCustomClasses() {
				st.classes[x.GetName()] = x
			}
		}
	}
	if b, err := os.ReadFile(pf); err == nil {
		var l speechpb.ListPhraseSetsResponse
		if protojson.Unmarshal(b, &l) == nil {
			for _, x := range l.GetPhraseSets() {
				st.phraseSets[x.GetName()] = x
			}
		}
	}
}

// saveAdaptation writes the custom classes and phrase sets; the caller
// holds s.mu.
func (st *speechStore) saveAdaptation() {
	cf, pf := adaptFiles(st.file)
	if cf == "" {
		return
	}
	cl := &speechpb.ListCustomClassesResponse{}
	for _, x := range st.classes {
		cl.CustomClasses = append(cl.CustomClasses, x)
	}
	sort.Slice(cl.CustomClasses, func(i, j int) bool { return cl.CustomClasses[i].Name < cl.CustomClasses[j].Name })
	pl := &speechpb.ListPhraseSetsResponse{}
	for _, x := range st.phraseSets {
		pl.PhraseSets = append(pl.PhraseSets, x)
	}
	sort.Slice(pl.PhraseSets, func(i, j int) bool { return pl.PhraseSets[i].Name < pl.PhraseSets[j].Name })
	for f, m := range map[string]proto.Message{cf: cl, pf: pl} {
		b, _ := protojson.Marshal(m)
		_ = os.MkdirAll(filepath.Dir(f), 0o755)
		if os.WriteFile(f+".tmp", b, 0o600) == nil {
			_ = os.Rename(f+".tmp", f)
		}
	}
}

// checkAdaptCommon checks the display name and annotations.
func checkAdaptCommon(display string, annotations map[string]string) error {
	if len(display) > adaptMaxLen {
		return apierror.InvalidArgument("display_name must be %d characters or less", adaptMaxLen)
	}
	if len(annotations) > adaptMaxAnnotations {
		return apierror.InvalidArgument("At most %d annotations are allowed", adaptMaxAnnotations)
	}
	for k, v := range annotations {
		if len(k) > adaptMaxLen || len(v) > adaptMaxLen {
			return apierror.InvalidArgument("annotation keys and values must be %d characters or less each", adaptMaxLen)
		}
	}
	return nil
}

func checkCustomClass(c *speechpb.CustomClass) error {
	if err := checkAdaptCommon(c.GetDisplayName(), c.GetAnnotations()); err != nil {
		return err
	}
	for i, it := range c.GetItems() {
		if strings.TrimSpace(it.GetValue()) == "" {
			return apierror.InvalidArgument("items[%d].value is required", i)
		}
	}
	return nil
}

func checkPhraseSet(p *speechpb.PhraseSet) error {
	if err := checkAdaptCommon(p.GetDisplayName(), p.GetAnnotations()); err != nil {
		return err
	}
	if b := p.GetBoost(); b < 0 || b > adaptMaxBoost {
		return apierror.InvalidArgument("Invalid boost %v: valid boost values are between 0 (exclusive) and %d; negative boost values are not supported", b, adaptMaxBoost)
	}
	for i, ph := range p.GetPhrases() {
		if strings.TrimSpace(ph.GetValue()) == "" {
			return apierror.InvalidArgument("phrases[%d].value is required", i)
		}
		if b := ph.GetBoost(); b < 0 || b > adaptMaxBoost {
			return apierror.InvalidArgument("Invalid boost %v for phrases[%d]: boost values must be between 0 and %d", b, i, adaptMaxBoost)
		}
	}
	return nil
}

// checkAdaptID checks a create request's parent and ID.
func checkAdaptID(parent, field, id string) error {
	if _, _, err := parseParent("parent", parent); err != nil {
		return err
	}
	if !recognizerIDPattern.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be 4 to 63 characters of lower-case letters, digits and hyphens, starting with a letter", field, id)
	}
	return nil
}

// adaptPage pages a list with the adaptation lists' default size of 5.
func adaptPage[T any](items []T, size int32, token string) ([]T, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must be between 0 and 100")
	}
	if size == 0 {
		size = 5
	}
	return docAIPage(items, size, token)
}

// adaptMask is an update mask's paths, all of fields for none or "*".
func adaptMask(paths []string, fields ...string) ([]string, error) {
	if len(paths) == 0 || len(paths) == 1 && paths[0] == "*" {
		return fields, nil
	}
	for _, p := range paths {
		ok := false
		for _, f := range fields {
			ok = ok || p == f
		}
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated", p)
		}
	}
	return paths, nil
}

// --- custom classes ---

func (v *speechService) CreateCustomClass(_ context.Context, req *speechpb.CreateCustomClassRequest) (*longrunningpb.Operation, error) {
	if err := checkAdaptID(req.GetParent(), "custom_class_id", req.GetCustomClassId()); err != nil {
		return nil, grpcError(err)
	}
	in := req.GetCustomClass()
	if err := checkCustomClass(in); err != nil {
		return nil, grpcError(err)
	}
	name := req.GetParent() + "/customClasses/" + req.GetCustomClassId()
	now := timestamppb.Now()
	c := &speechpb.CustomClass{Name: name, Uid: newEtag() + newEtag(), DisplayName: in.GetDisplayName(), Items: in.GetItems(),
		Annotations: in.GetAnnotations(), State: speechpb.CustomClass_ACTIVE, CreateTime: now, UpdateTime: now, Etag: newEtag()}
	v.s.mu.Lock()
	st := v.s.speechRecognizers()
	if _, ok := st.classes[name]; ok {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.AlreadyExists("CustomClass %s already exists", name))
	}
	if !req.GetValidateOnly() {
		st.classes[name] = c
		st.saveAdaptation()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, "CreateCustomClass", proto.Clone(c))
}

func (v *speechService) lookupClass(name string) (*speechpb.CustomClass, error) {
	if _, _, _, err := parseChild("name", "customClasses", name); err != nil {
		return nil, err
	}
	c, ok := v.s.speechRecognizers().classes[name]
	if !ok {
		return nil, apierror.NotFound("Unable to find CustomClass %s from storage", name)
	}
	return c, nil
}

func (v *speechService) GetCustomClass(_ context.Context, req *speechpb.GetCustomClassRequest) (*speechpb.CustomClass, error) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	c, err := v.lookupClass(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(c).(*speechpb.CustomClass), nil
}

func (v *speechService) ListCustomClasses(_ context.Context, req *speechpb.ListCustomClassesRequest) (*speechpb.ListCustomClassesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	v.s.mu.Lock()
	var all []*speechpb.CustomClass
	for _, c := range v.s.speechRecognizers().classes {
		if strings.HasPrefix(c.GetName(), req.GetParent()+"/customClasses/") && (req.GetShowDeleted() || c.GetState() != speechpb.CustomClass_DELETED) {
			all = append(all, proto.Clone(c).(*speechpb.CustomClass))
		}
	}
	v.s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	page, next, err := adaptPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &speechpb.ListCustomClassesResponse{CustomClasses: page, NextPageToken: next}, nil
}

// changeClass applies f to a custom class under its etag.
func (v *speechService) changeClass(name, etag, method string, validateOnly bool, f func(c *speechpb.CustomClass) error) (*longrunningpb.Operation, error) {
	v.s.mu.Lock()
	c, err := v.lookupClass(name)
	if err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if etag != "" && etag != c.GetEtag() {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.Aborted("The etag %q does not match the custom class's current etag", etag))
	}
	next := proto.Clone(c).(*speechpb.CustomClass)
	if err := f(next); err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if !validateOnly {
		next.UpdateTime, next.Etag = timestamppb.Now(), newEtag()
		st := v.s.speechRecognizers()
		st.classes[name] = next
		st.saveAdaptation()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, method, proto.Clone(next))
}

func (v *speechService) UpdateCustomClass(_ context.Context, req *speechpb.UpdateCustomClassRequest) (*longrunningpb.Operation, error) {
	in := req.GetCustomClass()
	if err := checkCustomClass(in); err != nil {
		return nil, grpcError(err)
	}
	paths, err := adaptMask(req.GetUpdateMask().GetPaths(), "display_name", "items", "annotations")
	if err != nil {
		return nil, grpcError(err)
	}
	return v.changeClass(in.GetName(), in.GetEtag(), "UpdateCustomClass", req.GetValidateOnly(), func(c *speechpb.CustomClass) error {
		if c.GetState() == speechpb.CustomClass_DELETED {
			return apierror.FailedPrecondition("CustomClass %s is deleted", c.GetName())
		}
		for _, p := range paths {
			switch p {
			case "display_name":
				c.DisplayName = in.GetDisplayName()
			case "items":
				c.Items = in.GetItems()
			case "annotations":
				c.Annotations = in.GetAnnotations()
			}
		}
		return nil
	})
}

func (v *speechService) DeleteCustomClass(_ context.Context, req *speechpb.DeleteCustomClassRequest) (*longrunningpb.Operation, error) {
	if req.GetAllowMissing() {
		v.s.mu.Lock()
		_, err := v.lookupClass(req.GetName())
		v.s.mu.Unlock()
		if err != nil && apierror.From(err).HTTPStatus() == 404 {
			return v.s.speechOp(req.GetName(), "DeleteCustomClass", &speechpb.CustomClass{Name: req.GetName()})
		}
	}
	return v.changeClass(req.GetName(), req.GetEtag(), "DeleteCustomClass", req.GetValidateOnly(), func(c *speechpb.CustomClass) error {
		if c.GetState() == speechpb.CustomClass_DELETED {
			return apierror.FailedPrecondition("CustomClass %s is already deleted", c.GetName())
		}
		now := time.Now()
		c.State, c.DeleteTime, c.ExpireTime = speechpb.CustomClass_DELETED, timestamppb.New(now), timestamppb.New(now.Add(30*24*time.Hour))
		return nil
	})
}

func (v *speechService) UndeleteCustomClass(_ context.Context, req *speechpb.UndeleteCustomClassRequest) (*longrunningpb.Operation, error) {
	return v.changeClass(req.GetName(), req.GetEtag(), "UndeleteCustomClass", req.GetValidateOnly(), func(c *speechpb.CustomClass) error {
		if c.GetState() != speechpb.CustomClass_DELETED {
			return apierror.FailedPrecondition("CustomClass %s is not deleted", c.GetName())
		}
		c.State, c.DeleteTime, c.ExpireTime = speechpb.CustomClass_ACTIVE, nil, nil
		return nil
	})
}

// --- phrase sets ---

func (v *speechService) CreatePhraseSet(_ context.Context, req *speechpb.CreatePhraseSetRequest) (*longrunningpb.Operation, error) {
	if err := checkAdaptID(req.GetParent(), "phrase_set_id", req.GetPhraseSetId()); err != nil {
		return nil, grpcError(err)
	}
	in := req.GetPhraseSet()
	if err := checkPhraseSet(in); err != nil {
		return nil, grpcError(err)
	}
	name := req.GetParent() + "/phraseSets/" + req.GetPhraseSetId()
	now := timestamppb.Now()
	p := &speechpb.PhraseSet{Name: name, Uid: newEtag() + newEtag(), DisplayName: in.GetDisplayName(), Phrases: in.GetPhrases(),
		Boost: in.GetBoost(), Annotations: in.GetAnnotations(), State: speechpb.PhraseSet_ACTIVE, CreateTime: now, UpdateTime: now, Etag: newEtag()}
	v.s.mu.Lock()
	st := v.s.speechRecognizers()
	if _, ok := st.phraseSets[name]; ok {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.AlreadyExists("PhraseSet %s already exists", name))
	}
	if !req.GetValidateOnly() {
		st.phraseSets[name] = p
		st.saveAdaptation()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, "CreatePhraseSet", proto.Clone(p))
}

func (v *speechService) lookupPhraseSet(name string) (*speechpb.PhraseSet, error) {
	if _, _, _, err := parseChild("name", "phraseSets", name); err != nil {
		return nil, err
	}
	p, ok := v.s.speechRecognizers().phraseSets[name]
	if !ok {
		return nil, apierror.NotFound("Unable to find PhraseSet %s from storage", name)
	}
	return p, nil
}

func (v *speechService) GetPhraseSet(_ context.Context, req *speechpb.GetPhraseSetRequest) (*speechpb.PhraseSet, error) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	p, err := v.lookupPhraseSet(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(p).(*speechpb.PhraseSet), nil
}

func (v *speechService) ListPhraseSets(_ context.Context, req *speechpb.ListPhraseSetsRequest) (*speechpb.ListPhraseSetsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	v.s.mu.Lock()
	var all []*speechpb.PhraseSet
	for _, p := range v.s.speechRecognizers().phraseSets {
		if strings.HasPrefix(p.GetName(), req.GetParent()+"/phraseSets/") && (req.GetShowDeleted() || p.GetState() != speechpb.PhraseSet_DELETED) {
			all = append(all, proto.Clone(p).(*speechpb.PhraseSet))
		}
	}
	v.s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	page, next, err := adaptPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &speechpb.ListPhraseSetsResponse{PhraseSets: page, NextPageToken: next}, nil
}

// changePhraseSet applies f to a phrase set under its etag.
func (v *speechService) changePhraseSet(name, etag, method string, validateOnly bool, f func(p *speechpb.PhraseSet) error) (*longrunningpb.Operation, error) {
	v.s.mu.Lock()
	p, err := v.lookupPhraseSet(name)
	if err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if etag != "" && etag != p.GetEtag() {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.Aborted("The etag %q does not match the phrase set's current etag", etag))
	}
	next := proto.Clone(p).(*speechpb.PhraseSet)
	if err := f(next); err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if !validateOnly {
		next.UpdateTime, next.Etag = timestamppb.Now(), newEtag()
		st := v.s.speechRecognizers()
		st.phraseSets[name] = next
		st.saveAdaptation()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, method, proto.Clone(next))
}

func (v *speechService) UpdatePhraseSet(_ context.Context, req *speechpb.UpdatePhraseSetRequest) (*longrunningpb.Operation, error) {
	in := req.GetPhraseSet()
	if err := checkPhraseSet(in); err != nil {
		return nil, grpcError(err)
	}
	paths, err := adaptMask(req.GetUpdateMask().GetPaths(), "display_name", "phrases", "boost", "annotations")
	if err != nil {
		return nil, grpcError(err)
	}
	return v.changePhraseSet(in.GetName(), in.GetEtag(), "UpdatePhraseSet", req.GetValidateOnly(), func(p *speechpb.PhraseSet) error {
		if p.GetState() == speechpb.PhraseSet_DELETED {
			return apierror.FailedPrecondition("PhraseSet %s is deleted", p.GetName())
		}
		for _, f := range paths {
			switch f {
			case "display_name":
				p.DisplayName = in.GetDisplayName()
			case "phrases":
				p.Phrases = in.GetPhrases()
			case "boost":
				p.Boost = in.GetBoost()
			case "annotations":
				p.Annotations = in.GetAnnotations()
			}
		}
		return nil
	})
}

func (v *speechService) DeletePhraseSet(_ context.Context, req *speechpb.DeletePhraseSetRequest) (*longrunningpb.Operation, error) {
	if req.GetAllowMissing() {
		v.s.mu.Lock()
		_, err := v.lookupPhraseSet(req.GetName())
		v.s.mu.Unlock()
		if err != nil && apierror.From(err).HTTPStatus() == 404 {
			return v.s.speechOp(req.GetName(), "DeletePhraseSet", &speechpb.PhraseSet{Name: req.GetName()})
		}
	}
	return v.changePhraseSet(req.GetName(), req.GetEtag(), "DeletePhraseSet", req.GetValidateOnly(), func(p *speechpb.PhraseSet) error {
		if p.GetState() == speechpb.PhraseSet_DELETED {
			return apierror.FailedPrecondition("PhraseSet %s is already deleted", p.GetName())
		}
		now := time.Now()
		p.State, p.DeleteTime, p.ExpireTime = speechpb.PhraseSet_DELETED, timestamppb.New(now), timestamppb.New(now.Add(30*24*time.Hour))
		return nil
	})
}

func (v *speechService) UndeletePhraseSet(_ context.Context, req *speechpb.UndeletePhraseSetRequest) (*longrunningpb.Operation, error) {
	return v.changePhraseSet(req.GetName(), req.GetEtag(), "UndeletePhraseSet", req.GetValidateOnly(), func(p *speechpb.PhraseSet) error {
		if p.GetState() != speechpb.PhraseSet_DELETED {
			return apierror.FailedPrecondition("PhraseSet %s is not deleted", p.GetName())
		}
		p.State, p.DeleteTime, p.ExpireTime = speechpb.PhraseSet_ACTIVE, nil, nil
		return nil
	})
}
