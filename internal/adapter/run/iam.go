package run

import (
	"context"
	"encoding/json"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// IAM policies on Cloud Run services and jobs (#1216), stored and never
// enforced, under ADR-0006 as amended by #1216.
//
// A policy is kept in a ConfigMap of its own, beside the resource, rather
// than on the resource's object: writing an annotation on the Knative
// Service or the job's ConfigMap would change the resource's
// resourceVersion, which is its etag, so a SetIamPolicy would make a caller's
// next UpdateService ABORTED. On Google a policy change leaves the resource's
// etag alone.
//
// The ConfigMap is owned by the resource (an ownerReference), so the cluster
// deletes it with the service or job. The owner's UID is also recorded on
// it: a service deleted and created again under the same name before the
// garbage collector ran must not inherit the old one's policy.

const (
	kindRunIAM   = "run-iam"
	iamCMPrefix  = "run-iam-"
	iamDataKey   = "policy.json"
	annOwnerUID  = "cloudburrow.dev/owner-uid"
	labelIAMKind = "cloudburrow.dev/run-iam-for"
)

// iamOwner is the resource a policy belongs to.
type iamOwner struct {
	// kind is "service", "job" or "workerpool".
	kind string
	// id is what the policy's ConfigMap is named by: the resource ID, and
	// for a job or worker pool the scope too (#1399).
	id string
	// name is the Cloud Run resource name.
	name string
	// apiVersion, objKind, objName and uid are the owning object's.
	apiVersion, objKind, objName, uid string
}

func (o iamOwner) cmName() string { return iamCMPrefix + o.kind + "-" + o.id }

// serviceOwner finds the Knative Service behind a Cloud Run service name.
func (s *Server) serviceOwner(ctx context.Context, name string) (iamOwner, error) {
	id, err := ServiceID(name)
	if err != nil {
		return iamOwner{}, err
	}
	k, err := s.service(ctx, name, id)
	if err != nil {
		return iamOwner{}, err
	}
	// By its Knative name: the ID for an older version's service, as its
	// policy was named then, and <id>-<scope> since (#1400).
	return iamOwner{kind: "service", id: k.Metadata.Name, name: name, apiVersion: "serving.knative.dev/v1",
		objKind: "Service", objName: k.Metadata.Name, uid: k.Metadata.UID}, nil
}

// jobOwner finds the ConfigMap behind a Cloud Run job name.
func (s *Server) jobOwner(ctx context.Context, name string) (iamOwner, error) {
	_, id, err := parseJobName(name)
	if err != nil {
		return iamOwner{}, err
	}
	cm, err := s.loadJob(ctx, name, id)
	if err != nil {
		return iamOwner{}, err
	}
	// The ConfigMap's name past its prefix: the ID alone for a job an older
	// version created, as its policy was named then, and <scope>-<id> for
	// one created since (#1399).
	return iamOwner{kind: "job", id: strings.TrimPrefix(cm.Metadata.Name, jobCMPrefix), name: name, apiVersion: "v1", objKind: "ConfigMap",
		objName: cm.Metadata.Name, uid: cm.Metadata.UID}, nil
}

// loadPolicy reads the policy stored for o. A ConfigMap left by an earlier
// resource of the same name is returned too, for its resourceVersion, but
// with no policy.
func (s *Server) loadPolicy(ctx context.Context, o iamOwner) (*iampolicy.Stored, *kconfigMap, error) {
	var cm kconfigMap
	if err := s.kn.getObject(ctx, &cm, resConfigMap, o.cmName(), "IAM policy of "+o.name); err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if cm.Metadata.Labels[labelKind] != kindRunIAM || cm.Metadata.Annotations[annOwnerUID] != o.uid {
		return nil, &cm, nil
	}
	var st iampolicy.Stored
	if err := json.Unmarshal([]byte(cm.Data[iamDataKey]), &st); err != nil {
		return nil, nil, apierror.Internal(err, "decode IAM policy of %s", o.name)
	}
	return &st, &cm, nil
}

// policyConfigMap renders the ConfigMap holding st for o.
func (s *Server) policyConfigMap(o iamOwner, st iampolicy.Stored, resourceVersion string) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", apierror.Internal(err, "encode IAM policy of %s", o.name)
	}
	md := map[string]any{
		"name":      o.cmName(),
		"namespace": s.kn.namespace(),
		"labels": map[string]string{
			labelOwned: "true", labelInstance: s.instance, labelKind: kindRunIAM, labelIAMKind: o.kind,
		},
		"annotations": map[string]string{annRunName: o.name, annOwnerUID: o.uid},
		"ownerReferences": []map[string]any{{
			"apiVersion": o.apiVersion, "kind": o.objKind, "name": o.objName, "uid": o.uid,
		}},
	}
	if resourceVersion != "" {
		md["resourceVersion"] = resourceVersion
	}
	out, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": md,
		"data": map[string]string{iamDataKey: string(raw)},
	})
	return string(out), err
}

