package run

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// ArtifactRegistryPrefix is the path under which the Run port serves
// Artifact Registry's v1 JSON API by itself (#1578): its plain /v1/ paths
// are shared with Cloud Build and Cloud Run v1, and
// GET /v1/projects/{p}/locations is Cloud Run v1's list, so Artifact
// Registry's Locations mixin is reached only here. The data source
// google_artifact_registry_locations cannot use it: the provider (8.4.0
// through main, read 2026-11-18) builds its URL from
// https://artifactregistry.googleapis.com/v1/ whatever
// artifact_registry_custom_endpoint says.
const ArtifactRegistryPrefix = "/artifactregistry"

// ArtifactRegistryLocations are the repository locations Google's
// "Repository locations" page lists (re-read 2026-11-18:
// https://docs.cloud.google.com/artifact-registry/docs/repositories/repo-locations),
// the regions and the multi-regions asia, europe and us, sorted.
var ArtifactRegistryLocations = []string{
	"africa-south1",
	"asia", "asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3",
	"asia-south1", "asia-south2", "asia-southeast1", "asia-southeast2", "asia-southeast3",
	"australia-southeast1", "australia-southeast2",
	"europe", "europe-central2", "europe-north1", "europe-north2", "europe-southwest1",
	"europe-west1", "europe-west10", "europe-west12", "europe-west2", "europe-west3", "europe-west4",
	"europe-west6", "europe-west8", "europe-west9",
	"me-central1", "me-central2", "me-west1",
	"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1",
	"southamerica-east1", "southamerica-west1",
	"us", "us-central1", "us-east1", "us-east4", "us-east5", "us-south1",
	"us-west1", "us-west2", "us-west3", "us-west4",
}

var (
	arProjectRE  = regexp.MustCompile(`^projects/([^/]+)$`)
	arLocationRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
)

// arLocations serves google.cloud.location.Locations as
// artifactregistry_v1.yaml binds it. Each Location has name and
// location_id; whether Google sets display_name, labels or metadata is
// UNVERIFIED (the reference names none).
// Docs re-read 2026-11-18, still unanswered: https://docs.cloud.google.com/artifact-registry/docs/reference/rest/v1/projects.locations/list
type arLocations struct {
	locationpb.UnimplementedLocationsServer
}

func (arLocations) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !arProjectRE.MatchString(req.GetName()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}", req.GetName()))
	}
	if req.GetFilter() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("ListLocations filter is not supported"))
	}
	page, next, err := paging.Page("artifactregistry-locations:"+req.GetName(), ArtifactRegistryLocations, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		resp.Locations = append(resp.Locations, &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id})
	}
	return resp, nil
}

func (arLocations) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := arLocationRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName()))
	}
	for _, id := range ArtifactRegistryLocations {
		if id == m[2] {
			return &locationpb.Location{Name: req.GetName(), LocationId: id}, nil
		}
	}
	return nil, apierror.Wrap(apierror.NotFound("Location %q not found", req.GetName()))
}

// arLocationsMixins are the Locations bindings artifactregistry_v1.yaml adds.
var arLocationsMixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
}

// isARLocationsPath reports whether p (prefix removed) is a Locations call.
func isARLocationsPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/v1/projects/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return parts[0] != "" && (len(parts) == 2 && parts[1] == "locations" ||
		len(parts) == 3 && parts[1] == "locations" && parts[2] != "")
}

// ArtifactRegistryHandlerOr serves Artifact Registry's v1 JSON API under
// ArtifactRegistryPrefix: its Locations mixin itself, and every other call,
// prefix removed, through api, the Run port's JSON handler, where its
// /v1/ paths are served. Any other path goes to api unchanged.
func ArtifactRegistryHandlerOr(api http.Handler) http.Handler {
	t := &rest.Transcoder{Mixins: arLocationsMixins}
	locations.Register(t, arLocations{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := strings.CutPrefix(r.URL.Path, ArtifactRegistryPrefix)
		if !ok || !strings.HasPrefix(p, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		r2.URL.RawPath = ""
		if isARLocationsPath(p) {
			t.ServeHTTP(w, r2)
			return
		}
		api.ServeHTTP(w, r2)
	})
}
