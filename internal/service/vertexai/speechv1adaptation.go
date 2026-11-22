package vertexai

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	speechv1pb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Speech-to-Text v1's google.cloud.speech.v1.Adaptation (#2106) on the local
// AI endpoint, over gRPC (v1 is gRPC only here, as speechv1.go): phrase
// sets and custom classes as resources, as
// https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v1
// documents them (re-read 2026-11-21):
//
//   - Create, Get, List (ListPhraseSet, ListCustomClasses), Update and
//     Delete answer the resource itself (Delete google.protobuf.Empty), not
//     an operation as v2's do.
//   - The parent is projects/{project}/locations/{location}, the location
//     one of "global", "us" and "eu" ("Speech-to-Text supports three
//     locations"); a custom class's parent may also end in /customClasses,
//     as the reference writes its format.
//   - phrase_set_id and custom_class_id "should restrict to letters,
//     numbers, and hyphens, with the first character a letter, the last a
//     letter or a number, and be 4-63 characters".
//   - List's page_size: "If unspecified, at most 50 ... The maximum value
//     is 1000; values above 1000 will be coerced to 1000."
//   - A boost is in [0, 20] ("between 0 (exclusive) and 20"; 0 is unset, a
//     negative one an error); a phrase needs a value of at most 100
//     characters, a set at most 1,200 phrases, a class at most 500 items of
//     at most 500 characters (speechbias.go's limits).
//   - update_mask: a phrase set's phrases and boost, a class's items; none
//     or "*" is every one of them.
//   - Kept in v1phrasesets.json and v1customclasses.json beside v2's files.
//
// Recognition applies them (v1Resolve): adaptation.phrase_set_references
// name stored phrase sets, which join the request's phrase sets, and a
// phrase's ${id} names a stored custom class by its ID ("To refer to
// custom classes resources, use the class' id wrapped in ${}") or by its
// resource name, which joins the request's inline classes; all then bias
// whisper.cpp exactly as v2's adaptation does (speechbias.go).
//
// UNVERIFIED (re-read 2026-11-21: the reference above, which does not say):
// the texts of the errors, which location's class a ${id} names when two
// locations hold one (here the phrase set's own, then the request's
// project's in name order).

const (
	v1AdaptPageDefault = 50
	v1AdaptPageMax     = 1000
)

