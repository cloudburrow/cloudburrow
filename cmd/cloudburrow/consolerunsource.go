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

func runDeploySourceAction() console.Action {
	return console.Action{ID: runDeploySource, Label: "Deploy from source", Fields: []console.Field{
		{Name: "file", Label: "Source archive", Type: console.FileFieldType, Required: true,
			Help: "A .tar.gz of the source directory. It is uploaded to this instance's Cloud Storage and built here, not by Cloud Build."},
		{Name: "build", Label: "Build with", Type: "select", Options: []string{"Dockerfile", "Buildpacks"}, Default: "Dockerfile",
			Help: "Dockerfile builds the Dockerfile at the archive's root with this computer's Docker; Buildpacks uses Google's builder through pack, which makes linux/amd64 images only."},
		{Name: "functionTarget", Label: "Function target",
			Help: "Buildpacks only: the exported function to serve with the Functions Framework. Empty builds an ordinary application."},
	}}
}

// sourceActions is the service's Deploy from source, offered only when both
// the adapter and Cloud Storage are running here.
func (p runProvider) sourceActions() []console.Action {
	if p.runEndpoint() == "" || p.storageAddr == "" {
		return nil
	}
	return []console.Action{runDeploySourceAction()}
}

// ActAtFile implements console.FileActor: Deploy from source.
func (p runProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if action != runDeploySource || len(path) != 1 {
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
	return nil, p.deployFromSource(ctx, project, path[0], values, file)
}

func (p runProvider) deployFromSource(ctx context.Context, project, service string, values map[string]string, file io.Reader) error {
	region := p.location()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	bucket := "run-sources-" + project + "-" + region
	object := "services/" + service + "/" + stamp + ".tar.gz"
	if err := p.uploadSource(ctx, project, bucket, object, file); err != nil {
		return err
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
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = bc.Close() }()
	resp, err := bc.SubmitBuild(ctx, req)
	if err != nil {
		return err
	}
	for op := resp.GetBuildOperation(); !op.GetDone(); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if op, err = bc.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
			return err
		}
		if op.GetDone() && op.GetError() != nil {
			return errors.New(op.GetError().GetMessage())
		}
	}

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
