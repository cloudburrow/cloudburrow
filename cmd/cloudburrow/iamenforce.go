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
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmd "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/service/workflows"
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

// HierarchyAncestry is a folder's, the organization's or a billing
// account's ancestry (#1879): a folder, its parent folders and the
// organization; the organization alone; a billing account as Cloud
// Billing holds it (#2109): itself, a subaccount's parent account, and
// the organization it belongs to, as "the billing account inherits IAM
// permissions from its parent organization" (https://docs.cloud.google.com/billing/docs/how-to/billing-access,
// read 2026-11-21). An account Cloud Billing does not hold has none.
func (s registrySource) HierarchyAncestry(root string) []string {
	reg := s.registry()
	switch {
	case strings.HasPrefix(root, "billingAccounts/"):
		if b := billingAccounts.Load(); b != nil {
			return b.Ancestry(root)
		}
		return nil
	case reg == nil:
		return nil
	case root == resourcemanager.OrganizationName:
		return []string{root}
	case !strings.HasPrefix(root, "folders/"):
		return nil
	}
	var out []string
	for p, i := root, 0; strings.HasPrefix(p, "folders/") && i <= resourcemanager.MaxFolderDepth+1; i++ {
		f, err := reg.Folder(p)
		if err != nil {
			return out
		}
		out = append(out, p)
		p = f.Parent
		if p == resourcemanager.OrganizationName {
			out = append(out, p)
		}
	}
	return out
}

func (s registrySource) Policy(resource string) *iampolicy.Stored {
	if strings.HasPrefix(resource, "billingAccounts/") && !strings.Contains(strings.TrimPrefix(resource, "billingAccounts/"), "/") {
		// A billing account's own policy, as Cloud Billing keeps it.
		if b := billingAccounts.Load(); b != nil {
			return b.Policy(resource)
		}
		return nil
	}
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

// BoundaryBindings are the principal access boundary policy bindings
// whose target is set, with their policies (#2057).
func (s registrySource) BoundaryBindings(set string) []iameval.Bound {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	return reg.BoundaryBindings(set)
}

// BoundaryOrganization is the instance's organization, whose principal set
// holds its Workspace users and workforce pools (#2057).
func (s registrySource) BoundaryOrganization() string { return resourcemanager.OrganizationName }

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

// tokenCaller is the caller a bearer token names.
func tokenCaller(creds *metadata.Credentials, bearer string) iameval.Caller {
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
			c := tokenCaller(creds, bearer)
			if !c.Invalid {
				// A token minted with scopes (a VM's, #2179) may call
				// only what they cover.
				c.Scopes, _ = creds.TokenScopes(bearer, time.Now())
			}
			return c
		},
		Policies: map[string]iameval.PolicyGetter{
			// A service account's own policy, which iam.serviceAccounts.actAs
			// reads (who may act as it), as IAM keeps it.
			iameval.IAM.Name: func(_ context.Context, resource string) *iampolicy.Stored {
				return registrySource{reg: reg}.Policy(resource)
			},
		},
		OwnerEmail: owner,
		Resources:  map[string]iameval.ResourceLister{},
	}
}

// iamEnforced are the services the fifth round enforces (#1879): Cloud
// Logging, Cloud Monitoring, Memorystore (Redis, Redis Cluster and
// Valkey), and Cloud Run's and Cloud Functions' iam.serviceAccounts.actAs.
type iamEnforced struct {
	logging     *loggingService
	monitoring  *monitoringService
	memorystore *memorystoreAdminService
	run         *runService
}

