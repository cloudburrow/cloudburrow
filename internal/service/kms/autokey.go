package kms

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Cloud KMS Autokey (google.cloud.kms.v1.Autokey and AutokeyAdmin, #1452),
// as autokey.proto and autokey_admin.proto document it:
//
//   - An AutokeyConfig on a folder (or a project) names the key project
//     where keys are made, or says to make them in the resource's own
//     project (RESOURCE_PROJECT), or turns Autokey off below it (DISABLED).
//     ShowEffectiveAutokeyConfig resolves a project's: its own config, then
//     its folders' nearest first, through the Resource Manager hierarchy.
//   - CreateKeyHandle provisions a CryptoKey for one resource type in the
//     key project and the handle's location, in the key ring "autokey"
//     (made when missing), and returns a done long-running operation whose
//     response is the KeyHandle. The key is what Google documents Autokey
//     making: ENCRYPT_DECRYPT, HSM (served without an HSM, protection.go),
//     rotated every 365 days.
//
// The CryptoKey ID follows Google's naming,
// PROJECT_NUMBER-SERVICE_SHORT_NAME-RESOURCE_TYPE_DESCRIPTOR_NAME-RANDOM_HEX
// (https://cloud.google.com/kms/docs/autokey-overview, read 2026-10-17), with
// the key project's number. Which resource types Google
// accepts is a published list; any {SERVICE}.googleapis.com/{TYPE} is
// accepted here. IAM grants on the new key, and the Cloud KMS service
// agent's permissions on the key project, are not modelled: IAM is stored,
// never enforced (ADR-0006). The codes for refused requests are UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/autokey.proto

const (
	autokeyConfigPrefix = "kms/autokeyconfig/"
	keyHandlePrefix     = "kms/keyhandle/"
	operationPrefix     = "kms/operation/"
	// autokeyRing is the key ring Autokey keys are made in.
	autokeyRing = "autokey"
	// autokeyRotation is how often an Autokey key rotates.
	autokeyRotation = 365 * 24 * time.Hour
)

var (
	resourceTypeRE = regexp.MustCompile(`^([a-z][a-z0-9-]*)\.googleapis\.com/([A-Za-z][A-Za-z0-9]*)$`)
	keyHandleIDRE  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
	projectRE      = regexp.MustCompile(`^projects/[a-z0-9][a-z0-9-]{0,62}$`)
)

// Hierarchy is the Resource Manager hierarchy Autokey resolves configs
// through. Without one, only a project's own AutokeyConfig applies.
type Hierarchy interface {
	// Ancestors returns a project's ancestors, nearest first, as
	// "folders/{n}" and "organizations/{n}".
	Ancestors(project string) ([]string, error)
	// FolderExists reports whether name ("folders/{n}") is a folder.
	FolderExists(name string) error
}

// UseHierarchy makes Autokey resolve configs through h. Call it before
// serving.
func (s *Server) UseHierarchy(h Hierarchy) { s.hierarchy = h }

type autokeyConfig struct {
	Name       string `json:"name"`
	KeyProject string `json:"keyProject,omitempty"`
	Mode       string `json:"keyProjectResolutionMode,omitempty"`
}

func (c autokeyConfig) mode() kmspb.AutokeyConfig_KeyProjectResolutionMode {
	return kmspb.AutokeyConfig_KeyProjectResolutionMode(kmspb.AutokeyConfig_KeyProjectResolutionMode_value[c.Mode])
}

// configured reports whether c says anything: an empty key project with no
// mode is the default, uninitialized config ("a request with an empty key
// project field will clear the configuration").
func (c autokeyConfig) configured() bool {
	return c.KeyProject != "" || (c.mode() != kmspb.AutokeyConfig_KEY_PROJECT_RESOLUTION_MODE_UNSPECIFIED && c.mode() != kmspb.AutokeyConfig_DEDICATED_KEY_PROJECT)
}

func (c autokeyConfig) etag() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (c autokeyConfig) proto() *kmspb.AutokeyConfig {
	st := kmspb.AutokeyConfig_UNINITIALIZED
	if c.configured() {
		st = kmspb.AutokeyConfig_ACTIVE
	}
	return &kmspb.AutokeyConfig{Name: c.Name, KeyProject: c.KeyProject, State: st, Etag: c.etag(), KeyProjectResolutionMode: c.mode()}
}

// parseAutokeyConfig checks folders/{n}/autokeyConfig or
// projects/{p}/autokeyConfig and returns the folder or project.
func parseAutokeyConfig(field, name string) (string, error) {
	parent, ok := strings.CutSuffix(name, "/autokeyConfig")
	parts := strings.Split(parent, "/")
	if !ok || len(parts) != 2 || (parts[0] != "folders" && parts[0] != "projects") || parts[1] == "" {
		return "", badName(field, name, "AutokeyConfig", "want folders/{folder}/autokeyConfig or projects/{project}/autokeyConfig")
	}
	if parts[0] == "folders" && strings.Trim(parts[1], "0123456789") != "" {
		return "", badName(field, name, "AutokeyConfig", "the folder must be its number")
	}
	return parent, nil
}

// AutokeyAdminServer serves google.cloud.kms.v1.AutokeyAdmin.
type AutokeyAdminServer struct {
	kmspb.UnimplementedAutokeyAdminServer
	s *Server
}

// AutokeyServer serves google.cloud.kms.v1.Autokey.
type AutokeyServer struct {
	kmspb.UnimplementedAutokeyServer
	s *Server
}

// AutokeyAdmin returns the AutokeyAdmin service over s's store.
func (s *Server) AutokeyAdmin() *AutokeyAdminServer { return &AutokeyAdminServer{s: s} }

// Autokey returns the Autokey service over s's store.
func (s *Server) Autokey() *AutokeyServer { return &AutokeyServer{s: s} }

func (a *AutokeyAdminServer) GetAutokeyConfig(_ context.Context, req *kmspb.GetAutokeyConfigRequest) (*kmspb.AutokeyConfig, error) {
	if _, err := parseAutokeyConfig("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	c := autokeyConfig{Name: req.GetName()}
	if _, err := a.s.get(dbKey(autokeyConfigPrefix, req.GetName()), &c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return c.proto(), nil
}

// UpdateAutokeyConfig sets the fields update_mask names: key_project and
// key_project_resolution_mode.
func (a *AutokeyAdminServer) UpdateAutokeyConfig(_ context.Context, req *kmspb.UpdateAutokeyConfigRequest) (*kmspb.AutokeyConfig, error) {
	s := a.s
	in := req.GetAutokeyConfig()
	parent, err := parseAutokeyConfig("autokey_config.name", in.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required"))
	}
	var setProject, setMode bool
	for _, p := range paths {
		switch p {
		case "key_project", "keyProject":
			setProject = true
		case "key_project_resolution_mode", "keyProjectResolutionMode":
			setMode = true
		case "name", "state":
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q names a field that cannot be updated", p))
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not an AutokeyConfig field", p))
		}
	}
	if setProject && in.GetKeyProject() != "" && !projectRE.MatchString(in.GetKeyProject()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("autokey_config.key_project %q must be projects/{project}", in.GetKeyProject()))
	}
	if strings.HasPrefix(parent, "folders/") && s.hierarchy != nil {
		if err := s.hierarchy.FolderExists(parent); err != nil {
			return nil, apierror.Wrap(apierror.NotFound("%s not found", parent))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := autokeyConfig{Name: in.GetName()}
	if _, err := s.get(dbKey(autokeyConfigPrefix, c.Name), &c); err != nil {
		return nil, apierror.Wrap(err)
	}
	if tag := in.GetEtag(); tag != "" && tag != c.etag() {
		// "The request will be rejected with an ABORTED error on a
		// mismatched etag."
		return nil, apierror.Wrap(apierror.Aborted("etag %q does not match %s: it has changed since it was read", tag, c.Name))
	}
	if setProject {
		c.KeyProject = in.GetKeyProject()
	}
	if setMode {
		c.Mode = ""
		if m := in.GetKeyProjectResolutionMode(); m != kmspb.AutokeyConfig_KEY_PROJECT_RESOLUTION_MODE_UNSPECIFIED {
			c.Mode = m.String()
		}
	}
	// "The key_project must not be set when [RESOURCE_PROJECT] is used."
	if c.mode() == kmspb.AutokeyConfig_RESOURCE_PROJECT && c.KeyProject != "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("autokey_config.key_project must be empty when key_project_resolution_mode is RESOURCE_PROJECT"))
	}
	if c.mode() == kmspb.AutokeyConfig_DEDICATED_KEY_PROJECT && c.KeyProject == "" && setMode {
		return nil, apierror.Wrap(apierror.InvalidArgument("autokey_config.key_project is required when key_project_resolution_mode is DEDICATED_KEY_PROJECT"))
	}
	if err := s.put(dbKey(autokeyConfigPrefix, c.Name), c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return c.proto(), nil
}

// effectiveKeyProject resolves the key project for a resource project:
// the nearest configured AutokeyConfig, its own first, then its folders'.
// It returns "" when Autokey is off or not configured.
func (s *Server) effectiveKeyProject(project string) (string, error) {
	chain := []string{project}
	if s.hierarchy != nil {
		// Cloud KMS takes any well-formed project, registered or not; one
		// Resource Manager does not know has no folders.
		anc, _ := s.hierarchy.Ancestors(project)
		for _, a := range anc {
			if strings.HasPrefix(a, "folders/") {
				chain = append(chain, a)
			}
		}
	}
	for _, r := range chain {
		var c autokeyConfig
		found, err := s.get(dbKey(autokeyConfigPrefix, r+"/autokeyConfig"), &c)
		if err != nil {
			return "", err
		}
		if !found || !c.configured() {
			continue
		}
		switch c.mode() {
		case kmspb.AutokeyConfig_DISABLED:
			return "", nil
		case kmspb.AutokeyConfig_RESOURCE_PROJECT:
			return project, nil
		}
		return c.KeyProject, nil
	}
	return "", nil
}

func (a *AutokeyAdminServer) ShowEffectiveAutokeyConfig(_ context.Context, req *kmspb.ShowEffectiveAutokeyConfigRequest) (*kmspb.ShowEffectiveAutokeyConfigResponse, error) {
	if !projectRE.MatchString(req.GetParent()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q must be projects/{project}", req.GetParent()))
	}
	kp, err := a.s.effectiveKeyProject(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &kmspb.ShowEffectiveAutokeyConfigResponse{KeyProject: kp}, nil
}

type keyHandle struct {
	Name         string `json:"name"`
	KmsKey       string `json:"kmsKey"`
	ResourceType string `json:"resourceTypeSelector"`
}

func (h keyHandle) proto() *kmspb.KeyHandle {
	return &kmspb.KeyHandle{Name: h.Name, KmsKey: h.KmsKey, ResourceTypeSelector: h.ResourceType}
}

func parseKeyHandle(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "keyHandles" || !keyHandleIDRE.MatchString(parts[5]) {
		return badName(field, name, "KeyHandle", "want projects/{project}/locations/{location}/keyHandles/{key_handle}")
	}
	return parseLocation(field, strings.Join(parts[:4], "/"))
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateKeyHandle provisions the key and returns a done operation.
func (a *AutokeyServer) CreateKeyHandle(ctx context.Context, req *kmspb.CreateKeyHandleRequest) (*longrunningpb.Operation, error) {
	s := a.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	id := req.GetKeyHandleId()
	if id == "" {
		// "If not provided by the caller, a new UUID is used."
		id = uuid.NewString()
	}
	if !keyHandleIDRE.MatchString(id) {
		return nil, apierror.Wrap(apierror.InvalidArgument("key_handle_id %q must be 1-63 letters, digits, - or _", id))
	}
	sel := req.GetKeyHandle().GetResourceTypeSelector()
	m := resourceTypeRE.FindStringSubmatch(sel)
	if m == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("key_handle.resource_type_selector %q must be {SERVICE}.googleapis.com/{TYPE}", sel))
	}
	name := req.GetParent() + "/keyHandles/" + id
	parts := strings.Split(req.GetParent(), "/")
	project, location := "projects/"+parts[1], parts[3]
	var existing keyHandle
	if found, err := s.get(dbKey(keyHandlePrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("KeyHandle %s already exists", name))
	}
	kp, err := s.effectiveKeyProject(project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if kp == "" {
		return nil, apierror.Wrap(apierror.FailedPrecondition("Autokey is not configured for %s: set an AutokeyConfig on it or on one of its folders", project))
	}
	ring := kp + "/locations/" + location + "/keyRings/" + autokeyRing
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: kp + "/locations/" + location, KeyRingId: autokeyRing}); err != nil &&
		apierror.From(err).Code != codes.AlreadyExists {
		return nil, err
	}
	// PROJECT_NUMBER-SERVICE_SHORT_NAME-RESOURCE_TYPE_DESCRIPTOR_NAME-RANDOM_HEX,
	// the key project's number first, as autokey-overview names it.
	keyID := strconv.FormatInt(resource.ProjectNumber(strings.TrimPrefix(kp, "projects/")), 10) + "-" +
		m[1] + "-" + strings.ToLower(m[2]) + "-" + randomSuffix()
	now := s.now().UTC()
	key, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: keyID, CryptoKey: &kmspb.CryptoKey{
		Purpose:          kmspb.CryptoKey_ENCRYPT_DECRYPT,
		VersionTemplate:  &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM, Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION},
		RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(autokeyRotation)},
		NextRotationTime: timestamppb.New(now.Add(autokeyRotation)),
	}})
	if err != nil {
		return nil, err
	}
	h := keyHandle{Name: name, KmsKey: key.GetName(), ResourceType: sel}
	s.mu.Lock()
	defer s.mu.Unlock()
	if found, err := s.get(dbKey(keyHandlePrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("KeyHandle %s already exists", name))
	}
	if err := s.put(dbKey(keyHandlePrefix, name), h); err != nil {
		return nil, apierror.Wrap(err)
	}
	resp, err := anypb.New(h.proto())
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the key handle"))
	}
	md, err := anypb.New(&kmspb.CreateKeyHandleMetadata{})
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the operation metadata"))
	}
	op := &longrunningpb.Operation{Name: req.GetParent() + "/operations/" + uuid.NewString(), Done: true, Metadata: md,
		Result: &longrunningpb.Operation_Response{Response: resp}}
	b, err := protojson.Marshal(op)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the operation"))
	}
	if err := s.db.Put(dbKey(operationPrefix, op.GetName()), b); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "store"))
	}
	return op, nil
}

