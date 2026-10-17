package logging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Log views' IAM policies (#1519): logging_v2.yaml's IAMPolicy mixin binds
// GetIamPolicy, SetIamPolicy and TestIamPermissions on
// {parent}/locations/*/buckets/*/views/*, the policy that grants
// roles/logging.viewAccessor on one view. Stored, never enforced
// (ADR-0006), as every other policy CloudBurrow keeps. A view's policy goes
// with the view.

// viewIAMServer is google.iam.v1.IAMPolicy on log views.
type viewIAMServer struct {
	iampb.UnimplementedIAMPolicyServer
	a *Admin
}

// RegisterViewIAM adds the views' IAMPolicy service to g. It is apart from
// Register because google.iam.v1.IAMPolicy is registered once per server,
// and a server shared with another service's IAM (tools/coverage's) has it
// already.
func (a *Admin) RegisterViewIAM(g grpc.ServiceRegistrar) {
	iampb.RegisterIAMPolicyServer(g, viewIAMServer{a: a})
}

// viewIAMKey is where a view's policy is kept: apart from the views, so a
// listing of them never meets it.
func viewIAMKey(b bucketRef, id string) string {
	return "logging/viewiam/" + strings.TrimPrefix(viewKey(b, id), "logging/views/")
}

// viewIAMMixins are the IAMPolicy bindings on views, one set per parent
// collection.
var viewIAMMixins = func() []*annotations.HttpRule {
	var out []*annotations.HttpRule
	for _, coll := range []string{"projects", "organizations", "folders", "billingAccounts"} {
		res := "/v2/{resource=" + coll + "/*/locations/*/buckets/*/views/*}"
		for _, m := range []string{"GetIamPolicy", "SetIamPolicy", "TestIamPermissions"} {
			out = append(out, &annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy." + m,
				Pattern: &annotations.HttpRule_Post{Post: res + ":" + strings.ToLower(m[:1]) + m[1:]}, Body: "*"})
		}
	}
	return out
}()

// view resolves a policy's view, which must exist. The caller holds a.mu.
func (s viewIAMServer) view(resource string) (bucketRef, string, error) {
	ref, id, err := parseView("resource", resource)
	if err != nil {
		return bucketRef{}, "", err
	}
	if err := s.a.check(ref.parent); err != nil {
		return bucketRef{}, "", err
	}
	if _, err := s.a.getBucket(ref); err != nil {
		return bucketRef{}, "", err
	}
	if _, err := s.a.getView(ref, id); err != nil {
		return bucketRef{}, "", err
	}
	return ref, id, nil
}

func (s viewIAMServer) stored(ref bucketRef, id string) (*iampolicy.Stored, error) {
	raw, err := s.a.db.Get(viewIAMKey(ref, id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the view's policy")
	}
	var p iampolicy.Stored
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, apierror.Internal(err, "decoding the view's policy")
	}
	return &p, nil
}

func (s viewIAMServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	ref, id, err := s.view(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	cur, err := s.stored(ref, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out, err := iampolicy.Get(cur, req.GetOptions())
	return out, apierror.Wrap(err)
}

func (s viewIAMServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	ref, id, err := s.view(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	cur, err := s.stored(ref, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	next, err := iampolicy.Set(cur, req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encoding the view's policy"))
	}
	if err := s.a.db.Put(viewIAMKey(ref, id), raw); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "writing the view's policy"))
	}
	return next.Proto(), nil
}

// TestIamPermissions returns every permission asked about on a view that
// exists: nothing is enforced, so nothing is denied.
func (s viewIAMServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, _, err := s.view(req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return iampolicy.Permissions(req), nil
}

// ViewIAM is the views' IAMPolicy service, for the console.
func (a *Admin) ViewIAM() iampb.IAMPolicyServer { return viewIAMServer{a: a} }
