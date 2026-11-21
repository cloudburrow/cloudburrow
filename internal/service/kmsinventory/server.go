// Package kmsinventory is the Cloud KMS Inventory API (#2053),
// kmsinventory.googleapis.com v1, as
// https://docs.cloud.google.com/kms/docs/view-key-usage and the
// google.cloud.kms.inventory.v1 protos describe it:
//
//   - KeyDashboardService.ListCryptoKeys lists every CryptoKey of a
//     project, across its locations and key rings;
//   - KeyTrackingService.GetProtectedResourcesSummary counts the resources a
//     key (or one of its versions) protects through CMEK, by project,
//     resource type, Cloud product and location, in the key's organization,
//     or in its project with an ORG_LESS_PROJECT_PARTIAL_DATA warning when
//     the project is in none;
//   - KeyTrackingService.SearchProtectedResources lists those resources in
//     an organization or a project scope, filtered by resource type (RE2,
//     an expression matching no tracked type is INVALID_ARGUMENT), at most
//     500 a page.
//
// Google answers from Cloud Asset Inventory. Here the answer is real usage
// read at request time: each Source asks one served product (Cloud Storage,
// BigQuery, Secret Manager, Pub/Sub, ...) which of its resources name a
// key, the same calls an SDK makes, so a resource appears as soon as it
// exists, where Google documents a delay. Only the tracked resource types
// the docs list are reported.
package kmsinventory

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	inventorypb "cloud.google.com/go/kms/inventory/apiv1/inventorypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Resource is one resource a product protects with a customer-managed key.
type Resource struct {
	// Name is the full resource name, //storage.googleapis.com/buckets/b.
	Name string
	// ProjectID is the project that owns it.
	ProjectID string
	// CloudProduct is the product, as Google spells it ("storage").
	CloudProduct string
	// ResourceType is the tracked type, storage.googleapis.com/Bucket.
	ResourceType string
	// Location is global, a region, a multi-region or a zone, lower case.
	Location string
	Labels   map[string]string
	// Keys are the CryptoKeys protecting it.
	Keys []string
	// Versions are the CryptoKeyVersions protecting it, where the product
	// records them; empty where it does not.
	Versions []string
	// Created is its creation time; zero when the product keeps none.
	Created time.Time
}

// protects reports whether r is protected by name, a CryptoKey or one of
// its versions.
func (r Resource) protects(name string) bool {
	if strings.Contains(name, "/cryptoKeyVersions/") {
		for _, v := range r.Versions {
			if v == name {
				return true
			}
		}
		return false
	}
	for _, k := range r.Keys {
		if k == name {
			return true
		}
	}
	for _, v := range r.Versions {
		if keyOf(v) == name {
			return true
		}
	}
	return false
}

// Source is one product's CMEK-protected resources in a project.
type Source func(ctx context.Context, project string) ([]Resource, error)

// Keys is the part of Cloud KMS the dashboard reads: the host's *kms.Server.
type Keys interface {
	GetCryptoKey(context.Context, *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error)
	ListCryptoKeys(context.Context, *kmspb.ListCryptoKeysRequest) (*kmspb.ListCryptoKeysResponse, error)
	KeyRingsInProject(project string) ([]*kmspb.KeyRing, error)
}

// Projects is Resource Manager's view of the projects.
type Projects interface {
	// Resolve is the ID of the project with this ID or number, false when
	// there is none.
	Resolve(idOrNumber string) (string, bool)
	// Organization is the number of the organization the project is in,
	// "" when it is in none.
	Organization(projectID string) string
	// InOrganization are the IDs of the active projects in organization
	// number.
	InOrganization(number string) []string
}

// Server serves both inventory services.
type Server struct {
	inventorypb.UnimplementedKeyDashboardServiceServer
	inventorypb.UnimplementedKeyTrackingServiceServer
	// KMS is the instance's Cloud KMS; nil (or returning nil) before it
	// starts.
	KMS func() Keys
	// Sources are the products that report usage, read at each call.
	Sources func() []Source
	// Projects resolves scopes; nil takes every project ID as given and
	// none to be in an organization.
	Projects Projects
}

// Tracker answers the tracking calls in process (the console).
type Tracker interface {
	GetProtectedResourcesSummary(context.Context, *inventorypb.GetProtectedResourcesSummaryRequest) (*inventorypb.ProtectedResourcesSummary, error)
	SearchProtectedResources(context.Context, *inventorypb.SearchProtectedResourcesRequest) (*inventorypb.SearchProtectedResourcesResponse, error)
}

