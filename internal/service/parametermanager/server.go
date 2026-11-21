// Package parametermanager serves Parameter Manager
// (parametermanager.googleapis.com v1, #2054): parameters, global and
// regional, in the UNFORMATTED, JSON or YAML format; their versions, which
// can be disabled; and RenderParameterVersion, which replaces each
// __REF__("//secretmanager.googleapis.com/...") in a JSON or YAML version
// with the secret version's payload, read through the instance's Secret
// Manager as the parameter's own identity.
//
// As Google documents it (docs.cloud.google.com/secret-manager/
// parameter-manager/docs, read 2026-11-20):
//
//   - a version's payload is at most 1 MiB (Secret Manager quotas page,
//     "Parameter Manager content limit");
//   - a JSON or YAML parameter's versions must parse as that format;
//   - a parameter can be deleted only when it has no versions, enabled or
//     disabled ("Delete a parameter");
//   - a disabled version is metadata only: its payload is never returned,
//     and rendering it fails (the proto's ParameterVersion.disabled);
//   - each parameter has a principal,
//     principal://parametermanager.googleapis.com/projects/NUMBER/uid/locations/LOCATION/parameters/UID,
//     which must hold roles/secretmanager.secretAccessor on each secret its
//     versions reference ("Reference secrets in a parameter");
//   - a parameter's customer-managed key is a Cloud KMS CryptoKey in the
//     parameter's location, used by the Parameter Manager service agent
//     (service-NUMBER@gcp-sa-pm.iam.gserviceaccount.com) to encrypt new
//     versions; existing versions are not re-encrypted when the key
//     changes, and a version cannot be read while its key cannot be used
//     ("Enable customer-managed encryption keys").
//
// The secret accessor check, like every IAM check of this instance, is made
// with `up --iam-enforcement=on`; without it IAM is recorded and every
// identity may read every secret, as on the rest of the instance (ADR-0006).
//
// Templates (in preview) are templates.go's; a parameter's tags, bound
// through Resource Manager, tags.go's; the request quotas, quotas.go's.
package parametermanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/ratequota"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

const (
	// MaxPayloadBytes is a version's payload limit, 1 MiB.
	MaxPayloadBytes = 1 << 20
	// AgentDomain is the Parameter Manager service agent's domain.
	AgentDomain     = "gcp-sa-pm.iam.gserviceaccount.com"
	maxLabels       = 64
	defaultPageSize = 100
)

// Options are the server's collaborators.
type Options struct {
	// AccessSecret reads a secret version's payload (a name as
	// AccessSecretVersion takes it) from the instance's Secret Manager; nil
	// when Secret Manager is not served, and then a reference fails.
	AccessSecret func(ctx context.Context, name string) ([]byte, error)
	// CheckSecretAccess checks principal holds secretmanager.versions.access
	// on the secret version name; nil checks nothing (IAM enforcement off).
	CheckSecretAccess func(ctx context.Context, principal, name string) error
	// Keys are the customer-managed keys (the instance's Cloud KMS).
	Keys cmek.Keys
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// CheckCaller checks the caller holds permission on resource, for the
	// second resource a call names (RenderTemplateVersion's parameter
	// version); nil checks nothing (IAM enforcement off).
	CheckCaller func(ctx context.Context, permission, resource string) error
	// Tags binds a parameter's tags through Resource Manager; nil (or one
	// answering nil) refuses a parameter with tags UNIMPLEMENTED.
	Tags func() TagBinder
	// Quotas are the instance's request quotas; nil enforces none.
	Quotas *ratequota.Limiter
}

// Server serves ParameterManager.
type Server struct {
	pmpb.UnimplementedParameterManagerServer

	opts  Options
	state string

	mu        sync.Mutex
	params    map[string]*paramRecord
	templates map[string]*templateRecord
}

type paramRecord struct {
	P        *pmpb.Parameter
	UID      string
	Versions map[string]*versionRecord
}

type versionRecord struct {
	V *pmpb.ParameterVersion
	// Data is the payload of a version without a customer-managed key.
	Data []byte
	// Sealed is the payload of a version with one.
	Sealed *cmek.Sealed
}

