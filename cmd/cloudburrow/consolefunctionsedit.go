package main

// Edit function, Download source and a 1st-gen function's Permissions tab
// (#1491): the console parity audit found UpdateFunction and
// GenerateDownloadUrl, in both APIs, and the v1 API's IAM methods reachable
// only from gcloud. Each goes through the official client of the API that
// serves the function's generation, as Google's console does.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	functionsv1 "cloud.google.com/go/functions/apiv1"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	functionsEditAction     = "edit-function"
	functionsDownloadAction = "download-source"
)

// functionEditAction is Edit function, prefilled from the function: its
// description, memory, timeout, maximum instances and runtime environment
// variables, the settings Google's Edit changes without a new source.
func functionEditAction(description, memory, timeout, maxInstances string, env map[string]string, gen1 bool) console.Action {
	memHelp := "A size such as 256M or 1G; a new revision of the function's Cloud Run service rolls out"
	if gen1 {
		memHelp = "In MB, such as 256, as the v1 API's availableMemoryMb takes it"
	}
	return console.Action{ID: functionsEditAction, Label: "Edit function", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: description},
		{Name: "memory", Label: "Memory", Type: "text", Required: true, Default: memory, Help: memHelp},
		{Name: "timeout", Label: "Timeout (seconds)", Type: "number", Default: timeout, Help: "Empty keeps the function's"},
		{Name: "maxInstances", Label: "Maximum instances", Type: "number", Default: maxInstances, Help: "Empty or 0 is no limit"},
		{Name: "env", Label: "Runtime environment variables", Type: "text", Default: repoLabelsText(env),
			Help: "KEY=value pairs, comma separated, replacing the function's; empty clears them"},
	}}
}

// functionDownloadAction is Download source: GenerateDownloadUrl, answered
// with the link it returns.
func functionDownloadAction() console.Action {
	return console.Action{ID: functionsDownloadAction, Label: "Download source"}
}

// functionDetailActions are Edit function and Download source on every
// function's page, prefilled from what the page read.
func (p functionsProvider) functionDetailActions(ctx context.Context, fn *functionspb.Function) []console.Action {
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		var v1 *fv1.CloudFunction
		if err := p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			var err error
			v1, err = c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fn.GetName()})
			return err
		}); err != nil {
			return []console.Action{functionDownloadAction()}
		}
		timeout := ""
		if d := v1.GetTimeout(); d != nil {
			timeout = strconv.Itoa(int(d.GetSeconds()))
		}
		return []console.Action{functionEditAction(v1.GetDescription(), strconv.Itoa(int(v1.GetAvailableMemoryMb())), timeout,
			strconv.Itoa(int(v1.GetMaxInstances())), v1.GetEnvironmentVariables(), true), functionDownloadAction()}
	}
	sc := fn.GetServiceConfig()
	timeout := ""
	if sc.GetTimeoutSeconds() > 0 {
		timeout = strconv.Itoa(int(sc.GetTimeoutSeconds()))
	}
	return []console.Action{functionEditAction(fn.GetDescription(), strings.TrimSuffix(sc.GetAvailableMemory(), "i"), timeout,
		strconv.Itoa(int(sc.GetMaxInstanceCount())), sc.GetEnvironmentVariables(), false), functionDownloadAction()}
}

// functionEdit is the form's values, validated.
type functionEdit struct {
	description, memory string
	timeout, max        int64
	hasTimeout          bool
	env                 map[string]string
}

func functionEditOf(v map[string]string) (functionEdit, error) {
	e := functionEdit{description: strings.TrimSpace(v["description"]), memory: strings.TrimSpace(v["memory"])}
	if e.memory == "" {
		return e, errors.New("memory is required")
	}
	if t := strings.TrimSpace(v["timeout"]); t != "" {
		n, err := strconv.ParseInt(t, 10, 32)
		if err != nil || n <= 0 {
			return e, fmt.Errorf("timeout %q: expected a whole number of seconds", t)
		}
		e.timeout, e.hasTimeout = n, true
	}
	if m := strings.TrimSpace(v["maxInstances"]); m != "" {
		n, err := strconv.ParseInt(m, 10, 32)
		if err != nil || n < 0 {
			return e, fmt.Errorf("maximum instances %q: expected a whole number", m)
		}
		e.max = n
	}
	env, err := parseAnnotations(v["env"])
	if err != nil {
		return e, fmt.Errorf("runtime environment variables: %w", err)
	}
	e.env = env
	return e, nil
}

