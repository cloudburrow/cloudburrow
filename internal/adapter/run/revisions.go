package run

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Revisions (#299): google.cloud.run.v2.Revisions over Knative Revisions.
//
// Knative models revisions natively — every change to a Service's template
// creates an immutable Revision — so these map onto what the cluster already
// holds rather than onto anything CloudBurrow keeps. A revision's name is
// Knative's; its service is the Cloud Run service it belongs to, named the way
// the caller named it.

// krev is the subset of a Knative Revision this reads.
type krev struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Generation        int64             `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		ContainerConcurrency int `json:"containerConcurrency"`
		Containers           []struct {
			Image string `json:"image"`
			Env   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
			Ports []struct {
				ContainerPort int `json:"containerPort"`
			} `json:"ports"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64           `json:"observedGeneration"`
		Conditions         []ksvcCondition `json:"conditions"`
	} `json:"status"`
}

// Knative's labels for which Service and which generation of its
// configuration a revision belongs to.
const (
	labelService   = "serving.knative.dev/service"
	labelConfigGen = "serving.knative.dev/configurationGeneration"
)

// Revisions lists a Service's revisions.
func (k *Knative) Revisions(ctx context.Context, service string) ([]krev, error) {
	out, err := k.kubectl(ctx, "", "get", "revisions", "-l", labelService+"="+service, "-o", "json")
	if err != nil {
		return nil, apierror.Internal(err, "list Knative Revisions of %s", service)
	}
	var list struct {
		Items []krev `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, apierror.Internal(err, "decode Knative Revision list")
	}
	return list.Items, nil
}

// Revision reads one revision of a Service. A revision that exists but
// belongs to another Service is not found: it is not this Service's.
func (k *Knative) Revision(ctx context.Context, service, name string) (krev, error) {
	out, err := k.kubectl(ctx, "", "get", "revision", name, "-o", "json")
	if err != nil {
		return krev{}, apierror.NotFound("revision %s not found", name)
	}
	var r krev
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return krev{}, apierror.Internal(err, "decode Knative Revision")
	}
	if r.Metadata.Labels[labelService] != service {
		return krev{}, apierror.NotFound("revision %s not found in service %s", name, service)
	}
	return r, nil
}

// DeleteRevision removes one Knative Revision.
func (k *Knative) DeleteRevision(ctx context.Context, name string) error {
	if _, err := k.kubectl(ctx, "", "delete", "revision", name); err != nil {
		return apierror.Internal(err, "delete Knative Revision %s", name)
	}
	return nil
}

// servingRevisions is the set of revisions a Service routes traffic to.
func (s ksvc) servingRevisions() map[string]bool {
	out := map[string]bool{}
	for _, t := range s.Status.Traffic {
		if t.Percent > 0 && t.RevisionName != "" {
			out[t.RevisionName] = true
		}
	}
	return out
}

// parseRevisionName splits .../services/{svc}/revisions/{rev}.
func parseRevisionName(name string) (serviceName, service, revision string, err error) {
	i := strings.LastIndex(name, "/revisions/")
	if i < 0 {
		return "", "", "", apierror.InvalidArgument("%q is not a Cloud Run revision name", name)
	}
	serviceName, revision = name[:i], name[i+len("/revisions/"):]
	if revision == "" || strings.Contains(revision, "/") {
		return "", "", "", apierror.InvalidArgument("%q is not a Cloud Run revision name", name)
	}
	service, err = ServiceID(serviceName)
	return serviceName, service, revision, err
}

// FromKnativeRevision maps a Knative Revision onto run.v2.Revision.
func FromKnativeRevision(r krev, serviceName string) *runpb.Revision {
	out := &runpb.Revision{
		Name:               serviceName + "/revisions/" + r.Metadata.Name,
		Uid:                r.Metadata.UID,
		Service:            serviceName,
		CreateTime:         timestamppb.New(r.Metadata.CreationTimestamp),
		ObservedGeneration: r.Status.ObservedGeneration,
	}
	// The revision's generation is the generation of the Service's
	// configuration it was cut from — 1 for the first deploy, 2 for the
	// next — which is what Cloud Run reports, not the Revision object's own
	// metadata.generation, which is 1 for every revision.
	if g, err := strconv.ParseInt(r.Metadata.Labels[labelConfigGen], 10, 64); err == nil {
		out.Generation = g
	} else {
		out.Generation = r.Metadata.Generation
	}
	for _, c := range r.Spec.Containers {
		container := &runpb.Container{Image: c.Image}
		for _, e := range c.Env {
			container.Env = append(container.Env, &runpb.EnvVar{Name: e.Name, Values: &runpb.EnvVar_Value{Value: e.Value}})
		}
		for _, p := range c.Ports {
			container.Ports = append(container.Ports, &runpb.ContainerPort{ContainerPort: int32(p.ContainerPort)})
		}
		out.Containers = append(out.Containers, container)
	}
	if cc := r.Spec.ContainerConcurrency; cc > 0 {
		out.MaxInstanceRequestConcurrency = int32(cc)
	}
	reconciling := false
	for _, c := range r.Status.Conditions {
		cond := &runpb.Condition{Type: c.Type, Message: c.Message}
		switch c.Status {
		case "True":
			cond.State = runpb.Condition_CONDITION_SUCCEEDED
		case "False":
			cond.State = runpb.Condition_CONDITION_FAILED
		default:
			cond.State = runpb.Condition_CONDITION_PENDING
			if c.Type == "Ready" {
				reconciling = true
			}
		}
		out.Conditions = append(out.Conditions, cond)
	}
	out.Reconciling = reconciling
	return out
}

// RevisionsServer serves google.cloud.run.v2.Revisions.
type RevisionsServer struct {
	runpb.UnimplementedRevisionsServer
	s *Server
}

// Revisions returns the Revisions service over the same adapter.
func (s *Server) Revisions() *RevisionsServer { return &RevisionsServer{s: s} }

// Register adds the Revisions service to a gRPC server.
func (r *RevisionsServer) Register(g *grpc.Server) { runpb.RegisterRevisionsServer(g, r) }

func (r *RevisionsServer) GetRevision(ctx context.Context, req *runpb.GetRevisionRequest) (*runpb.Revision, error) {
	serviceName, service, revision, err := parseRevisionName(req.GetName())
	if err != nil {
		return nil, err
	}
	kr, err := r.s.kn.Revision(ctx, service, revision)
	if err != nil {
		return nil, apierror.NotFound("revision %s not found", req.GetName())
	}
	return FromKnativeRevision(kr, serviceName), nil
}

func (r *RevisionsServer) ListRevisions(ctx context.Context, req *runpb.ListRevisionsRequest) (*runpb.ListRevisionsResponse, error) {
	service, err := ServiceID(req.GetParent())
	if err != nil {
		return nil, err
	}
	// The service first: listing the revisions of a service that does not
	// exist is NOT_FOUND, not an empty page.
	if _, err := r.s.kn.Get(ctx, service); err != nil {
		return nil, apierror.NotFound("service %s not found", req.GetParent())
	}
	items, err := r.s.kn.Revisions(ctx, service)
	if err != nil {
		return nil, err
	}
	// Newest first, as Cloud Run lists them. Paging sorts its keys
	// ascending, so the key is the generation counted down from a ceiling,
	// zero-padded so string order agrees with numeric order.
	byKey := map[string]*runpb.Revision{}
	keys := make([]string, 0, len(items))
	for _, kr := range items {
		rev := FromKnativeRevision(kr, req.GetParent())
		key := fmt.Sprintf("%019d/%s", math.MaxInt64-rev.GetGeneration(), rev.GetName())
		byKey[key] = rev
		keys = append(keys, key)
	}
	page, next, err := paging.Page("run-revisions:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListRevisionsResponse{NextPageToken: next}
	for _, k := range page {
		resp.Revisions = append(resp.Revisions, byKey[k])
	}
	return resp, nil
}

// DeleteRevision removes a revision that is not serving traffic.
//
// A revision the service routes to is refused, as Cloud Run refuses it:
// deleting it would take the service down, and the way to retire it is to
// move traffic first.
func (r *RevisionsServer) DeleteRevision(ctx context.Context, req *runpb.DeleteRevisionRequest) (*longrunningpb.Operation, error) {
	serviceName, service, revision, err := parseRevisionName(req.GetName())
	if err != nil {
		return nil, err
	}
	k, err := r.s.kn.Get(ctx, service)
	if err != nil {
		return nil, apierror.NotFound("service %s not found", serviceName)
	}
	if k.Metadata.Labels["cloudburrow.dev/owned"] != "true" {
		return nil, apierror.FailedPrecondition(
			"service %s was not created by CloudBurrow and its revisions will not be deleted", serviceName)
	}
	kr, err := r.s.kn.Revision(ctx, service, revision)
	if err != nil {
		return nil, apierror.NotFound("revision %s not found", req.GetName())
	}
	if k.servingRevisions()[revision] || k.Status.LatestReadyRevisionName == revision && len(k.Status.Traffic) == 0 {
		return nil, apierror.FailedPrecondition(
			"revision %s is serving traffic for %s and cannot be deleted; route its traffic to another revision first",
			revision, serviceName)
	}
	if req.GetValidateOnly() {
		op := r.s.ops.Create(serviceName, req.GetName())
		_ = r.s.ops.Succeed(op.Name, FromKnativeRevision(kr, serviceName))
		return r.s.toProtoOperation(op.Name)
	}
	if err := r.s.kn.DeleteRevision(ctx, revision); err != nil {
		return nil, err
	}
	op := r.s.ops.Create(serviceName, req.GetName())
	_ = r.s.ops.Succeed(op.Name, FromKnativeRevision(kr, serviceName))
	return r.s.toProtoOperation(op.Name)
}
