package main

// Cloud Run: Copy on a service's page and Write a function on the Services
// list (#1852), as Google's console has them.
//
// Copy makes a new service with the copied service's configuration, through
// GetService and CreateService. Write a function is Cloud Functions v2's
// Create function, which deploys the function as a Cloud Run service, its
// source written in Google's inline editor (consolerunwritefunction.go) and
// built with Google's buildpacks as Create function builds it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const (
	runCopyService   = "copy-service"
	runWriteFunction = "write-function"
)

// copyAction is a service's Copy, on a service CloudBurrow created, whose
// configuration the API reads.
func (p runProvider) copyAction(svc *ksvcStatus) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	return []console.Action{{ID: runCopyService, Label: "Copy", Submit: "Create", Fields: []console.Field{{
		Name: "name", Label: "Service name", Type: "text", Required: true, Default: copyName(svc.Metadata.Name),
		Pattern: `^[a-z]([a-z0-9\-]{0,47}[a-z0-9])?$`,
		Help: "The new service's name: lowercase letters, numbers and hyphens, starting with a letter; at most 49 " +
			"characters. Its containers, variables, volumes, scaling and labels are this service's; all its " +
			"traffic goes to its first revision.",
	}}}}
}

// runCopySelected is the Services list's Copy, on the service checked.
const runCopySelected = "copy-selected"

// ListSelectActions implements console.SelectionLister: the Services list's
// Copy, which opens the service copy page for the service whose checkbox is
// selected, as Google's does ("Select the checkbox next to the service you
// want to copy ... Click Copy. In the service copy page, set or change any
// values you want to change ... Click Create",
// https://docs.cloud.google.com/run/docs/managing/services). Delete on the
// checked services is the list's own.
func (p runProvider) ListSelectActions(_ context.Context, _ string) []console.Action {
	if p.runEndpoint() == "" {
		return nil
	}
	return []console.Action{{ID: runCopySelected, Label: "Copy", Href: "/run/create", SelectionField: "from"}}
}

// CreateFormFrom implements console.CreateCopier: the service copy page is
// the Create service page holding the copied service's settings, as its
// edit form reads them from the serving revision, under a new name.
func (p runProvider) CreateFormFrom(ctx context.Context, project, from string) ([]console.Field, error) {
	if strings.Contains(from, ",") {
		return nil, errors.New("select exactly one service to copy")
	}
	svc, err := p.service(ctx, project, from)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %s", from)
	}
	edit := p.editForm(ctx, from, svc)
	if len(p.copyAction(svc)) == 0 || edit == nil {
		return nil, fmt.Errorf("service %s cannot be copied here: the console copies a service with one container "+
			"that was created through Cloud Run on this instance", from)
	}
	return runCopyFields(p, from, edit), nil
}

// runCopyFields are the Create service page's fields holding edit's values,
// the name the copy's.
func runCopyFields(p runProvider, from string, edit *console.EditForm) []console.Field {
	values := map[string]string{}
	for _, f := range edit.Fields {
		values[f.Name] = f.Default
	}
	_, fields := p.CreateForm()
	for i := range fields {
		if v, ok := values[fields[i].Name]; ok {
			fields[i].Default = v
		}
		if fields[i].Name == "name" {
			fields[i].Default = copyName(from)
		}
	}
	return fields
}

func copyName(name string) string {
	out := name + "-copy"
	if len(out) > 49 {
		out = strings.TrimRight(out[:49], "-")
	}
	return out
}

// copyService creates a service with service's configuration.
func (p runProvider) copyService(ctx context.Context, project, service string, values map[string]string) error {
	project = p.projectOr(project)
	if project == "" {
		return errors.New("choose a project before copying a service")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return errors.New("a service name is required")
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	parent := fmt.Sprintf("projects/%s/locations/%s", project, p.location())
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: parent + "/services/" + service})
	if err != nil {
		return err
	}
	tmpl := proto.Clone(got.GetTemplate()).(*runpb.RevisionTemplate)
	tmpl.Revision = "" // a revision name is the copied service's
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Description: got.GetDescription(), Labels: got.GetLabels(), Template: tmpl,
		Scaling: got.GetScaling(), Ingress: got.GetIngress(),
	}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// writeFunctionAction is Write a function: Create function's fields, where
// Cloud Functions and Cloud Storage, where the source is uploaded, run here.
func (p runProvider) writeFunctionAction(ctx context.Context, project string) []console.Action {
	if p.functions == nil || !p.functions.storage {
		return nil
	}
	if project == "" {
		project = p.defaultProject
	}
	a := p.functions.createAction()
	a.ID, a.Label, a.Submit = runWriteFunction, "Write a function", "Create"
	a.Fields = inlineSourceFields(a.Fields)
	a = writeFunctionLayout(a)
	// Create makes the service with a placeholder revision and lands on
	// its Source tab, as Google's does (#2049).
	a.Fields = placeholderWriteFields(a.Fields, p.functions.eventarcPaneFields(ctx, project, "us-central1"))
	a.Then = "/run/{name}?tab=source"
	return []console.Action{a}
}

// writeFunction creates the function through Cloud Functions v2.
func (p runProvider) writeFunction(ctx context.Context, project string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if p.functions == nil {
		return nil, errors.New("Write a function needs Cloud Functions, which this instance does not serve")
	}
	return p.functions.ActAtFile(ctx, project, nil, functionsCreateAction, values, file)
}