// editFunction is UpdateFunction with the form's fields in its mask,
// waited on, through the API of the function's generation.
func (p functionsProvider) editFunction(ctx context.Context, fn *functionspb.Function, v map[string]string) error {
	e, err := functionEditOf(v)
	if err != nil {
		return err
	}
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		mb, err := functionMemoryMB(strings.TrimSuffix(strings.ToUpper(e.memory), "B"))
		if err != nil || mb <= 0 {
			return fmt.Errorf("memory %q: expected MB, such as 256", e.memory)
		}
		in := &fv1.CloudFunction{Name: fn.GetName(), Description: e.description, AvailableMemoryMb: mb,
			MaxInstances: int32(e.max), EnvironmentVariables: e.env}
		mask := []string{"description", "availableMemoryMb", "maxInstances", "environmentVariables"}
		if e.hasTimeout {
			in.Timeout, mask = durationpb.New(time.Duration(e.timeout)*time.Second), append(mask, "timeout")
		}
		return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			op, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
			if err != nil {
				return err
			}
			if _, err := op.Wait(ctx); err != nil {
				return fmt.Errorf("the function was not redeployed: %w", err)
			}
			return nil
		})
	}
	if mb, err := functionMemoryMB(e.memory); err != nil || mb <= 0 {
		return fmt.Errorf("memory %q: expected a size such as 256M or 1G", e.memory)
	}
	sc := &functionspb.ServiceConfig{AvailableMemory: e.memory, MaxInstanceCount: int32(e.max), EnvironmentVariables: e.env}
	mask := []string{"description", "service_config.available_memory", "service_config.max_instance_count", "service_config.environment_variables"}
	if e.hasTimeout {
		sc.TimeoutSeconds, mask = int32(e.timeout), append(mask, "service_config.timeout_seconds")
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		op, err := c.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{
			Function:   &functionspb.Function{Name: fn.GetName(), Description: e.description, ServiceConfig: sc},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		if err != nil {
			return err
		}
		if _, err := op.Wait(ctx); err != nil {
			return fmt.Errorf("the function was not redeployed: %w", err)
		}
		return nil
	})
}

// downloadSource is GenerateDownloadUrl, answered with its link as the
// result's one row.
func (p functionsProvider) downloadSource(ctx context.Context, fn *functionspb.Function) (*console.Listing, error) {
	var link string
	var err error
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		err = p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			r, err := c.GenerateDownloadUrl(ctx, &fv1.GenerateDownloadUrlRequest{Name: fn.GetName()})
			link = r.GetDownloadUrl()
			return err
		})
	} else {
		err = p.withClient(ctx, func(c *functions.FunctionClient) error {
			r, err := c.GenerateDownloadUrl(ctx, &functionspb.GenerateDownloadUrlRequest{Name: fn.GetName()})
			link = r.GetDownloadUrl()
			return err
		})
	}
	if err != nil {
		return nil, err
	}
	if link != "" && !strings.Contains(link, "://") {
		link = "http://" + link
	}
	return &console.Listing{Columns: []string{"URL"}, NameColumn: "Source", Noun: "archives",
		Note:  "The link reads the .zip the function was built from, as GenerateDownloadUrl returns it.",
		Items: []console.Resource{{Name: "Download the source (.zip)", Link: link, Fields: map[string]string{"URL": link}}}, Total: 1}, nil
}

// PolicyOn is the Permissions tab on a 1st-gen function's page: the v1
// API's GetIamPolicy, SetIamPolicy and TestIamPermissions. A 2nd-gen
// function's invokers are its Cloud Run service's, on the service's page.
func (functionsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	for _, fn := range runadapter.Functions("") {
		if strings.HasSuffix(fn.GetName(), "/functions/"+path[0]) && fn.GetEnvironment() == functionspb.Environment_GEN_1 {
			return &console.PolicyTarget{Link: compatibilityDoc + "functions-and-source-builds",
				RoleHelp: roleHelp("Cloud Functions", "roles/cloudfunctions.invoker")}
		}
	}
	return nil
}

// v1FunctionsIAM is the v1 client's IAM methods as iamServer.
type v1FunctionsIAM struct{ c *functionsv1.CloudFunctionsClient }

func (i v1FunctionsIAM) GetIamPolicy(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return i.c.GetIamPolicy(ctx, r)
}

func (i v1FunctionsIAM) SetIamPolicy(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return i.c.SetIamPolicy(ctx, r)
}

func (i v1FunctionsIAM) TestIamPermissions(ctx context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return i.c.TestIamPermissions(ctx, r)
}

// withFunctionIAM calls fn with the v1 API's IAM methods and the 1st-gen
// function's full name.
func (p functionsProvider) withFunctionIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	f, err := p.find(project, path[0])
	if err != nil {
		return err
	}
	if f.GetEnvironment() != functionspb.Environment_GEN_1 {
		return errors.New("a 2nd gen function's invokers are set on its Cloud Run service's Permissions tab")
	}
	return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
		return fn(v1FunctionsIAM{c}, f.GetName())
	})
}

func (p functionsProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withFunctionIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p functionsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withFunctionIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p functionsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withFunctionIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = functionsProvider{}
