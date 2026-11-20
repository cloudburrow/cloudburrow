package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Agent Platform (Vertex AI) Model Registry and Endpoints pages
// (#1940), in Google's layout: Model Registry lists the models with their
// default version, Import uploads one (or a new version), a model's page
// lists its versions and deploys one to an endpoint; Endpoints lists the
// endpoints (Model Garden's among them), Create endpoint makes one, and an
// endpoint's page lists its deployed models with their traffic split,
// tests a prediction, moves the traffic and undeploys. Every read and
// write goes through the official aiplatform client to the instance's
// local AI endpoint, the API an SDK calls.

// vertexAPI dials the local AI endpoint.
type vertexAPI struct{ addr func() string }

// at is the endpoint's address once it listens, "" before.
func (v vertexAPI) at() string {
	if v.addr == nil {
		return ""
	}
	return v.addr()
}

func (v vertexAPI) opts() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(v.at()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func (v vertexAPI) models(ctx context.Context) (*aiplatform.ModelClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewModelClient(ctx, v.opts()...)
}

func (v vertexAPI) endpoints(ctx context.Context) (*aiplatform.EndpointClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewEndpointClient(ctx, v.opts()...)
}

func (v vertexAPI) prediction(ctx context.Context) (*aiplatform.PredictionClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewPredictionClient(ctx, v.opts()...)
}

// vertexRegions are the regions the pages offer; any location name is
// served.
var vertexRegions = []string{"us-central1", "us-east1", "us-east4", "us-west1", "europe-west1", "europe-west4", "asia-east1", "asia-northeast1"}

const vertexDeployBudget = 16 * time.Minute

func vertexTime(t interface{ AsTime() time.Time }) string {
	return t.AsTime().UTC().Format("2006-01-02 15:04:05 UTC")
}

func vertexLocation(name string) string {
	p := strings.Split(name, "/")
	if len(p) >= 4 {
		return p[3]
	}
	return ""
}

func vertexLast(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func labelsLine(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+": "+m[k])
	}
	return dashIfEmpty(strings.Join(out, ", "))
}

// ---- Model Registry

type vertexModelsProvider struct{ api vertexAPI }

func (vertexModelsProvider) ID() string    { return "vertex-models" }
func (vertexModelsProvider) Title() string { return "Model Registry" }

// MutationBudget covers Deploy, which waits for the container to answer
// its health route.
func (vertexModelsProvider) MutationBudget() time.Duration { return vertexDeployBudget }

