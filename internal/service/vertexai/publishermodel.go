package vertexai

import (
	"context"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// GetPublisherModel (#1940) describes a publisher model this endpoint
// serves at projects/{p}/locations/{l}/publishers/google/models/{model}:
// the local catalogue's models with a runtime (the embedding and
// generation models of embeddings.md and generation.md). Any other model
// is NOT_FOUND, as Predict on it is; nothing is substituted. Hugging Face
// models (is_hugging_face_model) are Model Garden's Deploy, not served here.
func (g *modelGardenService) GetPublisherModel(_ context.Context, req *aiplatformpb.GetPublisherModelRequest) (*aiplatformpb.PublisherModel, error) {
	if req.GetIsHuggingFaceModel() {
		return nil, grpcError(apierror.Unimplemented("GetPublisherModel with is_hugging_face_model is not served: " +
			"deploy a Hugging Face model with ModelGardenService.Deploy (hugging_face_model_id)"))
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