var v1AdaptIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{2,61}[A-Za-z0-9]$`)

var v1AdaptLocations = map[string]bool{"global": true, "us": true, "eu": true}

type speechV1AdaptationService struct {
	speechv1pb.UnimplementedAdaptationServer
	s *Server
}

// v1AdaptFiles are the v1 phrase sets' and custom classes' files beside
// the recognizers' file.
func v1AdaptFiles(recognizers string) (string, string) {
	if recognizers == "" {
		return "", ""
	}
	d := filepath.Dir(recognizers)
	return filepath.Join(d, "v1phrasesets.json"), filepath.Join(d, "v1customclasses.json")
}

// loadV1Adaptation reads v1's phrase sets and custom classes; the caller
// holds s.mu.
func (st *speechStore) loadV1Adaptation() {
	st.v1PhraseSets = map[string]*speechv1pb.PhraseSet{}
	st.v1Classes = map[string]*speechv1pb.CustomClass{}
	pf, cf := v1AdaptFiles(st.file)
	if pf == "" {
		return
	}
	if b, err := os.ReadFile(pf); err == nil {
		var l speechv1pb.ListPhraseSetResponse
		if protojson.Unmarshal(b, &l) == nil {
			for _, x := range l.GetPhraseSets() {
				st.v1PhraseSets[x.GetName()] = x
			}
		}
	}
	if b, err := os.ReadFile(cf); err == nil {
		var l speechv1pb.ListCustomClassesResponse
		if protojson.Unmarshal(b, &l) == nil {
			for _, x := range l.GetCustomClasses() {
				st.v1Classes[x.GetName()] = x
			}
		}
	}
}

// saveV1Adaptation writes them; the caller holds s.mu.
func (st *speechStore) saveV1Adaptation() {
	pf, cf := v1AdaptFiles(st.file)
	if pf == "" {
		return
	}
	pl := &speechv1pb.ListPhraseSetResponse{}
	for _, x := range st.v1PhraseSets {
		pl.PhraseSets = append(pl.PhraseSets, x)
	}
	sort.Slice(pl.PhraseSets, func(i, j int) bool { return pl.PhraseSets[i].Name < pl.PhraseSets[j].Name })
	cl := &speechv1pb.ListCustomClassesResponse{}
	for _, x := range st.v1Classes {
		cl.CustomClasses = append(cl.CustomClasses, x)
	}
	sort.Slice(cl.CustomClasses, func(i, j int) bool { return cl.CustomClasses[i].Name < cl.CustomClasses[j].Name })
	for f, m := range map[string]proto.Message{pf: pl, cf: cl} {
		b, _ := protojson.Marshal(m)
		_ = os.MkdirAll(filepath.Dir(f), 0o755)
		if os.WriteFile(f+".tmp", b, 0o600) == nil {
			_ = os.Rename(f+".tmp", f)
		}
	}
}

// v1Store is the speech store with v1's maps made; the caller holds s.mu.
func (s *Server) v1Store() *speechStore {
	st := s.speechRecognizers()
	if st.v1PhraseSets == nil {
		st.loadV1Adaptation()
	}
	return st
}

// v1AdaptParent checks a parent, with collection as an optional last
// segment, and returns projects/{p}/locations/{l}.
func v1AdaptParent(parent, collection string) (string, error) {
	parent = strings.TrimSuffix(parent, "/"+collection)
	_, loc, err := parseParent("parent", parent)
	if err != nil {
		return "", err
	}
	if !v1AdaptLocations[loc] {
		return "", apierror.InvalidArgument("Location %q is not supported: Speech-to-Text supports three locations: global, us and eu", loc)
	}
	return parent, nil
}

func v1AdaptID(field, id string) error {
	if !v1AdaptIDPattern.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be 4 to 63 letters, numbers and hyphens, the first a letter and the last a letter or a number", field, id)
	}
	return nil
}

func v1AdaptName(collection, name string) error {
	_, loc, _, err := parseChild("name", collection, name)
	if err != nil {
		return err
	}
	if !v1AdaptLocations[loc] {
		return apierror.InvalidArgument("Location %q is not supported: Speech-to-Text supports three locations: global, us and eu", loc)
	}
	return nil
}

func checkV1PhraseSet(p *speechv1pb.PhraseSet) error {
	if p == nil {
		return apierror.InvalidArgument("phrase_set is required")
	}
	if b := p.GetBoost(); b < 0 || b > adaptMaxBoost {
		return apierror.InvalidArgument("Invalid boost %v: valid boost values are between 0 (exclusive) and %d; negative boost values are not supported", b, adaptMaxBoost)
	}
	if n := len(p.GetPhrases()); n > adaptMaxSetPhrases {
		return apierror.InvalidArgument("phrase_set has %d phrases: at most %d are allowed", n, adaptMaxSetPhrases)
	}
	for i, ph := range p.GetPhrases() {
		v := strings.TrimSpace(ph.GetValue())
		if v == "" {
			return apierror.InvalidArgument("phrases[%d].value is required", i)
		}
		if utf8.RuneCountInString(v) > adaptMaxPhraseChars {
			return apierror.InvalidArgument("Phrase %q is longer than %d characters", v, adaptMaxPhraseChars)
		}
		if b := ph.GetBoost(); b < 0 || b > adaptMaxBoost {
			return apierror.InvalidArgument("Invalid boost %v for phrases[%d]: boost values must be between 0 and %d", b, i, adaptMaxBoost)
		}
	}
	return nil
}

func checkV1CustomClass(c *speechv1pb.CustomClass) error {
	if c == nil {
		return apierror.InvalidArgument("custom_class is required")
	}
	if n := len(c.GetItems()); n > adaptMaxItems {
		return apierror.InvalidArgument("custom_class has %d items: at most %d are allowed", n, adaptMaxItems)
	}
	for i, it := range c.GetItems() {
		v := strings.TrimSpace(it.GetValue())
		if v == "" {
			return apierror.InvalidArgument("items[%d].value is required", i)
		}
		if utf8.RuneCountInString(v) > adaptMaxItemChars {
			return apierror.InvalidArgument("items[%d].value is longer than %d characters", i, adaptMaxItemChars)
		}
	}
	return nil
}

// v1Page pages a sorted list by page_size (default 50, at most 1000).
func v1Page[T any](items []T, size int32, token string) ([]T, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	if size == 0 {
		size = v1AdaptPageDefault
	}
	return docAIPage(items, min(size, v1AdaptPageMax), token)
}

func inLocation(name, parent string) bool { return strings.HasPrefix(name, parent+"/") }

// --- phrase sets ---

func (a *speechV1AdaptationService) CreatePhraseSet(_ context.Context, req *speechv1pb.CreatePhraseSetRequest) (*speechv1pb.PhraseSet, error) {
	parent, err := v1AdaptParent(req.GetParent(), "phraseSets")
	if err != nil {
		return nil, err
	}
	if err := v1AdaptID("phrase_set_id", req.GetPhraseSetId()); err != nil {
		return nil, err
	}
	if err := checkV1PhraseSet(req.GetPhraseSet()); err != nil {
		return nil, err
	}
	p := proto.Clone(req.GetPhraseSet()).(*speechv1pb.PhraseSet)
	p.Name = parent + "/phraseSets/" + req.GetPhraseSetId()
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	if _, ok := st.v1PhraseSets[p.Name]; ok {
		return nil, apierror.AlreadyExists("PhraseSet %s already exists", p.Name)
	}
	st.v1PhraseSets[p.Name] = p
	st.saveV1Adaptation()
	return proto.Clone(p).(*speechv1pb.PhraseSet), nil
}

func (a *speechV1AdaptationService) GetPhraseSet(_ context.Context, req *speechv1pb.GetPhraseSetRequest) (*speechv1pb.PhraseSet, error) {
	if err := v1AdaptName("phraseSets", req.GetName()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.v1Store().v1PhraseSets[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("PhraseSet %s not found", req.GetName())
	}
	return proto.Clone(p).(*speechv1pb.PhraseSet), nil
}

func (a *speechV1AdaptationService) ListPhraseSet(_ context.Context, req *speechv1pb.ListPhraseSetRequest) (*speechv1pb.ListPhraseSetResponse, error) {
	parent, err := v1AdaptParent(req.GetParent(), "phraseSets")
	if err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	var all []*speechv1pb.PhraseSet
	for n, p := range s.v1Store().v1PhraseSets {
		if inLocation(n, parent) {
			all = append(all, proto.Clone(p).(*speechv1pb.PhraseSet))
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := v1Page(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &speechv1pb.ListPhraseSetResponse{PhraseSets: page, NextPageToken: next}, nil
}

func (a *speechV1AdaptationService) UpdatePhraseSet(_ context.Context, req *speechv1pb.UpdatePhraseSetRequest) (*speechv1pb.PhraseSet, error) {
	in := req.GetPhraseSet()
	if in == nil {
		return nil, apierror.InvalidArgument("phrase_set is required")
	}
	if err := v1AdaptName("phraseSets", in.GetName()); err != nil {
		return nil, err
	}
	paths, err := adaptMask(req.GetUpdateMask().GetPaths(), "phrases", "boost")
	if err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	cur, ok := st.v1PhraseSets[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("PhraseSet %s not found", in.GetName())
	}
	next := proto.Clone(cur).(*speechv1pb.PhraseSet)
	for _, p := range paths {
		switch p {
		case "phrases":
			next.Phrases = in.GetPhrases()
		case "boost":
			next.Boost = in.GetBoost()
		}
	}
	if err := checkV1PhraseSet(next); err != nil {
		return nil, err
	}
	next = proto.Clone(next).(*speechv1pb.PhraseSet)
	st.v1PhraseSets[next.Name] = next
	st.saveV1Adaptation()
	return proto.Clone(next).(*speechv1pb.PhraseSet), nil
}

func (a *speechV1AdaptationService) DeletePhraseSet(_ context.Context, req *speechv1pb.DeletePhraseSetRequest) (*emptypb.Empty, error) {
	if err := v1AdaptName("phraseSets", req.GetName()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	if _, ok := st.v1PhraseSets[req.GetName()]; !ok {
		return nil, apierror.NotFound("PhraseSet %s not found", req.GetName())
	}
	delete(st.v1PhraseSets, req.GetName())
	st.saveV1Adaptation()
	return &emptypb.Empty{}, nil
}

// --- custom classes ---

func (a *speechV1AdaptationService) CreateCustomClass(_ context.Context, req *speechv1pb.CreateCustomClassRequest) (*speechv1pb.CustomClass, error) {
	parent, err := v1AdaptParent(req.GetParent(), "customClasses")
	if err != nil {
		return nil, err
	}
	if err := v1AdaptID("custom_class_id", req.GetCustomClassId()); err != nil {
		return nil, err
	}
	if err := checkV1CustomClass(req.GetCustomClass()); err != nil {
		return nil, err
	}
	c := proto.Clone(req.GetCustomClass()).(*speechv1pb.CustomClass)
	c.Name = parent + "/customClasses/" + req.GetCustomClassId()
	c.CustomClassId = req.GetCustomClassId()
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	if _, ok := st.v1Classes[c.Name]; ok {
		return nil, apierror.AlreadyExists("CustomClass %s already exists", c.Name)
	}
	st.v1Classes[c.Name] = c
	st.saveV1Adaptation()
	return proto.Clone(c).(*speechv1pb.CustomClass), nil
}

func (a *speechV1AdaptationService) GetCustomClass(_ context.Context, req *speechv1pb.GetCustomClassRequest) (*speechv1pb.CustomClass, error) {
	if err := v1AdaptName("customClasses", req.GetName()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.v1Store().v1Classes[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("CustomClass %s not found", req.GetName())
	}
	return proto.Clone(c).(*speechv1pb.CustomClass), nil
}

func (a *speechV1AdaptationService) ListCustomClasses(_ context.Context, req *speechv1pb.ListCustomClassesRequest) (*speechv1pb.ListCustomClassesResponse, error) {
	parent, err := v1AdaptParent(req.GetParent(), "customClasses")
	if err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	var all []*speechv1pb.CustomClass
	for n, c := range s.v1Store().v1Classes {
		if inLocation(n, parent) {
			all = append(all, proto.Clone(c).(*speechv1pb.CustomClass))
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := v1Page(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &speechv1pb.ListCustomClassesResponse{CustomClasses: page, NextPageToken: next}, nil
}

func (a *speechV1AdaptationService) UpdateCustomClass(_ context.Context, req *speechv1pb.UpdateCustomClassRequest) (*speechv1pb.CustomClass, error) {
	in := req.GetCustomClass()
	if in == nil {
		return nil, apierror.InvalidArgument("custom_class is required")
	}
	if err := v1AdaptName("customClasses", in.GetName()); err != nil {
		return nil, err
	}
	if _, err := adaptMask(req.GetUpdateMask().GetPaths(), "items"); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	cur, ok := st.v1Classes[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("CustomClass %s not found", in.GetName())
	}
	next := proto.Clone(cur).(*speechv1pb.CustomClass)
	next.Items = in.GetItems()
	if err := checkV1CustomClass(next); err != nil {
		return nil, err
	}
	next = proto.Clone(next).(*speechv1pb.CustomClass)
	st.v1Classes[next.Name] = next
	st.saveV1Adaptation()
	return proto.Clone(next).(*speechv1pb.CustomClass), nil
}

func (a *speechV1AdaptationService) DeleteCustomClass(_ context.Context, req *speechv1pb.DeleteCustomClassRequest) (*emptypb.Empty, error) {
	if err := v1AdaptName("customClasses", req.GetName()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	if _, ok := st.v1Classes[req.GetName()]; !ok {
		return nil, apierror.NotFound("CustomClass %s not found", req.GetName())
	}
	delete(st.v1Classes, req.GetName())
	st.saveV1Adaptation()
	return &emptypb.Empty{}, nil
}

// --- recognition ---

// v1Resolve is c with its adaptation's phrase_set_references replaced by
// the stored phrase sets they name, and each stored custom class a phrase
// names as ${id} or ${resource name} added to its inline custom_classes
// under that name, so v1Config maps it as inline adaptation. c is not
// changed.
func (s *Server) v1Resolve(c *speechv1pb.RecognitionConfig) (*speechv1pb.RecognitionConfig, error) {
	ad := c.GetAdaptation()
	if ad == nil && len(c.GetSpeechContexts()) == 0 {
		return c, nil
	}
	out := proto.Clone(c).(*speechv1pb.RecognitionConfig)
	if out.Adaptation == nil {
		out.Adaptation = &speechv1pb.SpeechAdaptation{}
	}
	a := out.Adaptation
	inline := map[string]bool{}
	for _, cc := range a.GetCustomClasses() {
		if cc.GetCustomClassId() != "" {
			inline[cc.GetCustomClassId()] = true
		}
		if cc.GetName() != "" {
			inline[cc.GetName()] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.v1Store()
	// home is where a ${id} is looked up first: each phrase's own set's
	// location.
	type phrase struct{ value, home string }
	var phrases []phrase
	for _, ref := range a.GetPhraseSetReferences() {
		ps, ok := st.v1PhraseSets[ref]
		if !ok {
			return nil, apierror.InvalidArgument("Unable to find PhraseSet %s from storage", ref)
		}
		home := ref[:strings.Index(ref, "/phraseSets/")]
		for _, ph := range ps.GetPhrases() {
			phrases = append(phrases, phrase{ph.GetValue(), home})
		}
		a.PhraseSets = append(a.PhraseSets, proto.Clone(ps).(*speechv1pb.PhraseSet))
	}
	a.PhraseSetReferences = nil
	for _, ps := range c.GetAdaptation().GetPhraseSets() {
		for _, ph := range ps.GetPhrases() {
			phrases = append(phrases, phrase{value: ph.GetValue()})
		}
	}
	for _, sc := range c.GetSpeechContexts() {
		for _, v := range sc.GetPhrases() {
			phrases = append(phrases, phrase{value: v})
		}
	}
	names := make([]string, 0, len(st.v1Classes))
	for n := range st.v1Classes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, ph := range phrases {
		for _, m := range classRef.FindAllStringSubmatch(ph.value, -1) {
			ref := m[1]
			if ref == "" || inline[ref] {
				continue
			}
			var found *speechv1pb.CustomClass
			if cc, ok := st.v1Classes[ref]; ok {
				found = cc
			} else if ph.home != "" {
				found = st.v1Classes[ph.home+"/customClasses/"+ref]
			}
			for _, n := range names {
				if found == nil && strings.HasSuffix(n, "/customClasses/"+ref) {
					found = st.v1Classes[n]
				}
			}
			if found == nil {
				continue // speechbias.go answers an unknown class
			}
			a.CustomClasses = append(a.CustomClasses, &speechv1pb.CustomClass{CustomClassId: ref, Items: found.GetItems()})
			inline[ref] = true
		}
	}
	return out, nil
}