func (p vertexModelsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Display name", "Default version", "Version aliases", "Deployed", "Type", "Region",
		"Last updated", "Labels"}, NameColumn: "Name", Noun: "models",
		Note: "Models uploaded through ModelService.UploadModel (gcloud ai models upload, the SDK's Model.upload, or Import). " +
			"A deployed version's serving container runs in this instance's cluster."}
	if project == "" {
		l.Prompt = "The Model Registry lists models per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.models(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListModels(ctx, &aiplatformpb.ListModelsRequest{Parent: "projects/" + project + "/locations/-"}); ; {
		m, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		kind := "Custom container"
		if m.GetModelSourceInfo().GetSourceType() == aiplatformpb.ModelSourceInfo_MODEL_GARDEN {
			kind = "Model Garden"
		}
		deployed := "No"
		if len(m.GetDeployedModels()) > 0 {
			deployed = fmt.Sprintf("Yes (%d)", len(m.GetDeployedModels()))
		}
		l.Items = append(l.Items, console.Resource{Name: m.GetName(), Fields: map[string]string{
			"Display name": m.GetDisplayName(), "Default version": m.GetVersionId(),
			"Version aliases": dashIfEmpty(strings.Join(m.GetVersionAliases(), ", ")), "Deployed": deployed, "Type": kind,
			"Region": vertexLocation(m.GetName()), "Last updated": vertexTime(m.GetUpdateTime()), "Labels": labelsLine(m.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (vertexModelsProvider) CreateTitle() string { return "Import model" }

func (vertexModelsProvider) CreateForm() (string, []console.Field) {
	return "Import", []console.Field{
		{Name: "region", Label: "Region", Type: "select", Options: vertexRegions, Default: "us-central1", Required: true},
		{Name: "displayName", Label: "Name", Type: "text", Required: true, Help: "Up to 128 characters."},
		{Name: "modelId", Label: "Model ID", Type: "text", Pattern: `^([a-z_][a-z0-9_\-]{0,62})?$`,
			Help: "Optional: up to 63 characters of a-z, 0-9, _ and -; empty is a generated numeric ID."},
		{Name: "parentModel", Label: "Import as new version of", Type: "text",
			Help: "Optional: an existing model's resource name, projects/…/locations/…/models/…; the import is then its next version."},
		{Name: "versionAliases", Label: "Version aliases", Type: "text", Help: "Optional, comma-separated; default makes this the default version."},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "imageUri", Label: "Container image", Type: "text", Required: true,
			Help: "A custom container image, or one of Google's prebuilt prediction containers; a locally built image is dev.local/NAME:TAG, loaded into the cluster."},
		{Name: "artifactUri", Label: "Model artifact location", Type: "text", Pattern: `^(gs://.+)?$`,
			Help: "Optional: a gs:// directory in this instance's Cloud Storage; it is copied, and AIP_STORAGE_URI names the copy."},
		{Name: "predictRoute", Label: "Prediction route", Type: "text", Pattern: `^(/.*)?$`, Help: "Optional; default /v1/endpoints/ENDPOINT/deployedModels/DEPLOYED_MODEL:predict."},
		{Name: "healthRoute", Label: "Health route", Type: "text", Pattern: `^(/.*)?$`, Help: "Optional; default /v1/endpoints/ENDPOINT/deployedModels/DEPLOYED_MODEL."},
		{Name: "port", Label: "Port", Type: "text", Default: "8080", Pattern: `^[0-9]{1,5}$`},
		{Name: "env", Label: "Environment variables", Type: "map", Help: "Variables starting AIP_ are reserved."},
		{Name: "labels", Label: "Labels", Type: "map"},
	}
}

func (p vertexModelsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before importing a model")
	}
	region := strings.TrimSpace(values["region"])
	if region == "" {
		region = "us-central1"
	}
	env, err := console.ParseMap(values["env"])
	if err != nil {
		return "", err
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	spec := &aiplatformpb.ModelContainerSpec{ImageUri: strings.TrimSpace(values["imageUri"]),
		PredictRoute: strings.TrimSpace(values["predictRoute"]), HealthRoute: strings.TrimSpace(values["healthRoute"])}
	if port := strings.TrimSpace(values["port"]); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return "", fmt.Errorf("port: %q is not a number", port)
		}
		spec.Ports = []*aiplatformpb.Port{{ContainerPort: int32(n)}}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.Env = append(spec.Env, &aiplatformpb.EnvVar{Name: k, Value: env[k]})
	}
	m := &aiplatformpb.Model{DisplayName: strings.TrimSpace(values["displayName"]), Description: values["description"],
		ArtifactUri: strings.TrimSpace(values["artifactUri"]), ContainerSpec: spec, Labels: labels}
	for _, a := range strings.Split(values["versionAliases"], ",") {
		if a = strings.TrimSpace(a); a != "" {
			m.VersionAliases = append(m.VersionAliases, a)
		}
	}
	c, err := p.api.models(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: "projects/" + project + "/locations/" + region,
		ModelId: strings.TrimSpace(values["modelId"]), ParentModel: strings.TrimSpace(values["parentModel"]), Model: m})
	if err != nil {
		return "", err
	}
	r, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	return r.GetModel(), nil
}

func (p vertexModelsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.models(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (p vertexModelsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.models(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	m, err := c.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	spec := m.GetContainerSpec()
	port := "8080"
	if len(spec.GetPorts()) > 0 {
		port = strconv.Itoa(int(spec.GetPorts()[0].GetContainerPort()))
	}
	d := console.Detail{Title: m.GetDisplayName(), Summary: []console.Property{
		{Label: "Name", Value: m.GetName(), Copy: m.GetName()},
		{Label: "Region", Value: vertexLocation(m.GetName())},
		{Label: "Default version", Value: m.GetVersionId()},
		{Label: "Description", Value: m.GetDescription()},
		{Label: "Labels", Value: labelsLine(m.GetLabels())},
	}}
	versions := console.Listing{Columns: []string{"Version aliases", "Description", "Artifact location", "Deployed on", "Created"},
		NameColumn: "Version ID", Noun: "versions"}
	if it := c.ListModelVersions(ctx, &aiplatformpb.ListModelVersionsRequest{Name: m.GetName()}); it != nil {
		for {
			v, err := it.Next()
			if err != nil {
				break
			}
			var on []string
			for _, dm := range v.GetDeployedModels() {
				on = append(on, vertexLast(dm.GetEndpoint())+" ("+dm.GetDeployedModelId()+")")
			}
			versions.Items = append(versions.Items, console.Resource{Name: v.GetVersionId(), Fields: map[string]string{
				"Version aliases": dashIfEmpty(strings.Join(v.GetVersionAliases(), ", ")), "Description": dashIfEmpty(v.GetVersionDescription()),
				"Artifact location": dashIfEmpty(v.GetArtifactUri()), "Deployed on": dashIfEmpty(strings.Join(on, ", ")),
				"Created": vertexTime(v.GetVersionCreateTime())}})
		}
	}
	versions.Total = len(versions.Items)
	var env []string
	for _, e := range spec.GetEnv() {
		env = append(env, e.GetName()+"="+e.GetValue())
	}
	evals := console.Listing{Columns: []string{"Name", "Metrics schema", "Created"}, NameColumn: "Evaluation ID", Noun: "evaluations"}
	if it := c.ListModelEvaluations(ctx, &aiplatformpb.ListModelEvaluationsRequest{Parent: m.GetName()}); it != nil {
		for {
			e, err := it.Next()
			if err != nil {
				break
			}
			evals.Items = append(evals.Items, console.Resource{Name: vertexLast(e.GetName()), Fields: map[string]string{
				"Name": e.GetDisplayName(), "Metrics schema": dashIfEmpty(e.GetMetricsSchemaUri()), "Created": vertexTime(e.GetCreateTime())}})
		}
	}
	evals.Total = len(evals.Items)
	d.Sections = []console.Section{
		{ID: "versions", Label: "Versions", Listing: versions},
		{ID: "evaluate", Label: "Evaluate", Listing: evals},
		{ID: "container", Label: "Container", Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Container image", Value: spec.GetImageUri()},
			{Label: "Prediction route", Value: dashIfEmpty(spec.GetPredictRoute())},
			{Label: "Health route", Value: dashIfEmpty(spec.GetHealthRoute())},
			{Label: "Port", Value: port},
			{Label: "Environment variables", Value: dashIfEmpty(strings.Join(env, ", "))},
			{Label: "Artifact location", Value: dashIfEmpty(m.GetArtifactUri())},
		}}}},
	}
	return d, nil
}

func (p vertexModelsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{
		{ID: "deploy", Label: "Deploy to endpoint", Fields: []console.Field{
			{Name: "version", Label: "Version", Type: "text", Help: "A version ID or alias; empty is the default version."},
			{Name: "endpoint", Label: "Endpoint", Type: "text", Required: true,
				Help: "An endpoint's resource name or ID in the model's region (Create one on the Endpoints page)."},
			{Name: "displayName", Label: "Deployed model name", Type: "text"},
			{Name: "machineType", Label: "Machine type", Type: "text", Default: "n1-standard-2", Required: true},
			{Name: "minReplicas", Label: "Minimum number of compute nodes", Type: "text", Default: "1", Pattern: `^[0-9]{1,3}$`},
			{Name: "maxReplicas", Label: "Maximum number of compute nodes", Type: "text", Default: "1", Pattern: `^[0-9]{1,3}$`},
			{Name: "traffic", Label: "Traffic split (%)", Type: "text", Default: "100", Pattern: `^[0-9]{1,3}$`,
				Help: "This deployed model's share; the rest stays on the endpoint's other models in their proportions."},
		}},
		{ID: "aliases", Label: "Edit version aliases", Fields: []console.Field{
			{Name: "version", Label: "Version ID", Type: "text", Required: true, Pattern: `^[0-9]+$`},
			{Name: "aliases", Label: "Aliases", Type: "text", Required: true,
				Help: "Comma-separated; prefix one with - to remove it (default can only move to another version)."},
		}},
		{ID: "deleteversion", Label: "Delete version", Destructive: true, Fields: []console.Field{
			{Name: "version", Label: "Version ID", Type: "text", Required: true, Pattern: `^[0-9]+$`},
		}},
		{ID: "edit", Label: "Edit", Fields: []console.Field{
			{Name: "version", Label: "Version", Type: "text", Help: "A version ID or alias; empty is the default version."},
			{Name: "displayName", Label: "Name", Type: "text", Help: "Empty keeps the name."},
			{Name: "description", Label: "Description", Type: "textarea", Help: "Empty keeps the description."},
			{Name: "versionDescription", Label: "Version description", Type: "textarea", Help: "Empty keeps it."},
			{Name: "labels", Label: "Labels", Type: "map", Help: "Replaces the labels when any is given."},
		}},
		{ID: "copy", Label: "Copy", Fields: []console.Field{
			{Name: "version", Label: "Version", Type: "text", Help: "A version ID or alias; empty is the default version."},
			{Name: "region", Label: "Destination region", Type: "select", Options: vertexRegions, Default: "us-central1", Required: true},
			{Name: "modelId", Label: "New model ID", Type: "text", Pattern: `^([a-z_][a-z0-9_\-]{0,62})?$`,
				Help: "A new model; or leave empty and name a parent model below."},
			{Name: "parentModel", Label: "Copy as new version of", Type: "text", Help: "An existing model's resource name in the destination region."},
		}},
		{ID: "export", Label: "Export", Fields: []console.Field{
			{Name: "version", Label: "Version", Type: "text", Help: "A version ID or alias; empty is the default version."},
			{Name: "destination", Label: "Cloud Storage destination", Type: "text", Pattern: `^(gs://.+)?$`,
				Help: "A gs:// prefix in this instance's Cloud Storage; the artifacts land under model-ID/custom-trained/TIMESTAMP. Give this, an image destination, or both."},
			{Name: "imageDestination", Label: "Artifact Registry image destination", Type: "text", Pattern: `^([a-z0-9\-]+-docker\.pkg\.dev/.+)?$`,
				Help: "LOCATION-docker.pkg.dev/PROJECT/REPOSITORY/IMAGE[:TAG] in this instance's Artifact Registry; the model's serving image is copied there (tag latest by default)."},
		}},
		{ID: "evaluation", Label: "Import evaluation", Fields: []console.Field{
			{Name: "version", Label: "Version", Type: "text", Help: "A version ID or alias; empty is the default version."},
			{Name: "displayName", Label: "Evaluation name", Type: "text", Required: true},
			{Name: "metricsSchemaUri", Label: "Metrics schema URI", Type: "text",
				Default: "gs://google-cloud-aiplatform/schema/modelevaluation/regression_metrics_1.0.0.yaml"},
			{Name: "metrics", Label: "Metrics (JSON)", Type: "textarea", Required: true, Default: `{"meanAbsoluteError": 0.1}`},
		}},
		{ID: "vieweval", Label: "View evaluation", Fields: []console.Field{
			{Name: "evaluation", Label: "Evaluation ID", Type: "text", Required: true, Help: "From the Evaluate tab."},
		}},
	}
}

// versioned is a model name with an optional @version.
func versioned(model, v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return model + "@" + v
	}
	return model
}

