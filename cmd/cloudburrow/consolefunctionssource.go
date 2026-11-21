package main

// Cloud Run functions console parity, round 3 (#1853): Write a function's
// Service name and Authentication, Create landing on the function's Source
// tab, and the Source tab's Show Payload and Test, as Google's deploy page
// walks them (https://docs.cloud.google.com/run/docs/deploy-functions,
// read 2026-11-20): "Click Create ... You're redirected to the Source
// tab. Click Save and redeploy ... Optionally, click Show Payload to see
// an example payload of incoming events ... click the Test button".

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	functionsv1 "cloud.google.com/go/functions/apiv1"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	functionsShowPayloadAction = "show-payload"
	functionsSourceTestAction  = "source-test"

	// functionV1InvokerRole is the role a 1st gen function's Allow public
	// access grants allUsers, through the v1 API's policy.
	functionV1InvokerRole = "roles/cloudfunctions.invoker"
)

// runServiceNamePattern is Google's service name rule on Write a function:
// a letter first, at most 49 characters of letters, numbers and hyphens,
// not ending in a hyphen (lower case, as the function's ID must be).
var runServiceNamePattern = `^[a-z]([a-z0-9\-]{0,47}[a-z0-9])?$`

// functionAuthenticationField is Write a function's Authentication, in
// Google's words.
func functionAuthenticationField() console.Field {
	return console.Field{Name: "authentication", Label: "Authentication", Type: "radio",
		Options: []string{runAllowPublic, runRequireAuth}, Default: runRequireAuth, Section: runSectionAuth,
		Help: "Allow public access for a public HTTP function such as a webhook; Require authentication for an " +
			"event-triggered function. Allow public access grants allUsers roles/run.invoker on the function's " +
			"service (roles/cloudfunctions.invoker on a 1st gen function). Invoker IAM is stored, not enforced, locally."}
}

// writeFunctionLayout is Write a function's fields in Google's order and
// words: Service name, Region, Runtime, Trigger, Authentication, then the
// rest; Create lands on the function's Source tab.
func writeFunctionLayout(a console.Action) console.Action {
	var head, trigger, rest []console.Field
	var runtime *console.Field
	auth := functionAuthenticationField()
	for _, f := range a.Fields {
		switch {
		case f.Name == "name":
			f.Label, f.Pattern = "Service name", runServiceNamePattern
			f.Help = "A letter first, at most 49 lower case letters, numbers or hyphens, not ending in a hyphen. " +
				"A service name cannot be changed later."
			head = append(head, f)
		case f.Name == "environment", f.Name == "region":
			head = append(head, f)
		case f.Name == "authentication":
			auth = f
		case f.Name == "runtime":
			r := f
			runtime = &r
		case f.Section == "Trigger":
			trigger = append(trigger, f)
		default:
			rest = append(rest, f)
		}
	}
	out := head
	if runtime != nil {
		out = append(out, *runtime)
	}
	out = append(out, trigger...)
	out = append(out, auth)
	settings := functionSettingsFields()
	// Memory with CPU on Container(s), the secrets with the variables, the
	// key and policy on Security, as Google's tabs hold them.
	tabs := map[string]string{"memory": runTabContainers, "workerPool": runTabContainers,
		"secretEnv": runTabVariables, "secretVolumes": runTabVolumes, "kmsKey": runTabSecurity, "binaryAuthorization": runTabSecurity}
	var kept []console.Field
	for _, f := range rest {
		if t, ok := tabs[f.Name]; ok {
			f.Section, f.Tab = runSectionMore, t
			settings = append(settings, f)
			continue
		}
		kept = append(kept, f)
	}
	slices.SortStableFunc(settings, func(x, y console.Field) int {
		return functionTabOrder(x) - functionTabOrder(y)
	})
	out = append(out, settings...)
	a.Fields = append(out, kept...)
	a.Then = "/functions/{name}?tab=source"
	return a
}

// functionPublicChoice reads Authentication: true for Allow public access.
func functionPublicChoice(v string) (bool, error) {
	switch v = strings.TrimSpace(v); v {
	case "", runRequireAuth:
		return false, nil
	case runAllowPublic:
		return true, nil
	}
	return false, fmt.Errorf("authentication %q is neither %q nor %q", v, runAllowPublic, runRequireAuth)
}

