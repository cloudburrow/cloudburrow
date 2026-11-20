package vertexai

import (
	"context"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetPublisherModel(t *testing.T) {
	g := &modelGardenService{s: NewServer(nil)}
	ctx := context.Background()
	m, err := g.GetPublisherModel(ctx, &aiplatformpb.GetPublisherModelRequest{Name: "publishers/google/models/embeddinggemma-300m-onnx-community@001"})
	if err != nil || m.GetName() != "publishers/google/models/embeddinggemma-300m-onnx-community" || m.GetFrameworks()[0] != "onnxruntime" {
		t.Fatalf("served model = %v, %v", m, err)
	}
	for name, want := range map[string]codes.Code{
		"publishers/google/models/gemini-2.0-flash":                 codes.NotFound,
		"publishers/google/models/embeddinggemma-300m-tensor-g4":    codes.NotFound, // catalogued, no runtime
		"publishers/meta/models/embeddinggemma-300m-onnx-community": codes.NotFound,
		"projects/p/locations/l/publishers/google/models/gemma":     codes.InvalidArgument,
		"publishers/google/models/":                                 codes.InvalidArgument,
	} {
		if _, err := g.GetPublisherModel(ctx, &aiplatformpb.GetPublisherModelRequest{Name: name}); status.Code(err) != want {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	if _, err := g.GetPublisherModel(ctx, &aiplatformpb.GetPublisherModelRequest{Name: "publishers/google/models/x", IsHuggingFaceModel: true}); status.Code(err) != codes.Unimplemented {
		t.Errorf("is_hugging_face_model: %v, want UNIMPLEMENTED", err)
	}
}
