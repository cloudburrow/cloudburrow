package main

// Cloud Run: Copy on a service's page and Write a function on the Services
// list (#1852), as Google's console has them.
//
// Copy makes a new service with the copied service's configuration, through
// GetService and CreateService. Write a function is Cloud Functions v2's
// Create function, which deploys the function as a Cloud Run service; Google's
// opens an inline editor, and here the source is a .zip, built with Google's
// buildpacks as Create function builds it.

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
func (p runProvider) writeFunctionAction() []console.Action {
	if p.functions == nil || !p.functions.storage {
		return nil
	}
	a := p.functions.createAction()
	a.ID, a.Label, a.Submit = runWriteFunction, "Write a function", "Create"
	return []console.Action{a}
}

// writeFunction creates the function through Cloud Functions v2.
func (p runProvider) writeFunction(ctx context.Context, project string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if p.functions == nil {
		return nil, errors.New("Write a function needs Cloud Functions, which this instance does not serve")
	}
	return p.functions.ActAtFile(ctx, project, nil, functionsCreateAction, values, file)
}
