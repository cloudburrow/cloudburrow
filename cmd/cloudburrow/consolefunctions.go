package main

// Cloud Functions (#1377): the functions the Cloud Functions v2 API on the
// Run port serves. The list reads the adapter's store in this process; Create
// function and Delete go through the official client, as gcloud does. A 1st
// gen function (#1428) is created, read and called through the v1 API, as
// Google's console does for one.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	functionsv1 "cloud.google.com/go/functions/apiv1"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	functionsCreateAction = "create-function"
	functionsTestAction   = "test-function"

	functionGen1 = "1st gen"
	functionGen2 = "2nd gen"

	functionTriggerHTTP    = "HTTP"
	functionTriggerPubSub  = "Cloud Pub/Sub"
	functionTriggerStorage = "Cloud Storage"

	functionPubSubEvent = "google.cloud.pubsub.topic.v1.messagePublished"
)

// functionStorageEvents are the Cloud Storage events Create function
// offers, by their 1st-gen event type, with the CloudEvent type a 2nd-gen
// trigger takes for each.
var functionStorageEvents = []struct{ v1, v2 string }{
	{"google.storage.object.finalize", "google.cloud.storage.object.v1.finalized"},
	{"google.storage.object.delete", "google.cloud.storage.object.v1.deleted"},
	{"google.storage.object.archive", "google.cloud.storage.object.v1.archived"},
	{"google.storage.object.metadataUpdate", "google.cloud.storage.object.v1.metadataUpdated"},
}

var functionNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`)

// functionsProvider is the Cloud Functions page.
type functionsProvider struct {
	defaultProject string
	runAddr        func() string
	// storage reports whether Cloud Storage runs here: a function's source
	// is uploaded to it.
	storage bool
}

func (functionsProvider) ID() string    { return "functions" }
func (functionsProvider) Title() string { return "Cloud Functions" }

// MutationBudget covers a create, which builds the source with Google's
// buildpacks before the service rolls out.
func (functionsProvider) MutationBudget() time.Duration { return 30 * time.Minute }

func (p functionsProvider) project(project string) string {
	if project == "" {
		return p.defaultProject
	}
	return project
}

func (p functionsProvider) clientOptions() ([]option.ClientOption, error) {
	addr := ""
	if p.runAddr != nil {
		addr = p.runAddr()
	}
	if addr == "" {
		return nil, errors.New("the Cloud Run adapter, which serves Cloud Functions, is not running")
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}, nil
}

func (p functionsProvider) withClient(ctx context.Context, fn func(*functions.FunctionClient) error) error {
	opts, err := p.clientOptions()
	if err != nil {
		return err
	}
	c, err := functions.NewFunctionClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("connect to Cloud Functions: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// withV1Client is withClient for the v1 API, which serves 1st gen (#1428).
func (p functionsProvider) withV1Client(ctx context.Context, fn func(*functionsv1.CloudFunctionsClient) error) error {
	opts, err := p.clientOptions()
	if err != nil {
		return err
	}
	c, err := functionsv1.NewCloudFunctionsClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("connect to Cloud Functions v1: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// functionColumns are Google's functions list columns (#1853), then
// CloudBurrow's URL.
var functionColumns = []string{"Environment", "Region", "Trigger", "Runtime", "Memory allocated", "Executed function",
	"Last deployed", "Deployed by", "Authentication", "URL"}

func (p functionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: functionColumns, NameColumn: "Name", NameAfter: "Environment", Noun: "functions", AlwaysStatus: true,
		Note: "Deployed by is the account that deployed the function on Google's console; calls here are not authenticated " +
			"(ADR-0006), so none is recorded and the column shows the API the function was deployed through."}
	for _, fn := range runadapter.Functions(p.project(project)) {
		parts := strings.Split(fn.GetName(), "/")
		by := "Cloud Functions v2 API"
		if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
			by = "Cloud Functions v1 API"
		}
		out.Items = append(out.Items, console.Resource{Name: parts[5], Status: functionStatus(fn), Fields: map[string]string{
			"Environment": functionGeneration(fn), "Region": parts[3], "Runtime": fn.GetBuildConfig().GetRuntime(),
			"Trigger": functionTrigger(fn), "Memory allocated": orDash(functionMemoryLabel(fn.GetServiceConfig().GetAvailableMemory())),
			"Executed function": orDash(fn.GetBuildConfig().GetEntryPoint()), "Last deployed": stampAge(fn.GetUpdateTime()),
			"Deployed by": by, "Authentication": orDash(p.functionAuthentication(ctx, fn)), "URL": orDash(fn.GetUrl())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// functionGeneration is a function's environment as Google's console words
// it: one made through the v1 API is 1st gen (#1428).
func functionGeneration(fn *functionspb.Function) string {
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		return functionGen1
	}
	return functionGen2
}

// functionTrigger is a function's trigger: HTTP, or its event type, in a
// 1st-gen function's legacy terms for one made through the v1 API.
func functionTrigger(fn *functionspb.Function) string {
	t := fn.GetEventTrigger().GetEventType()
	if t == "" {
		return functionTriggerHTTP
	}
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		if t == functionPubSubEvent {
			return "google.pubsub.topic.publish"
		}
		for _, e := range functionStorageEvents {
			if e.v2 == t {
				return e.v1
			}
		}
	}
	return t
}

// functionStatus is a function's state as the console's list words it.
func functionStatus(fn *functionspb.Function) string {
	switch fn.GetState() {
	case functionspb.Function_ACTIVE:
		return "ok"
	case functionspb.Function_FAILED:
		return "error"
	}
	return "pending"
}

// find returns the project's function of that ID; an ID in two regions is
// refused, since the page names functions by ID alone.
func (p functionsProvider) find(project, id string) (*functionspb.Function, error) {
	var match []*functionspb.Function
	for _, fn := range runadapter.Functions(p.project(project)) {
		if strings.HasSuffix(fn.GetName(), "/functions/"+id) {
			match = append(match, fn)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("function %q not found", id)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("function %q is in %d regions; use gcloud functions --region", id, len(match))
}

func (p functionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	fn, err := p.find(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		return p.gen1Detail(ctx, fn)
	}
	bc, sc := fn.GetBuildConfig(), fn.GetServiceConfig()
	summary := []console.Property{{Label: "State", Value: fn.GetState().String()}, {Label: "Name", Value: fn.GetName()}}
	add := func(label, v string) {
		if v != "" {
			summary = append(summary, console.Property{Label: label, Value: v})
		}
	}
	add("URL", fn.GetUrl())
	add("Runtime", bc.GetRuntime())
	add("Entry point", bc.GetEntryPoint())
	if s := bc.GetSource().GetStorageSource(); s != nil && s.GetBucket() != "" {
		add("Source", "gs://"+s.GetBucket()+"/"+s.GetObject())
	}
	add("Build", bc.GetBuild())
	add("Cloud Run service", sc.GetService())
	if svc := sc.GetService(); svc != "" {
		// The service's page has its revisions and its logs.
		add("Logs", "/run/"+svc[strings.LastIndex(svc, "/")+1:])
	}
	add("Memory", sc.GetAvailableMemory())
	if sc.GetTimeoutSeconds() > 0 {
		add("Timeout", strconv.Itoa(int(sc.GetTimeoutSeconds()))+"s")
	}
	add("Instances", fmt.Sprintf("%d to %d", sc.GetMinInstanceCount(), sc.GetMaxInstanceCount()))
	add("Service account", sc.GetServiceAccountEmail())
	add("Worker pool", bc.GetWorkerPool())
	add("KMS key", fn.GetKmsKeyName())
	add("Binary authorization", sc.GetBinaryAuthorizationPolicy())
	add("Description", fn.GetDescription())
	for _, m := range fn.GetStateMessages() {
		add("Status detail", m.GetMessage())
	}
	return console.Detail{Summary: summary, Sections: p.functionTabs(ctx, fn, summary, bc.GetRuntime(), bc.GetEntryPoint(), fn.GetUrl(),
		sc.GetEnvironmentVariables(), bc.GetEnvironmentVariables(), fn.GetEventTrigger())}, nil
}

// functionSecretsListing is a function's secrets, each exposed as an
// environment variable or mounted as a volume, as Google's console lists
// them on the Variables & Secrets tab (#1464).
func functionSecretsListing(sc *functionspb.ServiceConfig) console.Listing {
	l := console.Listing{Columns: []string{"Exposed as", "Secret", "Version"}, NameColumn: "Name", Noun: "secrets"}
	for _, s := range sc.GetSecretEnvironmentVariables() {
		l.Items = append(l.Items, console.Resource{Name: s.GetKey(), Fields: map[string]string{
			"Exposed as": "Environment variable", "Secret": functionSecretName(s.GetProjectId(), s.GetSecret()), "Version": s.GetVersion()}})
	}
	for _, v := range sc.GetSecretVolumes() {
		versions := []string{}
		for _, ver := range v.GetVersions() {
			versions = append(versions, ver.GetVersion()+" at "+ver.GetPath())
		}
		if len(versions) == 0 {
			versions = append(versions, "latest")
		}
		l.Items = append(l.Items, console.Resource{Name: v.GetMountPath(), Fields: map[string]string{
			"Exposed as": "Mounted volume", "Secret": functionSecretName(v.GetProjectId(), v.GetSecret()), "Version": strings.Join(versions, ", ")}})
	}
	l.Total = len(l.Items)
	return l
}

func functionSecretName(project, secret string) string {
	if project == "" || strings.HasPrefix(secret, "projects/") {
		return secret
	}
	return "projects/" + project + "/secrets/" + secret
}

// gen1Detail is a 1st-gen function's page: its CloudFunction as the v1 API
// reads it, as Google's console shows a 1st-gen function, with the Cloud
// Run service it is served by (#1428).
func (p functionsProvider) gen1Detail(ctx context.Context, v2 *functionspb.Function) (console.Detail, error) {
	var fn *fv1.CloudFunction
	if err := p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
		var err error
		fn, err = c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: v2.GetName()})
		return err
	}); err != nil {
		return console.Detail{Unavailable: "read the 1st gen function: " + err.Error()}, nil
	}
	summary := []console.Property{{Label: "Environment", Value: functionGen1},
		{Label: "Status", Value: fn.GetStatus().String()}, {Label: "Name", Value: fn.GetName()}}
	add := func(label, v string) {
		if v != "" {
			summary = append(summary, console.Property{Label: label, Value: v})
		}
	}
	if ht := fn.GetHttpsTrigger(); ht != nil {
		add("Trigger", functionTriggerHTTP)
		add("URL", ht.GetUrl())
	}
	if et := fn.GetEventTrigger(); et != nil {
		add("Trigger", et.GetEventType())
		add("Trigger resource", et.GetResource())
		retry := "No"
		if et.GetFailurePolicy().GetRetry() != nil {
			retry = "Yes"
		}
		add("Retry on failure", retry)
	}
	add("Runtime", fn.GetRuntime())
	add("Entry point", fn.GetEntryPoint())
	add("Source", fn.GetSourceArchiveUrl())
	add("Source upload URL", fn.GetSourceUploadUrl())
	add("Build ID", fn.GetBuildId())
	if fn.GetVersionId() > 0 {
		add("Version", strconv.FormatInt(fn.GetVersionId(), 10))
	}
	add("Cloud Run service", v2.GetServiceConfig().GetService())
	if svc := v2.GetServiceConfig().GetService(); svc != "" {
		add("Logs", "/run/"+svc[strings.LastIndex(svc, "/")+1:])
	}
	if mb := fn.GetAvailableMemoryMb(); mb > 0 {
		add("Memory allocated", strconv.Itoa(int(mb))+" MB")
	}
	if d := fn.GetTimeout(); d != nil {
		add("Timeout", strconv.Itoa(int(d.AsDuration()/time.Second))+"s")
	}
	if n := fn.GetMaxInstances(); n > 0 {
		add("Maximum instances", strconv.Itoa(int(n)))
	}
	add("Service account", fn.GetServiceAccountEmail())
	add("Ingress settings", fn.GetIngressSettings().String())
	add("Build worker pool", fn.GetBuildWorkerPool())
	add("KMS key", fn.GetKmsKeyName())
	add("Description", fn.GetDescription())
	for _, m := range v2.GetStateMessages() {
		add("Status detail", m.GetMessage())
	}
	return console.Detail{Summary: summary, Sections: p.functionTabs(ctx, v2, summary, fn.GetRuntime(), fn.GetEntryPoint(),
		fn.GetHttpsTrigger().GetUrl(), fn.GetEnvironmentVariables(), fn.GetBuildEnvironmentVariables(), v1EventTrigger(fn))}, nil
}

// Delete deletes the function and its Cloud Run service, as
// DeleteFunction does.
func (p functionsProvider) Delete(ctx context.Context, project, name string) error {
	fn, err := p.find(project, name)
	if err != nil {
		return err
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		op, err := c.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: fn.GetName()})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

func (p functionsProvider) createAction() console.Action {
	runtimes := make([]string, 0, len(runadapter.Runtimes))
	for _, r := range runadapter.Runtimes {
		runtimes = append(runtimes, r.Name)
	}
	return console.Action{ID: functionsCreateAction, Label: "Create from a ZIP upload", Submit: "Deploy", Fields: withTriggerFields([]console.Field{
		{Name: "environment", Label: "Environment", Type: "select", Options: []string{functionGen2, functionGen1}, Default: functionGen2,
			Help: "2nd gen is created through the Cloud Functions v2 API; 1st gen through the v1 API, as gcloud functions deploy --no-gen2 does"},
		{Name: "name", Label: "Function name", Type: "text", Required: true, Pattern: functionNamePattern.String(),
			Help: "Lower case letters, digits and hyphens, starting with a letter, at most 63"},
		{Name: "region", Label: "Region", Type: "select", Options: runadapter.FunctionRegions, Default: "us-central1"},
		{Name: "trigger"}, // functionTriggerFields, below
		functionAuthenticationField(),
		{Name: "runtime", Label: "Runtime", Type: "select", Options: runtimes, Default: "nodejs22"},
		{Name: "entryPoint", Label: "Entry point", Type: "text", Required: true,
			Help: "The function the Functions Framework serves, such as Hello"},
		{Name: "memory", Label: "Memory", Type: "select", Options: []string{"128M", "256M", "512M", "1G", "2G", "4G"}, Default: "256M"},
		{Name: "secretEnv", Label: "Secret environment variables", Type: "text", Section: "Security and image repo",
			Help: "KEY=secret:version, comma-separated (version a number or latest), from this instance's Secret Manager, as gcloud functions deploy --set-secrets takes them"},
		{Name: "secretVolumes", Label: "Secret volumes", Type: "text", Section: "Security and image repo",
			Help: "/mount/path=secret:version, comma-separated: the version is mounted at /mount/path/{secret}"},
		{Name: "kmsKey", Label: "Customer-managed encryption key", Type: "text", Section: "Security and image repo",
			Help: "projects/{project}/locations/{location}/keyRings/{ring}/cryptoKeys/{key}: stored on the function; nothing is encrypted with it here"},
		{Name: "binaryAuthorization", Label: "Binary Authorization policy (2nd gen)", Type: "text", Section: "Security and image repo",
			Help: "default, or a policy's name: stored on the function and its service, not enforced"},
		{Name: "workerPool", Label: "Build worker pool", Type: "text", Section: "Build",
			Help: "projects/{project}/locations/{region}/workerPools/{pool}: one of Cloud Build's private pools in the function's region; the build runs on this machine"},
		{Name: "file", Label: "Source (.zip)", Type: console.FileFieldType, Required: true,
			Help: "A .zip of the function's source, uploaded as gcloud functions deploy uploads it, and built here with Google's buildpacks (linux/amd64 images only)"},
	})}
}

// ListActions implements console.ListActor, where Cloud Storage, where
// the source is uploaded, runs here: Create function, which opens Cloud
// Run's Write a function as Google's does (#1853), and Create from a ZIP
// upload, the source given as an archive (1st gen's ZIP upload).
func (p functionsProvider) ListActions(context.Context, string) []console.Action {
	if !p.storage {
		return nil
	}
	return []console.Action{{ID: "write-function", Label: "Create function", Href: "/run?action=" + runWriteFunction}, p.createAction()}
}

// DetailActions offers Edit function and Download source on every
// function (#1491), and Test function on a 1st-gen HTTP function, which the
// v1 API's call reaches (#1428). A 2nd-gen function has no call method:
// it is reached at its URL.
func (p functionsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	fn, err := p.find(project, path[0])
	if err != nil {
		return nil
	}
	// Edit, Copy function and Download source beside the title; the tabs'
	// own actions after them (#1853).
	acts := p.functionDetailActions(ctx, fn)
	parts := strings.Split(fn.GetName(), "/")
	acts = append(acts[:1], append([]console.Action{copyAction(parts[5], parts[3])}, acts[1:]...)...)
	acts = append(acts, console.Action{ID: functionsDeleteAction, Label: "Delete", Destructive: true, Leaves: true,
		Confirm: "The function, its Cloud Run service and its trigger are deleted."})
	for _, sec := range p.functionActionTabs(ctx, fn) {
		acts = append(acts, sec.Actions...)
	}
	return acts
}

// functionActionTabs are the tabs that carry actions, read for
// DetailActions.
func (p functionsProvider) functionActionTabs(ctx context.Context, fn *functionspb.Function) []console.Section {
	runtime, entry := fn.GetBuildConfig().GetRuntime(), fn.GetBuildConfig().GetEntryPoint()
	out := []console.Section{{Actions: p.triggerActions(fn)}, {Actions: []console.Action{testAction()}}, {Actions: sourceTabActions()}}
	if a, ok := editSourceAction(fn, runtime, entry, nil); ok {
		// The files are the Source tab's; DetailActions only names it.
		out = append(out, console.Section{Actions: []console.Action{a}})
	}
	return out
}

func (p functionsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult performs Test function: the v1 API's CallFunction, answering
// with the function's result or its error and the execution ID.
func (p functionsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if action == functionsCreateAction {
		return nil, errors.New("Create function takes a source archive")
	}
	if len(path) != 1 {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	fn, err := p.find(project, path[0])
	if err != nil {
		return nil, err
	}
	switch action {
	case functionsEditAction:
		return nil, p.editFunction(ctx, fn, values)
	case functionsDownloadAction:
		return p.downloadSource(ctx, fn)
	case functionsCopyAction:
		return nil, p.copyFunction(ctx, fn, values)
	case functionsDeleteAction:
		return nil, p.Delete(ctx, project, path[0])
	case functionsEditSourceAction:
		return nil, p.saveAndRedeploy(ctx, fn, values)
	case functionsEditTriggerAction:
		return nil, p.editTrigger(ctx, fn, values)
	case functionsAddTriggerAction, functionsDeleteTriggerAction:
		return nil, p.actOnEventarc(ctx, fn, action, values)
	case functionsShowPayloadAction:
		return showPayload(fn, fn.GetEventTrigger()), nil
	case functionsTestAction, functionsSourceTestAction:
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
	if fn.GetEnvironment() != functionspb.Environment_GEN_1 {
		return testFunction(ctx, fn, values["data"])
	}
	var out *fv1.CallFunctionResponse
	if err := p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
		out, err = c.CallFunction(ctx, &fv1.CallFunctionRequest{Name: fn.GetName(), Data: values["data"]})
		return err
	}); err != nil {
		return nil, err
	}
	res := &console.Listing{Columns: []string{"Value"}, NameColumn: "Output", Noun: "outputs"}
	add := func(name, v string) {
		res.Items = append(res.Items, console.Resource{Name: name, Fields: map[string]string{"Value": v}})
	}
	add("Execution ID", out.GetExecutionId())
	if out.GetError() != "" {
		add("Error", out.GetError())
	} else {
		add("Result", out.GetResult())
	}
	res.Total = len(res.Items)
	return res, nil
}

// functionMemoryMB is a memory choice in MB, as the v1 API takes it.
func functionMemoryMB(m string) (int32, error) {
	if n, ok := strings.CutSuffix(m, "G"); ok {
		v, err := strconv.Atoi(n)
		return int32(v * 1024), err
	}
	v, err := strconv.Atoi(strings.TrimSuffix(m, "M"))
	return int32(v), err
}

// functionSecretSpec is one secret the form names: an environment
// variable's key or a volume's mount path, the secret and its version.
type functionSecretSpec struct{ at, secret, version string }

// functionSecretsOf parses a comma-separated list of at=secret[:version].
func functionSecretsOf(field, v string, mount bool) ([]functionSecretSpec, error) {
	var out []functionSecretSpec
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		at, ref, ok := strings.Cut(item, "=")
		at, ref = strings.TrimSpace(at), strings.TrimSpace(ref)
		secret, version, _ := strings.Cut(ref, ":")
		if version == "" {
			version = "latest"
		}
		if !ok || at == "" || secret == "" || (mount && !strings.HasPrefix(at, "/")) {
			want := "KEY=secret:version"
			if mount {
				want = "/mount/path=secret:version"
			}
			return nil, fmt.Errorf("%s %q: expected %s", field, item, want)
		}
		out = append(out, functionSecretSpec{at: at, secret: secret, version: version})
	}
	return out, nil
}

// functionSecrets is the form's secrets, validated.
type functionSecrets struct{ env, volumes []functionSecretSpec }

func functionSecretsFrom(values map[string]string) (functionSecrets, error) {
	env, err := functionSecretsOf("Secret environment variables", values["secretEnv"], false)
	if err != nil {
		return functionSecrets{}, err
	}
	vols, err := functionSecretsOf("Secret volumes", values["secretVolumes"], true)
	return functionSecrets{env: env, volumes: vols}, err
}

func (s functionSecrets) v2(sc *functionspb.ServiceConfig) {
	for _, e := range s.env {
		sc.SecretEnvironmentVariables = append(sc.SecretEnvironmentVariables, &functionspb.SecretEnvVar{Key: e.at, Secret: e.secret, Version: e.version})
	}
	for _, v := range s.volumes {
		sc.SecretVolumes = append(sc.SecretVolumes, &functionspb.SecretVolume{MountPath: v.at, Secret: v.secret,
			Versions: []*functionspb.SecretVolume_SecretVersion{{Version: v.version, Path: v.secret}}})
	}
}

func (s functionSecrets) setV1(fn *fv1.CloudFunction) {
	for _, e := range s.env {
		fn.SecretEnvironmentVariables = append(fn.SecretEnvironmentVariables, &fv1.SecretEnvVar{Key: e.at, Secret: e.secret, Version: e.version})
	}
	for _, v := range s.volumes {
		fn.SecretVolumes = append(fn.SecretVolumes, &fv1.SecretVolume{MountPath: v.at, Secret: v.secret,
			Versions: []*fv1.SecretVolume_SecretVersion{{Version: v.version, Path: v.secret}}})
	}
}

// functionTriggerSpec is the trigger the form chose, validated.
type functionTriggerSpec struct {
	kind, topic, bucket, event string
	// Cloud Firestore's database and document, Cloud Audit Logs' service,
	// method and resource names (#1853).
	database, document, service, method, resource string
}

func functionTriggerOf(project string, values map[string]string) (functionTriggerSpec, error) {
	t := functionTriggerSpec{kind: strings.TrimSpace(values["trigger"])}
	switch t.kind {
	case "", functionTriggerHTTP:
		t.kind = functionTriggerHTTP
	case functionTriggerPubSub:
		topic := strings.TrimSpace(values["topic"])
		if topic == "" {
			return t, errors.New("a Cloud Pub/Sub trigger needs a topic")
		}
		if !strings.HasPrefix(topic, "projects/") {
			topic = "projects/" + project + "/topics/" + topic
		}
		t.topic = topic
	case functionTriggerStorage:
		t.bucket = strings.TrimPrefix(strings.TrimSpace(values["bucket"]), "gs://")
		if t.bucket == "" {
			return t, errors.New("a Cloud Storage trigger needs a bucket")
		}
		t.event = strings.TrimSpace(values["storageEvent"])
		if t.event == "" {
			t.event = functionStorageEvents[0].v1
		}
		known := false
		for _, e := range functionStorageEvents {
			known = known || e.v1 == t.event
		}
		if !known {
			return t, fmt.Errorf("unknown Cloud Storage event type %q", t.event)
		}
	case functionTriggerFirestore:
		t.event = strings.TrimSpace(values["firestoreEvent"])
		if t.event == "" {
			t.event = "written"
		}
		if !slices.Contains(functionFirestoreEvents, t.event) {
			return t, fmt.Errorf("unknown Cloud Firestore event type %q", t.event)
		}
		t.database, t.document = strings.TrimSpace(values["database"]), strings.TrimSpace(values["document"])
		if t.database == "" {
			t.database = "(default)"
		}
	case functionTriggerAudit:
		t.service, t.method = strings.TrimSpace(values["serviceName"]), strings.TrimSpace(values["methodName"])
		t.resource = strings.TrimSpace(values["resourceName"])
		if t.service == "" || t.method == "" {
			return t, errors.New("a Cloud Audit Logs trigger needs its service name and method name")
		}
	default:
		return t, fmt.Errorf("unknown trigger type %q", t.kind)
	}
	return t, nil
}

// v2 is the trigger as a 2nd-gen function's event trigger, nil for HTTP.
func (t functionTriggerSpec) v2() *functionspb.EventTrigger {
	switch t.kind {
	case functionTriggerPubSub:
		return &functionspb.EventTrigger{EventType: functionPubSubEvent, PubsubTopic: t.topic,
			RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY}
	case functionTriggerStorage:
		typ := ""
		for _, e := range functionStorageEvents {
			if e.v1 == t.event {
				typ = e.v2
			}
		}
		return &functionspb.EventTrigger{EventType: typ, EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: t.bucket}},
			RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY}
	case functionTriggerFirestore:
		filters := []*functionspb.EventFilter{{Attribute: "database", Value: t.database}}
		if t.document != "" {
			filters = append(filters, pathFilter("document", t.document))
		}
		return &functionspb.EventTrigger{EventType: functionFirestorePrefix + t.event, EventFilters: filters,
			RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY}
	case functionTriggerAudit:
		filters := []*functionspb.EventFilter{{Attribute: "serviceName", Value: t.service}, {Attribute: "methodName", Value: t.method}}
		if t.resource != "" {
			filters = append(filters, pathFilter("resourceName", t.resource))
		}
		return &functionspb.EventTrigger{EventType: functionAuditEvent, EventFilters: filters,
			RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY}
	}
	return nil
}

// setV1 sets the trigger on a 1st-gen CloudFunction.
func (t functionTriggerSpec) setV1(fn *fv1.CloudFunction) {
	switch t.kind {
	case functionTriggerPubSub:
		fn.Trigger = &fv1.CloudFunction_EventTrigger{EventTrigger: &fv1.EventTrigger{EventType: "google.pubsub.topic.publish", Resource: t.topic}}
	case functionTriggerStorage:
		fn.Trigger = &fv1.CloudFunction_EventTrigger{EventTrigger: &fv1.EventTrigger{EventType: t.event, Resource: "projects/_/buckets/" + t.bucket}}
	default:
		fn.Trigger = &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}
	}
}

// putFunctionSource PUTs an archive to an upload URL, as gcloud does.
func putFunctionSource(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload the source: %w", err)
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload the source: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ActAtFile creates a function from an uploaded zip: GenerateUploadUrl, a
// PUT of the archive, then CreateFunction, waited on, through the v2 API
// for 2nd gen and the v1 API for 1st gen.
func (p functionsProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if action != functionsCreateAction || len(path) != 0 {
		return nil, fmt.Errorf("unknown action %q with a file", action)
	}
	if !p.storage {
		return nil, errors.New("Create function needs Cloud Storage running on this instance")
	}
	project = p.project(project)
	id := strings.TrimSpace(values["name"])
	if !functionNamePattern.MatchString(id) {
		return nil, fmt.Errorf("function name %q: lower case letters, digits and hyphens, starting with a letter", id)
	}
	region := strings.TrimSpace(values["region"])
	if region == "" {
		region = "us-central1"
	}
	trigger, err := functionTriggerOf(project, values)
	if err != nil {
		return nil, err
	}
	memory := values["memory"]
	if memory == "" {
		memory = "256M"
	}
	mb, err := functionMemoryMB(memory)
	if err != nil || mb <= 0 {
		return nil, fmt.Errorf("memory %q: expected a size such as 256M or 1G", memory)
	}
	secrets, err := functionSecretsFrom(values)
	if err != nil {
		return nil, err
	}
	pool, kms := strings.TrimSpace(values["workerPool"]), strings.TrimSpace(values["kmsKey"])
	binauthz := strings.TrimSpace(values["binaryAuthorization"])
	if binauthz != "" && values["environment"] == functionGen1 {
		return nil, errors.New("a Binary Authorization policy is 2nd gen's: a 1st gen function has none")
	}
	parent := "projects/" + project + "/locations/" + region
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if values["environment"] == functionGen1 && (trigger.kind == functionTriggerFirestore || trigger.kind == functionTriggerAudit) {
		return nil, fmt.Errorf("a 1st gen function's %s trigger is not served (the v1 API's legacy providers); choose 2nd gen, whose trigger is an Eventarc trigger", trigger.kind)
	}
	public, err := functionPublicChoice(values["authentication"])
	if err != nil {
		return nil, err
	}
	settings, err := functionSettingsOf(values)
	if err != nil {
		return nil, err
	}
	if err := p.createFunction(ctx, parent, id, values, data, trigger, secrets, settings, memory, mb, pool, kms, binauthz); err != nil {
		return nil, err
	}
	if public {
		return nil, p.allowPublicInvocation(ctx, parent+"/functions/"+id, values["environment"] == functionGen1)
	}
	return nil, nil
}

// createFunction uploads the archive and creates the function through the
// API of its environment, waiting for the deploy.
func (p functionsProvider) createFunction(ctx context.Context, parent, id string, values map[string]string, data []byte,
	trigger functionTriggerSpec, secrets functionSecrets, settings functionSettings, memory string, mb int32, pool, kms, binauthz string) error {
	switch env := values["environment"]; env {
	case functionGen1:
		if err := settings.gen1Refusal(); err != nil {
			return err
		}
		return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: kms})
			if err != nil {
				return err
			}
			if err := putFunctionSource(ctx, up.GetUploadUrl(), data); err != nil {
				return err
			}
			fn := &fv1.CloudFunction{Name: parent + "/functions/" + id, Runtime: values["runtime"],
				EntryPoint: strings.TrimSpace(values["entryPoint"]), AvailableMemoryMb: mb,
				SourceCode:      &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
				BuildWorkerPool: pool, KmsKeyName: kms}
			trigger.setV1(fn)
			secrets.setV1(fn)
			settings.setV1(fn)
			op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: fn})
			if err != nil {
				return err
			}
			if _, err := op.Wait(ctx); err != nil {
				return fmt.Errorf("the function was not deployed: %w", err)
			}
			return nil
		})
	case "", functionGen2:
	default:
		return fmt.Errorf("unknown environment %q: %s or %s", env, functionGen1, functionGen2)
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent, KmsKeyName: kms})
		if err != nil {
			return err
		}
		if err := putFunctionSource(ctx, up.GetUploadUrl(), data); err != nil {
			return err
		}
		sc := &functionspb.ServiceConfig{AvailableMemory: memory, BinaryAuthorizationPolicy: binauthz}
		secrets.v2(sc)
		settings.v2(sc)
		op, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			BuildConfig: &functionspb.BuildConfig{Runtime: values["runtime"], EntryPoint: strings.TrimSpace(values["entryPoint"]), WorkerPool: pool,
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
			ServiceConfig: sc,
			EventTrigger:  trigger.v2(),
			KmsKeyName:    kms,
		}})
		if err != nil {
			return err
		}
		if _, err := op.Wait(ctx); err != nil {
			return fmt.Errorf("the function was not deployed: %w", err)
		}
		return nil
	})
}
