package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmd "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// IAM enforcement (#1879): with `up --iam-enforcement=on`, the methods
// internal/iameval maps check their caller's permission on the resource
// against the stored allow policies, the resource's own, its service
// parents' and its project's, folders' and organization's, before they run,
// and refuse with Google's error. The caller is the account its bearer
// token was minted for; the instance's own account (the ADC fixture's) and a
// request with no bearer token are the project owner, and a bearer token
// this instance did not mint is UNAUTHENTICATED.

// registrySource is the evaluator's view of Resource Manager.
type registrySource struct {
	reg func() *resourcemanager.Registry
}

func (s registrySource) registry() *resourcemanager.Registry {
	if s.reg == nil {
		return nil
	}
	return s.reg()
}

// projectID is the ID of a project named by its ID or number.
func (s registrySource) projectID(reg *resourcemanager.Registry, p string) string {
	if _, err := strconv.ParseInt(p, 10, 64); err != nil {
		return p
	}
	list, err := reg.List()
	if err != nil {
		return p
	}
	for _, x := range list {
		if strconv.FormatInt(x.Number, 10) == p {
			return x.ProjectID
		}
	}
	return p
}

func (s registrySource) Ancestry(project string) []string {
	reg := s.registry()
	if reg == nil {
		return []string{"projects/" + project}
	}
	id := s.projectID(reg, project)
	anc, err := reg.Ancestry(id)
	if err != nil {
		return []string{"projects/" + id}
	}
	out := make([]string, 0, len(anc))
	for _, a := range anc {
		switch a.Type {
		case "project":
			out = append(out, "projects/"+a.ID)
		case "folder":
			out = append(out, "folders/"+a.ID)
		case "organization":
			out = append(out, "organizations/"+a.ID)
		}
	}
	return out
}

func (s registrySource) Policy(resource string) *iampolicy.Stored {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	p, err := reg.IAMPolicy(resource)
	if err != nil {
		return nil
	}
	return p
}

func (s registrySource) CustomRolePermissions(role string) ([]string, bool) {
	reg := s.registry()
	if reg == nil {
		return nil, false
	}
	return reg.RolePermissions(role)
}

func (s registrySource) Principals(email string) []string {
	reg := s.registry()
	if reg == nil {
		return []string{iameval.Member(email)}
	}
	return reg.Principals(email)
}

// DenyPolicies are the IAM v2 deny policies attached to resource (#1886).
func (s registrySource) DenyPolicies(resource string) []*iamv2.Policy {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	return reg.DenyPolicies(resource)
}

// Tags are a resource's effective tags, which deny conditions read.
func (s registrySource) Tags(full string) ([]iamcond.Tag, bool) {
	reg := s.registry()
	if reg == nil {
		return nil, false
	}
	eff, err := reg.EffectiveTags(full)
	if err != nil {
		return nil, false
	}
	out := make([]iamcond.Tag, 0, len(eff))
	for _, t := range eff {
		short := t.GetNamespacedTagValue()
		if i := strings.LastIndex(short, "/"); i >= 0 {
			short = short[i+1:]
		}
		out = append(out, iamcond.Tag{NamespacedKey: t.GetNamespacedTagKey(), ShortValue: short, KeyID: t.GetTagKey(), ValueID: t.GetTagValue()})
	}
	return out, true
}

// newIAMEnforcer is the enforcer of cfg's instance.
func newIAMEnforcer(cfg config.Config, creds *metadata.Credentials, reg func() *resourcemanager.Registry) *iameval.Enforcer {
	on := cfg.IAMEnforced()
	owner := ""
	if creds != nil {
		owner = creds.Email
	}
	return &iameval.Enforcer{
		Eval: &iameval.Evaluator{Source: registrySource{reg: reg}},
		On:   func() bool { return on },
		CallerOf: func(bearer string) iameval.Caller {
			email, ok := creds.TokenAccount(bearer, time.Now())
			switch {
			case !ok:
				return iameval.Caller{Invalid: true}
			case strings.EqualFold(email, creds.Email):
				return iameval.Caller{Email: email, Owner: true}
			}
			if strings.HasPrefix(email, "principal://") {
				// A federated subject (#1941) keeps its case: its
				// identifier is case-sensitive, and Google's errors
				// name it as it is.
				return iameval.Caller{Email: email}
			}
			return iameval.Caller{Email: strings.ToLower(email)}
		},
		Policies:   map[string]iameval.PolicyGetter{},
		OwnerEmail: owner,
	}
}