// allowPublicInvocation grants allUsers the invoker role on a function just
// made: roles/run.invoker on its Cloud Run service for 2nd gen, as Google's
// Allow public access does, and roles/cloudfunctions.invoker through the v1
// API for 1st gen.
func (p functionsProvider) allowPublicInvocation(ctx context.Context, name string, gen1 bool) error {
	grant := func(pol *iampb.Policy, role string) *iampb.Policy {
		if pol == nil {
			pol = &iampb.Policy{}
		}
		for _, b := range pol.GetBindings() {
			if b.GetRole() == role && b.GetCondition() == nil {
				if !slices.Contains(b.GetMembers(), "allUsers") {
					b.Members = append(b.Members, "allUsers")
				}
				return pol
			}
		}
		pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{"allUsers"}})
		return pol
	}
	if gen1 {
		return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				return fmt.Errorf("the function was created; Allow public access failed: %w", err)
			}
			if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: grant(pol, functionV1InvokerRole)}); err != nil {
				return fmt.Errorf("the function was created; Allow public access failed: %w", err)
			}
			return nil
		})
	}
	var svc string
	if err := p.withClient(ctx, func(c *functions.FunctionClient) error {
		fn, err := c.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name})
		svc = fn.GetServiceConfig().GetService()
		return err
	}); err != nil {
		return fmt.Errorf("the function was created; Allow public access failed: %w", err)
	}
	if svc == "" {
		return fmt.Errorf("the function was created; Allow public access failed: it has no Cloud Run service")
	}
	opts, err := p.clientOptions()
	if err != nil {
		return err
	}
	c, err := runclient.NewServicesClient(ctx, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svc})
	if err != nil {
		return fmt.Errorf("the function was created; Allow public access failed: %w", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: svc, Policy: grant(pol, runInvokerRole)}); err != nil {
		return fmt.Errorf("the function was created; Allow public access failed: %w", err)
	}
	return nil
}

// sourceTabActions are the Source tab's buttons beside Edit source: Show
// Payload and Test, as Google's Source tab has them.
func sourceTabActions() []console.Action {
	test := testAction()
	test.ID, test.Label = functionsSourceTestAction, "Test"
	return []console.Action{{ID: functionsShowPayloadAction, Label: "Show Payload"}, test}
}

// showPayload is Show Payload: an example payload of the function's
// incoming events. An HTTP function's is the request body the Testing tab
// sends; an event function's is a CloudEvent of its trigger's event type,
// its attributes as the ce-* headers and its data as the event's provider
// writes it (a 1st gen function's, the legacy data and context).
func showPayload(fn *functionspb.Function, et *functionspb.EventTrigger) *console.Listing {
	parts := strings.Split(fn.GetName(), "/")
	project := parts[1]
	res := &console.Listing{Columns: []string{"Value"}, NameColumn: "Field", Noun: "fields"}
	typ := et.GetEventType()
	if typ == "" {
		res.Note = "An HTTP function receives the request as it is sent: this body, as application/json, is what Test sends."
		res.Text = functionTestBody
		return res
	}
	v := functionTriggerValues(et)
	const id, at = "1234567890", "2026-01-01T00:00:00.000Z"
	var source, subject string
	var data any
	switch v["trigger"] {
	case functionTriggerPubSub:
		topic := et.GetPubsubTopic()
		source = "//pubsub.googleapis.com/" + topic
		data = map[string]any{"message": map[string]any{
			"data": base64.StdEncoding.EncodeToString([]byte("Hello World")), "messageId": id,
			"publishTime": at, "attributes": map[string]string{}},
			"subscription": "projects/" + project + "/subscriptions/eventarc-" + parts[3] + "-" + parts[5] + "-sub"}
	case functionTriggerStorage:
		bucket := v["bucket"]
		if bucket == "" {
			bucket = "BUCKET"
		}
		source, subject = "//storage.googleapis.com/projects/_/buckets/"+bucket, "objects/hello.txt"
		data = map[string]any{"kind": "storage#object", "bucket": bucket, "name": "hello.txt", "generation": "1",
			"metageneration": "1", "contentType": "text/plain", "size": "11", "timeCreated": at, "updated": at,
			"storageClass": "STANDARD", "id": bucket + "/hello.txt/1"}
	case functionTriggerFirestore:
		db := v["database"]
		if db == "" {
			db = "(default)"
		}
		doc := strings.ReplaceAll(strings.ReplaceAll(v["document"], "{", ""), "}", "")
		if doc == "" {
			doc = "users/alice"
		}
		source, subject = "//firestore.googleapis.com/projects/"+project+"/databases/"+db, "documents/"+doc
		data = map[string]any{"value": map[string]any{
			"name":       "projects/" + project + "/databases/" + db + "/documents/" + doc,
			"fields":     map[string]any{"name": map[string]string{"stringValue": "Hello World"}},
			"createTime": at, "updateTime": at}}
		res.Note = "Firestore delivers the data as a protobuf DocumentEventData (application/protobuf); it is shown here as JSON."
	case functionTriggerAudit:
		svc := v["serviceName"]
		source = "//cloudaudit.googleapis.com/projects/" + project + "/logs/activity"
		data = map[string]any{"protoPayload": map[string]any{"@type": "type.googleapis.com/google.cloud.audit.AuditLog",
			"serviceName": svc, "methodName": v["methodName"], "resourceName": v["resourceName"],
			"authenticationInfo": map[string]string{"principalEmail": "user@example.com"}},
			"logName": "projects/" + project + "/logs/cloudaudit.googleapis.com%2Factivity", "timestamp": at}
		subject = svc + "/" + v["resourceName"]
	default:
		data = map[string]any{}
	}
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		legacy := map[string]any{"data": data, "context": map[string]any{"eventId": id, "timestamp": at,
			"eventType": functionTrigger(fn), "resource": strings.TrimPrefix(source, "//")}}
		b, _ := json.MarshalIndent(legacy, "", "  ")
		res.Note = "A 1st gen event function receives the event's data and its context."
		res.Text = string(b)
		return res
	}
	b, _ := json.MarshalIndent(data, "", "  ")
	add := func(k, v string) {
		if v != "" {
			res.Items = append(res.Items, console.Resource{Name: k, Fields: map[string]string{"Value": v}})
		}
	}
	add("ce-specversion", "1.0")
	add("ce-type", typ)
	add("ce-source", source)
	add("ce-subject", subject)
	add("ce-id", id)
	add("ce-time", at)
	res.Total = len(res.Items)
	if res.Note == "" {
		res.Note = "A CloudEvent in binary mode: these ce-* headers, and this data as the body."
	}
	res.Text = string(b)
	return res
}

