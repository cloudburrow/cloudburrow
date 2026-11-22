// Package apphub serves App Hub (apphub.googleapis.com v1, #2110): service
// project attachments and boundaries, the services and workloads App Hub
// discovers in them, applications (global and regional) with their
// attributes, the services and workloads registered to an application,
// extended metadata schemas, and the operations, locations and IAM
// mixins.
//
// As Google documents it (docs.cloud.google.com/app-hub/docs, read
// 2026-11-21):
//
//   - a service project can be attached to one host project at a time
//     (key-concepts); a service project attachment lives in global, its
//     ID is the service project's ID, its service_project accepts an ID or
//     a number and answers the number (the proto);
//   - lookupServiceProjectAttachment can be called from any project, and
//     answers the project's attachment, or nothing (its reference page);
//   - a boundary is a project's: MANUAL when its service projects are
//     attached by hand, AUTOMATIC with a crm_node, the node's projects
//     included, and MANAGED_AUTOMATIC for the management project of an
//     app-enabled folder, whose projects are all included (the proto,
//     set-up-app-hub);
//   - discovered services and workloads are the boundary's resources of the
//     supported types (supported-resources), named
//     projects/{host}/locations/{location}/discovered{Services,Workloads}/{uuid}
//     in the resource's own location;
//   - a GLOBAL application lives in global and may register resources from
//     any location; a REGIONAL one in a region registers only that region's
//     (global-regional-applications);
//   - an EXCLUSIVE service is registered to one application at a time, a
//     SHARED one (a GKE cluster) to many, without attributes (key-concepts);
//   - a registered service or workload whose resource is deleted, or whose
//     project leaves the boundary, is DETACHED until it is unregistered
//     (key-concepts);
//   - display names are at most 63 characters, descriptions 2048 (the
//     proto).
//
// What the docs leave unsaid is marked UNVERIFIED where it is decided.
// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/key-concepts.
package apphub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Resource is one resource App Hub discovers: a service or a workload of a
// type supported-resources lists, which this instance serves.
type Resource struct {
	// Workload is true for a workload, false for a service.
	Workload bool
	// URI is the resource's URI as supported-resources writes it, such as
	// //run.googleapis.com/projects/P/locations/L/services/S.
	URI string
	// Project is the project ID the resource is in.
	Project string
	// Location is its region, or "global".
	Location string
	// Zone is its zone when it is zonal.
	Zone string
	// Shared is a service registered to many applications (registration
	// type SHARED), such as a GKE cluster.
	Shared bool
	// Principal is the identity it runs as, when it has one.
	Principal string
}

// Projects resolves projects and app-enabled folders through Resource
// Manager.
type Projects interface {
	// Project is the ID and number of the project ref names (an ID or a
	// number); ok is false when there is no such project.
	Project(ref string) (id string, number int64, ok bool)
	// ManagedFolder is the folder (folders/N) whose management project
	// project is, or "".
	ManagedFolder(project string) string
	// FolderProjects are the IDs of the projects under folder, at any
	// depth.
	FolderProjects(folder string) []string
}

// Options are the server's collaborators.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Discover lists the resources of project this instance serves; nil
	// discovers none.
	Discover func(ctx context.Context, project string) ([]Resource, error)
	// Projects resolves projects and folders; nil accepts every
	// well-formed project ID and knows no folder.
	Projects Projects
}

// Server serves google.cloud.apphub.v1.AppHub.
type Server struct {
	apphubpb.UnimplementedAppHubServer

	opts  Options
	state string

	mu          sync.Mutex
	attachments map[string]*apphubpb.ServiceProjectAttachment
	apps        map[string]*apphubpb.Application
	services    map[string]*apphubpb.Service
	workloads   map[string]*apphubpb.Workload
	boundaries  map[string]*apphubpb.Boundary
	ops         map[string]*longrunningpb.Operation
	policies    map[string]*iampolicy.Stored
	// requests are the operations of the request IDs already seen.
	requests map[string]string
}

