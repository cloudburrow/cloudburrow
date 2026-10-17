package run

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"

	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The rest of the Cloud Run v1 API that v2 already answers (#1349):
// projects.locations.list, which gcloud run calls to offer the regions, and
// the IAM methods on projects.locations.services, which gcloud run services
// add-iam-policy-binding and get-iam-policy call. A service's v1 policy is
// its v2 policy: the same stored ConfigMap (#1216), never enforced.

// Regions are the locations projects.locations.list answers for every
// project: the regions Google's Cloud Run locations page lists. Any
// well-formed location is accepted in a service's name, so a location that
// holds a service is listed too. Which fields Google sets on each Location
// (here name and locationId, which is what gcloud reads) is UNVERIFIED: no
// response was observed.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/run/docs/reference/rest/v1/projects.locations
var Regions = []string{
	"africa-south1",
	"asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3",
	"asia-south1", "asia-south2", "asia-southeast1", "asia-southeast2",
	"australia-southeast1", "australia-southeast2",
	"europe-central2", "europe-north1", "europe-north2", "europe-southwest1",
	"europe-west1", "europe-west2", "europe-west3", "europe-west4", "europe-west6",
	"europe-west8", "europe-west9", "europe-west10", "europe-west12",
	"me-central1", "me-central2", "me-west1",
	"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1",
	"southamerica-east1", "southamerica-west1",
	"us-central1", "us-east1", "us-east4", "us-east5", "us-south1",
	"us-west1", "us-west2", "us-west3", "us-west4",
}

// v1IAMRE is a v1 IAM call on a service or, as projects.locations.jobs
// (#1420), a job, or, as projects.locations.workerpools (#1533), a worker
// pool, or, as projects.locations.instances (#1546), an instance: the
// project, location, collection, ID and method.
var v1IAMRE = regexp.MustCompile(`^/v1/projects/([^/]+)/locations/([^/]+)/(services|jobs|workerpools|instances)/([^/:]+):(getIamPolicy|setIamPolicy|testIamPermissions)$`)

// v1IAMServer is the v2 server whose IAM methods a v1 IAM call is.
type v1IAMServer interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error)
}

// v1Locations lists a project's locations: the regions, and each location
// holding one of its services, paged with pageSize and pageToken.
func (s *Server) v1Locations(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("ns")
	if r.URL.Query().Get("filter") != "" {
		v1Write(w, apierror.Unimplemented("projects.locations.list filter is not supported"), nil)
		return
	}
	seen := map[string]bool{}
	for _, id := range Regions {
		seen[id] = true
	}
	svcs, _, err := s.v1Services(r.Context(), v1Scope{project: project})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	for _, svc := range svcs {
		if l := locationOf(svc.GetName()); l != "" {
			seen[l] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	page, next, err := paging.Page("run-v1-locations:"+project, ids, r.URL.Query().Get("pageToken"), size)
	if err != nil {
		v1Write(w, apierror.InvalidArgument("%v", err), nil)
		return
	}
	resp := &runv1.ListLocationsResponse{NextPageToken: next, Locations: []*runv1.Location{}}
	for _, id := range page {
		resp.Locations = append(resp.Locations, &runv1.Location{Name: "projects/" + project + "/locations/" + id,
			LocationId: id, DisplayName: id})
	}
	v1Write(w, nil, resp)
}

// v1IAM serves a v1 IAM call on a service or a job through v2's: the same
// stored policy, never enforced (ADR-0006).
func (s *Server) v1IAM(w http.ResponseWriter, r *http.Request, m []string) {
	name := "projects/" + m[1] + "/locations/" + m[2] + "/" + m[3] + "/" + m[4]
	var srv v1IAMServer = s
	switch m[3] {
	case "jobs":
		srv = s.Jobs()
		if _, err := s.Jobs().GetJob(r.Context(), &runpb.GetJobRequest{Name: name}); err != nil {
			v1Write(w, err, nil)
			return
		}
	case "instances":
		// v2's Instances has no IAM methods; the policy is stored beside
		// the instance as a pool's is (#1546).
		srv = s.Instances()
	case "workerpools":
		// v1's collection is workerpools, v2's workerPools. The pool's v2
		// IAM methods find the pool, and answer NOT_FOUND for one that
		// does not exist.
		name = "projects/" + m[1] + "/locations/" + m[2] + "/workerPools/" + m[4]
		srv = s.WorkerPools()
	default:
		if _, _, err := s.v1Service(r.Context(), v1Scope{project: m[1], location: m[2]}, m[4]); err != nil {
			v1Write(w, err, nil)
			return
		}
	}
	var (
		resp proto.Message
		err  error
	)
	switch m[5] {
	case "getIamPolicy":
		if r.Method != http.MethodGet {
			break
		}
		req := &iampb.GetIamPolicyRequest{Resource: name}
		if v := r.URL.Query().Get("options.requestedPolicyVersion"); v != "" {
			n, perr := strconv.Atoi(v)
			if perr != nil {
				v1Write(w, apierror.InvalidArgument("options.requestedPolicyVersion %q is not a number", v), nil)
				return
			}
			req.Options = &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(n)}
		}
		resp, err = srv.GetIamPolicy(r.Context(), req)
	case "setIamPolicy":
		req := &iampb.SetIamPolicyRequest{}
		if err = v1DecodeProto(r, req); err == nil {
			req.Resource = name
			resp, err = srv.SetIamPolicy(r.Context(), req)
		}
	case "testIamPermissions":
		req := &iampb.TestIamPermissionsRequest{}
		if err = v1DecodeProto(r, req); err == nil {
			req.Resource = name
			resp, err = srv.TestIamPermissions(r.Context(), req)
		}
	}
	if err == nil && resp == nil {
		err = apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path)
	}
	if err != nil {
		rest.WriteError(w, err)
		return
	}
	b, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(resp)
	if err != nil {
		rest.WriteError(w, apierror.Internal(err, "encode IAM response"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// v1DecodeProto reads a JSON request body into a proto message, refusing a
// field the message does not have rather than dropping it.
func v1DecodeProto(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return apierror.InvalidArgument("read body: %v", err)
	}
	if len(b) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("body: %v", err)
	}
	return nil
}
