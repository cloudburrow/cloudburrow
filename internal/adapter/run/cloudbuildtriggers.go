package run

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Build triggers and private worker pools (#1422).
//
// A trigger is a stored build: its inline `build`, with the trigger's
// substitutions and the built-in TRIGGER_NAME, run by RunBuildTrigger
// (a manual trigger) or by ReceiveTriggerWebhook (a webhookConfig
// trigger, whose secret query parameter must equal the Secret Manager
// version the trigger names, as Google checks it). Its build's source is
// a Cloud Storage archive, as every CreateBuild source here is.
//
// What needs a live service is refused UNIMPLEMENTED with the reason: a
// trigger's GitHub, GitLab, Bitbucket or Cloud Source Repositories events
// (github, triggerTemplate, repositoryEventConfig), and a build read from a
// repository (filename, gitFileSource, sourceToBuild, autodetect), which
// Google clones from a hosted git service, never from a path on the
// developer's machine. A Pub/Sub trigger (pubsubConfig) is refused too:
// Google pushes the topic's messages to Cloud Build through a subscription
// it makes and owns, which this instance does not run.
//
// A worker pool is stored, with its long-running operations; a build that
// names it in options.pool.name runs on the host's Docker like any build,
// with the pool recorded on the build.

var triggerStore = struct {
	mu       sync.Mutex
	triggers map[string]*cbpb.BuildTrigger // by resource name
	pools    map[string]*cbpb.WorkerPool   // by name
}{triggers: map[string]*cbpb.BuildTrigger{}, pools: map[string]*cbpb.WorkerPool{}}

// triggerContext is the trigger a build runs from.
type triggerContext struct {
	id, name string
}

// builtins are the substitutions Cloud Build adds to a triggered build.
// The repository ones (REPO_NAME, BRANCH_NAME, COMMIT_SHA, ...) are absent:
// no trigger here has a repository.
func (t *triggerContext) builtins() map[string]string {
	return map[string]string{"TRIGGER_NAME": t.name, "TRIGGER_BUILD_CONFIG_PATH": ""}
}

var (
	triggerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][-a-zA-Z0-9_]{0,62}$`)
	poolIDRE      = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	bindingRE     = regexp.MustCompile(`\$\((body(\.[A-Za-z0-9_\-]+)*)\)`)
)

// triggerLocation resolves a trigger request's project and location: its
// parent or name (projects/{p}/locations/{l}[/triggers/{t}]), else the
// global form's project_id.
func triggerLocation(name, project string) (p, l, id string, err error) {
	if name == "" {
		if project == "" {
			return "", "", "", apierror.InvalidArgument("project_id is required")
		}
		return project, "global", "", nil
	}
	loc, id, _ := strings.Cut(name, "/triggers/")
	p, l, err = resource.ParseLocation(loc)
	if err != nil {
		return "", "", "", apierror.InvalidArgument("%v", err)
	}
	if project != "" && project != p {
		return "", "", "", apierror.InvalidArgument("project_id %q does not match %q", project, name)
	}
	return p, l, id, nil
}

// unsupportedTrigger names why a trigger cannot run here, or nil.
func unsupportedTrigger(t *cbpb.BuildTrigger) error {
	repo := func(field string) error {
		return apierror.Unimplemented("BuildTrigger field %s is not supported: it reads the build from a git repository, "+
			"which Google clones from a hosted service (GitHub, GitLab, Bitbucket, Cloud Source Repositories), "+
			"not from a path on this machine; give the trigger an inline build whose source is a Cloud Storage archive", field)
	}
	events := func(field string) error {
		return apierror.Unimplemented("BuildTrigger field %s is not supported: its events come from a hosted git "+
			"service (GitHub, GitLab, Bitbucket, Cloud Source Repositories), which no instance runs; use a manual "+
			"trigger (RunBuildTrigger) or a webhook trigger (webhookConfig)", field)
	}
	switch {
	case t.GetGithub() != nil:
		return events("github")
	case t.GetTriggerTemplate() != nil:
		return events("triggerTemplate")
	case t.GetRepositoryEventConfig() != nil:
		return events("repositoryEventConfig")
	case t.GetPubsubConfig() != nil:
		return apierror.Unimplemented("BuildTrigger field pubsubConfig is not supported: Google delivers the topic's " +
			"messages to Cloud Build through a push subscription it creates and owns, which this instance does not run; " +
			"use a webhook trigger, or a Pub/Sub push subscription to its :webhook URL")
	case t.GetFilename() != "":
		return repo("filename")
	case t.GetGitFileSource() != nil:
		return repo("gitFileSource")
	case t.GetSourceToBuild() != nil:
		return repo("sourceToBuild")
	case t.GetAutodetect():
		return repo("autodetect")
	case t.GetFilter() != "":
		return apierror.Unimplemented("BuildTrigger field filter (a CEL expression over the event) is not supported")
	case t.GetServiceAccount() != "":
		return apierror.Unimplemented("BuildTrigger field serviceAccount is not supported: builds run as the host's Docker, under no account")
	case len(t.GetIncludedFiles()) > 0 || len(t.GetIgnoredFiles()) > 0:
		return apierror.Unimplemented("BuildTrigger fields includedFiles and ignoredFiles filter repository pushes, which no trigger here has")
	}
	if t.GetBuild() == nil {
		return apierror.InvalidArgument("invalid trigger: a trigger needs a build (an inline build config)")
	}
	if err := unsupportedCreate(t.GetBuild()); err != nil {
		return err
	}
	if wc := t.GetWebhookConfig(); wc != nil && !secretVersionRE.MatchString(wc.GetSecret()) {
		return apierror.InvalidArgument("invalid trigger: webhookConfig.secret %q is not a Secret Manager version, "+
			"projects/PROJECT/secrets/SECRET/versions/VERSION", wc.GetSecret())
	}
	for k := range t.GetSubstitutions() {
		if !strings.HasPrefix(k, "_") {
			return apierror.InvalidArgument("invalid trigger: substitution key %q must begin with an underscore", k)
		}
	}
	if !triggerNameRE.MatchString(t.GetName()) {
		return apierror.InvalidArgument("invalid trigger: name %q must be 1-64 letters, digits, - or _, starting with a letter or digit", t.GetName())
	}
	return nil
}

// CreateBuildTrigger stores a trigger.
func (c *CloudBuildServer) CreateBuildTrigger(_ context.Context, req *cbpb.CreateBuildTriggerRequest) (*cbpb.BuildTrigger, error) {
	project, location, _, err := triggerLocation(req.GetParent(), req.GetProjectId())
	if err != nil {
		return nil, err
	}
	t := proto.Clone(req.GetTrigger()).(*cbpb.BuildTrigger)
	if t == nil || req.GetTrigger() == nil {
		return nil, apierror.InvalidArgument("trigger is required")
	}
	if err := unsupportedTrigger(t); err != nil {
		return nil, err
	}
	t.Id = uuid.NewString()
	t.ResourceName = "projects/" + project + "/locations/" + location + "/triggers/" + t.Id
	t.CreateTime = timestamppb.Now()
	if wc := t.GetWebhookConfig(); wc != nil {
		wc.State = cbpb.WebhookConfig_OK
	}
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	prefix := "projects/" + project + "/locations/" + location + "/triggers/"
	for n, x := range triggerStore.triggers {
		if strings.HasPrefix(n, prefix) && x.GetName() == t.GetName() {
			return nil, apierror.AlreadyExists("trigger %q already exists", t.GetName())
		}
	}
	triggerStore.triggers[t.ResourceName] = t
	return proto.Clone(t).(*cbpb.BuildTrigger), nil
}

// findTrigger finds a trigger in project and location by its ID or its
// name, as Google's trigger_id accepts either. It is called under the lock.
func findTrigger(project, location, idOrName string) (*cbpb.BuildTrigger, error) {
	if idOrName == "" {
		return nil, apierror.InvalidArgument("trigger_id is required")
	}
	prefix := "projects/" + project + "/locations/" + location + "/triggers/"
	if t, ok := triggerStore.triggers[prefix+idOrName]; ok {
		return t, nil
	}
	for n, t := range triggerStore.triggers {
		if strings.HasPrefix(n, prefix) && t.GetName() == idOrName {
			return t, nil
		}
	}
	return nil, apierror.NotFound("trigger %s not found", idOrName)
}

// triggerOf resolves a request's (name | project_id, trigger_id).
func triggerOf(name, project, triggerID string) (*cbpb.BuildTrigger, error) {
	p, l, id, err := triggerLocation(name, project)
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = triggerID
	}
	return findTrigger(p, l, id)
}

// GetBuildTrigger reads a trigger by ID or name.
func (c *CloudBuildServer) GetBuildTrigger(_ context.Context, req *cbpb.GetBuildTriggerRequest) (*cbpb.BuildTrigger, error) {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	t, err := triggerOf(req.GetName(), req.GetProjectId(), req.GetTriggerId())
	if err != nil {
		return nil, err
	}
	return proto.Clone(t).(*cbpb.BuildTrigger), nil
}

// Triggers lists a project's triggers in one location ("" for all), by
// name; the console reads it.
func Triggers(project, location string) []*cbpb.BuildTrigger {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	prefix := "projects/" + project + "/locations/"
	if location != "" {
		prefix += location + "/triggers/"
	}
	var out []*cbpb.BuildTrigger
	for n, t := range triggerStore.triggers {
		if strings.HasPrefix(n, prefix) {
			out = append(out, proto.Clone(t).(*cbpb.BuildTrigger))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetName() != out[j].GetName() {
			return out[i].GetName() < out[j].GetName()
		}
		return out[i].GetResourceName() < out[j].GetResourceName()
	})
	return out
}

// page cuts items to one page: a token is the offset of the next page,
// bound to the collection it was issued for.
func page[T any](items []T, size int32, token, scope string) ([]T, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	offset := 0
	if token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		s, n, ok := strings.Cut(string(raw), "\x00")
		if err == nil && ok && s == scope {
			offset, err = strconv.Atoi(n)
		} else {
			err = fmt.Errorf("bad token")
		}
		if err != nil || offset < 0 || offset > len(items) {
			return nil, "", apierror.InvalidArgument("invalid page_token %q", token)
		}
	}
	end := len(items)
	if size > 0 && offset+int(size) < end {
		end = offset + int(size)
	}
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(scope + "\x00" + strconv.Itoa(end)))
	}
	return items[offset:end], next, nil
}

// ListBuildTriggers lists a location's triggers, page_size at a time.
func (c *CloudBuildServer) ListBuildTriggers(_ context.Context, req *cbpb.ListBuildTriggersRequest) (*cbpb.ListBuildTriggersResponse, error) {
	project, location, _, err := triggerLocation(req.GetParent(), req.GetProjectId())
	if err != nil {
		return nil, err
	}
	items, next, err := page(Triggers(project, location), req.GetPageSize(), req.GetPageToken(), project+"/"+location)
	if err != nil {
		return nil, err
	}
	return &cbpb.ListBuildTriggersResponse{Triggers: items, NextPageToken: next}, nil
}

// DeleteBuildTrigger removes a trigger; its builds stay.
func (c *CloudBuildServer) DeleteBuildTrigger(_ context.Context, req *cbpb.DeleteBuildTriggerRequest) (*emptypb.Empty, error) {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	t, err := triggerOf(req.GetName(), req.GetProjectId(), req.GetTriggerId())
	if err != nil {
		return nil, err
	}
	delete(triggerStore.triggers, t.GetResourceName())
	return &emptypb.Empty{}, nil
}

// triggerOutputOnly are the fields an update never changes.
var triggerOutputOnly = map[string]bool{"id": true, "resource_name": true, "create_time": true}

// applyMask sets dst's top-level fields named by mask from src, or every
// field but keep when mask is empty. A path under a top-level field
// replaces that whole field.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask, keep map[string]bool) error {
	d, s := dst.ProtoReflect(), src.ProtoReflect()
	fields := d.Descriptor().Fields()
	set := func(fd protoreflect.FieldDescriptor) {
		if s.Has(fd) {
			d.Set(fd, s.Get(fd))
		} else {
			d.Clear(fd)
		}
	}
	if len(mask.GetPaths()) == 0 {
		for i := 0; i < fields.Len(); i++ {
			if fd := fields.Get(i); !keep[string(fd.Name())] {
				set(fd)
			}
		}
		return nil
	}
	for _, p := range mask.GetPaths() {
		top, _, _ := strings.Cut(p, ".")
		fd := fields.ByName(protoreflect.Name(top))
		if fd == nil {
			fd = fields.ByJSONName(top)
		}
		if fd == nil {
			return apierror.InvalidArgument("update_mask path %q is not a field of %s", p, d.Descriptor().Name())
		}
		if !keep[string(fd.Name())] {
			set(fd)
		}
	}
	return nil
}

// UpdateBuildTrigger replaces a trigger's fields, those update_mask names
// or, with none, all of them.
func (c *CloudBuildServer) UpdateBuildTrigger(_ context.Context, req *cbpb.UpdateBuildTriggerRequest) (*cbpb.BuildTrigger, error) {
	if req.GetTrigger() == nil {
		return nil, apierror.InvalidArgument("trigger is required")
	}
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	t, err := triggerOf(req.GetTrigger().GetResourceName(), req.GetProjectId(), req.GetTriggerId())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(t).(*cbpb.BuildTrigger)
	if err := applyMask(next, req.GetTrigger(), req.GetUpdateMask(), triggerOutputOnly); err != nil {
		return nil, err
	}
	if err := unsupportedTrigger(next); err != nil {
		return nil, err
	}
	if wc := next.GetWebhookConfig(); wc != nil {
		wc.State = cbpb.WebhookConfig_OK
	}
	triggerStore.triggers[t.GetResourceName()] = next
	return proto.Clone(next).(*cbpb.BuildTrigger), nil
}

// RunBuildTrigger runs a trigger's build. Its source, a RepoSource, may
// carry substitutions, which override the trigger's; a branch, tag or
// commit is refused, as no trigger here has a repository.
func (c *CloudBuildServer) RunBuildTrigger(ctx context.Context, req *cbpb.RunBuildTriggerRequest) (*longrunning.Operation, error) {
	if s := req.GetSource(); s != nil && (s.GetRepoName() != "" || s.GetRevision() != nil || s.GetDir() != "" || s.GetProjectId() != "") {
		return nil, apierror.Unimplemented("RunBuildTrigger source names a repository revision, which needs a hosted git " +
			"repository; this trigger's build has its own source")
	}
	return c.runTrigger(ctx, req.GetName(), req.GetProjectId(), req.GetTriggerId(), req.GetSource().GetSubstitutions(), nil)
}

// runTrigger starts a build from a trigger, with extra substitutions over
// the trigger's and its $(body.…) bindings resolved against body.
func (c *CloudBuildServer) runTrigger(ctx context.Context, name, project, triggerID string, extra map[string]string, body any) (*longrunning.Operation, error) {
	triggerStore.mu.Lock()
	t, err := triggerOf(name, project, triggerID)
	if err == nil {
		t = proto.Clone(t).(*cbpb.BuildTrigger)
	}
	triggerStore.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if t.GetDisabled() {
		return nil, apierror.FailedPrecondition("trigger %s is disabled", t.GetName())
	}
	for k := range extra {
		if !strings.HasPrefix(k, "_") {
			return nil, apierror.InvalidArgument("substitution key %q must begin with an underscore", k)
		}
	}
	b := proto.Clone(t.GetBuild()).(*cbpb.Build)
	if b.Substitutions == nil {
		b.Substitutions = map[string]string{}
	}
	for k, v := range t.GetSubstitutions() {
		b.Substitutions[k] = bindBody(v, body)
	}
	for k, v := range extra {
		b.Substitutions[k] = v
	}
	p, l, _ := resource.ParseLocation(strings.SplitN(t.GetResourceName(), "/triggers/", 2)[0])
	return c.createBuild(ctx, p, l, l == "global", b, &triggerContext{id: t.GetId(), name: t.GetName()})
}

// bindBody resolves a substitution's $(body.a.b) payload bindings against
// a webhook's JSON body, as Cloud Build does; a binding with no value is
// empty.
func bindBody(v string, body any) string {
	return bindingRE.ReplaceAllStringFunc(v, func(m string) string {
		path := strings.Split(bindingRE.FindStringSubmatch(m)[1], ".")[1:]
		cur := body
		for _, k := range path {
			obj, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = obj[k]
		}
		switch x := cur.(type) {
		case nil:
			return ""
		case string:
			return x
		default:
			raw, _ := json.Marshal(x)
			return string(raw)
		}
	})
}

// ReceiveTriggerWebhook runs a webhookConfig trigger's build when the
// request's secret equals the Secret Manager version the trigger names.
func (c *CloudBuildServer) ReceiveTriggerWebhook(ctx context.Context, req *cbpb.ReceiveTriggerWebhookRequest) (*cbpb.ReceiveTriggerWebhookResponse, error) {
	name := req.GetName()
	if name == "" && req.GetProjectId() != "" && req.GetTrigger() != "" {
		name = "projects/" + req.GetProjectId() + "/locations/global/triggers/" + req.GetTrigger()
	}
	triggerStore.mu.Lock()
	t, err := triggerOf(name, req.GetProjectId(), req.GetTrigger())
	if err == nil {
		t = proto.Clone(t).(*cbpb.BuildTrigger)
	}
	triggerStore.mu.Unlock()
	if err != nil {
		return nil, err
	}
	wc := t.GetWebhookConfig()
	if wc == nil {
		return nil, apierror.FailedPrecondition("trigger %s is not a webhook trigger", t.GetName())
	}
	if c.builds == nil || c.builds.secretManager == nil {
		return nil, apierror.FailedPrecondition("trigger %s checks its webhook secret in Secret Manager, which is not "+
			"enabled on this instance: start it with secretmanager in --services", t.GetName())
	}
	want, err := c.builds.secretManager.AccessSecretVersion(ctx, wc.GetSecret())
	if err != nil {
		return nil, apierror.FailedPrecondition("trigger %s: reading webhook secret %s: %v", t.GetName(), wc.GetSecret(), status.Convert(err).Message())
	}
	if req.GetSecret() == "" || subtle.ConstantTimeCompare([]byte(req.GetSecret()), want) != 1 {
		return nil, &apierror.Error{Code: codes.PermissionDenied,
			Message: "the webhook request's secret does not match trigger " + t.GetName() + "'s secret"}
	}
	var body any
	if data := req.GetBody().GetData(); len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	if _, err := c.runTrigger(ctx, t.GetResourceName(), "", "", nil, body); err != nil {
		return nil, err
	}
	return &cbpb.ReceiveTriggerWebhookResponse{}, nil
}

// webhookTarget matches a :webhook path, global or regional.
var webhookTarget = regexp.MustCompile(`^/v1/projects/([^/]+)(/locations/([^/]+))?/triggers/([^/:]+):webhook$`)

// serveWebhook answers ReceiveTriggerWebhook's JSON form, whose body is the
// raw payload (google.api.HttpBody), which the transcoder cannot decode.
func serveWebhook(c *CloudBuildServer, w http.ResponseWriter, r *http.Request) bool {
	m := webhookTarget.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost || c == nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	location := m[3]
	if location == "" {
		location = "global"
	}
	req := &cbpb.ReceiveTriggerWebhookRequest{
		Name:      "projects/" + m[1] + "/locations/" + location + "/triggers/" + m[4],
		ProjectId: m[1], Trigger: m[4], Secret: r.URL.Query().Get("secret"),
		Body: &httpbody.HttpBody{ContentType: r.Header.Get("Content-Type"), Data: data},
	}
	resp, err := c.ReceiveTriggerWebhook(withCaller(r).Context(), req)
	if err != nil {
		apierror.WriteJSON(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	raw, _ := protojson.Marshal(resp)
	_, _ = w.Write(raw)
	return true
}

// captureCloudBuild passes registrations through and keeps the Cloud Build
// service, for serveWebhook.
type captureCloudBuild struct {
	grpc.ServiceRegistrar
	c *CloudBuildServer
}

func (r *captureCloudBuild) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if c, ok := impl.(*CloudBuildServer); ok {
		r.c = c
	}
	r.ServiceRegistrar.RegisterService(desc, impl)
}

// Worker pools.

// poolLocation parses projects/{p}/locations/{l} and refuses global:
// worker pools are regional.
func poolLocation(parent string) (string, string, error) {
	p, l, err := resource.ParseLocation(parent)
	if err != nil {
		return "", "", apierror.InvalidArgument("%v", err)
	}
	if l == "global" {
		return "", "", apierror.InvalidArgument("worker pools are regional: location must be a region, not global")
	}
	return p, l, nil
}

func (c *CloudBuildServer) poolOps() (*Server, error) {
	if c.builds == nil || c.builds.s == nil {
		return nil, apierror.FailedPrecondition("worker pools are served with Cloud Build's builds, which are not running here")
	}
	return c.builds.s, nil
}

// poolOp returns a finished operation in the pool's location.
func (c *CloudBuildServer) poolOp(pool string, metadata, response proto.Message) (*longrunning.Operation, error) {
	s, err := c.poolOps()
	if err != nil {
		return nil, err
	}
	loc := strings.SplitN(pool, "/workerPools/", 2)[0]
	op := s.ops.CreateNamed(loc+"/operations/"+uuid.NewString(), pool)
	_ = s.ops.SetMetadata(op.Name, metadata)
	_ = s.ops.Succeed(op.Name, response)
	return s.toProtoOperation(op.Name)
}

func poolEtag(p *cbpb.WorkerPool) string {
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&cbpb.WorkerPool{
		DisplayName: p.GetDisplayName(), Annotations: p.GetAnnotations(), Config: p.GetConfig(), UpdateTime: p.GetUpdateTime()})
	return fmt.Sprintf("W/\"%x\"", resource.ProjectNumber(string(raw)))
}

func validatePool(p *cbpb.WorkerPool) error {
	if p.GetPrivatePoolV1Config().GetPrivateServiceConnect() != nil {
		return apierror.Unimplemented("WorkerPool field privatePoolV1Config.privateServiceConnect is not supported: it attaches a VPC network, which builds here do not use")
	}
	return nil
}

// CreateWorkerPool stores a pool, RUNNING at once, and returns its finished
// operation.
func (c *CloudBuildServer) CreateWorkerPool(_ context.Context, req *cbpb.CreateWorkerPoolRequest) (*longrunning.Operation, error) {
	project, location, err := poolLocation(req.GetParent())
	if err != nil {
		return nil, err
	}
	if !poolIDRE.MatchString(req.GetWorkerPoolId()) {
		return nil, apierror.InvalidArgument("worker_pool_id %q must be 1-63 lowercase letters, digits or hyphens, starting with a letter", req.GetWorkerPoolId())
	}
	in := req.GetWorkerPool()
	if in == nil {
		in = &cbpb.WorkerPool{}
	}
	if err := validatePool(in); err != nil {
		return nil, err
	}
	p := proto.Clone(in).(*cbpb.WorkerPool)
	p.Name = "projects/" + project + "/locations/" + location + "/workerPools/" + req.GetWorkerPoolId()
	now := timestamppb.Now()
	p.Uid, p.CreateTime, p.UpdateTime, p.DeleteTime, p.State = uuid.NewString(), now, now, nil, cbpb.WorkerPool_RUNNING
	p.Etag = poolEtag(p)
	triggerStore.mu.Lock()
	if _, ok := triggerStore.pools[p.Name]; ok {
		triggerStore.mu.Unlock()
		return nil, apierror.AlreadyExists("worker pool %s already exists", p.Name)
	}
	if !req.GetValidateOnly() {
		triggerStore.pools[p.Name] = p
	}
	triggerStore.mu.Unlock()
	return c.poolOp(p.Name, &cbpb.CreateWorkerPoolOperationMetadata{WorkerPool: p.Name, CreateTime: now, CompleteTime: timestamppb.Now()}, p)
}

// GetWorkerPool reads a pool.
func (c *CloudBuildServer) GetWorkerPool(_ context.Context, req *cbpb.GetWorkerPoolRequest) (*cbpb.WorkerPool, error) {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	p, ok := triggerStore.pools[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("worker pool %s not found", req.GetName())
	}
	return proto.Clone(p).(*cbpb.WorkerPool), nil
}

// WorkerPools lists a project's pools, by name; the console reads it.
func WorkerPools(project string) []*cbpb.WorkerPool {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	var out []*cbpb.WorkerPool
	for n, p := range triggerStore.pools {
		if strings.HasPrefix(n, "projects/"+project+"/") {
			out = append(out, proto.Clone(p).(*cbpb.WorkerPool))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// ListWorkerPools lists a location's pools.
func (c *CloudBuildServer) ListWorkerPools(_ context.Context, req *cbpb.ListWorkerPoolsRequest) (*cbpb.ListWorkerPoolsResponse, error) {
	project, location, err := poolLocation(req.GetParent())
	if err != nil {
		return nil, err
	}
	var all []*cbpb.WorkerPool
	for _, p := range WorkerPools(project) {
		if strings.HasPrefix(p.GetName(), req.GetParent()+"/workerPools/") {
			all = append(all, p)
		}
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), project+"/"+location)
	if err != nil {
		return nil, err
	}
	return &cbpb.ListWorkerPoolsResponse{WorkerPools: items, NextPageToken: next}, nil
}

// poolOutputOnly are the fields UpdateWorkerPool never changes.
var poolOutputOnly = map[string]bool{"name": true, "uid": true, "create_time": true, "update_time": true,
	"delete_time": true, "state": true, "etag": true}

// UpdateWorkerPool changes a pool's display name, annotations or config.
func (c *CloudBuildServer) UpdateWorkerPool(_ context.Context, req *cbpb.UpdateWorkerPoolRequest) (*longrunning.Operation, error) {
	in := req.GetWorkerPool()
	if in == nil {
		return nil, apierror.InvalidArgument("worker_pool is required")
	}
	triggerStore.mu.Lock()
	p, ok := triggerStore.pools[in.GetName()]
	if !ok {
		triggerStore.mu.Unlock()
		return nil, apierror.NotFound("worker pool %s not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != p.GetEtag() {
		triggerStore.mu.Unlock()
		return nil, apierror.Aborted("worker pool %s has changed: etag %s is not its current etag", in.GetName(), in.GetEtag())
	}
	next := proto.Clone(p).(*cbpb.WorkerPool)
	err := applyMask(next, in, req.GetUpdateMask(), poolOutputOnly)
	if err == nil {
		err = validatePool(next)
	}
	if err != nil {
		triggerStore.mu.Unlock()
		return nil, err
	}
	next.UpdateTime = timestamppb.Now()
	next.Etag = poolEtag(next)
	if !req.GetValidateOnly() {
		triggerStore.pools[next.Name] = next
	}
	triggerStore.mu.Unlock()
	return c.poolOp(next.Name, &cbpb.UpdateWorkerPoolOperationMetadata{WorkerPool: next.Name,
		CreateTime: next.UpdateTime, CompleteTime: timestamppb.Now()}, next)
}

// DeleteWorkerPool removes a pool; builds that ran on it keep its name.
func (c *CloudBuildServer) DeleteWorkerPool(_ context.Context, req *cbpb.DeleteWorkerPoolRequest) (*longrunning.Operation, error) {
	triggerStore.mu.Lock()
	p, ok := triggerStore.pools[req.GetName()]
	switch {
	case !ok && !req.GetAllowMissing():
		triggerStore.mu.Unlock()
		return nil, apierror.NotFound("worker pool %s not found", req.GetName())
	case ok && req.GetEtag() != "" && req.GetEtag() != p.GetEtag():
		triggerStore.mu.Unlock()
		return nil, apierror.Aborted("worker pool %s has changed: etag %s is not its current etag", req.GetName(), req.GetEtag())
	}
	if ok && !req.GetValidateOnly() {
		delete(triggerStore.pools, req.GetName())
	}
	triggerStore.mu.Unlock()
	if _, _, err := poolLocation(strings.SplitN(req.GetName(), "/workerPools/", 2)[0]); err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	return c.poolOp(req.GetName(), &cbpb.DeleteWorkerPoolOperationMetadata{WorkerPool: req.GetName(), CreateTime: now, CompleteTime: now}, &emptypb.Empty{})
}

// buildPool checks a build's options.pool.name (or the deprecated
// options.workerPool) names a pool that exists, in the build's region
// unless the build is global, and returns it ("" for none).
func buildPool(location string, o *cbpb.BuildOptions) (string, error) {
	name := o.GetPool().GetName()
	if name == "" {
		name = o.GetWorkerPool()
	}
	if name == "" {
		return "", nil
	}
	loc, _, ok := strings.Cut(name, "/workerPools/")
	_, l, err := resource.ParseLocation(loc)
	if !ok || err != nil {
		return "", apierror.InvalidArgument("invalid build: options.pool.name %q is not projects/PROJECT/locations/REGION/workerPools/POOL", name)
	}
	triggerStore.mu.Lock()
	_, exists := triggerStore.pools[name]
	triggerStore.mu.Unlock()
	if !exists {
		return "", apierror.NotFound("invalid build: worker pool %s not found", name)
	}
	if location != "global" && l != location {
		return "", apierror.InvalidArgument("invalid build: worker pool %s is in %s, not the build's region %s", name, l, location)
	}
	return name, nil
}

// triggerPath matches a trigger's create or update path, global or
// regional.
var triggerPath = regexp.MustCompile(`^/v1/projects/[^/]+(/locations/[^/]+)?/triggers(/[^/:]+)?$`)

// stripApprovalConfig drops a trigger body's approvalConfig, which this
// release of the Cloud Build API's protos lacks, when it asks for no
// approval: Terraform's google_cloudbuild_trigger sends an empty one on
// update. A trigger whose builds need approval is refused.
func stripApprovalConfig(r *http.Request) error {
	if (r.Method != http.MethodPost && r.Method != http.MethodPatch) || !triggerPath.MatchString(r.URL.Path) || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return apierror.InvalidArgument("the request body could not be read")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return nil // the transcoder reports it
	}
	ac, ok := body["approvalConfig"]
	if !ok {
		return nil
	}
	var cfg struct {
		ApprovalRequired bool `json:"approvalRequired"`
	}
	_ = json.Unmarshal(ac, &cfg)
	if cfg.ApprovalRequired {
		return apierror.Unimplemented("BuildTrigger field approvalConfig.approvalRequired is not supported: build approvals are not served")
	}
	delete(body, "approvalConfig")
	raw, _ = json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return nil
}
