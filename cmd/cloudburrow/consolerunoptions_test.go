package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

func runEnvSummary(env []*runpb.EnvVar) []string {
	var out []string
	for _, e := range env {
		if ref := e.GetValueSource().GetSecretKeyRef(); ref != nil {
			out = append(out, e.GetName()+"<-"+ref.GetSecret()+":"+ref.GetVersion())
			continue
		}
		out = append(out, e.GetName()+"="+e.GetValue())
	}
	return out
}

// The secretEnv field is one NAME=secret or NAME=secret:version per line,
// and becomes a secretKeyRef variable; a variable cannot be both plain and
// secret-backed (#852).
func TestRunSecretEnvFieldBecomesSecretKeyRefs(t *testing.T) {
	c := &runpb.Container{Env: []*runpb.EnvVar{{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "live"}}}}
	if err := withSecretEnv(c, `{"TOKEN":"api-key","DB":"projects/p/secrets/db:3"}`); err != nil {
		t.Fatal(err)
	}
	if got, want := runEnvSummary(c.GetEnv()), []string{"MODE=live", "DB<-projects/p/secrets/db:3", "TOKEN<-api-key:"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v; want %v", got, want)
	}
	for _, bad := range []string{`{"MODE":"api-key"}`, `{"X":""}`, `{"X":"api-key:"}`, `{"X":":3"}`, "not json"} {
		c := &runpb.Container{Env: []*runpb.EnvVar{{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "live"}}}}
		if err := withSecretEnv(c, bad); err == nil {
			t.Errorf("secretEnv %s was accepted", bad)
		}
	}
}