func (s iamEnforced) use(e *iameval.Enforcer) {
	if s.logging != nil {
		s.logging.interpose = append(s.logging.interpose, e.Interceptor(iameval.Logging))
		s.logging.wrapJSON = jsonEnforcer(e, iameval.Logging, logging.Transcoder().Mixins)
		// TailLogEntries, a stream, and the JSON-only saved queries and
		// log scopes (#1879, round 13).
		s.logging.streamInterpose = append(s.logging.streamInterpose, e.StreamInterceptor(iameval.Logging))
		s.logging.wrapExtra = func(next http.Handler) http.Handler {
			return e.HTTPPaths(iameval.Logging, iameval.LoggingExtraJSON, next)
		}
		// A log view keeps its own policy (roles/logging.viewAccessor).
		e.Policies[iameval.Logging.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			a := s.logging.Admin()
			if a == nil {
				return nil
			}
			return a.ViewIAMPolicy
		})
	}
	if s.monitoring != nil {
		// Monitoring keeps no resource-level policies.
		s.monitoring.interpose = append(s.monitoring.interpose, e.Interceptor(iameval.Monitoring))
		s.monitoring.wrapJSON = jsonEnforcer(e, iameval.Monitoring, nil)
	}
	if s.memorystore != nil {
		// Three APIs on one port, each its own table; none keeps
		// resource-level policies.
		for _, svc := range []*iameval.Service{iameval.Redis, iameval.RedisCluster, iameval.Valkey} {
			s.memorystore.interpose = append(s.memorystore.interpose, e.Interceptor(svc))
		}
		redis, cluster := jsonEnforcer(e, iameval.Redis, nil), jsonEnforcer(e, iameval.RedisCluster, nil)
		s.memorystore.wrapRedisJSON = func(next http.Handler) http.Handler { return redis(cluster(next)) }
		valkey := jsonEnforcer(e, iameval.Valkey, nil)
		// Valkey's JSON API is under /memorystore, its paths beside
		// CloudRedis's: matched without the prefix.
		s.memorystore.wrapValkeyJSON = func(next http.Handler) http.Handler {
			return stripForMatch(redisadmin.MemorystoreRESTPrefix, valkey, next)
		}
	}
	if s.run != nil {
		runCheck := e.Interceptor(iameval.Run)
		// The v1 (Knative-shaped) API serves its requests through the v2
		// methods in process: each is checked as its v2 request.
		runadapter.UseV1Authorize(&runadapter.V1Authorizer{
			Method: func(ctx context.Context, full string, req proto.Message) error {
				_, err := runCheck(ctx, req, &grpc.UnaryServerInfo{FullMethod: full}, func(context.Context, any) (any, error) { return nil, nil })
				return err
			},
			Permission: func(ctx context.Context, permission, resource string) error {
				return e.Authorize(ctx, iameval.Run, iameval.IncomingBearer(ctx), permission, resource)
			},
		})
		// Cloud Run's services, jobs and worker pools keep their own
		// policies, as do functions and Artifact Registry repositories.
		e.Policies[iameval.Run.Name] = func(ctx context.Context, resource string) *iampolicy.Stored {
			a := s.run.adapter.Load()
			if a == nil {
				return nil
			}
			get := a.GetIamPolicy
			switch {
			case strings.Contains(resource, "/jobs/"):
				get = a.Jobs().GetIamPolicy
			case strings.Contains(resource, "/workerPools/"):
				get = a.WorkerPools().GetIamPolicy
			case strings.Contains(resource, "/instances/"):
				get = a.Instances().GetIamPolicy
			}
			return iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return get })(ctx, resource)
		}
		e.Policies[iameval.Functions.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			f := s.run.functions.Load()
			if f == nil {
				return nil
			}
			return f.IAMPolicy
		})
		e.Policies[iameval.ArtifactRegistry.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			a := s.run.adapter.Load()
			if a == nil {
				return nil
			}
			return a.ArtifactRegistry().GetIamPolicy
		})
		// Cloud Asset's analysis under a scope (#2112) lists them.
		e.Resources[iameval.Run.Name] = func(ctx context.Context, project string) []iameval.ChildResource {
			a := s.run.adapter.Load()
			if a == nil {
				return nil
			}
			var out []iameval.ChildResource
			for _, n := range a.ProjectResourceNames(ctx, project) {
				out = append(out, iameval.ChildResource{Name: n})
			}
			return out
		}
		e.Resources[iameval.Functions.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			if s.run.functions.Load() == nil {
				return nil
			}
			var out []iameval.ChildResource
			for _, f := range runadapter.Functions(project) {
				out = append(out, iameval.ChildResource{Name: f.GetName()})
			}
			return out
		}
		e.Resources[iameval.ArtifactRegistry.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			if s.run.adapter.Load() == nil {
				return nil
			}
			var out []iameval.ChildResource
			for _, n := range runadapter.RepositoryNames(project) {
				out = append(out, iameval.ChildResource{Name: n})
			}
			return out
		}
		s.run.interpose = append(s.run.interpose, runCheck, e.Interceptor(iameval.Functions), e.Interceptor(iameval.Eventarc),
			e.Interceptor(iameval.ArtifactRegistry), e.Interceptor(iameval.CloudBuild))
		run, functions := jsonEnforcer(e, iameval.Run, nil), jsonEnforcer(e, iameval.Functions, nil)
		eventarc := jsonEnforcer(e, iameval.Eventarc, nil)
		s.run.wrapJSON = func(next http.Handler) http.Handler {
			// Eventarc's JSON API is under /eventarc on this port, its
			// Publishing API's under /eventarcpublishing (#1879).
			return run(functions(stripForMatch("/eventarcpublishing", eventarc, stripForMatch("/eventarc", eventarc, next))))
		}
	}
}

