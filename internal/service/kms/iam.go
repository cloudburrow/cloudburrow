package kms

import (
	"context"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// iamServer serves the google.iam.v1.IAMPolicy mixin on the KMS port (#428).
// Policies on key rings, crypto keys, a location's ekmConfig and EKM
// connections (#1469) are stored on their records and never enforced: no KMS
// RPC, Encrypt and Decrypt included, reads one (ADR-0006, as amended by
// #421). Nothing is inherited: a key's policy is its own.
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

// IAMPolicy is the IAMPolicy mixin Register serves, for an in-process caller:
// the console's Permissions tab reads and writes policies through it, the
// methods an SDK's call reaches (#793).
func (s *Server) IAMPolicy() iampb.IAMPolicyServer { return &iamServer{s: s} }

// iamKind is the kind of record a policy is stored on.
type iamKind int

const (
	iamKeyRing iamKind = iota
	iamCryptoKey
	iamEkmConfig
	iamEkmConnection
)

// iamResource is what a resource name routes to.
type iamResource struct {
	key  string // the store key of the record
	name string // the resource name
	kind iamKind
}

func (r iamResource) label() string {
	return [...]string{"key ring", "crypto key", "EkmConfig", "EkmConnection"}[r.kind]
}

// iamTarget routes resource. Google serves IAM on key rings, crypto keys,
// import jobs, ekmConfig and ekmConnections (cloudkms_v1.yaml:75-105);
// IAM on import jobs is UNIMPLEMENTED, never an empty policy, as
// CloudBurrow keeps no policy on them. Anything else, a CryptoKeyVersion
// included, has no IAM on Google and is INVALID_ARGUMENT (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/kms/docs/reference/rest/v1/projects.locations.keyRings.cryptoKeys
func iamTarget(resource string) (iamResource, error) {
	if resource == "" {
		return iamResource{}, apierror.InvalidArgument("resource is required")
	}
	parts := strings.Split(resource, "/")
	switch {
	case len(parts) == 6 && parts[4] == "keyRings":
		if err := parseKeyRing("resource", resource); err != nil {
			return iamResource{}, err
		}
		return iamResource{key: dbKey(ringPrefix, resource), name: resource, kind: iamKeyRing}, nil
	case len(parts) == 8 && parts[6] == "cryptoKeys":
		if err := parseCryptoKey("resource", resource); err != nil {
			return iamResource{}, err
		}
		return iamResource{key: dbKey(keyPrefix, resource), name: resource, kind: iamCryptoKey}, nil
	case len(parts) == 8 && parts[6] == "importJobs":
		return iamResource{}, apierror.Unimplemented("IAM on import jobs is not implemented: an ImportJob keeps no IAM policy locally")
	case len(parts) == 5 && parts[4] == "ekmConfig":
		if _, err := parseEkmConfig("resource", resource); err != nil {
			return iamResource{}, err
		}
		return iamResource{key: dbKey(ekmConfigPrefix, resource), name: resource, kind: iamEkmConfig}, nil
	case len(parts) == 6 && parts[4] == "ekmConnections":
		if err := parseEkmConnection("resource", resource); err != nil {
			return iamResource{}, err
		}
		return iamResource{key: dbKey(ekmConnectionPrefix, resource), name: resource, kind: iamEkmConnection}, nil
	}
	return iamResource{}, apierror.InvalidArgument("resource %q is not a key ring, crypto key, ekmConfig or EkmConnection name", resource)
}

// policyOf reads the resource's stored policy. found is false only for a
// record that is absent; a failed read is an error (#389). A location's
// ekmConfig always exists ("the EkmConfig singleton", GetEkmConfig), with
// an empty policy until one is set.
func (i *iamServer) policyOf(r iamResource) (policy *iampolicy.Stored, found bool, err error) {
	switch r.kind {
	case iamKeyRing:
		var rec keyRing
		found, err = i.s.get(r.key, &rec)
		return rec.IAMPolicy, found, err
	case iamCryptoKey:
		var rec cryptoKey
		found, err = i.s.get(r.key, &rec)
		return rec.IAMPolicy, found, err
	case iamEkmConfig:
		var rec ekmConfig
		_, err = i.s.get(r.key, &rec)
		return rec.IAMPolicy, err == nil, err
	}
	var rec ekmConnection
	found, err = i.s.get(r.key, &rec)
	return rec.IAMPolicy, found, err
}

// setPolicy applies req to the record r names and writes it back. The
// caller holds the server's lock.
func (i *iamServer) setPolicy(r iamResource, req *iampb.SetIamPolicyRequest) (iampolicy.Stored, error) {
	switch r.kind {
	case iamKeyRing:
		var rec keyRing
		return setOn(i.s, r, &rec, &rec.IAMPolicy, req)
	case iamCryptoKey:
		var rec cryptoKey
		return setOn(i.s, r, &rec, &rec.IAMPolicy, req)
	case iamEkmConfig:
		rec := ekmConfig{Name: r.name}
		if _, err := i.s.get(r.key, &rec); err != nil {
			return iampolicy.Stored{}, apierror.Wrap(err)
		}
		return apply(i.s, r, &rec, &rec.IAMPolicy, req)
	}
	var rec ekmConnection
	return setOn(i.s, r, &rec, &rec.IAMPolicy, req)
}

// setOn loads the record, which must exist, and applies req to it.
func setOn(s *Server, r iamResource, rec any, policy **iampolicy.Stored, req *iampb.SetIamPolicyRequest) (iampolicy.Stored, error) {
	if err := s.load(r.key, rec, r.label(), r.name); err != nil {
		return iampolicy.Stored{}, err
	}
	return apply(s, r, rec, policy, req)
}

// apply sets *policy from req and writes rec back.
func apply(s *Server, r iamResource, rec any, policy **iampolicy.Stored, req *iampb.SetIamPolicyRequest) (iampolicy.Stored, error) {
	next, err := iampolicy.Set(*policy, req)
	if err != nil {
		return iampolicy.Stored{}, apierror.Wrap(err)
	}
	*policy = &next
	if err := s.put(r.key, rec); err != nil {
		return iampolicy.Stored{}, apierror.Wrap(err)
	}
	return next, nil
}

func (i *iamServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	r, err := iamTarget(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	policy, found, err := i.policyOf(r)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if !found {
		// The code is UNVERIFIED.
		// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/iam/v1/iam_policy.proto
		return nil, apierror.Wrap(apierror.NotFound("%s %s not found", r.label(), req.GetResource()))
	}
	p, err := iampolicy.Get(policy, req.GetOptions())
	return p, apierror.Wrap(err)
}

// SetIamPolicy reads, applies and writes under the server's lock, so two
// read-modify-write loops cannot both win against one etag.
func (i *iamServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	r, err := iamTarget(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	next, err := i.setPolicy(r, req)
	if err != nil {
		return nil, err
	}
	return next.Proto(), nil
}

// TestIamPermissions returns every requested permission on a ring, key or
// EkmConnection that exists, or on any location's ekmConfig, since nothing is enforced. On a well-formed name that does not
// exist it returns an empty set, as Google documents: "If the resource does
// not exist, this will return an empty set of permissions, not a NOT_FOUND
// error" (cloudkms_v1.yaml:59-64, and iam_policy.proto's
// TestIamPermissions comment, read 2026-10-17). Cloud Tasks
// answers NOT_FOUND there; the difference follows each service's spec.
func (i *iamServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	r, err := iamTarget(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	_, found, err := i.policyOf(r)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if !found {
		return &iampb.TestIamPermissionsResponse{}, nil
	}
	return iampolicy.Permissions(req), nil
}
