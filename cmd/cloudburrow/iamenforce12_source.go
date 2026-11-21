package main

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// useIAMEnforcement12Source enforces the twelfth round's source tools on
// the Cloud Run port (#1879): Secure Source Manager, Developer Connect and
// Container Analysis, over gRPC and JSON (under their /securesourcemanager,
// /developerconnect and /containeranalysis prefixes). Instances,
// repositories, notes and occurrences keep their own policies.
func useIAMEnforcement12Source(e *iameval.Enforcer, run *runService) {
	if e == nil || run == nil || e.On == nil || !e.On() {
		return
	}
	ssmInstances, ssmRepos := runadapter.SourceManagerInstanceIAM(), runadapter.SourceManagerRepositoryIAM()
	e.Policies[iameval.SecureSourceManager.Name] = func(ctx context.Context, resource string) *iampolicy.Stored {
		get := ssmInstances.GetIamPolicy
		if strings.Contains(resource, "/repositories/") {
			get = ssmRepos.GetIamPolicy
		}
		return iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return get })(ctx, resource)
	}
	e.Policies[iameval.ContainerAnalysis.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		ca := run.containerAnalysis.Load()
		if ca == nil {
			return nil
		}
		return ca.IAMPolicy
	})
	run.interpose = append(run.interpose, e.Interceptor(iameval.SecureSourceManager), ssmTestRepo(e),
		e.Interceptor(iameval.DeveloperConnect), e.Interceptor(iameval.ContainerAnalysis))

	ssmMatch := rest.NewMatcher([]string{"google.cloud.securesourcemanager.v1.SecureSourceManager"}, runadapter.SourceManagerMixins())
	ssm := func(next http.Handler) http.Handler {
		bound := e.HTTP(iameval.SecureSourceManagerJSON, ssmMatch, next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if g := iameval.SSMTestRepoJSON.FindStringSubmatch(r.URL.Path); g != nil && r.Method == http.MethodPost {
				e.TestPermissionsJSON(w, r, iameval.SecureSourceManager, g[1], next)
				return
			}
			bound.ServeHTTP(w, r)
		})
	}
	dcBound := jsonEnforcer(e, iameval.DeveloperConnect, nil)
	dc := func(next http.Handler) http.Handler {
		return e.HTTPPaths(iameval.DeveloperConnect, iameval.DeveloperConnectOpsJSON, dcBound(next))
	}
	ca := jsonEnforcer(e, iameval.ContainerAnalysis, nil)
	prev := run.wrapJSON
	run.wrapJSON = func(next http.Handler) http.Handler {
		h := sourceManagerGit(e, stripForMatch("/securesourcemanager", ssm,
			stripForMatch("/developerconnect", dc,
				stripForMatch(containerAnalysisJSONPrefix, ca, next))))
		if prev != nil {
			h = prev(h)
		}
		return h
	}
}

// ssmTestRepo answers Secure Source Manager's TestIamPermissionsRepo,
// whose page needs no permission, with what the caller holds.
func ssmTestRepo(e *iameval.Enforcer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod != iameval.SSMTestIamPermissionsRepo {
			return handler(ctx, req)
		}
		return e.TestPermissionsGRPC(ctx, iameval.SecureSourceManager, req, handler)
	}
}