// Edit and deploy new revision is prefilled with the service's labels and its
// secret-backed variables, the secret named as the adapter recorded it rather
// than as the Kubernetes Secret it resolved to; saving it replaces both on
// the service, so one removed on the form is removed (#852).
func TestRunEditSetsLabelsAndSecretBackedVariables(t *testing.T) {
	var svc ksvcStatus
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api", "annotations": {"`+runadapter.ServiceLabelsAnnotation+`": "{\"team\":\"payments\"}"}},
		"spec": {"template": {
			"metadata": {"annotations": {"`+runadapter.SecretEnvAnnotation+`": "{\"TOKEN\":{\"secret\":\"api-key\",\"version\":\"2\"}}"}},
			"spec": {"containers": [{"image": "example.com/api:v1", "env": [
				{"name": "MODE", "value": "live"},
				{"name": "TOKEN", "valueFrom": {"secretKeyRef": {"name": "cb-secret-api-key-2", "key": "payload"}}},
				{"name": "ORPHAN", "valueFrom": {"secretKeyRef": {"name": "someone-elses", "key": "k"}}}]}]}}}}`), &svc); err != nil {
		t.Fatal(err)
	}
	p := runProvider{runAddr: fixedAddr("127.0.0.1:1")}
	form := p.editForm(context.Background(), "api", &svc)
	if form == nil {
		t.Fatal("no edit form")
	}
	got := map[string]string{}
	for _, f := range form.Fields {
		got[f.Name] = f.Default
	}
	if got["labels"] != `{"team":"payments"}` || got["secretEnv"] != `{"TOKEN":"api-key:2"}` || got["env"] != `{"MODE":"live"}` {
		t.Errorf("prefilled labels %q, secretEnv %q, env %q", got["labels"], got["secretEnv"], got["env"])
	}
	if !strings.Contains(form.Note, "ORPHAN") {
		t.Errorf("the note does not name the variable whose secret has no record: %s", form.Note)
	}

	fake := &fakeRunServices{service: deployedService()}
	rp := fake.serve(t)
	values := editValues(map[string]string{"MODE": "test"})
	values["labels"] = `{"team":"ledger","env":"dev"}`
	values["secretEnv"] = `{"DB":"db-password:latest"}`
	if err := rp.Edit(context.Background(), "demo", []string{"api"}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	sent := fake.updates[0].GetService()
	if !reflect.DeepEqual(sent.GetLabels(), map[string]string{"team": "ledger", "env": "dev"}) {
		t.Errorf("labels sent = %v", sent.GetLabels())
	}
	// TOKEN was removed from the form, so it is gone; DB is new.
	if got, want := runEnvSummary(sent.GetTemplate().GetContainers()[0].GetEnv()), []string{"MODE=test", "DB<-db-password:latest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env sent = %v; want %v", got, want)
	}

	values["labels"] = `{"cloudburrow.dev/x":"1"}`
	values["secretEnv"] = `{"MODE":"clash"}`
	if err := rp.Edit(context.Background(), "demo", []string{"api"}, values); err == nil {
		t.Error("a variable both plain and from Secret Manager was accepted")
	}
}

// Create job carries labels and secret-backed variables; Edit job is
// prefilled with them and replaces them. Execute with overrides, on a job's
// page and not its row, is one RunJob whose overrides are the form's and
// which leaves the job as it is (#852).
func TestRunJobLabelsSecretsAndExecuteWithOverrides(t *testing.T) {
	ctx := context.Background()
	f := newFakeRunJobs()
	p := jobsProviderFor(f.serve(t))
	useFakeKube(t, `{"items":[]}`)

	if _, err := p.Create(ctx, "demo", map[string]string{
		"name": "nightly", "image": "docker.io/library/busybox:1.36", "env": `{"MODE":"full"}`,
		"secretEnv": `{"TOKEN":"etl-token"}`, "labels": `{"team":"data"}`, "taskCount": "1",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	job := f.creates[0].GetJob()
	if job.GetLabels()["team"] != "data" {
		t.Errorf("labels = %v", job.GetLabels())
	}
	if got, want := runEnvSummary(job.GetTemplate().GetTemplate().GetContainers()[0].GetEnv()), []string{"MODE=full", "TOKEN<-etl-token:"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v; want %v", got, want)
	}

	d, err := p.Detail(ctx, "demo", []string{"nightly"})
	if err != nil || d.Edit == nil {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	values := map[string]string{}
	for _, fld := range d.Edit.Fields {
		if !fld.Immutable {
			values[fld.Name] = fld.Default
		}
	}
	if values["labels"] != `{"team":"data"}` || values["secretEnv"] != `{"TOKEN":"etl-token"}` {
		t.Errorf("Edit job prefilled labels %q, secretEnv %q", values["labels"], values["secretEnv"])
	}
	values["labels"], values["secretEnv"] = "", `{"TOKEN":"etl-token:4"}`
	if err := p.Edit(ctx, "demo", []string{"nightly"}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	upd := f.updates[0].GetJob()
	if len(upd.GetLabels()) != 0 {
		t.Errorf("labels emptied on the form are %v", upd.GetLabels())
	}
	if got, want := runEnvSummary(upd.GetTemplate().GetTemplate().GetContainers()[0].GetEnv()), []string{"MODE=full", "TOKEN<-etl-token:4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env after edit = %v; want %v", got, want)
	}

	if ids := actionIDs(p.Actions(console.Resource{Name: "nightly"})); strings.Contains(strings.Join(ids, " "), actExecuteOverrides) {
		t.Errorf("a job row offers %v; Execute with overrides is on the job's page", ids)
	}
	if ids := actionIDs(p.DetailActions(ctx, "demo", []string{"nightly"})); !reflect.DeepEqual(ids, []string{"execute", actExecuteOverrides}) {
		t.Errorf("the job page offers %v", ids)
	}
	if err := p.ActAt(ctx, "demo", []string{"nightly"}, actExecuteOverrides, map[string]string{
		"args": `'echo "hi there"' --fast`, "env": `{"MODE":"delta"}`, "taskCount": "3", "timeout": "45",
	}); err != nil {
		t.Fatalf("execute with overrides: %v", err)
	}
	if len(f.overrides) != 1 {
		t.Fatalf("%d RunJob calls", len(f.overrides))
	}
	o := f.overrides[0]
	co := o.GetContainerOverrides()
	if o.GetTaskCount() != 3 || o.GetTimeout().AsDuration() != 45*time.Second || len(co) != 1 || co[0].GetName() != "" ||
		!reflect.DeepEqual(co[0].GetArgs(), []string{`echo "hi there"`, "--fast"}) ||
		!reflect.DeepEqual(runEnvSummary(co[0].GetEnv()), []string{"MODE=delta"}) {
		t.Errorf("overrides = %v", o)
	}
	if len(f.updates) != 1 {
		t.Errorf("Execute with overrides changed the job: %d UpdateJob calls", len(f.updates))
	}
	if err := p.ActAt(ctx, "demo", []string{"nightly"}, "execute", nil); err != nil || f.overrides[1] != nil {
		t.Errorf("Execute sent overrides %v (%v); want none", f.overrides[1], err)
	}
	for _, bad := range []map[string]string{{}, {"taskCount": "x"}, {"args": `'unclosed`}, {"env": "nope"}} {
		if err := p.ActAt(ctx, "demo", []string{"nightly"}, actExecuteOverrides, bad); err == nil {
			t.Errorf("overrides %v were accepted", bad)
		}
	}
	if len(f.overrides) != 2 {
		t.Errorf("a refused override form reached RunJob: %d calls", len(f.overrides))
	}
}