// NewServer loads the records in state ("" keeps them in memory).
func NewServer(state string, opts Options) (*Server, error) {
	s := &Server{opts: opts, state: state}
	s.clear()
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) clear() {
	s.attachments = map[string]*apphubpb.ServiceProjectAttachment{}
	s.apps = map[string]*apphubpb.Application{}
	s.services = map[string]*apphubpb.Service{}
	s.workloads = map[string]*apphubpb.Workload{}
	s.boundaries = map[string]*apphubpb.Boundary{}
	s.ops = map[string]*longrunningpb.Operation{}
	s.policies = map[string]*iampolicy.Stored{}
	s.requests = map[string]string{}
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

var (
	projectRE    = regexp.MustCompile(`^projects/([^/]+)$`)
	parentRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	attachmentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/serviceProjectAttachments/([^/]+)$`)
	appRE        = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/applications/([^/]+)$`)
	serviceRE    = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/applications/([^/]+))/services/([^/]+)$`)
	workloadRE   = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/applications/([^/]+))/workloads/([^/]+)$`)
	discSvcRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/discoveredServices/([^/]+)$`)
	discWlRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/discoveredWorkloads/([^/]+)$`)
	boundaryRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/boundary$`)
	schemaRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/extendedMetadataSchemas/(.+)$`)
	// idRE is an application's, service's or workload's ID: lower-case
	// letters, digits and hyphens, starting with a letter, at most 63
	// (UNVERIFIED: the docs give no rule; Google's examples, such as
	// my-app, fit it).
	// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/key-concepts.
	idRE      = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	projectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	emailRE   = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
)

const (
	maxDisplayName = 63
	maxDescription = 2048
)

// Regions are the regional locations Google's locations page lists.
var Regions = []string{
	"africa-south1",
	"asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3",
	"asia-south1", "asia-south2", "asia-southeast1", "asia-southeast2", "asia-southeast3",
	"australia-southeast1", "australia-southeast2",
	"europe-central2", "europe-north1", "europe-north2", "europe-southwest1",
	"europe-west1", "europe-west10", "europe-west12", "europe-west2", "europe-west3",
	"europe-west4", "europe-west6", "europe-west8", "europe-west9",
	"me-central1", "me-central2", "me-west1",
	"northamerica-northeast1", "northamerica-northeast2",
	"southamerica-east1", "southamerica-west1",
	"us-central1", "us-east1", "us-east4", "us-east5", "us-south1",
	"us-west1", "us-west2", "us-west3", "us-west4",
}

// MultiRegions are the multi-regions a MULTI_REGIONAL application lives in
// ("us": "Data centers in the United States (us-* regions)"; "eu": "Data
// centers within member states of the European Union", the locations
// page).
var MultiRegions = []string{"eu", "us"}

// euRegions are the regions of Regions in member states of the European
// Union: Warsaw, Finland, Stockholm, Madrid, Belgium, Frankfurt, the
// Netherlands, Milan, Paris, Berlin and Turin (not London or Zurich).
var euRegions = map[string]bool{"europe-central2": true, "europe-north1": true, "europe-north2": true,
	"europe-southwest1": true, "europe-west1": true, "europe-west3": true, "europe-west4": true,
	"europe-west8": true, "europe-west9": true, "europe-west10": true, "europe-west12": true}

// InMultiRegion is whether loc is multi or one of its constituent regions.
func InMultiRegion(multi, loc string) bool {
	switch multi {
	case "us":
		return loc == "us" || strings.HasPrefix(loc, "us-")
	case "eu":
		return loc == "eu" || euRegions[loc]
	}
	return false
}

// ScopeMultiRegional is Scope.Type MULTI_REGIONAL = 3, which googleapis
// added on 2026-11-16 (google/cloud/apphub/v1/application.proto) and no
// release of cloud.google.com/go/apphub carries yet (v1.4.1 stops at
// GLOBAL). Proto3 enums are open, so the value travels over gRPC as is;
// the JSON API names it through restScopeNames.
const ScopeMultiRegional = apphubpb.Scope_Type(3)

func isMultiRegion(loc string) bool { return loc == "eu" || loc == "us" }

// Locations are global, the multi-regions and the regions.
func Locations() []string {
	return append(append([]string{"global"}, MultiRegions...), Regions...)
}

func isRegion(loc string) bool {
	for _, r := range Regions {
		if r == loc {
			return true
		}
	}
	return false
}

func validLocation(loc string) bool { return loc == "global" || isMultiRegion(loc) || isRegion(loc) }

// checkParent is a parent's project and location.
func checkParent(field, parent string) (string, string, error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("location %q is not an App Hub location", m[2])
	}
	return m[1], m[2], nil
}

// project resolves a project reference (ID or number) to its ID and
// number.
func (s *Server) project(ref string) (string, int64, error) {
	if s.opts.Projects != nil {
		if id, n, ok := s.opts.Projects.Project(ref); ok {
			return id, n, nil
		}
		return "", 0, apierror.NotFound("project %q not found", ref)
	}
	if !projectID.MatchString(ref) {
		return "", 0, apierror.InvalidArgument("project %q is not a project ID", ref)
	}
	return ref, derivedNumber(ref), nil
}