// NewServer loads the records in state ("" keeps them in memory).
func NewServer(state string, opts Options) (*Server, error) {
	s := &Server{opts: opts, state: state, params: map[string]*paramRecord{}, templates: map[string]*templateRecord{}}
	opts.Quotas.Register(Quotas()...)
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

var (
	parentRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	paramRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/parameters/([^/]+)$`)
	versionRE = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/parameters/([^/]+))/versions/([^/]+)$`)
	// idRE is a parameter's or a version's ID: letters, digits, hyphens and
	// underscores, at most 63 characters.
	idRE         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	labelKeyRE   = regexp.MustCompile(`^\p{Ll}[\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValueRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
	// refRE is a secret reference: __REF__(//secretmanager.googleapis.com/NAME),
	// the name quoted with " or ' (escaped or not, as it is inside a JSON
	// string) or bare.
	refRE = regexp.MustCompile(`__REF__\(\s*(?:\\?["'])?//secretmanager\.googleapis\.com/([^"'\\)\s]*)(?:\\?["'])?\s*\)`)
	// secretVersionRE is the name a reference must give: a secret version,
	// global or regional.
	secretVersionRE = regexp.MustCompile(`^projects/[^/]+/(?:locations/[^/]+/)?secrets/[^/]+/versions/[^/]+$`)
)

// validLocation is "global" or a region Parameter Manager serves.
func validLocation(loc string) bool {
	if loc == "global" {
		return true
	}
	for _, r := range locations.Regions {
		if r == loc {
			return true
		}
	}
	return false
}

func checkParent(parent string) (project, location string, err error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("location %q is not a Parameter Manager location", m[2])
	}
	return m[1], m[2], nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return apierror.InvalidArgument("parameter.labels has %d labels: at most %d are allowed", len(labels), maxLabels)
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return apierror.InvalidArgument("parameter.labels key %q is not a valid label key", k)
		}
		if !labelValueRE.MatchString(v) {
			return apierror.InvalidArgument("parameter.labels value for %q is not a valid label value", k)
		}
	}
	return nil
}

// checkKMSKey checks a parameter's key names a CryptoKey in its location.
func checkKMSKey(key, location string) error {
	if key == "" {
		return nil
	}
	loc, ok := cmek.Location(key)
	if !ok {
		return apierror.InvalidArgument("parameter.kms_key %q must be projects/*/locations/*/keyRings/*/cryptoKeys/*", key)
	}
	if loc != location {
		return apierror.InvalidArgument("parameter.kms_key %q is in location %q: a parameter's key must be in the parameter's location %q", key, loc, location)
	}
	return nil
}

func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Principal is a parameter's own identity, by UID.
func Principal(project, location, uid string) string {
	return "principal://parametermanager.googleapis.com/projects/" + strconv.FormatInt(resource.ProjectNumber(project), 10) +
		"/uid/locations/" + location + "/parameters/" + uid
}

func namePrincipal(project, location, id string) string {
	return "principal://parametermanager.googleapis.com/projects/" + strconv.FormatInt(resource.ProjectNumber(project), 10) +
		"/name/locations/" + location + "/parameters/" + id
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

// CreateParameter creates a parameter, binding its tags.
func (s *Server) CreateParameter(ctx context.Context, req *pmpb.CreateParameterRequest) (*pmpb.Parameter, error) {
	project, location, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.charge(ctx, quotaWrite, project); err != nil {
		return nil, err
	}
	id := req.GetParameterId()
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("parameter_id %q must be 1 to 63 letters, digits, hyphens or underscores", id)
	}
	in := req.GetParameter()
	if in == nil {
		in = &pmpb.Parameter{}
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if err := checkKMSKey(in.GetKmsKey(), location); err != nil {
		return nil, err
	}
	format := in.GetFormat()
	if format == pmpb.ParameterFormat_PARAMETER_FORMAT_UNSPECIFIED {
		format = pmpb.ParameterFormat_UNFORMATTED
	}
	if _, ok := pmpb.ParameterFormat_name[int32(format)]; !ok {
		return nil, apierror.InvalidArgument("parameter.format %d is not a ParameterFormat", format)
	}
	name := req.GetParent() + "/parameters/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.params[name]; ok {
		return nil, apierror.AlreadyExists("Parameter %s already exists", name)
	}
	if err := s.checkTags(name, in.GetTags()); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	uid := newUID()
	p := &pmpb.Parameter{Name: name, CreateTime: now, UpdateTime: now, Labels: in.GetLabels(), Format: format,
		PolicyMember: &iampb.ResourcePolicyMember{IamPolicyUidPrincipal: Principal(project, location, uid),
			IamPolicyNamePrincipal: namePrincipal(project, location, id)}}
	if in.KmsKey != nil && in.GetKmsKey() != "" {
		k := in.GetKmsKey()
		p.KmsKey = &k
	}
	s.params[name] = &paramRecord{P: p, UID: uid, Versions: map[string]*versionRecord{}}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.bindTags(name, in.GetTags()); err != nil {
		delete(s.params, name)
		return nil, errors.Join(err, s.save())
	}
	return clone(p), nil
}

// GetParameter reads a parameter. Its tags are input only.
func (s *Server) GetParameter(ctx context.Context, req *pmpb.GetParameterRequest) (*pmpb.Parameter, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.param(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(rec.P), nil
}

// param is the named parameter. The caller holds mu.
func (s *Server) param(name string) (*paramRecord, error) {
	if !paramRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/parameters/{parameter}", name)
	}
	rec := s.params[name]
	if rec == nil {
		return nil, apierror.NotFound("Parameter %s not found", name)
	}
	return rec, nil
}

// ListParameters lists a location's parameters.
func (s *Server) ListParameters(ctx context.Context, req *pmpb.ListParametersRequest) (*pmpb.ListParametersResponse, error) {
	project, _, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.charge(ctx, quotaRead, project); err != nil {
		return nil, err
	}
	prefix := req.GetParent() + "/parameters/"
	s.mu.Lock()
	var items []*pmpb.Parameter
	for _, name := range sortedKeys(s.params) {
		if strings.HasPrefix(name, prefix) {
			items = append(items, clone(s.params[name].P))
		}
	}
	s.mu.Unlock()
	items, err = aipfilter.List(req.GetFilter(), req.GetOrderBy(), items)
	if err != nil {
		return nil, err
	}
	page, next, err := pageOf("parameters:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), items,
		func(p *pmpb.Parameter) string { return p.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &pmpb.ListParametersResponse{Parameters: page, NextPageToken: next}, nil
}

// UpdateParameter changes a parameter's labels or key. Its format is
// immutable.
func (s *Server) UpdateParameter(ctx context.Context, req *pmpb.UpdateParameterRequest) (*pmpb.Parameter, error) {
	in := req.GetParameter()
	if in == nil {
		return nil, apierror.InvalidArgument("parameter is required")
	}
	if err := s.chargeName(ctx, quotaWrite, in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.param(in.GetName())
	if err != nil {
		return nil, err
	}
	m := paramRE.FindStringSubmatch(in.GetName())
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		// No mask: every mutable field the request sets.
		paths = []string{"labels"}
		if in.KmsKey != nil {
			paths = append(paths, "kms_key")
		}
		if in.GetFormat() != pmpb.ParameterFormat_PARAMETER_FORMAT_UNSPECIFIED {
			paths = append(paths, "format")
		}
	}
	next := clone(rec.P)
	for _, p := range paths {
		switch snake(p) {
		case "labels":
			if err := validateLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "kms_key":
			if err := checkKMSKey(in.GetKmsKey(), m[2]); err != nil {
				return nil, err
			}
			if in.GetKmsKey() == "" {
				next.KmsKey = nil
			} else {
				k := in.GetKmsKey()
				next.KmsKey = &k
			}
		case "format":
			if in.GetFormat() != rec.P.GetFormat() {
				return nil, apierror.InvalidArgument("parameter.format is immutable: the parameter's format is %s", rec.P.GetFormat())
			}
		case "name", "create_time", "update_time", "policy_member":
			return nil, apierror.InvalidArgument("update_mask path %q is an output-only field", p)
		case "tags":
			return nil, apierror.InvalidArgument("update_mask path %q: a parameter's tags are immutable, set when it is created; change its tag bindings through Resource Manager", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of Parameter that can be updated", p)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	rec.P = next
	return clone(next), apierror.Wrap(s.save())
}

func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			b.WriteRune(r + 'a' - 'A')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// DeleteParameter deletes a parameter that has no versions.
func (s *Server) DeleteParameter(ctx context.Context, req *pmpb.DeleteParameterRequest) (*emptypb.Empty, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.param(req.GetName())
	if err != nil {
		return nil, err
	}
	if n := len(rec.Versions); n > 0 {
		return nil, apierror.FailedPrecondition("Parameter %s has %d versions: delete its versions, enabled or disabled, before the parameter", req.GetName(), n)
	}
	delete(s.params, req.GetName())
	s.unbindTags(req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(s.save())
}

// version is the named version and its parameter. The caller holds mu.
func (s *Server) version(name string) (*paramRecord, *versionRecord, error) {
	m := versionRE.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/parameters/{parameter}/versions/{version}", name)
	}
	rec := s.params[m[1]]
	if rec == nil {
		return nil, nil, apierror.NotFound("Parameter %s not found", m[1])
	}
	v := rec.Versions[m[5]]
	if v == nil {
		return nil, nil, apierror.NotFound("ParameterVersion %s not found", name)
	}
	return rec, v, nil
}

// validatePayload checks data parses as format and that every secret
// reference names a secret version.
func validatePayload(format pmpb.ParameterFormat, data []byte) error {
	if len(data) > MaxPayloadBytes {
		return apierror.InvalidArgument("parameter_version.payload.data is %d bytes: a parameter version's payload can be at most 1 MiB (%d bytes)", len(data), MaxPayloadBytes)
	}
	switch format {
	case pmpb.ParameterFormat_JSON:
		if !json.Valid(data) {
			return apierror.InvalidArgument("parameter_version.payload.data is not valid JSON, and the parameter's format is JSON")
		}
	case pmpb.ParameterFormat_YAML:
		var v any
		if err := yaml.Unmarshal(data, &v); err != nil {
			return apierror.InvalidArgument("parameter_version.payload.data is not valid YAML, and the parameter's format is YAML: %v", err)
		}
	default:
		return nil
	}
	for _, m := range refRE.FindAllSubmatch(data, -1) {
		if !secretVersionRE.Match(m[1]) {
			return apierror.InvalidArgument("secret reference %q must name a secret version: //secretmanager.googleapis.com/projects/{project}/secrets/{secret}/versions/{version}, or a regional secret's under locations/{location}", m[0])
		}
	}
	return nil
}

// CreateParameterVersion adds a version, encrypted with the parameter's
// customer-managed key when it has one.
func (s *Server) CreateParameterVersion(ctx context.Context, req *pmpb.CreateParameterVersionRequest) (*pmpb.ParameterVersion, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetParent()); err != nil {
		return nil, err
	}
	id := req.GetParameterVersionId()
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("parameter_version_id %q must be 1 to 63 letters, digits, hyphens or underscores", id)
	}
	in := req.GetParameterVersion()
	if in.GetPayload() == nil {
		return nil, apierror.InvalidArgument("parameter_version.payload is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.param(req.GetParent())
	if err != nil {
		return nil, err
	}
	if _, ok := rec.Versions[id]; ok {
		return nil, apierror.AlreadyExists("ParameterVersion %s/versions/%s already exists", req.GetParent(), id)
	}
	data := in.GetPayload().GetData()
	if err := validatePayload(rec.P.GetFormat(), data); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/versions/" + id
	now := timestamppb.New(s.now())
	v := &pmpb.ParameterVersion{Name: name, CreateTime: now, UpdateTime: now, Disabled: in.GetDisabled()}
	vr := &versionRecord{V: v}
	if key := rec.P.GetKmsKey(); key != "" {
		project := paramRE.FindStringSubmatch(req.GetParent())[1]
		sealed, err := s.opts.Keys.Seal(ctx, project, cmek.ServiceAgent(project, AgentDomain), key, data, []byte(name))
		if err != nil {
			return nil, keyError(err)
		}
		vr.Sealed = &sealed
		kv := sealed.KeyVersion
		v.KmsKeyVersion = &kv
	} else {
		vr.Data = bytes.Clone(data)
	}
	rec.Versions[id] = vr
	return s.view(vr, pmpb.View_BASIC, nil), apierror.Wrap(s.save())
}

// keyError is a customer-managed key's refusal, with Google's code.
func keyError(err error) error {
	if e := cmek.As(err); e != nil {
		return &apierror.Error{Code: e.Code(), Message: "the parameter's customer-managed key cannot be used: " + e.Error()}
	}
	return err
}

// payload is a version's plaintext. The caller holds mu.
func (s *Server) payload(ctx context.Context, rec *paramRecord, vr *versionRecord) ([]byte, error) {
	if vr.Sealed == nil {
		return vr.Data, nil
	}
	project := paramRE.FindStringSubmatch(rec.P.GetName())[1]
	b, err := s.opts.Keys.Open(ctx, project, cmek.ServiceAgent(project, AgentDomain), *vr.Sealed, []byte(vr.V.GetName()))
	if err != nil {
		return nil, keyError(err)
	}
	return b, nil
}

// view is a version as answered: with its payload in the FULL view of an
// enabled version.
func (s *Server) view(vr *versionRecord, view pmpb.View, data []byte) *pmpb.ParameterVersion {
	out := clone(vr.V)
	if view != pmpb.View_BASIC && !vr.V.GetDisabled() {
		out.Payload = &pmpb.ParameterVersionPayload{Data: data}
	}
	return out
}

// GetParameterVersion reads a version; the FULL view (the default) carries
// the payload of an enabled version.
func (s *Server) GetParameterVersion(ctx context.Context, req *pmpb.GetParameterVersionRequest) (*pmpb.ParameterVersion, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, vr, err := s.version(req.GetName())
	if err != nil {
		return nil, err
	}
	view := req.GetView()
	if view == pmpb.View_VIEW_UNSPECIFIED {
		view = pmpb.View_FULL
	}
	var data []byte
	if view == pmpb.View_FULL && !vr.V.GetDisabled() {
		if data, err = s.payload(ctx, rec, vr); err != nil {
			return nil, err
		}
	}
	return s.view(vr, view, data), nil
}

// ListParameterVersions lists a parameter's versions, without payloads.
func (s *Server) ListParameterVersions(ctx context.Context, req *pmpb.ListParameterVersionsRequest) (*pmpb.ListParameterVersionsResponse, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	rec, err := s.param(req.GetParent())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var items []*pmpb.ParameterVersion
	for _, id := range sortedKeys(rec.Versions) {
		items = append(items, s.view(rec.Versions[id], pmpb.View_BASIC, nil))
	}
	s.mu.Unlock()
	items, err = aipfilter.List(req.GetFilter(), req.GetOrderBy(), items)
	if err != nil {
		return nil, err
	}
	page, next, err := pageOf("versions:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), items,
		func(v *pmpb.ParameterVersion) string { return v.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &pmpb.ListParameterVersionsResponse{ParameterVersions: page, NextPageToken: next}, nil
}

// UpdateParameterVersion enables or disables a version: disabled is its
// one mutable field.
func (s *Server) UpdateParameterVersion(ctx context.Context, req *pmpb.UpdateParameterVersionRequest) (*pmpb.ParameterVersion, error) {
	in := req.GetParameterVersion()
	if in == nil {
		return nil, apierror.InvalidArgument("parameter_version is required")
	}
	if err := s.chargeName(ctx, quotaWrite, in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, vr, err := s.version(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"disabled"}
	}
	next := clone(vr.V)
	for _, p := range paths {
		switch snake(p) {
		case "disabled":
			next.Disabled = in.GetDisabled()
		case "payload", "payload.data":
			return nil, apierror.InvalidArgument("update_mask path %q: a parameter version's payload is immutable; create a new version", p)
		case "name", "create_time", "update_time", "kms_key_version", "checksum_source":
			return nil, apierror.InvalidArgument("update_mask path %q is an output-only field", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of ParameterVersion that can be updated", p)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	vr.V = next
	return s.view(vr, pmpb.View_BASIC, nil), apierror.Wrap(s.save())
}

// DeleteParameterVersion deletes a version.
func (s *Server) DeleteParameterVersion(ctx context.Context, req *pmpb.DeleteParameterVersionRequest) (*emptypb.Empty, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, _, err := s.version(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(rec.Versions, versionRE.FindStringSubmatch(req.GetName())[5])
	return &emptypb.Empty{}, apierror.Wrap(s.save())
}

// RenderParameterVersion answers a version's payload and, for a JSON or
// YAML parameter, the payload with each secret reference replaced by the
// secret version's payload, read as the parameter's principal.
func (s *Server) RenderParameterVersion(ctx context.Context, req *pmpb.RenderParameterVersionRequest) (*pmpb.RenderParameterVersionResponse, error) {
	if err := s.chargeName(ctx, quotaRender, req.GetName()); err != nil {
		return nil, err
	}
	return s.renderVersion(ctx, req.GetName())
}

// renderVersion is RenderParameterVersion's answer for the version name,
// charged to no quota.
func (s *Server) renderVersion(ctx context.Context, name string) (*pmpb.RenderParameterVersionResponse, error) {
	req := &pmpb.RenderParameterVersionRequest{Name: name}
	s.mu.Lock()
	rec, vr, err := s.version(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if vr.V.GetDisabled() {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("ParameterVersion %s is disabled: a disabled version cannot be rendered", req.GetName())
	}
	data, err := s.payload(ctx, rec, vr)
	format := rec.P.GetFormat()
	m := paramRE.FindStringSubmatch(rec.P.GetName())
	principal := Principal(m[1], m[2], rec.UID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &pmpb.RenderParameterVersionResponse{ParameterVersion: req.GetName(),
		Payload: &pmpb.ParameterVersionPayload{Data: data}, RenderedPayload: data}
	if format != pmpb.ParameterFormat_JSON && format != pmpb.ParameterFormat_YAML {
		return out, nil
	}
	rendered, err := s.render(ctx, format, principal, data)
	if err != nil {
		return nil, err
	}
	out.RenderedPayload = rendered
	return out, nil
}

// render replaces each reference in data.
func (s *Server) render(ctx context.Context, format pmpb.ParameterFormat, principal string, data []byte) ([]byte, error) {
	var failed error
	out := refRE.ReplaceAllFunc(data, func(ref []byte) []byte {
		if failed != nil {
			return ref
		}
		name := string(refRE.FindSubmatch(ref)[1])
		value, err := s.resolve(ctx, principal, name)
		if err != nil {
			failed = err
			return ref
		}
		if format == pmpb.ParameterFormat_JSON {
			// Inside a JSON string: the value escaped as one, without its
			// quotes.
			b, _ := json.Marshal(string(value))
			return b[1 : len(b)-1]
		}
		return value
	})
	if failed != nil {
		return nil, failed
	}
	return out, nil
}

// resolve reads one referenced secret version as principal.
func (s *Server) resolve(ctx context.Context, principal, name string) ([]byte, error) {
	if !secretVersionRE.MatchString(name) {
		return nil, apierror.InvalidArgument("secret reference %q must name a secret version", name)
	}
	if s.opts.AccessSecret == nil {
		return nil, apierror.FailedPrecondition("secret reference %s: Secret Manager is not served by this CloudBurrow instance: start it with secretmanager in --services", name)
	}
	if s.opts.CheckSecretAccess != nil {
		if err := s.opts.CheckSecretAccess(ctx, principal, name); err != nil {
			return nil, err
		}
	}
	b, err := s.opts.AccessSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ResetProject forgets one project's parameters and templates, or every
// project's for "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.params {
		if project == "" || strings.HasPrefix(name, "projects/"+project+"/") {
			delete(s.params, name)
			s.unbindTags(name)
		}
	}
	for name := range s.templates {
		if project == "" || strings.HasPrefix(name, "projects/"+project+"/") {
			delete(s.templates, name)
		}
	}
	return s.save()
}

// pageOf pages items in their order, by their keys.
func pageOf[T any](scope string, items []T, key func(T) string, token string, size int) ([]T, string, error) {
	if size <= 0 {
		size = defaultPageSize
	}
	keys := make([]string, len(items))
	byKey := map[string]T{}
	for i, it := range items {
		keys[i] = fmt.Sprintf("%08d\x00%s", i, key(it))
		byKey[keys[i]] = it
	}
	page, next, err := paging.Page(scope, keys, token, size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token")
	}
	out := make([]T, 0, len(page))
	for _, k := range page {
		out = append(out, byKey[k])
	}
	return out, next, nil
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- persistence ----

type diskParam struct {
	Parameter json.RawMessage `json:"parameter"`
	UID       string          `json:"uid"`
	Versions  []diskVersion   `json:"versions,omitempty"`
}

type diskVersion struct {
	Version json.RawMessage `json:"version"`
	Data    []byte          `json:"data,omitempty"`
	Sealed  *cmek.Sealed    `json:"sealed,omitempty"`
}

type diskTemplate struct {
	Template json.RawMessage `json:"template"`
	Versions []diskVersion   `json:"versions,omitempty"`
}

// diskState is the state file: before templates (#2063) it was the
// parameters' array alone, which load still reads.
type diskState struct {
	Parameters []diskParam    `json:"parameters"`
	Templates  []diskTemplate `json:"templates,omitempty"`
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st diskState
	if b = bytes.TrimSpace(b); len(b) > 0 && b[0] == '[' {
		err = json.Unmarshal(b, &st.Parameters)
	} else {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		return fmt.Errorf("parameter manager state %s: %w", s.state, err)
	}
	for _, dt := range st.Templates {
		rec := &templateRecord{T: &pmpb.Template{}, Versions: map[string]*templateVersionRecord{}}
		if err := protojson.Unmarshal(dt.Template, rec.T); err != nil {
			return err
		}
		for _, dv := range dt.Versions {
			vr := &templateVersionRecord{V: &pmpb.TemplateVersion{}, Data: dv.Data}
			if err := protojson.Unmarshal(dv.Version, vr.V); err != nil {
				return err
			}
			m := templateVersionRE.FindStringSubmatch(vr.V.GetName())
			if m == nil {
				return fmt.Errorf("parameter manager state %s: template version %q", s.state, vr.V.GetName())
			}
			rec.Versions[m[5]] = vr
		}
		s.templates[rec.T.GetName()] = rec
	}
	for _, dp := range st.Parameters {
		rec := &paramRecord{P: &pmpb.Parameter{}, UID: dp.UID, Versions: map[string]*versionRecord{}}
		if err := protojson.Unmarshal(dp.Parameter, rec.P); err != nil {
			return err
		}
		for _, dv := range dp.Versions {
			vr := &versionRecord{V: &pmpb.ParameterVersion{}, Data: dv.Data, Sealed: dv.Sealed}
			if err := protojson.Unmarshal(dv.Version, vr.V); err != nil {
				return err
			}
			m := versionRE.FindStringSubmatch(vr.V.GetName())
			if m == nil {
				return fmt.Errorf("parameter manager state %s: version %q", s.state, vr.V.GetName())
			}
			rec.Versions[m[5]] = vr
		}
		s.params[rec.P.GetName()] = rec
	}
	return nil
}

// save writes the records. The caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	st := diskState{Parameters: []diskParam{}}
	for _, name := range sortedKeys(s.templates) {
		rec := s.templates[name]
		tb, err := protojson.Marshal(rec.T)
		if err != nil {
			return err
		}
		dt := diskTemplate{Template: tb}
		for _, id := range sortedKeys(rec.Versions) {
			vb, err := protojson.Marshal(rec.Versions[id].V)
			if err != nil {
				return err
			}
			dt.Versions = append(dt.Versions, diskVersion{Version: vb, Data: rec.Versions[id].Data})
		}
		st.Templates = append(st.Templates, dt)
	}
	for _, name := range sortedKeys(s.params) {
		rec := s.params[name]
		pb, err := protojson.Marshal(rec.P)
		if err != nil {
			return err
		}
		dp := diskParam{Parameter: pb, UID: rec.UID}
		for _, id := range sortedKeys(rec.Versions) {
			vr := rec.Versions[id]
			vb, err := protojson.Marshal(vr.V)
			if err != nil {
				return err
			}
			dp.Versions = append(dp.Versions, diskVersion{Version: vb, Data: vr.Data, Sealed: vr.Sealed})
		}
		st.Parameters = append(st.Parameters, dp)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}
