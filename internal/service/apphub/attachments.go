package apphub

import (
	"context"
	"sort"
	"strconv"
	"strings"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ---- service project attachments ----

// globalParent checks a name or parent is projects/P/locations/global:
// "Service project attachments only support global location" (gcloud
// apphub service-projects add; lookupServiceProjectAttachment's reference).
func globalParent(field, parent string) (string, error) {
	p, loc, err := checkParent(field, parent)
	if err != nil {
		return "", err
	}
	if loc != "global" {
		return "", apierror.InvalidArgument("%s %q: service project attachments only support the global location", field, parent)
	}
	return p, nil
}

// attachmentOf is the attachment of service project id, wherever it is;
// the caller holds mu.
func (s *Server) attachmentOf(id string) *apphubpb.ServiceProjectAttachment {
	for k, a := range s.attachments {
		if strings.HasSuffix(k, "/serviceProjectAttachments/"+id) {
			return a
		}
	}
	return nil
}

func (s *Server) CreateServiceProjectAttachment(_ context.Context, req *apphubpb.CreateServiceProjectAttachmentRequest) (*longrunningpb.Operation, error) {
	host, err := globalParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	in := req.GetServiceProjectAttachment()
	ref, ok := strings.CutPrefix(in.GetServiceProject(), "projects/")
	if !ok || ref == "" || strings.Contains(ref, "/") {
		return nil, apierror.InvalidArgument("service_project_attachment.service_project %q must be projects/{project id or number}", in.GetServiceProject())
	}
	if _, _, err := s.project(host); err != nil {
		return nil, err
	}
	id, number, err := s.project(ref)
	if err != nil {
		return nil, err
	}
	// The attachment's ID is the service project's ID (the proto's name
	// format, .../serviceProjectAttachments/{service-project-id}).
	if req.GetServiceProjectAttachmentId() != id {
		return nil, apierror.InvalidArgument("service_project_attachment_id %q must be the service project's ID, %q", req.GetServiceProjectAttachmentId(), id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	name := req.GetParent() + "/serviceProjectAttachments/" + id
	if _, ok := s.attachments[name]; ok {
		return nil, apierror.AlreadyExists("ServiceProjectAttachment %s already exists", name)
	}
	// "A service project can only be connected to one host project at a
	// time" (key-concepts). The code is UNVERIFIED.
	// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/key-concepts.
	if other := s.attachmentOf(id); other != nil {
		return nil, apierror.AlreadyExists("project %s is already attached to host project %s", id, strings.Split(other.GetName(), "/")[1])
	}
	a := &apphubpb.ServiceProjectAttachment{Name: name, ServiceProject: "projects/" + strconv.FormatInt(number, 10),
		CreateTime: timestamppb.New(s.now()), Uid: newUID(), State: apphubpb.ServiceProjectAttachment_ACTIVE}
	s.attachments[name] = a
	return s.done(req.GetParent(), "create", name, req.GetRequestId(), clone(a))
}

func (s *Server) GetServiceProjectAttachment(_ context.Context, req *apphubpb.GetServiceProjectAttachmentRequest) (*apphubpb.ServiceProjectAttachment, error) {
	if !attachmentRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/global/serviceProjectAttachments/{service_project}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attachments[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("ServiceProjectAttachment %s not found", req.GetName())
	}
	return clone(a), nil
}

func (s *Server) ListServiceProjectAttachments(_ context.Context, req *apphubpb.ListServiceProjectAttachmentsRequest) (*apphubpb.ListServiceProjectAttachmentsResponse, error) {
	if _, err := globalParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*apphubpb.ServiceProjectAttachment
	for _, k := range sortedKeys(s.attachments, req.GetParent()+"/serviceProjectAttachments/") {
		all = append(all, clone(s.attachments[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("apphub-spa:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListServiceProjectAttachmentsResponse{ServiceProjectAttachments: out, NextPageToken: next}, nil
}

func (s *Server) DeleteServiceProjectAttachment(_ context.Context, req *apphubpb.DeleteServiceProjectAttachmentRequest) (*longrunningpb.Operation, error) {
	m := attachmentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/global/serviceProjectAttachments/{service_project}", req.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, ok := s.attachments[req.GetName()]; !ok {
		return nil, apierror.NotFound("ServiceProjectAttachment %s not found", req.GetName())
	}
	delete(s.attachments, req.GetName())
	return s.done("projects/"+m[1]+"/locations/"+m[2], "delete", req.GetName(), req.GetRequestId(), &emptypb.Empty{})
}

// DetachServiceProjectAttachment detaches the service project named from
// its host project, called from the service project.
func (s *Server) DetachServiceProjectAttachment(_ context.Context, req *apphubpb.DetachServiceProjectAttachmentRequest) (*apphubpb.DetachServiceProjectAttachmentResponse, error) {
	ref, err := globalParent("name", req.GetName())
	if err != nil {
		return nil, err
	}
	id, _, err := s.project(ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attachmentOf(id)
	if a == nil {
		return nil, apierror.NotFound("project %s is not attached to a host project", id)
	}
	delete(s.attachments, a.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &apphubpb.DetachServiceProjectAttachmentResponse{}, nil
}

// LookupServiceProjectAttachment answers the project's attachment, "if
// exists, empty otherwise".
func (s *Server) LookupServiceProjectAttachment(_ context.Context, req *apphubpb.LookupServiceProjectAttachmentRequest) (*apphubpb.LookupServiceProjectAttachmentResponse, error) {
	ref, err := globalParent("name", req.GetName())
	if err != nil {
		return nil, err
	}
	id, _, err := s.project(ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &apphubpb.LookupServiceProjectAttachmentResponse{}
	if a := s.attachmentOf(id); a != nil {
		out.ServiceProjectAttachment = clone(a)
	}
	return out, nil
}

// ---- boundary ----

// boundary is project's boundary, the stored one or the one it has
// without an update; the caller holds mu.
func (s *Server) boundary(project, name string) *apphubpb.Boundary {
	if s.opts.Projects != nil {
		if f := s.opts.Projects.ManagedFolder(project); f != "" {
			// The management project of an app-enabled folder: its
			// boundary is the folder's, set by the app-management
			// capability. crm_node names the folder (UNVERIFIED: the
			// proto's format names projects only).
			// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/set-up-app-hub.
			return &apphubpb.Boundary{Name: name, Type: apphubpb.Boundary_MANAGED_AUTOMATIC,
				Scope: &apphubpb.Boundary_CrmNode{CrmNode: f}}
		}
	}
	if b, ok := s.boundaries[name]; ok {
		return clone(b)
	}
	// A host project whose service projects are attached by hand
	// (UNVERIFIED: the type a project that never set one answers).
	// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/key-concepts.
	return &apphubpb.Boundary{Name: name, Type: apphubpb.Boundary_MANUAL}
}

func (s *Server) GetBoundary(_ context.Context, req *apphubpb.GetBoundaryRequest) (*apphubpb.Boundary, error) {
	m := boundaryRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/global/boundary", req.GetName())
	}
	if m[2] != "global" {
		return nil, apierror.InvalidArgument("name %q: a boundary is in the global location", req.GetName())
	}
	id, _, err := s.project(m[1])
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundary(id, req.GetName()), nil
}

func (s *Server) UpdateBoundary(_ context.Context, req *apphubpb.UpdateBoundaryRequest) (*longrunningpb.Operation, error) {
	in := req.GetBoundary()
	m := boundaryRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("boundary.name %q must be projects/{project}/locations/global/boundary", in.GetName())
	}
	if m[2] != "global" {
		return nil, apierror.InvalidArgument("boundary.name %q: a boundary is in the global location", in.GetName())
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	id, _, err := s.project(m[1])
	if err != nil {
		return nil, err
	}
	node := in.GetCrmNode()
	if node != "" {
		ref, ok := strings.CutPrefix(node, "projects/")
		if !ok {
			return nil, apierror.InvalidArgument("boundary.crm_node %q must be projects/{project-number} or projects/{project-id}", node)
		}
		nid, nnum, err := s.project(ref)
		if err != nil {
			return nil, err
		}
		node = "projects/" + nid
		if s.opts.Projects != nil {
			// Answered by number, as a service project attachment's
			// service_project is (UNVERIFIED for crm_node).
			// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/reference/rest/v1/projects.locations/updateBoundary.
			node = "projects/" + strconv.FormatInt(nnum, 10)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur := s.boundary(id, in.GetName())
	if cur.GetType() == apphubpb.Boundary_MANAGED_AUTOMATIC {
		return nil, apierror.FailedPrecondition("the boundary of management project %s is its folder's, set by the folder's app-management capability", id)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	for _, p := range paths {
		if p != "crm_node" && p != "*" {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated on a boundary", p)
		}
	}
	now := timestamppb.New(s.now())
	if cur.GetCreateTime() == nil {
		cur.CreateTime = now
	}
	cur.UpdateTime = now
	if node == "" {
		cur.Scope = nil
		cur.Type = apphubpb.Boundary_MANUAL
	} else {
		cur.Scope = &apphubpb.Boundary_CrmNode{CrmNode: node}
		cur.Type = apphubpb.Boundary_AUTOMATIC
	}
	s.boundaries[in.GetName()] = cur
	return s.done("projects/"+m[1]+"/locations/global", "update", in.GetName(), req.GetRequestId(), clone(cur))
}

// boundaryProjects are the IDs of the projects whose resources host's
// App Hub discovers: its attached service projects, its boundary's CRM
// node, and, for a management project, its folder's projects. The caller
// holds mu.
func (s *Server) boundaryProjects(host string) []string {
	seen := map[string]bool{}
	for k := range s.attachments {
		if strings.HasPrefix(k, "projects/"+host+"/locations/global/serviceProjectAttachments/") {
			seen[k[strings.LastIndex(k, "/")+1:]] = true
		}
	}
	b := s.boundary(host, "projects/"+host+"/locations/global/boundary")
	if node := b.GetCrmNode(); node != "" {
		if f, ok := strings.CutPrefix(node, "folders/"); ok && s.opts.Projects != nil {
			for _, p := range s.opts.Projects.FolderProjects("folders/" + f) {
				seen[p] = true
			}
		} else if ref, ok := strings.CutPrefix(node, "projects/"); ok {
			if id, _, err := s.project(ref); err == nil {
				seen[id] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