// functionSettingsFields are Write a function's settings after
// Authentication, as Google's form has them: Billing, Service scaling,
// Ingress, then Container(s), Volumes, Networking, Security's tabs. Each
// is one the function's ServiceConfig carries to its Cloud Run service.
func functionSettingsFields() []console.Field {
	tab := func(f console.Field, t string) console.Field {
		f.Section, f.Tab = runSectionMore, t
		return f
	}
	return []console.Field{
		{Name: "billing", Label: "Billing", Type: "radio", Options: []string{runBillingRequest}, Default: runBillingRequest,
			Section: runSectionBilling, Help: "Request-based is Cloud Run's default; nothing is billed locally."},
		{Name: "minInstances", Label: "Minimum number of instances", Type: "text", Default: "0", Section: "Service scaling",
			Pattern: `^[0-9]{0,4}$`, Help: "0 lets the function scale to nothing between requests."},
		{Name: "maxInstances", Label: "Maximum number of instances", Type: "text", Default: "100", Section: "Service scaling",
			Pattern: `^[0-9]{0,4}$`, Help: "Optional. The most instances the function scales to."},
		{Name: "ingress", Label: "Ingress", Type: "radio", Options: []string{runIngressAll}, Default: runIngressAll,
			Section: runSectionIngress, Help: "All: allow direct access to the function from the internet (ALLOW_ALL). " +
				"Internal is not offered: every function is reachable from the host and the cluster."},
		tab(console.Field{Name: "cpu", Label: "CPU", Type: "text", Pattern: `^([0-9]+(\.[0-9]+)?|[0-9]+m)?$`,
			Help: `Optional, as the v2 API's availableCpu takes it: "1", "2", "0.5". Empty is the default for the memory.`}, runTabContainers),
		tab(console.Field{Name: "timeout", Label: "Request timeout", Type: "text", Default: "300", Pattern: `^[0-9]{0,4}$`,
			Help: "Seconds: at most 3600 for an HTTP function, 540 for an event function."}, runTabContainers),
		tab(console.Field{Name: "concurrency", Label: "Maximum concurrent requests per instance", Type: "text", Pattern: `^[0-9]{0,4}$`,
			Help: "Optional. Empty is the API's default, 1."}, runTabContainers),
		tab(console.Field{Name: "env", Label: "Environment variables", Type: "map",
			Help: "Optional. Each variable's name and value."}, runTabVariables),
		tab(console.Field{Name: "serviceAccount", Label: "Service account", Type: "text",
			Help: "Optional. The identity the function runs as; empty is the default compute service account."}, runTabSecurity),
	}
}