// Register adds both services to g, a gRPC server or the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	inventorypb.RegisterKeyDashboardServiceServer(g, s)
	inventorypb.RegisterKeyTrackingServiceServer(g, s)
}

// TrackedTypes are the resource types Google's docs list as tracked
// (view-key-usage, "Tracked resource types", read 2026-11-20).
var TrackedTypes = []string{
	"discoveryengine.googleapis.com/DataStore",
	"alloydb.googleapis.com/Backup", "alloydb.googleapis.com/Cluster",
	"apigee.googleapis.com/Organization", "apigee.googleapis.com/Instance",
	"apihub.googleapis.com/ApiHubInstance",
	"artifactregistry.googleapis.com/Repository",
	"backupdr.googleapis.com/BackupVault", "backupdr.googleapis.com/Backup",
	"bigquery.googleapis.com/Dataset", "bigquery.googleapis.com/Model", "bigquery.googleapis.com/Table",
	"bigquerydatatransfer.googleapis.com/TransferConfig",
	"bigtableadmin.googleapis.com/Backup", "bigtableadmin.googleapis.com/Cluster", "bigtableadmin.googleapis.com/Table",
	"datafusion.googleapis.com/Instance",
	"healthcare.googleapis.com/Dataset",
	"logging.googleapis.com/LogBucket",
	"run.googleapis.com/Revision",
	"cloudfunctions.googleapis.com/CloudFunction", "cloudfunctions.googleapis.com/Function",
	"sqladmin.googleapis.com/BackupRun", "sqladmin.googleapis.com/Instance",
	"storage.googleapis.com/Bucket",
	"workstations.googleapis.com/Workstation", "workstations.googleapis.com/WorkstationConfig",
	"compute.googleapis.com/Disk", "compute.googleapis.com/Image", "compute.googleapis.com/MachineImage", "compute.googleapis.com/Snapshot",
	"datamigration.googleapis.com/MigrationJob", "datamigration.googleapis.com/ConnectionProfile",
	"dataflow.googleapis.com/Job",
	"metastore.googleapis.com/Service",
	"datastream.googleapis.com/Stream",
	"documentai.googleapis.com/HumanReviewConfig", "documentai.googleapis.com/Processor", "documentai.googleapis.com/ProcessorVersion",
	"file.googleapis.com/Instance", "file.googleapis.com/Backup",
	"firestore.googleapis.com/Database", "datastore.googleapis.com/Database",
	"lustre.googleapis.com/Instance",
	"observability.googleapis.com/Bucket",
	"container.googleapis.com/Cluster",
	"looker.googleapis.com/Instance",
	"composer.googleapis.com/Environment",
	"dataproc.googleapis.com/Cluster", "dataproc.googleapis.com/Batch",
	"redis.googleapis.com/Instance",
	"vmmigration.googleapis.com/Source",
	"pubsub.googleapis.com/Topic",
	"secretmanager.googleapis.com/Secret", "secretmanager.googleapis.com/SecretVersion",
	"securesourcemanager.googleapis.com/Instance",
	"spanner.googleapis.com/Database",
	"aiplatform.googleapis.com/Dataset", "aiplatform.googleapis.com/Featurestore", "aiplatform.googleapis.com/Tensorboard",
	"aiplatform.googleapis.com/BatchPredictionJob", "aiplatform.googleapis.com/CustomJob", "aiplatform.googleapis.com/Endpoint",
	"aiplatform.googleapis.com/Model", "aiplatform.googleapis.com/TrainingPipeline", "aiplatform.googleapis.com/PipelineJob",
	"aiplatform.googleapis.com/MetadataStore",
	"notebooks.googleapis.com/Instance",
	"workflows.googleapis.com/Workflow",
}

var tracked = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range TrackedTypes {
		m[t] = true
	}
	return m
}()

