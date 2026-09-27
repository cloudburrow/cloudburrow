package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// fakeRunServices is a Cloud Run Services server holding one service. Its
// operations are returned already done, so a client's Wait returns at once
// with the result or the failure the test chose.
type fakeRunServices struct {
	runpb.UnimplementedServicesServer

	mu      sync.Mutex
	service *runpb.Service
	updates []*runpb.UpdateServiceRequest
	gets    int
	rollout *status.Status // non-nil: the update's operation fails with it
}

func (f *fakeRunServices) GetService(_ context.Context, req *runpb.GetServiceRequest) (*runpb.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.service == nil || req.GetName() != f.service.GetName() {
		return nil, status.Errorf(codes.NotFound, "service %s not found", req.GetName())
	}
	return proto.Clone(f.service).(*runpb.Service), nil
}

func (f *fakeRunServices) UpdateService(_ context.Context, req *runpb.UpdateServiceRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, proto.Clone(req).(*runpb.UpdateServiceRequest))
	op := &longrunningpb.Operation{Name: "operations/update-1", Done: true}
	if f.rollout != nil {
		op.Result = &longrunningpb.Operation_Error{Error: f.rollout.Proto()}
		return op, nil
	}
	resp, err := anypb.New(req.GetService())
	if err != nil {
		return nil, err
	}
	op.Result = &longrunningpb.Operation_Response{Response: resp}
	return op, nil
}

// serve starts the fake on a loopback port and returns a provider pointed at it.
func (f *fakeRunServices) serve(t *testing.T) runProvider {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	runpb.RegisterServicesServer(g, f)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return runProvider{runAddr: fixedAddr(ln.Addr().String()), defaultProject: "demo", region: "us-central1"}
}

const editedServiceName = "projects/demo/locations/us-central1/services/api"

// deployedService is what the adapter's GetService answers for a service
// with a label, a probe, a working directory, a secret-backed variable and
// two plain ones: the settings the edit form does not show and must not drop.
func deployedService() *runpb.Service {
	return &runpb.Service{
		Name:   editedServiceName,
		Etag:   "12345",
		Labels: map[string]string{"team": "payments"},
		Template: &runpb.RevisionTemplate{
			Labels: map[string]string{"tier": "web"},
			Containers: []*runpb.Container{{
				Image:      "example.com/api:v1",
				WorkingDir: "/srv",
				StartupProbe: &runpb.Probe{ProbeType: &runpb.Probe_TcpSocket{
					TcpSocket: &runpb.TCPSocketAction{Port: 8080}}},
				Env: []*runpb.EnvVar{
					{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "live"}},
					{Name: "STALE", Values: &runpb.EnvVar_Value{Value: "old"}},
					{Name: "TOKEN", Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-key", Version: "latest"}}}},
				},
				Resources: &runpb.ResourceRequirements{Limits: map[string]string{"cpu": "1"}, CpuIdle: true},
			}},
		},
	}
}

// The form as the browser submits it: every field but the immutable name.
func editValues(env map[string]string) map[string]string {
	return map[string]string{
		"image": "example.com/api:v2", "port": "9090", "command": "/bin/api serve", "args": "",
		"env": console.FormatMap(env), "cpu": "2", "memory": "1Gi",
		"minInstances": "1", "maxInstances": "5", "concurrency": "40", "timeout": "120",
	}
}