// functionSettings are the settings' values, checked.
type functionSettings struct {
	cpu, serviceAccount                string
	timeout, concurrency, min, max     int64
	hasTimeout, hasConcurrency, hasMax bool
	env                                map[string]string
}

func functionSettingsOf(v map[string]string) (functionSettings, error) {
	var s functionSettings
	if b := strings.TrimSpace(v["billing"]); b != "" && b != runBillingRequest {
		return s, fmt.Errorf("billing %q is not offered locally: only %s is mapped", b, runBillingRequest)
	}
	if i := strings.TrimSpace(v["ingress"]); i != "" && i != runIngressAll {
		return s, fmt.Errorf("ingress %q is not offered locally: only ALLOW_ALL is mapped", i)
	}
	num := func(field, label string, limit int64) (int64, bool, error) {
		t := strings.TrimSpace(v[field])
		if t == "" {
			return 0, false, nil
		}
		n, err := strconv.ParseInt(t, 10, 32)
		if err != nil || n < 0 || n > limit {
			return 0, false, fmt.Errorf("%s %q: a whole number from 0 to %d", label, t, limit)
		}
		return n, true, nil
	}
	var err error
	if s.min, _, err = num("minInstances", "minimum number of instances", 1000); err != nil {
		return s, err
	}
	if s.max, s.hasMax, err = num("maxInstances", "maximum number of instances", 1000); err != nil {
		return s, err
	}
	if s.hasMax && s.max > 0 && s.min > s.max {
		return s, fmt.Errorf("the minimum number of instances (%d) is more than the maximum (%d)", s.min, s.max)
	}
	if s.timeout, s.hasTimeout, err = num("timeout", "request timeout", 3600); err != nil {
		return s, err
	}
	if s.concurrency, s.hasConcurrency, err = num("concurrency", "maximum concurrent requests per instance", 1000); err != nil {
		return s, err
	}
	s.cpu, s.serviceAccount = strings.TrimSpace(v["cpu"]), strings.TrimSpace(v["serviceAccount"])
	if s.env, err = console.ParseMap(v["env"]); err != nil {
		return s, fmt.Errorf("environment variables: %w", err)
	}
	return s, nil
}

// gen1Refusal refuses the settings a 1st gen function does not have.
func (s functionSettings) gen1Refusal() error {
	if s.cpu != "" || s.hasConcurrency {
		return errors.New("CPU and maximum concurrent requests per instance are 2nd gen's: a 1st gen function serves one request at a time")
	}
	if s.hasTimeout && s.timeout > 540 {
		return fmt.Errorf("request timeout %d: a 1st gen function's is at most 540 seconds", s.timeout)
	}
	return nil
}

func (s functionSettings) v2(sc *functionspb.ServiceConfig) {
	sc.AvailableCpu, sc.ServiceAccountEmail = s.cpu, s.serviceAccount
	sc.MinInstanceCount = int32(s.min)
	if s.hasMax {
		sc.MaxInstanceCount = int32(s.max)
	}
	if s.hasTimeout {
		sc.TimeoutSeconds = int32(s.timeout)
	}
	if s.hasConcurrency {
		sc.MaxInstanceRequestConcurrency = int32(s.concurrency)
	}
	if len(s.env) > 0 {
		sc.EnvironmentVariables = s.env
	}
	sc.IngressSettings = functionspb.ServiceConfig_ALLOW_ALL
}

func (s functionSettings) setV1(fn *fv1.CloudFunction) {
	fn.ServiceAccountEmail, fn.MinInstances = s.serviceAccount, int32(s.min)
	if s.hasMax {
		fn.MaxInstances = int32(s.max)
	}
	if s.hasTimeout {
		fn.Timeout = durationpb.New(time.Duration(s.timeout) * time.Second)
	}
	if len(s.env) > 0 {
		fn.EnvironmentVariables = s.env
	}
}

// functionTabOrder orders the settings: the sections first, then
// Container(s), Variables & Secrets, Volumes and Security, as Google's tabs
// run.
func functionTabOrder(f console.Field) int {
	if f.Section != runSectionMore {
		return 0
	}
	return 1 + slices.Index([]string{runTabContainers, runTabVariables, runTabVolumes, runTabNetworking, runTabSecurity}, f.Tab)
}

// MutationBudget is Cloud Run's: Write a function's Create builds the
// function's source with Google's buildpacks before its service rolls out,
// as the functions page's own create does, so it has the same budget
// (#1853); the default 60 s answered "did not answer in time" mid-build.
func (p runProvider) MutationBudget() time.Duration {
	if p.functions != nil && p.functions.storage {
		return p.functions.MutationBudget()
	}
	return 0
}
