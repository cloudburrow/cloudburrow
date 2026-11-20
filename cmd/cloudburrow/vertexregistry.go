package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// The Vertex AI Model Registry and Endpoints' backends (#1940): a deployed
// model's container runs as a Cloud Run service in the instance's cluster,
// deployed through the Cloud Run API, and a model's artifacts are copied
// within the instance's Cloud Storage.

// withModelRegistry serves the Model Registry and Endpoints on srv when it
// serves custom prediction endpoints (Cloud Run is enabled).
func withModelRegistry(cfg config.Config, srv *vertexai.Server, run interface{ Addr() string }, storageAddr func() string) {
	if srv == nil || !srv.ServesEndpoints() {
		return
	}
	runner := vertexRunner{run: runProvider{kubeconfig: cfg.KubeconfigPath(), namespace: runadapter.WorkloadNamespace,
		runAddr: runAddrOf(run), defaultProject: cfg.DefaultProject()}}
	var store vertexai.ArtifactStore
	if serviceEnabled(cfg, config.ServiceStorage) {
		store = vertexArtifacts{addr: storageAddr, cfg: cfg, client: &http.Client{Timeout: 5 * time.Minute}}
	}
	srv.SetRegistry(filepath.Join(cfg.StateDir, "vertexai", "registry.json"), runner, store)
	srv.SetModelImages(vertexImages{saver: newLocalImageBuilder(cfg)})
}

// vertexImages is the Model Registry's view of this instance's Artifact
// Registry (#1940): ExportModel's image_destination is pushed to it, from
// the registry itself or from the local Docker daemon (`docker save`,
// never a pull), and a prebuilt container is served only when it holds it.
type vertexImages struct{ saver runadapter.ImageSaver }

func (v vertexImages) Export(ctx context.Context, src, dst string) (string, error) {
	return runadapter.ExportImage(ctx, v.saver, src, dst)
}

func (v vertexImages) Held(ref string) bool { return runadapter.HeldImage(ref) }

// vertexRunner runs deployed models as Cloud Run services.
type vertexRunner struct{ run runProvider }

// deployTimeout bounds a deployed model's start: pulling its image and
// its health route answering.
const deployTimeout = 15 * time.Minute

func (v vertexRunner) Run(ctx context.Context, project string, spec vertexai.ContainerRun) (string, error) {
	if v.run.runEndpoint() == "" {
		return "", apierror.FailedPrecondition("the Cloud Run adapter is not running, so the model's container cannot be started")
	}
	c, err := v.run.servicesClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()
	container := &runpb.Container{Image: spec.Image, Command: spec.Command, Args: spec.Args,
		Ports: []*runpb.ContainerPort{{ContainerPort: int32(spec.Port)}},
		StartupProbe: &runpb.Probe{PeriodSeconds: 10, TimeoutSeconds: 10, FailureThreshold: 60,
			ProbeType: &runpb.Probe_HttpGet{HttpGet: &runpb.HTTPGetAction{Path: spec.HealthRoute}}}}
	for _, e := range spec.Env {
		container.Env = append(container.Env, &runpb.EnvVar{Name: e.Name, Values: &runpb.EnvVar_Value{Value: e.Value}})
	}
	parent := fmt.Sprintf("projects/%s/locations/%s", project, v.run.location())
	_ = v.Remove(ctx, project, spec.ID) // a leftover of an earlier, failed deploy
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: spec.ID, Service: &runpb.Service{
		Labels: spec.Labels, Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{container}, Labels: spec.Labels,
			Scaling: &runpb.RevisionScaling{MinInstanceCount: int32(spec.MinReplicas), MaxInstanceCount: int32(spec.MaxReplicas)}}}})
	if err != nil {
		return "", apierror.FailedPrecondition("starting the model's container (image %s): %v", spec.Image, status.Convert(err).Message())
	}
	svc, err := op.Wait(ctx)
	if err != nil {
		return "", apierror.FailedPrecondition("the model's container (image %s) did not become healthy on %s: %v",
			spec.Image, spec.HealthRoute, status.Convert(err).Message())
	}
	if svc.GetUri() == "" {
		return "", apierror.FailedPrecondition("the model's container is running but has no URL")
	}
	return svc.GetUri(), nil
}

func (v vertexRunner) Remove(ctx context.Context, project, id string) error {
	if v.run.runEndpoint() == "" {
		return nil
	}
	c, err := v.run.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/services/%s", project, v.run.location(), id)})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// vertexArtifacts copies artifacts through this instance's Cloud Storage
// JSON API.
type vertexArtifacts struct {
	addr   func() string
	cfg    config.Config
	client *http.Client
}

// ManagedBucket is Vertex's service-managed artifact bucket, one per
// project.
func (a vertexArtifacts) ManagedBucket(project string) string {
	b := "cloudburrow-vertex-models-" + strings.ToLower(project)
	if len(b) > 63 {
		b = b[:63]
	}
	return strings.TrimRight(b, "-.")
}

func (a vertexArtifacts) base() string {
	return storageSourceFetcher{cfg: a.cfg, addr: a.addr}.base()
}

func splitGS(uri string) (string, string, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	b, p, _ := strings.Cut(rest, "/")
	if !ok || b == "" {
		return "", "", apierror.InvalidArgument("%q is not a gs:// URI", uri)
	}
	return b, strings.TrimSuffix(p, "/"), nil
}

func (a vertexArtifacts) do(ctx context.Context, method, u string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: "Cloud Storage did not answer: " + err.Error()}
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return out, resp.StatusCode, err
}

func (a vertexArtifacts) Copy(ctx context.Context, project, src, dst string) error {
	sb, sp, err := splitGS(src)
	if err != nil {
		return err
	}
	db, dp, err := splitGS(dst)
	if err != nil {
		return err
	}
	base := a.base()
	if db == a.ManagedBucket(project) {
		_, code, err := a.do(ctx, http.MethodPost, base+"/storage/v1/b?project="+url.QueryEscape(project), map[string]string{"name": db})
		if err != nil {
			return err
		}
		if code != http.StatusOK && code != http.StatusConflict {
			return apierror.Internal(nil, "creating the managed artifact bucket %s answered %d", db, code)
		}
	}
	prefix := sp
	if prefix != "" {
		prefix += "/"
	}
	copied := 0
	token := ""
	for {
		u := base + "/storage/v1/b/" + url.PathEscape(sb) + "/o?prefix=" + url.QueryEscape(prefix)
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		b, code, err := a.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			return apierror.InvalidArgument("artifact_uri %s: bucket %s does not exist", src, sb)
		}
		if code != http.StatusOK {
			return apierror.Internal(nil, "listing %s answered %d: %s", src, code, b)
		}
		var list struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &list); err != nil {
			return apierror.Internal(err, "listing %s: %v", src, err)
		}
		for _, it := range list.Items {
			if strings.HasSuffix(it.Name, "/") {
				continue
			}
			to := strings.TrimPrefix(it.Name, prefix)
			if dp != "" {
				to = dp + "/" + to
			}
			cu := base + "/storage/v1/b/" + url.PathEscape(sb) + "/o/" + url.PathEscape(it.Name) + "/copyTo/b/" +
				url.PathEscape(db) + "/o/" + url.PathEscape(to)
			out, code, err := a.do(ctx, http.MethodPost, cu, map[string]string{})
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return apierror.Internal(nil, "copying gs://%s/%s answered %d: %s", sb, it.Name, code, out)
			}
			copied++
		}
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	if copied == 0 {
		return apierror.InvalidArgument("artifact_uri %s holds no objects: it must be a Cloud Storage directory holding the model's artifacts", src)
	}
	return nil
}
