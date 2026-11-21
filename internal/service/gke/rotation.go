package gke

import (
	"context"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Credential and IP rotation (#2100), as GKE does them (re-read
// 2026-11-21: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/credential-rotation,
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/ip-rotation):
// both rotate the cluster root CA. StartIPRotation makes a new CA, trusted
// with the old one; CompleteIPRotation stops trusting the old one, and the
// pods are re-created, as GKE's node re-creation re-creates them. GKE
// completes a rotation by itself seven days after it started.
//
// The control plane's address rotates too, as documented: a cluster has
// two published host ports (the API server's and a spare one), and a
// rotation opens the other one as the new endpoint while the old one
// keeps serving; completing it refuses the old one. A cluster made with
// one published port keeps its endpoint.
//
// masterAuth.clusterCaCertificate is the new CA from the start of the
// rotation, and so are the credentials made from it; the old CA keeps
// verifying the API server until the rotation completes.

// Rotator is what a Backend does for rotations.
type Rotator interface {
	StartCredentialRotation(ctx context.Context, kind string) (Access, error)
	CompleteCredentialRotation(ctx context.Context, kind string) (Access, error)
}

// rotationState is a rotation in progress.
type rotationState struct {
	Start time.Time `json:"start"`
	// Credentials is rotate_credentials of the start (gcloud's
	// --start-credential-rotation).
	Credentials bool `json:"credentials,omitempty"`
}

// rotationAutoComplete is when GKE completes a rotation by itself.
const rotationAutoComplete = 7 * 24 * time.Hour

func (s *Server) rotator() (Rotator, error) {
	r, ok := s.backend.(Rotator)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Rotating a cluster's credentials is not available with this backend.")
	}
	return r, nil
}

// StartIPRotation starts a rotation of the cluster's root CA.
func (s *Server) StartIPRotation(_ context.Context, req *containerpb.StartIPRotationRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	rot, err := s.rotator()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if e.extra.Rotation != nil {
		// UNVERIFIED wording.
		// Docs re-read 2026-11-21: nothing more is published.
		return nil, status.Error(codes.FailedPrecondition, "A rotation is already in progress on this cluster; complete it first.")
	}
	c := e.cluster
	kind := e.kind
	c.Status = containerpb.Cluster_RECONCILING
	var access Access
	started := s.now()
	creds := req.GetRotateCredentials()
	return s.startOpLocked(p, containerpb.Operation_UPGRADE_MASTER, c.SelfLink, e, func(ctx context.Context) error {
		a, err := rot.StartCredentialRotation(ctx, kind)
		if err != nil {
			return status.Errorf(codes.Internal, "Starting the rotation failed: %v", err)
		}
		access = a
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		e.access = access
		applyAccess(c, access)
		e.extra.Rotation = &rotationState{Start: started, Credentials: creds}
	}), nil
}

// CompleteIPRotation completes the rotation: the old CA is trusted no
// longer.
func (s *Server) CompleteIPRotation(_ context.Context, req *containerpb.CompleteIPRotationRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	rot, err := s.rotator()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if e.extra.Rotation == nil {
		// UNVERIFIED wording.
		// Docs re-read 2026-11-21: nothing more is published.
		return nil, status.Error(codes.FailedPrecondition, "No rotation is in progress on this cluster.")
	}
	return s.completeRotationLocked(p, e, rot), nil
}

func (s *Server) completeRotationLocked(p parsed, e *entry, rot Rotator) *containerpb.Operation {
	c := e.cluster
	kind := e.kind
	c.Status = containerpb.Cluster_RECONCILING
	var access Access
	return s.startOpLocked(p, containerpb.Operation_UPGRADE_MASTER, c.SelfLink, e, func(ctx context.Context) error {
		a, err := rot.CompleteCredentialRotation(ctx, kind)
		if err != nil {
			return status.Errorf(codes.Internal, "Completing the rotation failed: %v", err)
		}
		access = a
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		e.access = access
		applyAccess(c, access)
		e.extra.Rotation = nil
	})
}

// completeExpiredRotationsLocked completes every rotation started seven
// days ago; s.mu is held.
func (s *Server) completeExpiredRotationsLocked(now time.Time) []*containerpb.Operation {
	rot, ok := s.backend.(Rotator)
	if !ok {
		return nil
	}
	var ops []*containerpb.Operation
	for _, e := range s.clusters {
		r := e.extra.Rotation
		if r == nil || now.Before(r.Start.Add(rotationAutoComplete)) || e.busy != "" || e.cluster.Status != containerpb.Cluster_RUNNING {
			continue
		}
		p := parsed{project: e.project, location: e.cluster.Location, cluster: e.cluster.Name}
		ops = append(ops, s.completeRotationLocked(p, e, rot))
	}
	return ops
}

// applyAccess puts a's endpoint and certificates in c's record, the
// client certificate only where the cluster issues one.
func applyAccess(c *containerpb.Cluster, a Access) {
	if c.MasterAuth == nil {
		c.MasterAuth = &containerpb.MasterAuth{}
	}
	c.Endpoint = a.Endpoint
	c.MasterAuth.ClusterCaCertificate = a.CACert
	if c.GetMasterAuth().GetClientCertificateConfig().GetIssueClientCertificate() {
		c.MasterAuth.ClientCertificate = a.ClientCert
		c.MasterAuth.ClientKey = a.ClientKey
	}
}

// Rotation is when the rotation in progress on a cluster
// (projects/P/locations/L/clusters/C) started; false when none is.
func (s *Server) Rotation(name string) (time.Time, bool) {
	p, err := parseName(name, "", "", "", "", "", "cluster")
	if err != nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil || e.extra.Rotation == nil {
		return time.Time{}, false
	}
	return e.extra.Rotation.Start, true
}
