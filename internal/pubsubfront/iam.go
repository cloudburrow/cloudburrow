package pubsubfront

// IAM policies on topics, subscriptions, snapshots and schemas (#1367),
// stored and never enforced, as ADR-0006 decides for every service that
// serves them. The emulator serves none of google.iam.v1.IAMPolicy: over
// gRPC it is UNIMPLEMENTED, and over REST its answer is a plain-text 404. So
// the front answers GetIamPolicy, SetIamPolicy and TestIamPermissions itself,
// over gRPC and REST (`:getIamPolicy`, `:setIamPolicy`,
// `:testIamPermissions`), through the shared internal/iampolicy: a set
// returns a new etag and a stale one is ABORTED, conditions and audit
// configs are UNIMPLEMENTED naming the field, and a version 3 policy without
// conditions is accepted.
//
// The resource must exist in the emulator (NOT_FOUND otherwise), and its
// policy goes with it: a delete through the front, an expiry and a create of
// the same name all drop it, as on Google, where a resource made again has
// no policy. The policies are kept in the front's state file (state.go), so
// they have the lifetime the emulator's resources have.
//
// TestIamPermissions answers, of the permissions asked, those Pub/Sub
// defines on that kind of resource: nothing is enforced, so nothing is
// denied, and a permission that is not Pub/Sub's on the resource is not one
// the caller could hold on it.

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

const (
	iamService    = "/google.iam.v1.IAMPolicy/"
	schemaService = "/google.pubsub.v1.SchemaService/"
)

// iamPermissions are the permissions Pub/Sub defines on each kind of
// resource, from Google's IAM permissions reference
// (https://cloud.google.com/iam/docs/permissions-reference, pubsub.*), less
// the ones checked on the project (create, list).
var iamPermissions = map[string][]string{
	"topics": {
		"pubsub.topics.attachSubscription", "pubsub.topics.delete", "pubsub.topics.detachSubscription",
		"pubsub.topics.get", "pubsub.topics.getIamPolicy", "pubsub.topics.publish",
		"pubsub.topics.setIamPolicy", "pubsub.topics.update", "pubsub.topics.updateTag",
	},
	"subscriptions": {
		"pubsub.subscriptions.consume", "pubsub.subscriptions.delete", "pubsub.subscriptions.get",
		"pubsub.subscriptions.getIamPolicy", "pubsub.subscriptions.setIamPolicy", "pubsub.subscriptions.update",
	},
	"snapshots": {
		"pubsub.snapshots.delete", "pubsub.snapshots.get", "pubsub.snapshots.getIamPolicy",
		"pubsub.snapshots.seek", "pubsub.snapshots.setIamPolicy", "pubsub.snapshots.update",
	},
	"schemas": {
		"pubsub.schemas.attach", "pubsub.schemas.commit", "pubsub.schemas.delete", "pubsub.schemas.get",
		"pubsub.schemas.getIamPolicy", "pubsub.schemas.listRevisions", "pubsub.schemas.rollback",
		"pubsub.schemas.setIamPolicy", "pubsub.schemas.validate",
	},
}

// iamResource is the kind of resource a policy's resource name names:
// "topics", "subscriptions", "snapshots" or "schemas".
func iamResource(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" || parts[3] == "" || iamPermissions[parts[2]] == nil {
		return "", apierror.InvalidArgument("resource %q is not a Pub/Sub topic, subscription, snapshot or schema "+
			"(projects/{project}/topics/{topic}, and so on)", name)
	}
	return parts[2], nil
}

// lookupResource asks the emulator whether a resource exists: its own NOT_FOUND
// is the answer when it does not.
func (f *Front) lookupResource(ctx context.Context, kind, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var err error
	switch kind {
	case "topics":
		_, err = f.topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	case "subscriptions":
		_, err = f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	case "snapshots":
		_, err = pubsubpb.NewSubscriberClient(f.upstream).GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name})
	case "schemas":
		_, err = pubsubpb.NewSchemaServiceClient(f.upstream).GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_BASIC})
	}
	return err
}

// iamTarget checks a request's resource and that it exists.
func (f *Front) iamTarget(ctx context.Context, name string) (string, error) {
	kind, err := iamResource(name)
	if err != nil {
		return "", err
	}
	f.sawProjectID(projectOf(name))
	exists := f.exists
	if exists == nil {
		exists = f.lookupResource
	}
	if err := exists(ctx, kind, name); err != nil {
		return "", err
	}
	return kind, nil
}

// storedIAM is a resource's policy, nil for none.
func (f *Front) storedIAM(name string) *iampolicy.Stored {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.iam[name]; ok {
		return &p
	}
	return nil
}

// GetIamPolicy is google.iam.v1.IAMPolicy's, on a Pub/Sub resource.
func (f *Front) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	if _, err := f.iamTarget(ctx, req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := iampolicy.Get(f.storedIAM(req.GetResource()), req.GetOptions())
	return p, apierror.Wrap(err)
}

// SetIamPolicy stores a policy, which is written to the state file before it
// is answered. Stored, never enforced.
func (f *Front) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	if _, err := f.iamTarget(ctx, req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	f.mu.Lock()
	var current *iampolicy.Stored
	if p, ok := f.iam[req.GetResource()]; ok {
		current = &p
	}
	next, err := iampolicy.Set(current, req)
	if err == nil {
		f.iam[req.GetResource()] = next
	}
	f.mu.Unlock()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	f.persist()
	return next.Proto(), nil
}

