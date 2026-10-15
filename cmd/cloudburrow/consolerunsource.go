package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Deploy from source (#1268): a service page's action that uploads a source
// archive to the instance's Cloud Storage, builds it through the adapter's
// Builds.SubmitBuild, and redeploys the service with the image, as `gcloud
// run deploy --source` does: the archive goes to run-sources-{project}-{region},
// the image to {region}-docker.pkg.dev/{project}/cloud-run-source-deploy.

const runDeploySource = "deploy-source"

// runCreateFromSource is the list page's Create service from source (#1289):
// the same upload and build, then CreateService with the image, as `gcloud
// run deploy --source` does for a service that does not exist yet.
const runCreateFromSource = "create-from-source"

func runDeploySourceAction() console.Action {
	return console.Action{ID: runDeploySource, Label: "Deploy from source", Fields: runSourceFields()}
}

func runSourceFields() []console.Field {
	return []console.Field{
		{Name: "file", Label: "Source archive", Type: console.FileFieldType, Required: true,
			Help: "A .tar.gz of the source directory. It is uploaded to this instance's Cloud Storage and built here, not by Cloud Build."},
		{Name: "build", Label: "Build with", Type: "select", Options: []string{"Dockerfile", "Buildpacks"}, Default: "Dockerfile",
			Help: "Dockerfile builds the Dockerfile at the archive's root with this computer's Docker; Buildpacks uses Google's builder through pack, which makes linux/amd64 images only."},
		{Name: "functionTarget", Label: "Function target",
			Help: "Buildpacks only: the exported function to serve with the Functions Framework. Empty builds an ordinary application."},
	}
}

// runCreateFromSourceAction is the list page's action: the service's name
// and region, then the source fields Deploy from source takes.
func (p runProvider) runCreateFromSourceAction() console.Action {
	_, create := p.CreateForm()
	name := create[0] // the deploy form's own name field and its pattern
	name.Section = ""
	return console.Action{ID: runCreateFromSource, Label: "Create service from source", Fields: append([]console.Field{
		name,
		{Name: "region", Label: "Region", Type: "select", Options: []string{p.location()}, Default: p.location(),
			Help: "This instance serves Cloud Run in one region."},
	}, runSourceFields()...)}
}

// ListActions implements console.ListActor: Create service from source,
// offered only when both the adapter and Cloud Storage are running here.
func (p runProvider) ListActions(context.Context, string) []console.Action {
	if p.runEndpoint() == "" || p.storageAddr == "" {
		return nil
	}
	return []console.Action{p.runCreateFromSourceAction()}
}

// sourceActions is the service's Deploy from source, offered only when both
// the adapter and Cloud Storage are running here.
func (p runProvider) sourceActions() []console.Action {
	if p.runEndpoint() == "" || p.storageAddr == "" {
		return nil
	}
	return []console.Action{runDeploySourceAction()}
}

// ActAtFile implements console.FileActor: Deploy from source on a service's
// page, and Create service from source on the list page (no path).
func (p runProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	deploy := action == runDeploySource && len(path) == 1
	create := action == runCreateFromSource && len(path) == 0
	if !deploy && !create {
		return nil, fmt.Errorf("unknown action %q with a file", action)
	}
	if p.runEndpoint() == "" || p.storageAddr == "" {
		return nil, errors.New("Deploy from source needs Cloud Run and Cloud Storage running on this instance")
	}
	if project == "" {
		project = p.defaultProject
	}
	if project == "" {
		return nil, errors.New("choose a project before deploying from source")
	}
	if create {
		return nil, p.createFromSource(ctx, project, values, file)
	}
	return nil, p.deployFromSource(ctx, project, path[0], values, file)
}

// createFromSource builds the archive and creates the service with the image.
func (p runProvider) createFromSource(ctx context.Context, project string, values map[string]string, file io.Reader) error {
	service := strings.TrimSpace(values["name"])
	if service == "" {
		return errors.New("a service name is required")
	}
	_, create := p.CreateForm()
	if !regexp.MustCompile(create[0].Pattern).MatchString(service) {
		return fmt.Errorf("service name %q: %s", service, create[0].Help)
	}
	if r := strings.TrimSpace(values["region"]); r != "" && r != p.location() {
		return fmt.Errorf("region %s is not served here; this instance serves Cloud Run in %s", r, p.location())
	}
	sc, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sc.Close() }()
	parent := "projects/" + project + "/locations/" + p.location()
	// Refused before the upload and build, which take minutes, rather than
	// by CreateService after them.
	if _, err := sc.GetService(ctx, &runpb.GetServiceRequest{Name: parent + "/services/" + service}); err == nil {
		return fmt.Errorf("service %s already exists; use Deploy from source on its page", service)
	}
	image, err := p.buildSource(ctx, project, service, values, file)
	if err != nil {
		return err
	}
	op, err := sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: image}}}}})
	if err != nil {
		return err
	}
	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("the service never became ready: %w", err)
	}
	return nil
}

