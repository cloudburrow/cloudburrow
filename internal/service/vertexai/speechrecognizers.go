package vertexai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
)

// Speech-to-Text v2's recognizers (#1799), on the local AI endpoint, gRPC
// and JSON (the JSON through the REST transcoder): CreateRecognizer,
// GetRecognizer, ListRecognizers, UpdateRecognizer, DeleteRecognizer,
// UndeleteRecognizer and Recognize, as
// https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers
// documents them (read 2026-11-19), so BigQuery ML's SPEECH_RECOGNIZER
// (internal/bigqueryfront/modeltranscribe.go) can name one, and the official
// cloud.google.com/go/speech client reaches them with the endpoint's
// address. Nothing is sent to Google.
//
//   - A recognizer keeps its display name, annotations and
//     default_recognition_config; the deprecated model and language_codes
//     fields and kms_key_name are UNIMPLEMENTED. recognizer_id is 4 to 63
//     characters of [a-z0-9-], starting with a letter. Recognizers are kept
//     in <state-dir>/speech/recognizers.json.
//   - The methods that return an operation return it done, with
//     OperationMetadata (resource, method, times). validate_only checks and
//     changes nothing. DeleteRecognizer marks a recognizer DELETED, with its
//     delete_time and an expire_time 30 days on; UndeleteRecognizer makes it
//     ACTIVE again; ListRecognizers shows deleted ones with show_deleted.
//     etag is checked when sent (ABORTED when stale).
//   - Recognize on a recognizer merges the request's config over the
//     recognizer's default_recognition_config (a field the request sets
//     replaces the recognizer's, messages merged field by field), as the
//     reference describes config_mask's absence; config_mask itself is
//     UNIMPLEMENTED. The merged config is then the default recognizer's
//     (speech.go): Google's general models on local whisper.cpp, and the same
//     refusals. Recognize's uri reads a gs:// object (speechbatch.go).
//
// UNVERIFIED (re-read 2026-11-19, the reference defines the fields, not
// these): recognizer_id's exact rule (the reference: "4-63 characters, and
// valid characters are /[a-z][0-9]-/"), which locations are served (any),
// uid and etag formats, the 30-day expire_time, the texts of the errors.

var recognizerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,61}[a-z0-9]$`)

// speechStore is the recognizers and where they are kept.
type speechStore struct {
	file        string
	recognizers map[string]*speechpb.Recognizer
	// classes and phraseSets are the custom classes and phrase sets
	// (speechadaptation.go, #1930), kept beside file.
	classes    map[string]*speechpb.CustomClass
	phraseSets map[string]*speechpb.PhraseSet
}

// SetSpeechState keeps Speech-to-Text's recognizers in file, loading those it
// already holds.
func (s *Server) SetSpeechState(file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speech = &speechStore{file: file, recognizers: map[string]*speechpb.Recognizer{}}
	if b, err := os.ReadFile(file); err == nil {
		var p speechpb.ListRecognizersResponse
		if protojson.Unmarshal(b, &p) == nil {
			for _, x := range p.GetRecognizers() {
				s.speech.recognizers[x.GetName()] = x
			}
		}
	}
	s.speech.loadAdaptation()
}

// speechRecognizers returns the store; the caller holds s.mu.
func (s *Server) speechRecognizers() *speechStore {
	if s.speech == nil {
		s.speech = &speechStore{recognizers: map[string]*speechpb.Recognizer{}}
		s.speech.loadAdaptation()
	}
	if s.ops == nil {
		s.ops = lro.NewStore(nil)
	}
	return s.speech
}

func (st *speechStore) save() {
	if st.file == "" {
		return
	}
	p := &speechpb.ListRecognizersResponse{}
	for _, x := range st.recognizers {
		p.Recognizers = append(p.Recognizers, x)
	}
	sort.Slice(p.Recognizers, func(i, j int) bool { return p.Recognizers[i].Name < p.Recognizers[j].Name })
	b, _ := protojson.Marshal(p)
	_ = os.MkdirAll(filepath.Dir(st.file), 0o755)
	tmp := st.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, st.file)
	}
}

// speechService implements speechpb.SpeechServer.
type speechService struct {
	speechpb.UnimplementedSpeechServer
	s *Server
}

func newEtag() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// speechOp returns a done operation for a recognizer method.
func (s *Server) speechOp(name, method string, resp proto.Message) (*longrunningpb.Operation, error) {
	p := strings.Split(name, "/")
	parent := strings.Join(p[:4], "/")
	now := timestamppb.Now()
	op := s.newOp(parent)
	_ = s.ops.SetMetadata(op, &speechpb.OperationMetadata{CreateTime: now, UpdateTime: now, Resource: name,
		Method: "google.cloud.speech.v2.Speech." + method})
	_ = s.ops.Succeed(op, resp)
	got, _ := s.ops.Get(op)
	out, err := opProto(got)
	return out, grpcError(err)
}

// checkRecognizer refuses what a recognizer may not carry.
func checkRecognizer(r *speechpb.Recognizer) error {
	switch {
	case r.GetModel() != "" || len(r.GetLanguageCodes()) > 0:
		return apierror.Unimplemented("Recognizer.model and Recognizer.language_codes are deprecated: set them in default_recognition_config")
	case r.GetKmsKeyName() != "":
		return apierror.Unimplemented("Not implemented here: Recognizer.kms_key_name: nothing is encrypted with a key")
	case len(r.GetDisplayName()) > 63:
		return apierror.InvalidArgument("display_name must be at most 63 characters")
	}
	return nil
}

func cloneRecognizer(r *speechpb.Recognizer) *speechpb.Recognizer {
	return proto.Clone(r).(*speechpb.Recognizer)
}

func (v *speechService) CreateRecognizer(_ context.Context, req *speechpb.CreateRecognizerRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	if !recognizerIDPattern.MatchString(req.GetRecognizerId()) {
		return nil, grpcError(apierror.InvalidArgument("recognizer_id %q must be 4 to 63 characters of lower-case letters, digits and hyphens, "+
			"starting with a letter", req.GetRecognizerId()))
	}
	in := req.GetRecognizer()
	if err := checkRecognizer(in); err != nil {
		return nil, grpcError(err)
	}
	name := req.GetParent() + "/recognizers/" + req.GetRecognizerId()
	now := timestamppb.Now()
	r := &speechpb.Recognizer{Name: name, Uid: newEtag() + newEtag(), DisplayName: in.GetDisplayName(),
		DefaultRecognitionConfig: in.GetDefaultRecognitionConfig(), Annotations: in.GetAnnotations(),
		State: speechpb.Recognizer_ACTIVE, CreateTime: now, UpdateTime: now, Etag: newEtag()}
	v.s.mu.Lock()
	st := v.s.speechRecognizers()
	if _, ok := st.recognizers[name]; ok {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.AlreadyExists("Recognizer %s already exists", name))
	}
	if !req.GetValidateOnly() {
		st.recognizers[name] = r
		st.save()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, "CreateRecognizer", cloneRecognizer(r))
}

// lookup returns a recognizer; the caller holds s.mu.
func (v *speechService) lookup(name string) (*speechpb.Recognizer, error) {
	if _, _, _, err := parseChild("name", "recognizers", name); err != nil {
		return nil, err
	}
	r, ok := v.s.speechRecognizers().recognizers[name]
	if !ok {
		return nil, apierror.NotFound("Unable to find Recognizer %s from storage", name)
	}
	return r, nil
}

func (v *speechService) GetRecognizer(_ context.Context, req *speechpb.GetRecognizerRequest) (*speechpb.Recognizer, error) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	r, err := v.lookup(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return cloneRecognizer(r), nil
}

func (v *speechService) ListRecognizers(_ context.Context, req *speechpb.ListRecognizersRequest) (*speechpb.ListRecognizersResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	v.s.mu.Lock()
	var all []*speechpb.Recognizer
	for _, r := range v.s.speechRecognizers().recognizers {
		if strings.HasPrefix(r.GetName(), req.GetParent()+"/recognizers/") && (req.GetShowDeleted() || r.GetState() != speechpb.Recognizer_DELETED) {
			all = append(all, cloneRecognizer(r))
		}
	}
	v.s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	page, next, err := docAIPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, grpcError(err)
	}
	return &speechpb.ListRecognizersResponse{Recognizers: page, NextPageToken: next}, nil
}

// change applies f to a recognizer under its etag and returns the done
// operation, its response the recognizer as changed.
func (v *speechService) change(name, etag, method string, validateOnly bool, f func(r *speechpb.Recognizer) error) (*longrunningpb.Operation, error) {
	v.s.mu.Lock()
	r, err := v.lookup(name)
	if err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if etag != "" && etag != r.GetEtag() {
		v.s.mu.Unlock()
		return nil, grpcError(apierror.Aborted("The etag %q does not match the recognizer's current etag", etag))
	}
	next := cloneRecognizer(r)
	if err := f(next); err != nil {
		v.s.mu.Unlock()
		return nil, grpcError(err)
	}
	if !validateOnly {
		next.UpdateTime, next.Etag = timestamppb.Now(), newEtag()
		st := v.s.speechRecognizers()
		st.recognizers[name] = next
		st.save()
	}
	v.s.mu.Unlock()
	return v.s.speechOp(name, method, cloneRecognizer(next))
}

func (v *speechService) UpdateRecognizer(_ context.Context, req *speechpb.UpdateRecognizerRequest) (*longrunningpb.Operation, error) {
	in := req.GetRecognizer()
	if err := checkRecognizer(in); err != nil {
		return nil, grpcError(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 || len(paths) == 1 && paths[0] == "*" {
		paths = []string{"display_name", "default_recognition_config", "annotations"}
	}
	return v.change(in.GetName(), in.GetEtag(), "UpdateRecognizer", req.GetValidateOnly(), func(r *speechpb.Recognizer) error {
		if r.GetState() == speechpb.Recognizer_DELETED {
			return apierror.FailedPrecondition("Recognizer %s is deleted", r.GetName())
		}
		for _, p := range paths {
			switch {
			case p == "display_name":
				r.DisplayName = in.GetDisplayName()
			case p == "annotations":
				r.Annotations = in.GetAnnotations()
			case p == "default_recognition_config":
				r.DefaultRecognitionConfig = in.GetDefaultRecognitionConfig()
			case strings.HasPrefix(p, "default_recognition_config."):
				// A field of the config: merged through its JSON.
				merged, err := mergeConfigPath(r.GetDefaultRecognitionConfig(), in.GetDefaultRecognitionConfig(), strings.TrimPrefix(p, "default_recognition_config."))
				if err != nil {
					return err
				}
				r.DefaultRecognitionConfig = merged
			default:
				return apierror.InvalidArgument("update_mask path %q cannot be updated", p)
			}
		}
		return nil
	})
}

// mergeConfigPath sets one field, by its snake_case path, of base from in.
func mergeConfigPath(base, in *speechpb.RecognitionConfig, path string) (*speechpb.RecognitionConfig, error) {
	var b, i map[string]any
	bj, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(base)
	ij, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(in)
	_ = json.Unmarshal(bj, &b)
	_ = json.Unmarshal(ij, &i)
	if b == nil {
		b = map[string]any{}
	}
	keys := strings.Split(path, ".")
	dst, src := b, i
	for _, k := range keys[:len(keys)-1] {
		next, _ := dst[k].(map[string]any)
		if next == nil {
			next = map[string]any{}
			dst[k] = next
		}
		dst = next
		src, _ = src[k].(map[string]any)
	}
	last := keys[len(keys)-1]
	if v, ok := src[last]; ok {
		dst[last] = v
	} else {
		delete(dst, last)
	}
	out := &speechpb.RecognitionConfig{}
	mj, _ := json.Marshal(b)
	if err := protojson.Unmarshal(mj, out); err != nil {
		return nil, apierror.InvalidArgument("update_mask path %q: %v", "default_recognition_config."+path, err)
	}
	return out, nil
}

func (v *speechService) DeleteRecognizer(_ context.Context, req *speechpb.DeleteRecognizerRequest) (*longrunningpb.Operation, error) {
	if req.GetAllowMissing() {
		v.s.mu.Lock()
		_, err := v.lookup(req.GetName())
		v.s.mu.Unlock()
		if err != nil && apierror.From(err).HTTPStatus() == 404 {
			return v.s.speechOp(req.GetName(), "DeleteRecognizer", &speechpb.Recognizer{Name: req.GetName()})
		}
	}
	return v.change(req.GetName(), req.GetEtag(), "DeleteRecognizer", req.GetValidateOnly(), func(r *speechpb.Recognizer) error {
		if r.GetState() == speechpb.Recognizer_DELETED {
			return apierror.FailedPrecondition("Recognizer %s is already deleted", r.GetName())
		}
		now := time.Now()
		r.State, r.DeleteTime, r.ExpireTime = speechpb.Recognizer_DELETED, timestamppb.New(now), timestamppb.New(now.Add(30*24*time.Hour))
		return nil
	})
}

func (v *speechService) UndeleteRecognizer(_ context.Context, req *speechpb.UndeleteRecognizerRequest) (*longrunningpb.Operation, error) {
	return v.change(req.GetName(), req.GetEtag(), "UndeleteRecognizer", req.GetValidateOnly(), func(r *speechpb.Recognizer) error {
		if r.GetState() != speechpb.Recognizer_DELETED {
			return apierror.FailedPrecondition("Recognizer %s is not deleted", r.GetName())
		}
		r.State, r.DeleteTime, r.ExpireTime = speechpb.Recognizer_ACTIVE, nil, nil
		return nil
	})
}

func (v *speechService) Recognize(ctx context.Context, req *speechpb.RecognizeRequest) (*speechpb.RecognizeResponse, error) {
	if _, _, _, err := parseChild("recognizer", "recognizers", req.GetRecognizer()); err != nil {
		return nil, grpcError(err)
	}
	var config json.RawMessage
	if req.GetConfig() != nil {
		config, _ = protojson.Marshal(req.GetConfig())
	}
	config, err := v.s.effectiveConfig(req.GetRecognizer(), config, req.GetConfigMask().GetPaths())
	if err != nil {
		return nil, grpcError(err)
	}
	content := req.GetContent()
	if req.GetUri() != "" {
		data, err := v.s.readAudioURI(ctx, req.GetUri(), recognizeMaxContent)
		if err != nil {
			return nil, grpcError(err)
		}
		content = data
	}
	out, err := v.s.recognizeAudio(ctx, defaultRecognizer(req.GetRecognizer()), config, content)
	if err != nil {
		return nil, grpcError(err)
	}
	b, _ := json.Marshal(out)
	res := &speechpb.RecognizeResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, res); err != nil {
		return nil, grpcError(apierror.Internal(err, "encoding the response: %v", err))
	}
	return res, nil
}

// recognizerConfig is a recognizer's default_recognition_config with the
// request's config merged over it, as JSON.
func (s *Server) recognizerConfig(name string, config json.RawMessage) (json.RawMessage, error) {
	return s.maskedRecognizerConfig(name, config, nil)
}

// effectiveConfig is the config a request recognises with: on the default
// recognizer _, config itself (only mask's fields of it when a mask is
// given); on another, its default_recognition_config with config applied
// through mask (configMask's rule: no mask, every field config sets
// overrides; a mask, only its fields; "*", config replaces the recognizer's
// whole).
func (s *Server) effectiveConfig(name string, config json.RawMessage, mask []string) (json.RawMessage, error) {
	if strings.HasSuffix(name, "/recognizers/_") {
		if len(mask) == 0 || len(mask) == 1 && mask[0] == "*" {
			return config, nil
		}
		return applyConfigMask(map[string]any{}, config, mask)
	}
	return s.maskedRecognizerConfig(name, config, mask)
}

// defaultRecognizer is the default recognizer _ beside a recognizer.
func defaultRecognizer(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 4 {
		return name
	}
	return strings.Join(p[:4], "/") + "/recognizers/_"
}

// applyConfigMask sets each masked field of config (a JSON
// RecognitionConfig) over base, removing it where config does not set it.
func applyConfigMask(base map[string]any, config json.RawMessage, mask []string) (json.RawMessage, error) {
	var c map[string]any
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &c); err != nil {
			return nil, apierror.InvalidArgument("config is not a RecognitionConfig")
		}
	}
	c = normaliseKeys(c)
	if len(mask) == 1 && mask[0] == "*" {
		return json.Marshal(c)
	}
	for _, path := range mask {
		keys := strings.Split(path, ".")
		for i, k := range keys {
			keys[i] = camelKey(k)
		}
		dst, src := base, c
		for _, k := range keys[:len(keys)-1] {
			next, _ := dst[k].(map[string]any)
			if next == nil {
				next = map[string]any{}
				dst[k] = next
			}
			dst = next
			src, _ = src[k].(map[string]any)
		}
		last := keys[len(keys)-1]
		if !knownConfigPath(keys) {
			return nil, apierror.InvalidArgument("config_mask path %q is not a field of RecognitionConfig", path)
		}
		if v, ok := src[last]; ok {
			dst[last] = v
		} else {
			delete(dst, last)
		}
	}
	return json.Marshal(base)
}

// knownConfigPath reports whether a camelCase path names a field of
// RecognitionConfig.
func knownConfigPath(keys []string) bool {
	md := (&speechpb.RecognitionConfig{}).ProtoReflect().Descriptor()
	for i, k := range keys {
		f := md.Fields().ByJSONName(k)
		if f == nil {
			return false
		}
		if i < len(keys)-1 {
			if f.Message() == nil {
				return false
			}
			md = f.Message()
		}
	}
	return true
}

func camelKey(k string) string {
	parts := strings.Split(k, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func (s *Server) maskedRecognizerConfig(name string, config json.RawMessage, mask []string) (json.RawMessage, error) {
	s.mu.Lock()
	r, ok := s.speechRecognizers().recognizers[name]
	var base *speechpb.RecognitionConfig
	if ok {
		base = r.GetDefaultRecognitionConfig()
	}
	deleted := ok && r.GetState() == speechpb.Recognizer_DELETED
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("Unable to find Recognizer %s from storage", name)
	}
	if deleted {
		return nil, apierror.FailedPrecondition("Recognizer %s is deleted", name)
	}
	var b map[string]any
	if base != nil {
		bj, _ := protojson.Marshal(base)
		_ = json.Unmarshal(bj, &b)
	}
	if b == nil {
		b = map[string]any{}
	}
	if len(mask) > 0 {
		return applyConfigMask(b, config, mask)
	}
	if len(config) > 0 && string(config) != "null" {
		var c map[string]any
		if err := json.Unmarshal(config, &c); err != nil {
			return nil, apierror.InvalidArgument("RecognizeRequest.config is not a RecognitionConfig")
		}
		mergeJSON(b, normaliseKeys(c))
	}
	return json.Marshal(b)
}

// normaliseKeys rewrites snake_case keys as protojson's camelCase, so a
// config written either way merges over a stored one.
func normaliseKeys(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for k, e := range v {
		if m, ok := e.(map[string]any); ok {
			e = normaliseKeys(m)
		}
		parts := strings.Split(k, "_")
		for i := 1; i < len(parts); i++ {
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
		out[strings.Join(parts, "")] = e
	}
	return out
}

// mergeJSON sets each field of src over dst, merging objects.
func mergeJSON(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeJSON(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
