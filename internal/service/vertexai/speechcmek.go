package vertexai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// Speech-to-Text v2's customer-managed encryption (#1950), as
// https://docs.cloud.google.com/speech-to-text/v2/docs/encryption and the
// projects.locations.config reference document it (read 2026-11-19):
//
//   - GetConfig and UpdateConfig serve each location's Config
//     (projects/{project}/locations/{location}/config): kms_key_name, a
//     CryptoKey in the endpoint's location ("The Cloud KMS key's location
//     must match the Cloud STT endpoint used"), checked usable and, with IAM
//     enforcement on, usable by the Speech-to-Text service agent
//     service-PROJECT_NUMBER@gcp-sa-speech.iam.gserviceaccount.com, which
//     needs roles/cloudkms.cryptoKeyEncrypterDecrypter; an empty one removes
//     it.
//   - A recognizer, custom class or phrase set created while its location's
//     Config has a key is encrypted with it, and answers kms_key_name and
//     kms_key_version_name (output only: what a request sets is ignored).
//     What Google lists as encrypted is sealed with a data key the key
//     wrapped, and only the sealed form is written to the state files: a
//     recognizer's default_recognition_config (its language codes and
//     adaptation), a phrase set's phrases, a custom class's items.
//   - "Existing resources stay encrypted with their earlier key. A resource
//     is re-encrypted with the new key only when updated through an Update*
//     method", and an Update* after the key is removed decrypts it again.
//   - Reading such a resource (Get, List, an Update*, Recognize and
//     BatchRecognize naming a recognizer, an adaptation naming a phrase set
//     or class) unwraps its data key first, and is FAILED_PRECONDITION while
//     its version is disabled or destroyed. Delete and Undelete change only
//     the resource's state, and still work.
//
// Operations are kept in memory alone here, so they hold nothing to
// encrypt. UNVERIFIED (re-read 2026-11-19:
// https://docs.cloud.google.com/speech-to-text/v2/docs/encryption, which
// gives no text): the error codes and texts.

// configsFile and sealedFile are kept beside recognizers.json.
func (st *speechStore) configsFile() string {
	if st.file == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(st.file), "config.json")
}

func (st *speechStore) sealedFile() string {
	if st.file == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(st.file), "sealed.json")
}

// savedConfig is a Config as config.json keeps it.
type savedConfig struct {
	KMSKeyName string    `json:"kmsKeyName,omitempty"`
	UpdateTime time.Time `json:"updateTime"`
}

// loadCMEK reads the configs and sealed parts, and marks each resource
// with a key version locked until its sealed part is opened.
func (st *speechStore) loadCMEK() {
	st.configs = map[string]savedConfig{}
	st.sealed = readSealed(st.sealedFile())
	st.locked = map[string]bool{}
	if f := st.configsFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			_ = json.Unmarshal(b, &st.configs)
		}
	}
	for n, r := range st.recognizers {
		if r.GetKmsKeyVersionName() != "" {
			st.locked[n] = true
		}
	}
	for n, c := range st.classes {
		if c.GetKmsKeyVersionName() != "" {
			st.locked[n] = true
		}
	}
	for n, p := range st.phraseSets {
		if p.GetKmsKeyVersionName() != "" {
			st.locked[n] = true
		}
	}
}

// saveCMEK writes the configs and the sealed parts.
func (st *speechStore) saveCMEK() {
	writeSealed(st.sealedFile(), st.sealed)
	if f := st.configsFile(); f != "" {
		b, _ := json.Marshal(st.configs)
		_ = os.MkdirAll(filepath.Dir(f), 0o755)
		if os.WriteFile(f+".tmp", b, 0o600) == nil {
			_ = os.Rename(f+".tmp", f)
		}
	}
}

// Stripped copies, as the state files keep a resource with a key.
func strippedRecognizer(r *speechpb.Recognizer) *speechpb.Recognizer {
	if r.GetKmsKeyVersionName() == "" {
		return r
	}
	c := cloneRecognizer(r)
	c.DefaultRecognitionConfig = nil
	return c
}

func strippedClass(x *speechpb.CustomClass) *speechpb.CustomClass {
	if x.GetKmsKeyVersionName() == "" {
		return x
	}
	c := proto.Clone(x).(*speechpb.CustomClass)
	c.Items = nil
	return c
}

func strippedPhraseSet(x *speechpb.PhraseSet) *speechpb.PhraseSet {
	if x.GetKmsKeyVersionName() == "" {
		return x
	}
	c := proto.Clone(x).(*speechpb.PhraseSet)
	c.Phrases = nil
	return c
}

// configName is the Config of name's location.
func configName(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 4 {
		return name
	}
	return strings.Join(p[:4], "/") + "/config"
}

func cmekContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// speechSeal encrypts part of the resource name with its location's
// Config key, and answers the key and the version (both "" with no key).
// With store false (validate_only) nothing is kept. The caller holds s.mu.
func (s *Server) speechSeal(name string, part proto.Message, store bool) (string, string, error) {
	st := s.speechRecognizers()
	key := st.configs[configName(name)].KMSKeyName
	if key == "" {
		if store {
			delete(st.sealed, name)
			delete(st.locked, name)
		}
		return "", "", nil
	}
	b, err := proto.Marshal(part)
	if err != nil {
		return "", "", apierror.Internal(err, "encoding %s", name)
	}
	if b == nil {
		b = []byte{}
	}
	project := strings.Split(name, "/")[1]
	ctx, cancel := cmekContext()
	defer cancel()
	sealed, err := s.keysOf().Seal(ctx, project, cmek.ServiceAgent(project, speechAgentDomain), key, b, []byte(name))
	if err != nil {
		return "", "", cmekError(name, err)
	}
	if store {
		st.sealed[name] = sealed
		delete(st.locked, name)
	}
	return key, sealed.KeyVersion, nil
}