func (s *Server) getPolicy(ctx context.Context, o iamOwner, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	st, _, err := s.loadPolicy(ctx, o)
	if err != nil {
		return nil, err
	}
	return iampolicy.Get(st, req.GetOptions())
}

func (s *Server) setPolicy(ctx context.Context, o iamOwner, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	st, cm, err := s.loadPolicy(ctx, o)
	if err != nil {
		return nil, err
	}
	next, err := iampolicy.Set(st, req)
	if err != nil {
		return nil, err
	}
	verb, rv := "create", ""
	if cm != nil {
		verb, rv = "replace", cm.Metadata.ResourceVersion
	}
	manifest, err := s.policyConfigMap(o, next, rv)
	if err != nil {
		return nil, err
	}
	if err := s.kn.writeObject(ctx, verb, manifest, "the IAM policy of "+o.name); err != nil {
		// Two concurrent first sets: the loser's read is stale, as a
		// mismatched etag is.
		if apierror.From(err).Code == codes.AlreadyExists {
			return nil, apierror.Aborted("the IAM policy of %s was changed concurrently; read it again and retry", o.name)
		}
		return nil, err
	}
	return next.Proto(), nil
}

// testPermissions answers every requested permission on a resource that
// exists, and an empty set on a well-formed name that does not: Google's
// iam_policy.proto says TestIamPermissions on a missing resource "will
// return an empty set of permissions, not a NOT_FOUND error".
func testPermissions(err error, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return &iampb.TestIamPermissionsResponse{}, nil
		}
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// GetIamPolicy is a service's stored policy; nothing enforces it.
func (s *Server) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := s.serviceOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return s.getPolicy(ctx, o, req)
}

// SetIamPolicy stores a service's policy with a new etag; a stale etag is
// ABORTED.
func (s *Server) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := s.serviceOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return s.setPolicy(ctx, o, req)
}

// TestIamPermissions returns every requested permission on a service.
func (s *Server) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	_, err := s.serviceOwner(ctx, req.GetResource())
	if err != nil && apierror.From(err).Code == codes.InvalidArgument {
		return nil, err
	}
	return testPermissions(err, req)
}

// GetIamPolicy is a job's stored policy; nothing enforces it.
func (j *JobsServer) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := j.s.jobOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return j.s.getPolicy(ctx, o, req)
}

// SetIamPolicy stores a job's policy with a new etag; a stale etag is
// ABORTED. RunJob does not read it.
func (j *JobsServer) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := j.s.jobOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return j.s.setPolicy(ctx, o, req)
}

// TestIamPermissions returns every requested permission on a job.
func (j *JobsServer) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	_, err := j.s.jobOwner(ctx, req.GetResource())
	if err != nil && apierror.From(err).Code == codes.InvalidArgument {
		return nil, err
	}
	return testPermissions(err, req)
}