// stripForMatch checks a request with wrap as if its path had no prefix,
// then serves it to next with its path as it came.
func stripForMatch(prefix string, wrap func(http.Handler) http.Handler, next http.Handler) http.Handler {
	type orig struct{}
	inner := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o, ok := r.Context().Value(orig{}).(*http.Request); ok {
			o.Body = r.Body
			next.ServeHTTP(w, o)
			return
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		c := r.Clone(context.WithValue(r.Context(), orig{}, r))
		c.URL.Path = path
		c.URL.RawPath = ""
		inner.ServeHTTP(w, c)
	})
}

// useIAMEnforcement interposes the enforcer on the in-process services
// whose methods it maps, and gives it their policies.
func useIAMEnforcement(e *iameval.Enforcer, secretsSvc *secretsService, kmsSvc *kmsService, tasksSvc *tasksService, hub *analyticsHubSvc,
	schedulerSvc *schedulerService, workflowsSvc *workflowsService, more iamEnforced) {
	more.use(e)
	// Cloud Scheduler and Workflows keep no resource-level policies, so
	// only the project's and its ancestors' apply: no policy reader.
	if schedulerSvc != nil {
		schedulerSvc.interpose = append(schedulerSvc.interpose, e.Interceptor(iameval.Scheduler))
		schedulerSvc.wrapJSON = jsonEnforcer(e, iameval.Scheduler, scheduler.Transcoder().Mixins)
	}
	if workflowsSvc != nil {
		workflowsSvc.interpose = append(workflowsSvc.interpose, e.Interceptor(iameval.Workflows))
		bound := jsonEnforcer(e, iameval.Workflows, workflows.Transcoder().Mixins)
		workflowsSvc.wrapJSON = func(next http.Handler) http.Handler {
			return e.HTTPPaths(iameval.Workflows, iameval.WorkflowsJSON, bound(next))
		}
	}
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
		e.Resources[iameval.SecretManager.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			st := secretsSvc.Store()
			if st == nil {
				return nil
			}
			list, _ := st.ListSecrets(project)
			out := make([]iameval.ChildResource, 0, len(list))
			for _, x := range list {
				out = append(out, iameval.ChildResource{Name: x.Name})
			}
			return out
		}
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
		e.Resources[iameval.KMS.Name] = func(ctx context.Context, project string) []iameval.ChildResource {
			api := kmsSvc.API()
			if api == nil {
				return nil
			}
			rings, _ := api.KeyRingsInProject(project)
			var out []iameval.ChildResource
			for _, r := range rings {
				out = append(out, iameval.ChildResource{Name: r.GetName()})
				keys, err := api.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: r.GetName(), PageSize: 1000})
				if err != nil {
					continue
				}
				for _, k := range keys.GetCryptoKeys() {
					out = append(out, iameval.ChildResource{Name: k.GetName(), Parent: r.GetName()})
				}
				jobs, err := api.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: r.GetName(), PageSize: 1000})
				if err != nil {
					continue
				}
				for _, j := range jobs.GetImportJobs() {
					out = append(out, iameval.ChildResource{Name: j.GetName(), Parent: r.GetName()})
				}
			}
			conns, _ := api.EkmConnectionsInProject(project)
			for _, c := range conns {
				out = append(out, iameval.ChildResource{Name: c.GetName()})
			}
			return out
		}
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
		e.Resources[iameval.Tasks.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			st := tasksSvc.Store()
			if st == nil {
				return nil
			}
			all, _ := st.AllQueues()
			var out []iameval.ChildResource
			for _, q := range all {
				if strings.HasPrefix(q.Name, "projects/"+project+"/") {
					out = append(out, iameval.ChildResource{Name: q.Name})
				}
			}
			return out
		}
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