func (p runProvider) deployFromSource(ctx context.Context, project, service string, values map[string]string, file io.Reader) error {
	image, err := p.buildSource(ctx, project, service, values, file)
	if err != nil {
		return err
	}
	parent := "projects/" + project + "/locations/" + p.location()
	sc, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sc.Close() }()
	name := parent + "/services/" + service
	svc, err := sc.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		return err
	}
	if len(svc.GetTemplate().GetContainers()) == 0 {
		return fmt.Errorf("service %s has no container to deploy the image into", service)
	}
	svc.Template.Containers[0].Image = image
	// A new revision name would collide with the one the template names.
	svc.Template.Revision = ""
	op, err := sc.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// buildSource uploads the archive and builds it through SubmitBuild,
// returning the image it pushed.
func (p runProvider) buildSource(ctx context.Context, project, service string, values map[string]string, file io.Reader) (string, error) {
	region := p.location()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	bucket := "run-sources-" + project + "-" + region
	object := "services/" + service + "/" + stamp + ".tar.gz"
	if err := p.uploadSource(ctx, project, bucket, object, file); err != nil {
		return "", err
	}
	image := region + "-docker.pkg.dev/" + project + "/cloud-run-source-deploy/" + service + ":" + stamp
	parent := "projects/" + project + "/locations/" + region
	req := &runpb.SubmitBuildRequest{
		Parent:   parent,
		Source:   &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucket, Object: object}},
		ImageUri: image,
	}
	if values["build"] == "Buildpacks" {
		req.BuildType = &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{
			FunctionTarget: strings.TrimSpace(values["functionTarget"])}}
	} else {
		req.BuildType = &runpb.SubmitBuildRequest_DockerBuild_{DockerBuild: &runpb.SubmitBuildRequest_DockerBuild{}}
	}
	opts := []option.ClientOption{option.WithEndpoint(p.runEndpoint()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	bc, err := runclient.NewBuildsClient(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = bc.Close() }()
	resp, err := bc.SubmitBuild(ctx, req)
	if err != nil {
		return "", err
	}
	op := resp.GetBuildOperation()
	for !op.GetDone() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		if op, err = bc.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
			return "", err
		}
	}
	if op.GetError() != nil {
		return "", errors.New(op.GetError().GetMessage())
	}
	return image, nil
}

// uploadSource writes the archive to bucket/object through the instance's
// Cloud Storage JSON API, creating the bucket the first time, as gcloud
// creates run-sources-{project}-{region}.
func (p runProvider) uploadSource(ctx context.Context, project, bucket, object string, file io.Reader) error {
	body, _ := json.Marshal(map[string]string{"name": bucket})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.storageAddr+"/storage/v1/b?project="+url.QueryEscape(project), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("create bucket %s: HTTP %d", bucket, resp.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, p.storageAddr+"/upload/storage/v1/b/"+url.PathEscape(bucket)+
		"/o?uploadType=media&name="+url.QueryEscape(object), file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload the source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload the source: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

var _ console.FileActor = runProvider{}
var _ console.ListActor = runProvider{}