func (a *AutokeyServer) GetKeyHandle(_ context.Context, req *kmspb.GetKeyHandleRequest) (*kmspb.KeyHandle, error) {
	if err := parseKeyHandle("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var h keyHandle
	if err := a.s.load(dbKey(keyHandlePrefix, req.GetName()), &h, "KeyHandle", req.GetName()); err != nil {
		return nil, err
	}
	return h.proto(), nil
}

// keyHandleFilterFields: the filter the proto shows,
// resource_type_selector="{SERVICE}.googleapis.com/{TYPE}".
var keyHandleFilterFields = fieldSupport{"resource_type_selector": {"=", "!="}, "name": {"=", "!=", ":"}}

func (a *AutokeyServer) ListKeyHandles(_ context.Context, req *kmspb.ListKeyHandlesRequest) (*kmspb.ListKeyHandlesResponse, error) {
	s := a.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), keyHandleFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(keyHandlePrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var handles []keyHandle
	for _, n := range names {
		var h keyHandle
		found, err := s.get(dbKey(keyHandlePrefix, n), &h)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !found || (filter != nil && !filter.match(func(field string) (string, bool) {
			switch field {
			case "name":
				return h.Name, true
			case "resource_type_selector":
				return h.ResourceType, true
			}
			return "", false
		})) {
			continue
		}
		handles = append(handles, h)
	}
	keep := make([]string, len(handles))
	byName := map[string]keyHandle{}
	for i, h := range handles {
		keep[i] = h.Name
		byName[h.Name] = h
	}
	// "If unspecified, at most 100 KeyHandles will be returned."
	size := int(req.GetPageSize())
	if size <= 0 || size > 100 {
		size = 100
	}
	page, next, err := orderedPage("kms-keyhandles:"+req.GetParent()+"|filter="+req.GetFilter(), keep, func(a, b string) bool { return a < b }, false, req.GetPageToken(), size)
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListKeyHandlesResponse{NextPageToken: next}
	for _, n := range page {
		resp.KeyHandles = append(resp.KeyHandles, byName[n].proto())
	}
	return resp, nil
}

// OperationsServer serves google.longrunning.Operations.GetOperation, the
// one method cloudkms_v1.yaml binds, for the operations CreateKeyHandle
// returns (the SDK's op.Wait and gcloud poll it). The others are
// UNIMPLEMENTED, as Google does not serve them.
type OperationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

// Operations returns the KMS operations service. It is registered apart
// from Register, since other services on a shared server serve
// google.longrunning.Operations too.
func (s *Server) Operations() *OperationsServer { return &OperationsServer{s: s} }

// Register adds the service to a gRPC server or the JSON transcoder.
func (o *OperationsServer) Register(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, o)
}