// TestRunEditFormIsTheDeployFormPrefilledFromTheServingRevision.
//
// Changing one variable meant retyping the name, image, port, limits, scaling
// and every other variable, because the deploy form opened on the helloworld
// image. The edit form is the same form holding what the serving revision runs.
func TestRunEditFormIsTheDeployFormPrefilledFromTheServingRevision(t *testing.T) {
	var rev knTemplate
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api-00001", "annotations": {
			"autoscaling.knative.dev/min-scale": "1", "autoscaling.knative.dev/max-scale": "10"}},
		"spec": {"containerConcurrency": 80, "timeoutSeconds": 600, "containers": [{
			"image": "example.com/api:v1", "command": ["/bin/api"], "args": ["--verbose", "--port=8080"],
			"ports": [{"containerPort": 8080}],
			"resources": {"limits": {"cpu": "1", "memory": "512Mi"}},
			"env": [{"name": "MODE", "value": "live"},
			        {"name": "TOKEN", "valueFrom": {"secretKeyRef": {"name": "api-key", "key": "latest"}}}]}]}}`),
		&rev); err != nil {
		t.Fatal(err)
	}

	form := runEditForm("api", rev, "Prefilled from the serving revision, api-00001.")
	if form == nil {
		t.Fatal("a one-container service offers no edit form")
	}
	if form.Label != "Edit and deploy new revision" {
		t.Errorf("label = %q", form.Label)
	}

	_, deploy := runProvider{}.CreateForm()
	if len(form.Fields) != len(deploy) {
		t.Fatalf("edit form has %d fields, the deploy form %d: the two must offer the same settings",
			len(form.Fields), len(deploy))
	}
	byName := map[string]console.Field{}
	for i, f := range form.Fields {
		if f.Name != deploy[i].Name {
			t.Errorf("field %d is %q, the deploy form's is %q", i, f.Name, deploy[i].Name)
		}
		byName[f.Name] = f
	}
	for name, want := range map[string]string{
		"name": "api", "image": "example.com/api:v1", "port": "8080",
		"command": "/bin/api", "args": "--verbose --port=8080",
		"cpu": "1", "memory": "512Mi", "minInstances": "1", "maxInstances": "10",
		"concurrency": "80", "timeout": "600",
	} {
		if got := byName[name].Default; got != want {
			t.Errorf("%s prefilled as %q, want %q", name, got, want)
		}
	}
	// The secret-backed variable is not a plain value, so it is not offered as
	// one; the note says it is kept.
	env, err := console.ParseMap(byName["env"].Default)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env["MODE"] != "live" {
		t.Errorf("env prefilled as %v, want only MODE=live", env)
	}
	if !strings.Contains(form.Note, "TOKEN") || !strings.Contains(form.Note, "api-00001") {
		t.Errorf("the note names neither the kept secret variable nor the source revision: %s", form.Note)
	}

	name := byName["name"]
	if !name.Immutable || name.Help != runNameImmutable {
		t.Errorf("service name field = %+v, want immutable with the help %q", name, runNameImmutable)
	}
}

// A form describes one container. Offering it for a multi-container revision
// would be a control that has to guess which container it means.
func TestRunEditFormIsAbsentForSeveralContainers(t *testing.T) {
	var rev knTemplate
	if err := json.Unmarshal([]byte(`{"spec": {"containers": [
		{"image": "example.com/api:v1"}, {"image": "example.com/proxy:v1"}]}}`), &rev); err != nil {
		t.Fatal(err)
	}
	if form := runEditForm("api", rev, ""); form != nil {
		t.Errorf("a two-container service offers an edit form: %+v", form)
	}
}

// The form splits the entrypoint and arguments on spaces, so an argument that
// holds one cannot survive a round trip through it. Offering the form would
// deploy a different command from the one serving.
func TestRunEditFormIsAbsentWhenAnArgumentHoldsASpace(t *testing.T) {
	var rev knTemplate
	if err := json.Unmarshal([]byte(`{"spec": {"containers": [
		{"image": "example.com/api:v1", "command": ["sh"], "args": ["-c", "echo hi"]}]}}`), &rev); err != nil {
		t.Fatal(err)
	}
	if form := runEditForm("api", rev, ""); form != nil {
		t.Error("an argument with a space in it was offered for editing through a space-split field")
	}
}

// TestRunEditFormReadsTheServingRevisionNotTheLatestTemplate.
//
// After a failed rollout the service's template is the configuration that
// failed. Prefilling from it would hand back the broken image as the starting
// point for the fix.
func TestRunEditFormReadsTheServingRevisionNotTheLatestTemplate(t *testing.T) {
	bin := t.TempDir()
	revisions := `{"items": [
		{"metadata": {"name": "api-00001", "labels": {"serving.knative.dev/service": "other"}},
		 "spec": {"containers": [{"image": "example.com/other:v1"}]}},
		{"metadata": {"name": "api-00001", "labels": {"serving.knative.dev/service": "api"}},
		 "spec": {"containers": [{"image": "example.com/api:good"}]}}]}`
	script := "#!/bin/sh\ncat <<'EOF'\n" + revisions + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var svc ksvcStatus
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api"},
		"status": {"latestReadyRevisionName": "api-00001", "latestCreatedRevisionName": "api-00002"},
		"spec": {"template": {"spec": {"containers": [{"image": "example.com/api:broken"}]}}}}`), &svc); err != nil {
		t.Fatal(err)
	}
	p := runProvider{kubeconfig: "/instance/kubeconfig", namespace: "default", runAddr: fixedAddr("127.0.0.1:1")}
	form := p.editForm(context.Background(), "api", &svc)
	if form == nil {
		t.Fatal("no edit form")
	}
	for _, f := range form.Fields {
		if f.Name == "image" && f.Default != "example.com/api:good" {
			t.Errorf("image prefilled as %q, want the serving revision's example.com/api:good", f.Default)
		}
	}

	// With no adapter there is no update path, and so no button.
	if form := (runProvider{}).editForm(context.Background(), "api", &svc); form != nil {
		t.Error("an edit form is offered with no Cloud Run adapter to submit it to")
	}
}