// endpointName reads an endpoint ID or name in a model's location.
func endpointName(model, ep string) string {
	ep = strings.TrimSpace(ep)
	if strings.HasPrefix(ep, "projects/") {
		return ep
	}
	p := strings.Split(model, "/")
	return strings.Join(p[:4], "/") + "/endpoints/" + ep
}

func (p vertexModelsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

func (p vertexModelsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 1 {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	switch action {
	case "edit", "copy", "export", "evaluation", "vieweval":
		return p.manage(ctx, path[0], action, values)
	}
	return nil, p.act(ctx, path, action, values)
}

func (p vertexModelsProvider) act(ctx context.Context, path []string, action string, values map[string]string) error {
	model := path[0]
	mc, err := p.api.models(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = mc.Close() }()
	switch action {
	case "deploy":
		ec, err := p.api.endpoints(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = ec.Close() }()
		ref := model
		if v := strings.TrimSpace(values["version"]); v != "" {
			ref += "@" + v
		}
		minR, _ := strconv.Atoi(values["minReplicas"])
		maxR, _ := strconv.Atoi(values["maxReplicas"])
		pct, err := strconv.Atoi(strings.TrimSpace(values["traffic"]))
		if err != nil || pct < 0 || pct > 100 {
			return fmt.Errorf("traffic split: %q is not a percentage", values["traffic"])
		}
		ep := endpointName(model, values["endpoint"])
		cur, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: ep})
		if err != nil {
			return err
		}
		split := map[string]int32{"0": int32(pct)}
		if rest := 100 - pct; len(cur.GetTrafficSplit()) > 0 {
			total := int32(0)
			for _, v := range cur.GetTrafficSplit() {
				total += v
			}
			left := int32(rest)
			ids := make([]string, 0, len(cur.GetTrafficSplit()))
			for id := range cur.GetTrafficSplit() {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for i, id := range ids {
				share := cur.GetTrafficSplit()[id] * int32(rest) / max(total, 1)
				if i == len(ids)-1 {
					share = left
				}
				split[id] = share
				left -= share
			}
		} else if pct != 100 {
			return errors.New("traffic split: the endpoint has no other deployed model, so this one takes 100%")
		}
		op, err := ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: ep, TrafficSplit: split,
			DeployedModel: &aiplatformpb.DeployedModel{Model: ref, DisplayName: strings.TrimSpace(values["displayName"]),
				PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
					MachineSpec:     &aiplatformpb.MachineSpec{MachineType: strings.TrimSpace(values["machineType"])},
					MinReplicaCount: int32(minR), MaxReplicaCount: int32(maxR)}}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "aliases":
		var aliases []string
		for _, a := range strings.Split(values["aliases"], ",") {
			if a = strings.TrimSpace(a); a != "" {
				aliases = append(aliases, a)
			}
		}
		_, err := mc.MergeVersionAliases(ctx, &aiplatformpb.MergeVersionAliasesRequest{Name: model + "@" + strings.TrimSpace(values["version"]),
			VersionAliases: aliases})
		return err
	case "deleteversion":
		op, err := mc.DeleteModelVersion(ctx, &aiplatformpb.DeleteModelVersionRequest{Name: model + "@" + strings.TrimSpace(values["version"])})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	return fmt.Errorf("unknown action %q", action)
}

// manage is the model page's Edit, Copy, Export and evaluation actions.
func (p vertexModelsProvider) manage(ctx context.Context, model, action string, values map[string]string) (*console.Listing, error) {
	mc, err := p.api.models(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mc.Close() }()
	ref := versioned(model, values["version"])
	switch action {
	case "edit":
		in := &aiplatformpb.Model{Name: ref}
		var mask []string
		if v := strings.TrimSpace(values["displayName"]); v != "" {
			in.DisplayName, mask = v, append(mask, "display_name")
		}
		if v := values["description"]; strings.TrimSpace(v) != "" {
			in.Description, mask = v, append(mask, "description")
		}
		if v := values["versionDescription"]; strings.TrimSpace(v) != "" {
			in.VersionDescription, mask = v, append(mask, "version_description")
		}
		if strings.TrimSpace(values["labels"]) != "" {
			if in.Labels, err = console.ParseMap(values["labels"]); err != nil {
				return nil, err
			}
			mask = append(mask, "labels")
		}
		if len(mask) == 0 {
			return nil, errors.New("nothing to change: fill in at least one field")
		}
		_, err := mc.UpdateModel(ctx, &aiplatformpb.UpdateModelRequest{Model: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		return nil, err
	case "copy":
		region := strings.TrimSpace(values["region"])
		req := &aiplatformpb.CopyModelRequest{Parent: strings.Join(strings.Split(model, "/")[:2], "/") + "/locations/" + region, SourceModel: ref}
		if pm := strings.TrimSpace(values["parentModel"]); pm != "" {
			req.DestinationModel = &aiplatformpb.CopyModelRequest_ParentModel{ParentModel: pm}
		} else {
			req.DestinationModel = &aiplatformpb.CopyModelRequest_ModelId{ModelId: strings.TrimSpace(values["modelId"])}
		}
		op, err := mc.CopyModel(ctx, req)
		if err != nil {
			return nil, err
		}
		r, err := op.Wait(ctx)
		if err != nil {
			return nil, err
		}
		return &console.Listing{Columns: []string{"Version"}, NameColumn: "Model", Noun: "models", Total: 1,
			Items: []console.Resource{{Name: r.GetModel(), Fields: map[string]string{"Version": r.GetModelVersionId()}}}}, nil
	case "export":
		oc := &aiplatformpb.ExportModelRequest_OutputConfig{ExportFormatId: "custom-trained"}
		if d := strings.TrimSpace(values["destination"]); d != "" {
			oc.ArtifactDestination = &aiplatformpb.GcsDestination{OutputUriPrefix: d}
		}
		if d := strings.TrimSpace(values["imageDestination"]); d != "" {
			oc.ImageDestination = &aiplatformpb.ContainerRegistryDestination{OutputUri: d}
		}
		op, err := mc.ExportModel(ctx, &aiplatformpb.ExportModelRequest{Name: ref, OutputConfig: oc})
		if err != nil {
			return nil, err
		}
		if _, err := op.Wait(ctx); err != nil {
			return nil, err
		}
		meta, _ := op.Metadata()
		return &console.Listing{Columns: []string{"Artifact output", "Image output"}, NameColumn: "Model", Noun: "exports", Total: 1,
			Items: []console.Resource{{Name: ref, Fields: map[string]string{"Artifact output": meta.GetOutputInfo().GetArtifactOutputUri(),
				"Image output": meta.GetOutputInfo().GetImageOutputUri()}}}}, nil
	case "evaluation":
		var metrics any
		if err := json.Unmarshal([]byte(values["metrics"]), &metrics); err != nil {
			return nil, fmt.Errorf("the metrics are not JSON: %v", err)
		}
		mv, err := structpb.NewValue(metrics)
		if err != nil {
			return nil, err
		}
		_, err = mc.ImportModelEvaluation(ctx, &aiplatformpb.ImportModelEvaluationRequest{Parent: ref,
			ModelEvaluation: &aiplatformpb.ModelEvaluation{DisplayName: strings.TrimSpace(values["displayName"]),
				MetricsSchemaUri: strings.TrimSpace(values["metricsSchemaUri"]), Metrics: mv}})
		return nil, err
	case "vieweval":
		e, err := mc.GetModelEvaluation(ctx, &aiplatformpb.GetModelEvaluationRequest{Name: model + "/evaluations/" + strings.TrimSpace(values["evaluation"])})
		if err != nil {
			return nil, err
		}
		b, _ := protojson.Marshal(e.GetMetrics())
		return &console.Listing{Columns: []string{"Name", "Metrics schema", "Metrics"}, NameColumn: "Evaluation", Noun: "evaluations", Total: 1,
			Items: []console.Resource{{Name: vertexLast(e.GetName()), Fields: map[string]string{"Name": e.GetDisplayName(),
				"Metrics schema": e.GetMetricsSchemaUri(), "Metrics": string(b)}}}}, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// ---- Endpoints

type vertexEndpointsProvider struct{ api vertexAPI }

func (vertexEndpointsProvider) ID() string    { return "vertex-endpoints" }
func (vertexEndpointsProvider) Title() string { return "Endpoints" }

func (vertexEndpointsProvider) MutationBudget() time.Duration { return vertexDeployBudget }

func (p vertexEndpointsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Display name", "ID", "Status", "Models", "Region", "Last updated", "Labels"},
		NameColumn: "Name", Noun: "endpoints",
		Note: "Endpoints made with EndpointService.CreateEndpoint and Model Garden's deployments. A deployed model's container runs in this instance's cluster."}
	if project == "" {
		l.Prompt = "Endpoints are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.endpoints(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListEndpoints(ctx, &aiplatformpb.ListEndpointsRequest{Parent: "projects/" + project + "/locations/-"}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		status := "No models deployed"
		if n := len(e.GetDeployedModels()); n > 0 {
			status = "Active"
		}
		r := console.Resource{Name: e.GetName(), Fields: map[string]string{"Display name": e.GetDisplayName(), "ID": vertexLast(e.GetName()),
			"Status": status, "Models": strconv.Itoa(len(e.GetDeployedModels())), "Region": vertexLocation(e.GetName()),
			"Last updated": vertexTime(e.GetUpdateTime()), "Labels": labelsLine(e.GetLabels())}}
		if p.isGardenEndpoint(ctx, e) {
			// Model Garden's own page, with its deploy progress and test.
			r.Link = "/agent-platform/endpoints?endpoint=" + e.GetName()
		}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

// isGardenEndpoint reports a Model Garden deployment, which keeps its own
// page: a dedicated endpoint whose model came from Model Garden, or with
// no model yet (one still deploying lists none).
func (p vertexEndpointsProvider) isGardenEndpoint(ctx context.Context, e *aiplatformpb.Endpoint) bool {
	if !e.GetDedicatedEndpointEnabled() {
		return false
	}
	if len(e.GetDeployedModels()) == 0 {
		return true
	}
	mc, err := p.api.models(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = mc.Close() }()
	m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: e.GetDeployedModels()[0].GetModel()})
	return err == nil && m.GetModelSourceInfo().GetSourceType() == aiplatformpb.ModelSourceInfo_MODEL_GARDEN
}

func (vertexEndpointsProvider) CreateTitle() string { return "Create endpoint" }

func (vertexEndpointsProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "region", Label: "Region", Type: "select", Options: vertexRegions, Default: "us-central1", Required: true},
		{Name: "displayName", Label: "Endpoint name", Type: "text", Required: true},
		{Name: "endpointId", Label: "Endpoint ID", Type: "text", Pattern: `^([0-9]{1,10}|[a-z]([a-z0-9\-]{0,61}[a-z0-9])?)?$`,
			Help: "Optional: up to 10 digits, or lowercase letters, digits and hyphens; empty is generated."},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "labels", Label: "Labels", Type: "map"},
	}
}

func (p vertexEndpointsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating an endpoint")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	region := strings.TrimSpace(values["region"])
	if region == "" {
		region = "us-central1"
	}
	c, err := p.api.endpoints(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: "projects/" + project + "/locations/" + region,
		EndpointId: strings.TrimSpace(values["endpointId"]), Endpoint: &aiplatformpb.Endpoint{DisplayName: strings.TrimSpace(values["displayName"]),
			Description: values["description"], Labels: labels}})
	if err != nil {
		return "", err
	}
	e, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	return e.GetName(), nil
}