// derivedNumber is a stable project number for an ID, without Resource
// Manager.
func derivedNumber(id string) int64 {
	h := sha256.Sum256([]byte(id))
	var n int64
	for _, b := range h[:6] {
		n = n<<8 | int64(b)
	}
	return n%900000000000 + 100000000000
}

func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// discoveredID is a discovered resource's UUID, stable for its URI, so a
// discovered name means the same resource across restarts.
func discoveredID(uri string) string {
	h := sha256.Sum256([]byte("apphub:" + uri))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

// done records a finished operation in location (projects/P/locations/L)
// for target, with resp its response; the caller holds mu.
func (s *Server) done(location, verb, target, requestID string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.now())
	md, err := anypb.New(&apphubpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(12), Done: true, Metadata: md,
		Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[op.Name] = op
	if requestID != "" {
		s.requests[requestID] = op.Name
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(op), nil
}

// replay is the operation an earlier request with requestID returned, so a
// retried request is not applied twice (AIP-155); the caller holds mu.
func (s *Server) replay(requestID string) (*longrunningpb.Operation, bool) {
	if requestID == "" {
		return nil, false
	}
	if name, ok := s.requests[requestID]; ok {
		if op, ok := s.ops[name]; ok {
			return clone(op), true
		}
	}
	return nil, false
}

func checkRequestID(id string) error {
	if id == "" {
		return nil
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(id) {
		return apierror.InvalidArgument("request_id %q must be a UUID", id)
	}
	return nil
}

func checkText(field, displayName, description string) error {
	if len([]rune(displayName)) > maxDisplayName {
		return apierror.InvalidArgument("%s.display_name can have a maximum length of %d characters", field, maxDisplayName)
	}
	if len([]rune(description)) > maxDescription {
		return apierror.InvalidArgument("%s.description can have a maximum length of %d characters", field, maxDescription)
	}
	return nil
}

func checkAttributes(field string, a *apphubpb.Attributes) error {
	if a == nil {
		return nil
	}
	for kind, owners := range map[string][]*apphubpb.ContactInfo{"developer_owners": a.GetDeveloperOwners(),
		"operator_owners": a.GetOperatorOwners(), "business_owners": a.GetBusinessOwners()} {
		for i, o := range owners {
			if o.GetEmail() == "" {
				return apierror.InvalidArgument("%s.attributes.%s[%d].email is required", field, kind, i)
			}
			if !emailRE.MatchString(o.GetEmail()) {
				return apierror.InvalidArgument("%s.attributes.%s[%d].email %q is not an email address", field, kind, i, o.GetEmail())
			}
			if len([]rune(o.GetDisplayName())) > maxDisplayName {
				return apierror.InvalidArgument("%s.attributes.%s[%d].display_name can have a maximum length of %d characters", field, kind, i, maxDisplayName)
			}
		}
	}
	return nil
}

// applyMask copies the paths of mask from src into dst, each one of
// allowed or under one; the caller checks the mask is not empty.
func applyMask(field string, paths []string, allowed map[string]func(), check func(path string) bool) error {
	if len(paths) == 0 {
		return apierror.InvalidArgument("update_mask is required")
	}
	for _, p := range paths {
		if p == "*" {
			for _, f := range allowed {
				f()
			}
			continue
		}
		f, ok := allowed[p]
		if !ok && check != nil && check(p) {
			continue
		}
		if !ok {
			return apierror.InvalidArgument("update_mask path %q cannot be updated on %s", p, field)
		}
		f()
	}
	return nil
}

// attributesPath copies one attributes.* path from src to dst.
func attributesPath(dst **apphubpb.Attributes, src *apphubpb.Attributes, path string) bool {
	sub, ok := strings.CutPrefix(path, "attributes.")
	if !ok {
		return false
	}
	if *dst == nil {
		*dst = &apphubpb.Attributes{}
	}
	switch sub {
	case "criticality":
		(*dst).Criticality = src.GetCriticality()
	case "environment":
		(*dst).Environment = src.GetEnvironment()
	case "developer_owners":
		(*dst).DeveloperOwners = src.GetDeveloperOwners()
	case "operator_owners":
		(*dst).OperatorOwners = src.GetOperatorOwners()
	case "business_owners":
		(*dst).BusinessOwners = src.GetBusinessOwners()
	default:
		return false
	}
	return true
}

func sortedKeys[V any](m map[string]V, prefix string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func listPage[T proto.Message](scope, filter, orderBy string, items []T, token string, size int32) ([]T, string, error) {
	out, next, err := aipfilter.ListPage(scope+"|"+filter+"|"+orderBy, filter, orderBy, items, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	return out, next, nil
}

// ResetProject deletes project's records, those it hosts and the
// attachments naming it; "" deletes everything.
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.clear()
		return s.save()
	}
	prefix := "projects/" + project + "/"
	for k := range s.attachments {
		if strings.HasPrefix(k, prefix) || strings.HasSuffix(k, "/serviceProjectAttachments/"+project) {
			delete(s.attachments, k)
		}
	}
	for k := range s.apps {
		if strings.HasPrefix(k, prefix) {
			delete(s.apps, k)
			delete(s.policies, k)
		}
	}
	for k := range s.services {
		if strings.HasPrefix(k, prefix) {
			delete(s.services, k)
		}
	}
	for k := range s.workloads {
		if strings.HasPrefix(k, prefix) {
			delete(s.workloads, k)
		}
	}
	for k := range s.boundaries {
		if strings.HasPrefix(k, prefix) {
			delete(s.boundaries, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
		}
	}
	return s.save()
}

// ---- state ----

type diskState struct {
	Attachments []json.RawMessage           `json:"attachments,omitempty"`
	Apps        []json.RawMessage           `json:"applications,omitempty"`
	Services    []json.RawMessage           `json:"services,omitempty"`
	Workloads   []json.RawMessage           `json:"workloads,omitempty"`
	Boundaries  []json.RawMessage           `json:"boundaries,omitempty"`
	Ops         []json.RawMessage           `json:"operations,omitempty"`
	Policies    map[string]iampolicy.Stored `json:"policies,omitempty"`
	Requests    map[string]string           `json:"requests,omitempty"`
}

func marshalAll[T proto.Message](m map[string]T) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, k := range sortedKeys(m, "") {
		b, err := protojson.Marshal(m[k])
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func unmarshalAll[T proto.Message](raw []json.RawMessage, mk func() T, name func(T) string, into map[string]T) error {
	for _, b := range raw {
		m := mk()
		if err := protojson.Unmarshal(b, m); err != nil {
			return err
		}
		into[name(m)] = m
	}
	return nil
}

func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	var st diskState
	var err error
	if st.Attachments, err = marshalAll(s.attachments); err != nil {
		return err
	}
	if st.Apps, err = marshalAll(s.apps); err != nil {
		return err
	}
	if st.Services, err = marshalAll(s.services); err != nil {
		return err
	}
	if st.Workloads, err = marshalAll(s.workloads); err != nil {
		return err
	}
	if st.Boundaries, err = marshalAll(s.boundaries); err != nil {
		return err
	}
	if st.Ops, err = marshalAll(s.ops); err != nil {
		return err
	}
	st.Policies = map[string]iampolicy.Stored{}
	for k, p := range s.policies {
		if p != nil {
			st.Policies[k] = *p
		}
	}
	st.Requests = s.requests
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o755); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
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
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("app hub state %s: %w", s.state, err)
	}
	errs := []error{
		unmarshalAll(st.Attachments, func() *apphubpb.ServiceProjectAttachment { return &apphubpb.ServiceProjectAttachment{} },
			(*apphubpb.ServiceProjectAttachment).GetName, s.attachments),
		unmarshalAll(st.Apps, func() *apphubpb.Application { return &apphubpb.Application{} }, (*apphubpb.Application).GetName, s.apps),
		unmarshalAll(st.Services, func() *apphubpb.Service { return &apphubpb.Service{} }, (*apphubpb.Service).GetName, s.services),
		unmarshalAll(st.Workloads, func() *apphubpb.Workload { return &apphubpb.Workload{} }, (*apphubpb.Workload).GetName, s.workloads),
		unmarshalAll(st.Boundaries, func() *apphubpb.Boundary { return &apphubpb.Boundary{} }, (*apphubpb.Boundary).GetName, s.boundaries),
		unmarshalAll(st.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} }, (*longrunningpb.Operation).GetName, s.ops),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("app hub state %s: %w", s.state, err)
	}
	for k, p := range st.Policies {
		p := p
		s.policies[k] = &p
	}
	if st.Requests != nil {
		s.requests = st.Requests
	}
	return nil
}