// TestRunEditRefusesARenameWithTheFieldsOwnMessage.
//
// The client never sends an immutable field, but the route is an API, and a
// request that renames the service must be refused with the sentence the
// form already showed, before anything reaches the adapter.
func TestRunEditRefusesARenameWithTheFieldsOwnMessage(t *testing.T) {
	fake := &fakeRunServices{service: deployedService()}
	p := fake.serve(t)

	values := editValues(map[string]string{"MODE": "live"})
	values["name"] = "renamed"
	err := p.Edit(context.Background(), "demo", []string{"api"}, values)
	if err == nil || err.Error() != runNameImmutable {
		t.Fatalf("rename = %v, want %q", err, runNameImmutable)
	}
	if fake.gets != 0 || len(fake.updates) != 0 {
		t.Errorf("a refused rename still reached the adapter: %d gets, %d updates", fake.gets, len(fake.updates))
	}

	// The same name is not a rename.
	values["name"] = "api"
	if err := p.Edit(context.Background(), "demo", []string{"api"}, values); err != nil {
		t.Errorf("submitting the unchanged name = %v", err)
	}

	// And a revision is not editable at all.
	if err := p.Edit(context.Background(), "demo", []string{"api", "api-00001"}, values); err == nil {
		t.Error("a revision was accepted for editing")
	}
}

