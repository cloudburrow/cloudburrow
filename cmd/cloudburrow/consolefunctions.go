package main

// Cloud Functions (#1377): the functions the Cloud Functions v2 API on the
// Run port serves. The list reads the adapter's store in this process; Create
// function and Delete go through the official client, as gcloud does.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const functionsCreateAction = "create-function"

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

func (p functionsProvider) withClient(ctx context.Context, fn func(*functions.FunctionClient) error) error {
	addr := ""
	if p.runAddr != nil {
		addr = p.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter, which serves Cloud Functions, is not running")
	}
	c, err := functions.NewFunctionClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Cloud Functions: %w", err)
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
		return "1st gen"
	}
	return "2nd gen"
}

// functionTrigger is a function's trigger: HTTP, or its event type.
func functionTrigger(fn *functionspb.Function) string {
	if t := fn.GetEventTrigger().GetEventType(); t != "" {
		return t
	}
	return "HTTP"
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

func (p functionsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	fn, err := p.find(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
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
	return console.Action{ID: functionsCreateAction, Label: "Create function", Fields: []console.Field{
		{Name: "name", Label: "Function name", Type: "text", Required: true, Pattern: functionNamePattern.String(),
			Help: "Lower case letters, digits and hyphens, starting with a letter, at most 63"},
		{Name: "region", Label: "Region", Type: "select", Options: runadapter.FunctionRegions, Default: "us-central1"},
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

func (p functionsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

func (p functionsProvider) ActAt(context.Context, string, []string, string, map[string]string) error {
	return errors.New("Create function takes a source archive")
}

// ActAtFile creates a function from an uploaded zip: GenerateUploadUrl, a
// PUT of the archive, then CreateFunction, waited on.
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
	parent := "projects/" + project + "/locations/" + region
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return nil, p.withClient(ctx, func(c *functions.FunctionClient) error {
		up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.GetUploadUrl(), bytes.NewReader(data))
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
		op, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			BuildConfig: &functionspb.BuildConfig{Runtime: values["runtime"], EntryPoint: strings.TrimSpace(values["entryPoint"]),
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
			ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: values["memory"]},
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