// useIAMEnforcement interposes the enforcer on the in-process services
// whose methods it maps, and gives it their policies.
func useIAMEnforcement(e *iameval.Enforcer, secretsSvc *secretsService, kmsSvc *kmsService, tasksSvc *tasksService, hub *analyticsHubSvc) {
	if hub != nil {
		// BigQuery sharing (#1989): the table's checks, then those it
		// cannot express, on gRPC and JSON alike.
		hub.enforce = append(hub.enforce, e.Interceptor(iameval.AnalyticsHub), analyticsHubChecks(e))
		e.Policies[iameval.AnalyticsHub.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			api := hub.API()
			if api == nil {
				return nil
			}
			return api.GetIamPolicy
		})
	}
	if secretsSvc != nil {
		secretsSvc.interpose = append(secretsSvc.interpose, e.Interceptor(iameval.SecretManager))
		secretsSvc.wrapJSON = jsonEnforcer(e, iameval.SecretManager, nil)
		e.Policies[iameval.SecretManager.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			st := secretsSvc.Store()
			if st == nil {
				return nil
			}
			return secrets.NewGRPCServer(st).GetIamPolicy
		})
	}
	if kmsSvc != nil {
		kmsSvc.interpose = append(kmsSvc.interpose, e.Interceptor(iameval.KMS))
		kmsSvc.wrapJSON = jsonEnforcer(e, iameval.KMS, kms.Mixins())
		e.Policies[iameval.KMS.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			api := kmsSvc.API()
			if api == nil {
				return nil
			}
			return api.IAMPolicy().GetIamPolicy
		})
	}
	if tasksSvc != nil {
		tasksSvc.interpose = append(tasksSvc.interpose, e.Interceptor(iameval.Tasks))
		tasksSvc.wrapJSON = jsonEnforcer(e, iameval.Tasks, nil)
		e.Policies[iameval.Tasks.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			st := tasksSvc.Store()
			if st == nil {
				return nil
			}
			return tasks.NewGRPCServer(st).GetIamPolicy
		})
	}
}

// jsonEnforcer checks svc's JSON requests, bound to their methods by the
// protos' HTTP rules and mixins, as its gRPC calls are checked.
func jsonEnforcer(e *iameval.Enforcer, svc *iameval.Service, mixins []*annotations.HttpRule) func(http.Handler) http.Handler {
	var services []string
	for name, s := range iameval.Services {
		if s == svc {
			services = append(services, name)
		}
	}
	m := rest.NewMatcher(services, mixins)
	return func(next http.Handler) http.Handler { return e.HTTP(svc, m, next) }
}

// iamEnforcementMode is cfg's --iam-enforcement, "off" when unset.
func iamEnforcementMode(cfg config.Config) string {
	if cfg.IAMEnforced() {
		return config.IAMEnforcementOn
	}
	return config.IAMEnforcementOff
}

// analyticsHubChecks are BigQuery sharing's checks that iameval's method
// table cannot express (#1989): SubscribeListing also needs
// bigquery.datasets.create on the subscriber's project, as "Create new
// datasets: bigquery.datasets.create on the subscriber project"
// (https://docs.cloud.google.com/bigquery/docs/analytics-hub-view-subscribe-listings,
// read 2026-11-20); and ListOrgDataExchanges, whose reference names no
// permission, answers only the exchanges the caller holds
// analyticshub.dataExchanges.list on (UNVERIFIED, re-read 2026-11-20:
// https://docs.cloud.google.com/bigquery/docs/reference/analytics-hub/rest/v1/organizations.locations.dataExchanges/list).
func analyticsHubChecks(e *iameval.Enforcer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		bearer := ""
		if md, ok := grpcmd.FromIncomingContext(ctx); ok {
			for _, v := range md.Get("authorization") {
				if b := iameval.Bearer(v); b != "" {
					bearer = b
				}
			}
		}
		switch in := req.(type) {
		case *ahpb.SubscribeListingRequest:
			if p := in.GetDestinationDataset().GetDatasetReference().GetProjectId(); p != "" {
				if err := e.Authorize(ctx, iameval.AnalyticsHub, bearer, "bigquery.datasets.create", "projects/"+p); err != nil {
					return nil, err
				}
			}
		case *ahpb.ListOrgDataExchangesRequest:
			resp, err := handler(ctx, req)
			out, ok := resp.(*ahpb.ListOrgDataExchangesResponse)
			if err != nil || !ok {
				return resp, err
			}
			kept := make([]*ahpb.DataExchange, 0, len(out.GetDataExchanges()))
			for _, x := range out.GetDataExchanges() {
				err := e.Authorize(ctx, iameval.AnalyticsHub, bearer, "analyticshub.dataExchanges.list", x.GetName())
				if status.Code(err) == codes.Unauthenticated {
					return nil, err
				}
				if err == nil {
					kept = append(kept, x)
				}
			}
			out.DataExchanges = kept
			return out, nil
		}
		return handler(ctx, req)
	}
}

// organizationOf is the organization ("organizations/N") a project is
// under, "" for none.
func organizationOf(src registrySource, project string) string {
	for _, a := range src.Ancestry(project) {
		if strings.HasPrefix(a, "organizations/") {
			return a
		}
	}
	return ""
}