func (o *OperationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	parts := strings.Split(req.GetName(), "/")
	if len(parts) != 6 || parts[4] != "operations" || parseLocation("name", strings.Join(parts[:4], "/")) != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/operations/{operation}", req.GetName()))
	}
	b, err := o.s.db.Get(dbKey(operationPrefix, req.GetName()))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.Wrap(apierror.NotFound("operation %s not found", req.GetName()))
	}
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "read %s", req.GetName()))
	}
	op := &longrunningpb.Operation{}
	if err := protojson.Unmarshal(b, op); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "decode %s", req.GetName()))
	}
	return op, nil
}

// KeyHandlesInProject returns every KeyHandle of project, in every location,
// ordered by name, for the console.
func (s *Server) KeyHandlesInProject(project string) ([]*kmspb.KeyHandle, error) {
	if err := parseLocation("project", "projects/"+project+"/locations/global"); err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(keyHandlePrefix, "projects/"+project+"/locations")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var out []*kmspb.KeyHandle
	for _, n := range names {
		var h keyHandle
		found, err := s.get(dbKey(keyHandlePrefix, n), &h)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			out = append(out, h.proto())
		}
	}
	return out, nil
}

// AutokeyChain is where Autokey looks for project's configuration, nearest
// first: the project's own AutokeyConfig name, then each ancestor folder's.
func (s *Server) AutokeyChain(project string) []string {
	out := []string{"projects/" + project + "/autokeyConfig"}
	if s.hierarchy != nil {
		anc, _ := s.hierarchy.Ancestors("projects/" + project)
		for _, a := range anc {
			if strings.HasPrefix(a, "folders/") {
				out = append(out, a+"/autokeyConfig")
			}
		}
	}
	return out
}