// speechUnseal opens the sealed part of the resource name, encrypted with
// version ("" for none, which needs nothing), and, when the resource was
// loaded without it, gives it to fill. The caller holds s.mu.
func (s *Server) speechUnseal(name, version string, fill func([]byte) error) error {
	if version == "" {
		return nil
	}
	st := s.speechRecognizers()
	sealed, ok := st.sealed[name]
	if !ok {
		return apierror.FailedPrecondition("%s is encrypted with %s, and its sealed data is missing", name, version)
	}
	project := strings.Split(name, "/")[1]
	ctx, cancel := cmekContext()
	defer cancel()
	pt, err := s.openSealed(ctx, project, cmek.ServiceAgent(project, speechAgentDomain), name, name, sealed)
	if err != nil {
		return err
	}
	if st.locked[name] {
		if err := fill(pt); err != nil {
			return apierror.Internal(err, "decoding %s", name)
		}
		delete(st.locked, name)
	}
	return nil
}

// unsealRecognizer opens r, the stored recognizer; the caller holds s.mu.
func (s *Server) unsealRecognizer(r *speechpb.Recognizer) error {
	return s.speechUnseal(r.GetName(), r.GetKmsKeyVersionName(), func(b []byte) error {
		c := &speechpb.RecognitionConfig{}
		if err := proto.Unmarshal(b, c); err != nil {
			return err
		}
		if len(b) > 0 {
			r.DefaultRecognitionConfig = c
		}
		return nil
	})
}

// unsealClass opens c, a stored custom class; the caller holds s.mu.
func (s *Server) unsealClass(c *speechpb.CustomClass) error {
	return s.speechUnseal(c.GetName(), c.GetKmsKeyVersionName(), func(b []byte) error {
		x := &speechpb.CustomClass{}
		if err := proto.Unmarshal(b, x); err != nil {
			return err
		}
		c.Items = x.GetItems()
		return nil
	})
}

// unsealPhraseSet opens p, a stored phrase set; the caller holds s.mu.
func (s *Server) unsealPhraseSet(p *speechpb.PhraseSet) error {
	return s.speechUnseal(p.GetName(), p.GetKmsKeyVersionName(), func(b []byte) error {
		x := &speechpb.PhraseSet{}
		if err := proto.Unmarshal(b, x); err != nil {
			return err
		}
		p.Phrases = x.GetPhrases()
		return nil
	})
}

// sealRecognizer encrypts r with its location's key; the caller holds s.mu.
func (s *Server) sealRecognizer(r *speechpb.Recognizer, store bool) error {
	part := r.GetDefaultRecognitionConfig()
	if part == nil {
		part = &speechpb.RecognitionConfig{}
	}
	var err error
	r.KmsKeyName, r.KmsKeyVersionName, err = s.speechSeal(r.GetName(), part, store)
	return err
}

func (s *Server) sealClass(c *speechpb.CustomClass, store bool) error {
	var err error
	c.KmsKeyName, c.KmsKeyVersionName, err = s.speechSeal(c.GetName(), &speechpb.CustomClass{Items: c.GetItems()}, store)
	return err
}

func (s *Server) sealPhraseSet(p *speechpb.PhraseSet, store bool) error {
	var err error
	p.KmsKeyName, p.KmsKeyVersionName, err = s.speechSeal(p.GetName(), &speechpb.PhraseSet{Phrases: p.GetPhrases()}, store)
	return err
}

// parseConfigName checks projects/{p}/locations/{l}/config.
func parseConfigName(name string) (string, string, error) {
	p := strings.Split(name, "/")
	if len(p) != 5 || p[0] != "projects" || p[2] != "locations" || p[4] != "config" || p[1] == "" || p[3] == "" {
		return "", "", apierror.InvalidArgument("name %q is not projects/{project}/locations/{location}/config", name)
	}
	return p[1], p[3], nil
}

func configProto(name string, c savedConfig) *speechpb.Config {
	out := &speechpb.Config{Name: name, KmsKeyName: c.KMSKeyName}
	if !c.UpdateTime.IsZero() {
		out.UpdateTime = timestamppb.New(c.UpdateTime)
	}
	return out
}

func (v *speechService) GetConfig(_ context.Context, req *speechpb.GetConfigRequest) (*speechpb.Config, error) {
	if _, _, err := parseConfigName(req.GetName()); err != nil {
		return nil, grpcError(err)
	}
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	return configProto(req.GetName(), v.s.speechRecognizers().configs[req.GetName()]), nil
}

func (v *speechService) UpdateConfig(ctx context.Context, req *speechpb.UpdateConfigRequest) (*speechpb.Config, error) {
	in := req.GetConfig()
	project, loc, err := parseConfigName(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "kms_key_name" && p != "kmsKeyName" && p != "*" {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated: kms_key_name is the one field of Config", p))
		}
	}
	key := in.GetKmsKeyName()
	if key != "" {
		if err := cmek.CheckName("config.kms_key_name", key, loc); err != nil {
			return nil, grpcError(cmekError(in.GetName(), err))
		}
		if err := v.s.keysOf().Usable(ctx, project, cmek.ServiceAgent(project, speechAgentDomain), key); err != nil {
			return nil, grpcError(cmekError(in.GetName(), err))
		}
	}
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	st := v.s.speechRecognizers()
	c := savedConfig{KMSKeyName: key, UpdateTime: time.Now().UTC()}
	st.configs[in.GetName()] = c
	st.saveCMEK()
	return configProto(in.GetName(), c), nil
}
