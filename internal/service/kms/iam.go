package kms

import (
	"context"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// iamServer serves the google.iam.v1.IAMPolicy mixin on the KMS port (#428).
// Policies on key rings and crypto keys are stored on their records and never
// enforced: no KMS RPC, Encrypt and Decrypt included, reads one (ADR-0006, as
// amended by #421). Nothing is inherited: a key's policy is its own.
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

// iamResource is what a resource name routes to.
type iamResource struct {
	key  string // the store key of the ring or key record
	kind string // "key ring" or "crypto key"
	ring bool
}

// iamTarget routes resource. Google serves IAM on key rings, crypto keys,
// import jobs, ekmConfig and ekmConnections (cloudkms_v1.yaml:75-105);
// CloudBurrow serves none of the last three, so IAM on them is UNIMPLEMENTED,
// never an empty policy. Anything else, a CryptoKeyVersion included, has no
// IAM on Google and is INVALID_ARGUMENT (UNVERIFIED).
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
		return iamResource{key: dbKey(ringPrefix, resource), kind: "key ring", ring: true}, nil
	case len(parts) == 8 && parts[6] == "cryptoKeys":
		if err := parseCryptoKey("resource", resource); err != nil {
			return iamResource{}, err
		}
		return iamResource{key: dbKey(keyPrefix, resource), kind: "crypto key"}, nil
	case len(parts) == 8 && parts[6] == "importJobs":
		return iamResource{}, apierror.Unimplemented("IAM on import jobs is not implemented: CloudBurrow serves no import jobs")
	case len(parts) == 5 && parts[4] == "ekmConfig":
		return iamResource{}, apierror.Unimplemented("IAM on ekmConfig is not implemented: CloudBurrow serves no EKM configuration")
	case len(parts) == 6 && parts[4] == "ekmConnections":
		return iamResource{}, apierror.Unimplemented("IAM on ekmConnections is not implemented: CloudBurrow serves no EKM connections")
	}
	return iamResource{}, apierror.InvalidArgument("resource %q is not a key ring or crypto key name", resource)
}

// policyOf reads the resource's stored policy. found is false only for a
// record that is absent; a failed read is an error (#389).
func (i *iamServer) policyOf(r iamResource) (policy *iampolicy.Stored, found bool, err error) {
	if r.ring {
		var rec keyRing
		found, err = i.s.get(r.key, &rec)
		return rec.IAMPolicy, found, err
	}
	var rec cryptoKey
	found, err = i.s.get(r.key, &rec)
	return rec.IAMPolicy, found, err
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
		return nil, apierror.Wrap(apierror.NotFound("%s %s not found", r.kind, req.GetResource()))
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
	var next iampolicy.Stored
	if r.ring {
		var rec keyRing
		if err := i.s.load(r.key, &rec, r.kind, req.GetResource()); err != nil {
			return nil, err
		}
		if next, err = iampolicy.Set(rec.IAMPolicy, req); err != nil {
			return nil, apierror.Wrap(err)
		}
		rec.IAMPolicy = &next
		if err := i.s.put(r.key, rec); err != nil {
			return nil, apierror.Wrap(err)
		}
	} else {
		var rec cryptoKey
		if err := i.s.load(r.key, &rec, r.kind, req.GetResource()); err != nil {
			return nil, err
		}
		if next, err = iampolicy.Set(rec.IAMPolicy, req); err != nil {
			return nil, apierror.Wrap(err)
		}
		rec.IAMPolicy = &next
		if err := i.s.put(r.key, rec); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	return next.Proto(), nil
}

// TestIamPermissions returns every requested permission on a ring or key
// that exists, since nothing is enforced. On a well-formed name that does not
// exist it returns an empty set, as Google documents: "If the resource does
// not exist, this will return an empty set of permissions, not a NOT_FOUND
// error" (cloudkms_v1.yaml:59-64; measured behaviour UNVERIFIED). Cloud Tasks
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
