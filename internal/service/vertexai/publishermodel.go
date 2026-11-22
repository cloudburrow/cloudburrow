package vertexai

import (
	"context"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
)

// GetPublisherModel (#1940) describes a publisher model this endpoint
// serves at projects/{p}/locations/{l}/publishers/google/models/{model}:
// the local catalogue's models with a runtime (the embedding and
// generation models of embeddings.md and generation.md). Any other model
// is NOT_FOUND, as Predict on it is; nothing is substituted. A Hugging Face
// model (is_hugging_face_model, #2148) is read from the Hub and described
// with its two deployment environments: supported_actions.deploy (the
// serving container ModelGardenService.Deploy runs for it) and deploy_gke
// (gke_yaml_configs, its GKE manifests).
func (g *modelGardenService) GetPublisherModel(ctx context.Context, req *aiplatformpb.GetPublisherModelRequest) (*aiplatformpb.PublisherModel, error) {
	if req.GetIsHuggingFaceModel() {
		out, err := g.hfPublisherModel(ctx, req.GetName())
		return out, grpcError(err)
	}
	pub, id, ok := parsePublisherModel(req.GetName())
	if !ok {
		return nil, grpcError(apierror.InvalidArgument("name %q is not publishers/{publisher}/models/{model}", req.GetName()))
	}
	m, err := localai.Lookup(id)
	if pub != "google" || err != nil || m.Runtime == "" || m.ID != id {
		return nil, grpcError(apierror.NotFound("publisher model %q is not served by this instance; the served publisher models are %s",
			req.GetName(), strings.Join(servedPublisherModels(), ", ")))
	}
	out := &aiplatformpb.PublisherModel{
		Name:                   "publishers/google/models/" + m.ID,
		VersionId:              "001",
		OpenSourceCategory:     aiplatformpb.PublisherModel_THIRD_PARTY_OWNED_OSS,
		LaunchStage:            aiplatformpb.PublisherModel_EXPERIMENTAL,
		VersionState:           aiplatformpb.PublisherModel_VERSION_STATE_STABLE,
		Frameworks:             []string{m.Runtime},
		PublisherModelTemplate: "projects/{project}/locations/{location}/publishers/google/models/" + m.ID,
	}
	if m.GooglePublished() {
		out.OpenSourceCategory = aiplatformpb.PublisherModel_GOOGLE_OWNED_OSS
	}
	return out, nil
}

// parsePublisherModel reads publishers/{publisher}/models/{model}, with an
// optional @version.
func parsePublisherModel(name string) (publisher, model string, ok bool) {
	p := strings.Split(name, "/")
	if len(p) != 4 || p[0] != "publishers" || p[2] != "models" || p[1] == "" || p[3] == "" {
		return "", "", false
	}
	model, _, _ = strings.Cut(p[3], "@")
	return p[1], model, true
}

// servedPublisherModels lists the catalogue's models with a runtime.
func servedPublisherModels() []string {
	var out []string
	for _, m := range localai.Models() {
		if m.Runtime != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// hfPublisherModel describes a Hugging Face model: publishers/{author}/models/{name}
// (or Model Garden's publishers/hf-{author}/...) is the Hub's {author}/{name}.
func (g *modelGardenService) hfPublisherModel(ctx context.Context, name string) (*aiplatformpb.PublisherModel, error) {
	m := g.s.modelGarden()
	if m == nil {
		return nil, gardenUnavailable()
	}
	pub, model, ok := parsePublisherModel(name)
	if !ok {
		return nil, apierror.InvalidArgument("name %q is not publishers/{publisher}/models/{model}", name)
	}
	hfID := strings.TrimPrefix(pub, "hf-") + "/" + model
	arch := "amd64"
	if m.Cluster != nil {
		if a, err := m.Cluster.Arch(ctx); err == nil {
			arch = a
		}
	}
	plan, err := m.PlanGKE(ctx, hfID, arch)
	if err != nil {
		return nil, err
	}
	eng, _ := modelgarden.EngineByName(plan.Engine)
	return &aiplatformpb.PublisherModel{
		Name:               modelgarden.PublisherModel(hfID),
		VersionId:          "001",
		OpenSourceCategory: aiplatformpb.PublisherModel_THIRD_PARTY_OWNED_OSS,
		LaunchStage:        aiplatformpb.PublisherModel_GA,
		VersionState:       aiplatformpb.PublisherModel_VERSION_STATE_STABLE,
		Frameworks:         []string{plan.Engine},
		SupportedActions: &aiplatformpb.PublisherModel_CallToAction{
			Deploy: &aiplatformpb.PublisherModel_CallToAction_Deploy{
				Title: "Deploy " + hfID, ModelDisplayName: hfID,
				ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: eng.ImageFor(arch),
					HealthRoute: eng.HealthRoute, PredictRoute: eng.PredictRoute,
					Ports: []*aiplatformpb.Port{{ContainerPort: 8080}}},
				PredictionResources: &aiplatformpb.PublisherModel_CallToAction_Deploy_DedicatedResources{
					DedicatedResources: &aiplatformpb.DedicatedResources{
						MachineSpec: &aiplatformpb.MachineSpec{MachineType: "local-cpu"}, MinReplicaCount: 1, MaxReplicaCount: 1}},
			},
			DeployGke: &aiplatformpb.PublisherModel_CallToAction_DeployGke{GkeYamlConfigs: plan.Configs},
		},
	}, nil
}