// TestIamPermissions answers, of the permissions asked, the ones Pub/Sub
// defines on the resource, in the order asked: nothing is enforced, so
// nothing is denied. On a resource that does not exist it answers no
// permissions: iam_policy.proto says "If the resource does not exist, this
// will return an empty set of permissions, not a `NOT_FOUND` error" (#1595).
func (f *Front) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	kind, err := f.iamTarget(ctx, req.GetResource())
	if err != nil && apierror.From(err).Code == codes.NotFound {
		return &iampb.TestIamPermissionsResponse{}, nil
	}
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all := iampolicy.Permissions(req).GetPermissions()
	out := &iampb.TestIamPermissionsResponse{}
	for _, p := range all {
		if slices.Contains(iamPermissions[kind], p) && !slices.Contains(out.Permissions, p) {
			out.Permissions = append(out.Permissions, p)
		}
	}
	return out, nil
}

// dropIAMLocked forgets a resource's policy, under f.mu, and reports
// whether there was one; the caller writes the state.
func (f *Front) dropIAMLocked(name string) bool {
	_, ok := f.iam[name]
	delete(f.iam, name)
	return ok
}

// dropIAM forgets a resource's policy and writes the state if it had one.
func (f *Front) dropIAM(name string) {
	if name == "" {
		return
	}
	f.mu.Lock()
	had := f.dropIAMLocked(name)
	f.mu.Unlock()
	if had {
		f.persist()
	}
}

// handleIAM serves one google.iam.v1.IAMPolicy call, a unary one, over gRPC.
func (f *Front) handleIAM(ctx context.Context, ss grpc.ServerStream, method string) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var resp proto.Message
	var err error
	switch method[len(iamService):] {
	case "GetIamPolicy":
		var req iampb.GetIamPolicyRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return apierror.InvalidArgument("the request is not a google.iam.v1.GetIamPolicyRequest: %v", err)
		}
		resp, err = f.GetIamPolicy(ctx, &req)
	case "SetIamPolicy":
		var req iampb.SetIamPolicyRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return apierror.InvalidArgument("the request is not a google.iam.v1.SetIamPolicyRequest: %v", err)
		}
		resp, err = f.SetIamPolicy(ctx, &req)
	case "TestIamPermissions":
		var req iampb.TestIamPermissionsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return apierror.InvalidArgument("the request is not a google.iam.v1.TestIamPermissionsRequest: %v", err)
		}
		resp, err = f.TestIamPermissions(ctx, &req)
	default:
		return apierror.Unimplemented("method %s is not implemented", strings.TrimPrefix(method, "/"))
	}
	if err != nil {
		return err
	}
	b, err := proto.Marshal(resp)
	if err != nil {
		return apierror.Internal(err, "encode the answer")
	}
	out := frame(b)
	return ss.SendMsg(&out)
}

// iamPath is the resource and IAM verb a REST path names:
// /v1/projects/{p}/{topics|subscriptions|snapshots|schemas}/{id}:{verb}.
func iamPath(path string) (name, verb string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v1/")
	if !found {
		return "", "", false
	}
	name, verb, found = strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	switch verb {
	case "getIamPolicy", "setIamPolicy", "testIamPermissions":
	default:
		return "", "", false
	}
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || iamPermissions[parts[2]] == nil {
		return "", "", false
	}
	return name, verb, true
}

// serveRESTIAM answers a REST IAM call, as gRPC's are answered: Pub/Sub's
// HTTP rules are GET for getIamPolicy (with options.requestedPolicyVersion
// as a query parameter) and POST with the request as the body for the
// others. A POST of getIamPolicy is accepted too, as Google's front accepts
// it.
func (f *Front) serveRESTIAM(w http.ResponseWriter, r *http.Request, name, verb string) {
	var body []byte
	if r.Method == http.MethodPost {
		b, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return
		}
		body = b
	} else if r.Method != http.MethodGet || verb != "getIamPolicy" {
		writeRESTError(w, apierror.Unimplemented("%s %s is not a Pub/Sub method", r.Method, r.URL.Path))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	var resp proto.Message
	var err error
	switch verb {
	case "getIamPolicy":
		var req iampb.GetIamPolicyRequest
		if err := restJSON.Unmarshal(body, &req); err != nil {
			writeRESTError(w, apierror.InvalidArgument("the body is not a GetIamPolicyRequest: %v", err))
			return
		}
		if v := r.URL.Query().Get("options.requestedPolicyVersion"); v != "" {
			n, perr := strconv.ParseInt(v, 10, 32)
			if perr != nil {
				writeRESTError(w, apierror.InvalidArgument("options.requestedPolicyVersion %q is not a number", v))
				return
			}
			req.Options = &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(n)}
		}
		req.Resource = name
		resp, err = f.GetIamPolicy(r.Context(), &req)
	case "setIamPolicy":
		var req iampb.SetIamPolicyRequest
		if err := restJSON.Unmarshal(body, &req); err != nil {
			writeRESTError(w, apierror.InvalidArgument("the body is not a SetIamPolicyRequest: %v", err))
			return
		}
		req.Resource = name
		resp, err = f.SetIamPolicy(r.Context(), &req)
	case "testIamPermissions":
		var req iampb.TestIamPermissionsRequest
		if err := restJSON.Unmarshal(body, &req); err != nil {
			writeRESTError(w, apierror.InvalidArgument("the body is not a TestIamPermissionsRequest: %v", err))
			return
		}
		req.Resource = name
		resp, err = f.TestIamPermissions(r.Context(), &req)
	}
	if err != nil {
		writeRESTError(w, err)
		return
	}
	b, err := protojson.Marshal(resp)
	if err != nil {
		writeRESTError(w, apierror.Internal(err, "encode the answer"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}
