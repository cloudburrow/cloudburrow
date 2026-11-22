package vertexai

// The IAM policies of Colab Enterprise's runtime templates (#2105):
// google.iam.v1.IAMPolicy on this endpoint, for
// projects/*/locations/*/notebookRuntimeTemplates/* (the only resource
// with a policy served here), over gRPC and REST.
//
// Documented behaviour (https://docs.cloud.google.com/colab/docs/manage-access-runtime-template;
// .../reference/rest/v1/projects.locations.notebookRuntimeTemplates/getIamPolicy;
// the v1 discovery document, revision 20261114; read 2026-11-22):
//   - getIamPolicy is POST v1/{+resource}:getIamPolicy with
//     options.requestedPolicyVersion in the query, setIamPolicy POST with
//     a SetIamPolicyRequest body, testIamPermissions POST with permissions
//     in the query;
//   - getIamPolicy "Returns an empty policy if the resource exists and
//     does not have a policy set"; it needs
//     aiplatform.notebookRuntimeTemplates.getIamPolicy on the template,
//     setIamPolicy .setIamPolicy;
//   - sharing a template lets its principals make runtimes from it (the
//     Notebook Runtime User role, roles/aiplatform.notebookRuntimeUser,
//     holds aiplatform.notebookRuntimeTemplates.apply); "You can't share
//     your runtimes".
//
// With IAM enforcement on, the policies are read by the enforcer
// (cmd/cloudburrow/iamenforce8.go) beside the project's.

import (
	"context"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/api/annotations"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

const templateIAMPath = "/v1/{resource=projects/*/locations/*/notebookRuntimeTemplates/*}"

// NotebookIAMMixins are the REST bindings of the templates' IAM methods,
// for the transcoder and the enforcer's JSON matcher.
func NotebookIAMMixins() []*annotations.HttpRule {
	post := func(sel, verb, body string) *annotations.HttpRule {
		return &annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy." + sel, Pattern: &annotations.HttpRule_Post{Post: templateIAMPath + ":" + verb}, Body: body}
	}
	return []*annotations.HttpRule{
		post("GetIamPolicy", "getIamPolicy", ""),
		post("SetIamPolicy", "setIamPolicy", "*"),
		post("TestIamPermissions", "testIamPermissions", ""),
	}
}

type notebookIAM struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

// templateForIAM checks resource is a template that exists; the caller
// holds st.mu.
func (st *notebookStore) templateForIAM(resource string) error {
	if !strings.Contains(resource, "/notebookRuntimeTemplates/") {
		return apierror.InvalidArgument("resource %q: IAM policies on this endpoint are served for notebook runtime templates (projects/{project}/locations/{location}/notebookRuntimeTemplates/{template}) only", resource)
	}
	_, err := st.template(resource)
	return err
}

func (x *notebookIAM) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.templateForIAM(req.GetResource()); err != nil {
		return nil, grpcError(err)
	}
	p, err := iampolicy.Get(st.policies[req.GetResource()], req.GetOptions())
	return p, grpcError(err)
}

func (x *notebookIAM) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.templateForIAM(req.GetResource()); err != nil {
		return nil, grpcError(err)
	}
	next, err := iampolicy.Set(st.policies[req.GetResource()], req)
	if err != nil {
		return nil, grpcError(err)
	}
	st.policies[req.GetResource()] = &next
	st.save()
	return next.Proto(), nil
}

func (x *notebookIAM) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.templateForIAM(req.GetResource()); err != nil {
		return nil, grpcError(err)
	}
	return iampolicy.Permissions(req), nil
}

// NotebookTemplatePolicy is a runtime template's policy, nil when it has
// none, for the IAM enforcer.
func (s *Server) NotebookTemplatePolicy(resource string) *iampolicy.Stored {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.policies[resource]
}