func (p vertexEndpointsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.endpoints(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (p vertexEndpointsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.endpoints(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	e, err := c.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	d := console.Detail{Title: e.GetDisplayName(), Summary: []console.Property{
		{Label: "Name", Value: e.GetName(), Copy: e.GetName()},
		{Label: "ID", Value: vertexLast(e.GetName())},
		{Label: "Region", Value: vertexLocation(e.GetName())},
		{Label: "Description", Value: e.GetDescription()},
		{Label: "Labels", Value: labelsLine(e.GetLabels())},
		{Label: "Prediction", Value: "POST /v1/" + e.GetName() + ":predict, :rawPredict and :explain"},
	}}
	models := console.Listing{Columns: []string{"Model", "Version", "Display name", "Traffic split", "Machine type", "Compute nodes", "Created"},
		NameColumn: "Deployed model ID", Noun: "deployed models"}
	for _, dm := range e.GetDeployedModels() {
		res := dm.GetDedicatedResources()
		models.Items = append(models.Items, console.Resource{Name: dm.GetId(), Fields: map[string]string{
			"Model": dm.GetModel(), "Version": dashIfEmpty(dm.GetModelVersionId()), "Display name": dm.GetDisplayName(),
			"Traffic split": fmt.Sprintf("%d%%", e.GetTrafficSplit()[dm.GetId()]),
			"Machine type":  dashIfEmpty(res.GetMachineSpec().GetMachineType()),
			"Compute nodes": fmt.Sprintf("%d–%d", res.GetMinReplicaCount(), res.GetMaxReplicaCount()),
			"Created":       vertexTime(dm.GetCreateTime())}})
	}
	models.Total = len(models.Items)
	d.Sections = []console.Section{{ID: "models", Label: "Models", Listing: models}}
	return d, nil
}

func (p vertexEndpointsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{
		{ID: "predict", Label: "Test model", Fields: []console.Field{
			{Name: "request", Label: "JSON request", Type: "textarea", Required: true, Default: `{"instances": [1]}`,
				Help: `A prediction request: {"instances": [...], "parameters": {...}}.`},
		}},
		{ID: "traffic", Label: "Edit traffic split", Fields: []console.Field{
			{Name: "split", Label: "Traffic split", Type: "text", Required: true,
				Help: "DEPLOYED_MODEL_ID=PERCENT pairs, comma-separated, adding up to 100."},
		}},
		{ID: "replicas", Label: "Edit compute nodes", Fields: []console.Field{
			{Name: "deployedModelId", Label: "Deployed model ID", Type: "text", Required: true, Pattern: `^[0-9]{1,19}$`},
			{Name: "minReplicas", Label: "Minimum number of compute nodes", Type: "text", Required: true, Default: "1", Pattern: `^[0-9]{1,3}$`},
			{Name: "maxReplicas", Label: "Maximum number of compute nodes", Type: "text", Required: true, Default: "1", Pattern: `^[0-9]{1,3}$`},
		}},
		{ID: "edit", Label: "Edit settings", Fields: []console.Field{
			{Name: "displayName", Label: "Endpoint name", Type: "text", Help: "Empty keeps the name."},
			{Name: "description", Label: "Description", Type: "textarea", Help: "Empty keeps the description."},
			{Name: "labels", Label: "Labels", Type: "map", Help: "Replaces the labels when any is given."},
		}},
		{ID: "undeploy", Label: "Undeploy model from endpoint", Destructive: true, Fields: []console.Field{
			{Name: "deployedModelId", Label: "Deployed model ID", Type: "text", Required: true, Pattern: `^[0-9]{1,19}$`},
		}},
	}
}

func parseSplit(s string) (map[string]int32, error) {
	out := map[string]int32{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if !ok || err != nil {
			return nil, fmt.Errorf("traffic split: %q is not DEPLOYED_MODEL_ID=PERCENT", kv)
		}
		out[strings.TrimSpace(k)] = int32(n)
	}
	return out, nil
}

func (p vertexEndpointsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

func (p vertexEndpointsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 1 {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	ep := path[0]
	switch action {
	case "predict":
		pc, err := p.api.prediction(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = pc.Close() }()
		var body struct {
			Instances  []any `json:"instances"`
			Parameters any   `json:"parameters"`
		}
		if err := json.Unmarshal([]byte(values["request"]), &body); err != nil {
			return nil, fmt.Errorf("the request is not JSON: %v", err)
		}
		req := &aiplatformpb.PredictRequest{Endpoint: ep}
		for _, x := range body.Instances {
			v, err := structpb.NewValue(x)
			if err != nil {
				return nil, err
			}
			req.Instances = append(req.Instances, v)
		}
		if body.Parameters != nil {
			if req.Parameters, err = structpb.NewValue(body.Parameters); err != nil {
				return nil, err
			}
		}
		resp, err := pc.Predict(ctx, req)
		if err != nil {
			return nil, err
		}
		l := &console.Listing{Columns: []string{"Prediction"}, NameColumn: "Instance", Noun: "predictions",
			Note: "Answered by deployed model " + resp.GetDeployedModelId()}
		for i, v := range resp.GetPredictions() {
			b, _ := protojson.Marshal(v)
			l.Items = append(l.Items, console.Resource{Name: strconv.Itoa(i), Fields: map[string]string{"Prediction": string(b)}})
		}
		l.Total = len(l.Items)
		return l, nil
	case "traffic":
		split, err := parseSplit(values["split"])
		if err != nil {
			return nil, err
		}
		c, err := p.api.endpoints(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		_, err = c.UpdateEndpoint(ctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: &aiplatformpb.Endpoint{Name: ep, TrafficSplit: split},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_split"}}})
		return nil, err
	case "replicas":
		minR, err1 := strconv.Atoi(strings.TrimSpace(values["minReplicas"]))
		maxR, err2 := strconv.Atoi(strings.TrimSpace(values["maxReplicas"]))
		if err1 != nil || err2 != nil {
			return nil, errors.New("the compute node counts must be numbers")
		}
		c, err := p.api.endpoints(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		op, err := c.MutateDeployedModel(ctx, &aiplatformpb.MutateDeployedModelRequest{Endpoint: ep,
			DeployedModel: &aiplatformpb.DeployedModel{Id: strings.TrimSpace(values["deployedModelId"]),
				PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
					MinReplicaCount: int32(minR), MaxReplicaCount: int32(maxR)}}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"dedicated_resources.min_replica_count", "dedicated_resources.max_replica_count"}}})
		if err != nil {
			return nil, err
		}
		_, err = op.Wait(ctx)
		return nil, err
	case "edit":
		in := &aiplatformpb.Endpoint{Name: ep}
		var mask []string
		if v := strings.TrimSpace(values["displayName"]); v != "" {
			in.DisplayName, mask = v, append(mask, "display_name")
		}
		if v := values["description"]; strings.TrimSpace(v) != "" {
			in.Description, mask = v, append(mask, "description")
		}
		if strings.TrimSpace(values["labels"]) != "" {
			labels, err := console.ParseMap(values["labels"])
			if err != nil {
				return nil, err
			}
			in.Labels, mask = labels, append(mask, "labels")
		}
		if len(mask) == 0 {
			return nil, errors.New("nothing to change: fill in at least one field")
		}
		c, err := p.api.endpoints(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		_, err = c.UpdateEndpoint(ctx, &aiplatformpb.UpdateEndpointRequest{Endpoint: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		return nil, err
	case "undeploy":
		c, err := p.api.endpoints(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		op, err := c.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: ep, DeployedModelId: strings.TrimSpace(values["deployedModelId"])})
		if err != nil {
			return nil, err
		}
		_, err = op.Wait(ctx)
		return nil, err
	}
	return nil, fmt.Errorf("unknown action %q", action)
}