var (
	keyRE     = regexp.MustCompile(`^projects/([^/]+)/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)
	versionRE = regexp.MustCompile(`^(projects/([^/]+)/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+)/cryptoKeyVersions/[0-9]+$`)
	projectRE = regexp.MustCompile(`^projects/([^/]+)$`)
	orgRE     = regexp.MustCompile(`^organizations/([0-9]+)$`)
)

// keyOf is the CryptoKey of a version's name.
func keyOf(version string) string {
	if m := versionRE.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return ""
}

func (s *Server) kms() (Keys, error) {
	if s.KMS != nil {
		if k := s.KMS(); k != nil {
			return k, nil
		}
	}
	return nil, status.Error(codes.Unavailable, "Cloud KMS has not started")
}

func (s *Server) resolve(idOrNumber string) (string, bool) {
	if s.Projects == nil {
		return idOrNumber, true
	}
	return s.Projects.Resolve(idOrNumber)
}

// ListCryptoKeys lists every key of a project.
func (s *Server) ListCryptoKeys(ctx context.Context, in *inventorypb.ListCryptoKeysRequest) (*inventorypb.ListCryptoKeysResponse, error) {
	m := projectRE.FindStringSubmatch(in.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}, got %q", in.GetParent())
	}
	if in.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	project, ok := s.resolve(m[1])
	if !ok {
		return nil, apierror.NotFound("project %q not found", m[1])
	}
	k, err := s.kms()
	if err != nil {
		return nil, err
	}
	rings, err := k.KeyRingsInProject(project)
	if err != nil {
		return nil, err
	}
	byName := map[string]*kmspb.CryptoKey{}
	var names []string
	for _, r := range rings {
		token := ""
		for {
			resp, err := k.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: r.GetName(), PageSize: 1000, PageToken: token})
			if err != nil {
				return nil, err
			}
			for _, ck := range resp.GetCryptoKeys() {
				byName[ck.GetName()] = ck
				names = append(names, ck.GetName())
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	page, next, err := paging.Page("kmsinventory/cryptoKeys/"+project, names, in.GetPageToken(), int(in.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &inventorypb.ListCryptoKeysResponse{NextPageToken: next}
	for _, n := range page {
		out.CryptoKeys = append(out.CryptoKeys, byName[n])
	}
	return out, nil
}

// scopeProjects are the projects of a scope.
func (s *Server) scopeProjects(scope string) ([]string, error) {
	if m := orgRE.FindStringSubmatch(scope); m != nil {
		if s.Projects == nil {
			return nil, apierror.NotFound("organization %q not found", scope)
		}
		return s.Projects.InOrganization(m[1]), nil
	}
	if m := projectRE.FindStringSubmatch(scope); m != nil {
		id, ok := s.resolve(m[1])
		if !ok {
			return nil, apierror.NotFound("project %q not found", m[1])
		}
		return []string{id}, nil
	}
	return nil, apierror.InvalidArgument("scope must be organizations/{ORGANIZATION_NUMBER}, projects/{PROJECT_ID} or projects/{PROJECT_NUMBER}, got %q", scope)
}

// checkKey checks name is a CryptoKey's or a version's, and that the key
// exists; it returns the key's project.
func (s *Server) checkKey(ctx context.Context, field, name string) (string, error) {
	key, project := name, ""
	if m := keyRE.FindStringSubmatch(name); m != nil {
		project = m[1]
	} else if m := versionRE.FindStringSubmatch(name); m != nil {
		key, project = m[1], m[2]
	} else {
		return "", apierror.InvalidArgument("%s must be a CryptoKey's name, projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}, or one of its versions', got %q", field, name)
	}
	k, err := s.kms()
	if err != nil {
		return "", err
	}
	if _, err := k.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil {
		return "", err
	}
	if id, ok := s.resolve(project); ok {
		project = id
	}
	return project, nil
}

// protected are the resources of projects that name protects, matching
// types when it is set, sorted by name.
func (s *Server) protected(ctx context.Context, name string, projects []string, types []*regexp.Regexp) []Resource {
	var sources []Source
	if s.Sources != nil {
		sources = s.Sources()
	}
	var out []Resource
	seen := map[string]bool{}
	for _, p := range projects {
		for _, src := range sources {
			// A product that does not answer leaves its resources out, as
			// an asset Google's inventory has not indexed yet.
			rs, _ := src(ctx, p)
			for _, r := range rs {
				if !tracked[r.ResourceType] || seen[r.Name] || !r.protects(name) || !matches(types, r.ResourceType) {
					continue
				}
				seen[r.Name] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func matches(types []*regexp.Regexp, t string) bool {
	if len(types) == 0 {
		return true
	}
	for _, re := range types {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// GetProtectedResourcesSummary counts what a key protects in its
// organization, or in its project when that is in none.
func (s *Server) GetProtectedResourcesSummary(ctx context.Context, in *inventorypb.GetProtectedResourcesSummaryRequest) (*inventorypb.ProtectedResourcesSummary, error) {
	name := in.GetName()
	project, err := s.checkKey(ctx, "name", name)
	if err != nil {
		return nil, err
	}
	out := &inventorypb.ProtectedResourcesSummary{Name: name + "/protectedResourcesSummary",
		ResourceTypes: map[string]int64{}, CloudProducts: map[string]int64{}, Locations: map[string]int64{}}
	projects := []string{project}
	org := ""
	if s.Projects != nil {
		org = s.Projects.Organization(project)
	}
	if org != "" {
		projects = s.Projects.InOrganization(org)
	} else {
		out.Warnings = append(out.Warnings, &inventorypb.Warning{WarningCode: inventorypb.Warning_ORG_LESS_PROJECT_PARTIAL_DATA,
			DisplayMessage: "Project " + project + " is not in an organization: the summary covers this project only and may be partial."})
	}
	inProject := map[string]bool{}
	for _, r := range s.protected(ctx, name, projects, nil) {
		out.ResourceCount++
		out.ResourceTypes[r.ResourceType]++
		out.CloudProducts[r.CloudProduct]++
		out.Locations[r.Location]++
		inProject[r.ProjectID] = true
	}
	out.ProjectCount = int32(len(inProject))
	return out, nil
}

// maxSearchPage is the documented maximum, and the default, page size.
const maxSearchPage = 500

// SearchProtectedResources lists what a key protects in a scope.
func (s *Server) SearchProtectedResources(ctx context.Context, in *inventorypb.SearchProtectedResourcesRequest) (*inventorypb.SearchProtectedResourcesResponse, error) {
	if in.GetScope() == "" {
		return nil, apierror.InvalidArgument("scope is required")
	}
	if in.GetCryptoKey() == "" {
		return nil, apierror.InvalidArgument("crypto_key is required")
	}
	if in.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	var types []*regexp.Regexp
	for _, t := range in.GetResourceTypes() {
		re, err := regexp.Compile("^(?:" + t + ")$")
		if err != nil {
			return nil, apierror.InvalidArgument("resource_types: %q is not a valid RE2 expression: %v", t, err)
		}
		hit := false
		for _, tt := range TrackedTypes {
			if re.MatchString(tt) {
				hit = true
				break
			}
		}
		if !hit {
			return nil, apierror.InvalidArgument("resource_types: %q matches no supported resource type", t)
		}
		types = append(types, re)
	}
	projects, err := s.scopeProjects(in.GetScope())
	if err != nil {
		return nil, err
	}
	if _, err := s.checkKey(ctx, "crypto_key", in.GetCryptoKey()); err != nil {
		return nil, err
	}
	rs := s.protected(ctx, in.GetCryptoKey(), projects, types)
	byName := map[string]Resource{}
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		byName[r.Name] = r
		names = append(names, r.Name)
	}
	size := int(in.GetPageSize())
	if size == 0 || size > maxSearchPage {
		size = maxSearchPage
	}
	scope := "kmsinventory/search/" + in.GetScope() + "/" + in.GetCryptoKey() + "/" + strings.Join(in.GetResourceTypes(), ",")
	page, next, err := paging.Page(scope, names, in.GetPageToken(), size)
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token: the other parameters must match the call that returned it")
	}
	out := &inventorypb.SearchProtectedResourcesResponse{NextPageToken: next}
	for _, n := range page {
		out.ProtectedResources = append(out.ProtectedResources, protoOf(byName[n]))
	}
	return out, nil
}

func protoOf(r Resource) *inventorypb.ProtectedResource {
	p := &inventorypb.ProtectedResource{Name: r.Name, ProjectId: r.ProjectID,
		Project:      "projects/" + strconv.FormatInt(resource.ProjectNumber(r.ProjectID), 10),
		CloudProduct: r.CloudProduct, ResourceType: r.ResourceType, Location: r.Location, Labels: r.Labels}
	if len(r.Versions) > 0 {
		p.CryptoKeyVersions = append([]string(nil), r.Versions...)
		p.CryptoKeyVersion = r.Versions[0]
	}
	if !r.Created.IsZero() {
		p.CreateTime = timestamppb.New(r.Created.Truncate(time.Second))
	}
	return p
}
