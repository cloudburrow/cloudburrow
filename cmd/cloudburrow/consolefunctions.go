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
	"sort"
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

var functionNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

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

func (p functionsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Environment", "Region", "Runtime", "Trigger", "URL", "Last deployed"},
		NameColumn: "Function", Noun: "functions", AlwaysStatus: true}
	for _, fn := range runadapter.Functions(p.project(project)) {
		parts := strings.Split(fn.GetName(), "/")
		out.Items = append(out.Items, console.Resource{Name: parts[5], Status: functionStatus(fn), Fields: map[string]string{
			"Environment": functionGeneration(fn), "Region": parts[3], "Runtime": fn.GetBuildConfig().GetRuntime(),
			"Trigger": functionTrigger(fn), "URL": orDash(fn.GetUrl()), "Last deployed": stampAge(fn.GetUpdateTime())}})
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
	add("Description", fn.GetDescription())
	for _, m := range fn.GetStateMessages() {
		add("Status detail", m.GetMessage())
	}
	env := console.Listing{Columns: []string{"Value"}, NameColumn: "Name", Noun: "variables"}
	for k, v := range sc.GetEnvironmentVariables() {
		env.Items = append(env.Items, console.Resource{Name: k, Fields: map[string]string{"Value": v}})
	}
	env.Total = len(env.Items)
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "variables", Label: "Runtime environment variables", Listing: env},
	}}, nil
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
	add("Description", fn.GetDescription())
	for _, m := range v2.GetStateMessages() {
		add("Status detail", m.GetMessage())
	}
	env := console.Listing{Columns: []string{"Value"}, NameColumn: "Name", Noun: "variables"}
	for k, v := range fn.GetEnvironmentVariables() {
		env.Items = append(env.Items, console.Resource{Name: k, Fields: map[string]string{"Value": v}})
	}
	sort.Slice(env.Items, func(i, j int) bool { return env.Items[i].Name < env.Items[j].Name })
	env.Total = len(env.Items)
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "variables", Label: "Runtime environment variables", Listing: env},
	}}, nil
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
	events := make([]string, 0, len(functionStorageEvents))
	for _, e := range functionStorageEvents {
		events = append(events, e.v1)
	}
	return console.Action{ID: functionsCreateAction, Label: "Create function", Fields: []console.Field{
		{Name: "environment", Label: "Environment", Type: "select", Options: []string{functionGen2, functionGen1}, Default: functionGen2,
			Help: "2nd gen is created through the Cloud Functions v2 API; 1st gen through the v1 API, as gcloud functions deploy --no-gen2 does"},
		{Name: "name", Label: "Function name", Type: "text", Required: true, Pattern: functionNamePattern.String(),
			Help: "Lower case letters, digits and hyphens, starting with a letter, at most 63"},
		{Name: "region", Label: "Region", Type: "select", Options: runadapter.FunctionRegions, Default: "us-central1"},
		{Name: "trigger", Label: "Trigger type", Type: "select", Options: []string{functionTriggerHTTP, functionTriggerPubSub, functionTriggerStorage},
			Default: functionTriggerHTTP, Section: "Trigger"},
		{Name: "topic", Label: "Cloud Pub/Sub topic", Type: "text", Section: "Trigger",
			Help: "For a Cloud Pub/Sub trigger: a topic ID in this project, or projects/{project}/topics/{topic}"},
		{Name: "bucket", Label: "Bucket", Type: "text", Section: "Trigger",
			Help: "For a Cloud Storage trigger: the bucket whose objects' events are delivered"},
		{Name: "storageEvent", Label: "Event type", Type: "select", Options: events, Default: events[0], Section: "Trigger",
			Help: "For a Cloud Storage trigger: finalize is an object created or overwritten"},
		{Name: "runtime", Label: "Runtime", Type: "select", Options: runtimes, Default: "nodejs22"},
		{Name: "entryPoint", Label: "Entry point", Type: "text", Required: true,
			Help: "The function the Functions Framework serves, such as Hello"},
		{Name: "memory", Label: "Memory", Type: "select", Options: []string{"128M", "256M", "512M", "1G", "2G", "4G"}, Default: "256M"},
		{Name: "file", Label: "Source (.zip)", Type: console.FileFieldType, Required: true,
			Help: "A .zip of the function's source, uploaded as gcloud functions deploy uploads it, and built here with Google's buildpacks (linux/amd64 images only)"},
	}}
}

// ListActions implements console.ListActor: Create function, offered when
// Cloud Storage, where the source is uploaded, runs here.
func (p functionsProvider) ListActions(context.Context, string) []console.Action {
	if !p.storage {
		return nil
	}
	return []console.Action{p.createAction()}
}

// DetailActions offers Test function on a 1st-gen HTTP function, which the
// v1 API's call reaches (#1428). A 2nd-gen function has no call method:
// it is reached at its URL.
func (p functionsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	fn, err := p.find(project, path[0])
	if err != nil || fn.GetEnvironment() != functionspb.Environment_GEN_1 || fn.GetEventTrigger() != nil {
		return nil
	}
	return []console.Action{{ID: functionsTestAction, Label: "Test function", Fields: []console.Field{
		{Name: "data", Label: "Triggering event", Type: "textarea", Default: "{}",
			Help: "The request body sent to the function, as gcloud functions call --data sends it; JSON is sent as application/json"},
	}}}
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
	if action != functionsTestAction || len(path) != 1 {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	fn, err := p.find(project, path[0])
	if err != nil {
		return nil, err
	}
	if fn.GetEnvironment() != functionspb.Environment_GEN_1 {
		return nil, errors.New("Test function calls a 1st gen function; a 2nd gen function is reached at its URL")
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

// functionTriggerSpec is the trigger the form chose, validated.
type functionTriggerSpec struct {
	kind, topic, bucket, event string
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
	parent := "projects/" + project + "/locations/" + region
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	switch env := values["environment"]; env {
	case functionGen1:
		return nil, p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
			if err != nil {
				return err
			}
			if err := putFunctionSource(ctx, up.GetUploadUrl(), data); err != nil {
				return err
			}
			fn := &fv1.CloudFunction{Name: parent + "/functions/" + id, Runtime: values["runtime"],
				EntryPoint: strings.TrimSpace(values["entryPoint"]), AvailableMemoryMb: mb,
				SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()}}
			trigger.setV1(fn)
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
		return nil, fmt.Errorf("unknown environment %q: %s or %s", env, functionGen1, functionGen2)
	}
	return nil, p.withClient(ctx, func(c *functions.FunctionClient) error {
		up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
		if err != nil {
			return err
		}
		if err := putFunctionSource(ctx, up.GetUploadUrl(), data); err != nil {
			return err
		}
		op, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			BuildConfig: &functionspb.BuildConfig{Runtime: values["runtime"], EntryPoint: strings.TrimSpace(values["entryPoint"]),
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
			ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: memory},
			EventTrigger:  trigger.v2(),
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