// TestRunEditDeploysTheFormThroughUpdateService.
//
// UpdateService replaces the whole configuration, so the edit reads the
// service back and replaces only what the form holds. Sending the form alone
// would drop the label, the probe, the working directory and the
// secret-backed variable in silence.
func TestRunEditDeploysTheFormThroughUpdateService(t *testing.T) {
	fake := &fakeRunServices{service: deployedService()}
	p := fake.serve(t)

	if err := p.Edit(context.Background(), "demo", []string{"api"},
		editValues(map[string]string{"MODE": "test", "EXTRA": "1"})); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(fake.updates) != 1 {
		t.Fatalf("%d UpdateService calls, want 1", len(fake.updates))
	}
	req := fake.updates[0]
	if len(req.GetUpdateMask().GetPaths()) != 0 {
		t.Errorf("update mask %v: the adapter refuses a mask, and the full service is sent", req.GetUpdateMask())
	}
	svc := req.GetService()
	if svc.GetName() != editedServiceName {
		t.Errorf("name = %q", svc.GetName())
	}
	if svc.GetEtag() != "" {
		t.Errorf("etag %q sent; Knative's status writes would make it stale", svc.GetEtag())
	}
	if svc.GetLabels()["team"] != "payments" || svc.GetTemplate().GetLabels()["tier"] != "web" {
		t.Errorf("labels dropped: %v / %v", svc.GetLabels(), svc.GetTemplate().GetLabels())
	}

	c := svc.GetTemplate().GetContainers()[0]
	if c.GetImage() != "example.com/api:v2" || c.GetWorkingDir() != "/srv" || c.GetStartupProbe() == nil {
		t.Errorf("container = %v", c)
	}
	if strings.Join(c.GetCommand(), " ") != "/bin/api serve" || len(c.GetArgs()) != 0 {
		t.Errorf("command %v args %v", c.GetCommand(), c.GetArgs())
	}
	if len(c.GetPorts()) != 1 || c.GetPorts()[0].GetContainerPort() != 9090 {
		t.Errorf("ports = %v", c.GetPorts())
	}
	if l := c.GetResources().GetLimits(); l["cpu"] != "2" || l["memory"] != "1Gi" || !c.GetResources().GetCpuIdle() {
		t.Errorf("resources = %v", c.GetResources())
	}

	env := map[string]*runpb.EnvVar{}
	for _, e := range c.GetEnv() {
		env[e.GetName()] = e
	}
	if env["MODE"].GetValue() != "test" || env["EXTRA"].GetValue() != "1" {
		t.Errorf("plain variables = %v", c.GetEnv())
	}
	if _, ok := env["STALE"]; ok {
		t.Error("a plain variable removed from the form was kept")
	}
	if env["TOKEN"].GetValueSource().GetSecretKeyRef().GetSecret() != "api-key" {
		t.Errorf("the secret-backed variable was dropped: %v", c.GetEnv())
	}

	tmpl := svc.GetTemplate()
	if tmpl.GetScaling().GetMinInstanceCount() != 1 || tmpl.GetScaling().GetMaxInstanceCount() != 5 ||
		tmpl.GetMaxInstanceRequestConcurrency() != 40 || tmpl.GetTimeout().GetSeconds() != 120 {
		t.Errorf("template settings = scaling %v, concurrency %d, timeout %v",
			tmpl.GetScaling(), tmpl.GetMaxInstanceRequestConcurrency(), tmpl.GetTimeout())
	}
}

// TestRunEditReportsTheAdaptersRolloutFailure.
//
// A rollout that fails is reported with the adapter's own message and code,
// and without the gRPC envelope, so the operations ledger says why.
func TestRunEditReportsTheAdaptersRolloutFailure(t *testing.T) {
	fake := &fakeRunServices{
		service: deployedService(),
		rollout: status.New(codes.FailedPrecondition,
			"revision failed: Unable to fetch image \"example.com/api:missing\""),
	}
	p := fake.serve(t)

	values := editValues(map[string]string{"MODE": "live"})
	values["image"] = "example.com/api:missing"
	err := p.Edit(context.Background(), "demo", []string{"api"}, values)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rollout failure = %v, want FailedPrecondition", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "revision failed: Unable to fetch image") {
		t.Errorf("the adapter's message was lost: %q", msg)
	}
	if strings.Contains(msg, "rpc error") {
		t.Errorf("the gRPC envelope reached the message: %q", msg)
	}
}

// A service the adapter does not have is its NOT_FOUND, not a guess.
func TestRunEditOfAMissingServiceIsTheAdaptersNotFound(t *testing.T) {
	fake := &fakeRunServices{}
	p := fake.serve(t)
	err := p.Edit(context.Background(), "demo", []string{"api"}, editValues(nil))
	if status.Code(err) != codes.NotFound {
		t.Errorf("edit of a missing service = %v, want NotFound", err)
	}
	if len(fake.updates) != 0 {
		t.Error("UpdateService was called for a service that does not exist")
	}
}

// fixedAddr is an adapter address that is already known.
func fixedAddr(a string) func() string { return func() string { return a } }
