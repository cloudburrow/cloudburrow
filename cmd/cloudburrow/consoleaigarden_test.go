package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// TestModelGardenNoteMatchesTheCatalogue (#41): the Model Garden list's note
// says what the catalogue holds. It said "every embedding artifact is
// gated" after three ungated ones were catalogued, and that the one model
// that runs is a community conversion after the ONNX EmbeddingGemma joined
// the Gemma one.
func TestModelGardenNoteMatchesTheCatalogue(t *testing.T) {
	list, err := aiProvider{}.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ungatedEmbedding, googleUngated := false, false
	for _, m := range localai.Models() {
		if m.Modality == localai.ModalityEmbedding && m.Access != localai.AccessGated {
			ungatedEmbedding = true
		}
		if m.Publisher == localai.PublisherGoogle && m.Access != localai.AccessGated {
			googleUngated = true
		}
	}
	if ungatedEmbedding && strings.Contains(list.Note, "every embedding artifact") {
		t.Errorf("the note says every embedding artifact is gated, and the catalogue has an ungated one: %s", list.Note)
	}
	if googleUngated && strings.Contains(list.Note, "every Google-published artifact is gated") {
		t.Errorf("the note says every Google-published artifact is gated, and the catalogue has an ungated one: %s", list.Note)
	}
	if strings.Contains(list.Note, "the one model that runs") {
		t.Errorf("the note says one model runs; the Gemma and ONNX EmbeddingGemma conversions both can: %s", list.Note)
	}
}
