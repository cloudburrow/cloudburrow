// Package localai handles acquisition of local AI model artifacts.
//
// Acquisition is deliberately separate from execution, because the two fail
// for different reasons: a model can be unobtainable while the runtime works,
// which is exactly the situation for embeddings today. The runtime itself is
// built from source by deploy/litert-lm; docs/local-ai.md §4 records the
// measured run and corrects the earlier audit, which wrongly concluded no
// Linux runtime could exist.
package localai

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Publisher records who actually publishes an artifact.
//
// This distinction is the point of the type. A converted model hosted beside
// Google's own is still a community conversion, and calling it
// "Google-published" would misrepresent its provenance.
type Publisher string

const (
	// PublisherGoogle is the verified Google organisation.
	PublisherGoogle Publisher = "google"
	// PublisherCommunity is a third-party conversion, however reputable.
	PublisherCommunity Publisher = "community"
)

// Access records how an artifact may be obtained.
type Access string

const (
	// AccessOpen needs no credentials.
	AccessOpen Access = "open"
	// AccessGated requires accepting a licence and being granted access, so
	// it can never be downloaded automatically.
	AccessGated Access = "gated"
)

// Modality is what a model does.
type Modality string

const (
	ModalityText      Modality = "text-generation"
	ModalityEmbedding Modality = "embedding"
)

// Model is a catalogued artifact.
type Model struct {
	// ID is the CloudBurrow-facing identifier.
	ID string
	// Repo is the upstream repository.
	Repo string
	// Publisher records verified provenance, not merely where it is hosted.
	Publisher Publisher
	// Access records whether credentials and licence acceptance are needed.
	Access Access
	// License is the licence the artifact is published under.
	License string
	// Modality is what the model does.
	Modality Modality
	// Artifact is the specific file, because a repository usually holds
	// several and they are not interchangeable.
	Artifact string
	// Runtime names the runtime that can execute it.
	Runtime string
	// Notes records anything a user must know before choosing it.
	Notes string
}

// ErrUnknownModel means the identifier is not catalogued.
var ErrUnknownModel = errors.New("unknown model")

// ErrGated means the artifact cannot be fetched without credentials.
var ErrGated = errors.New("model is gated")

// catalog is the verified set. Every entry's repository, gating and artifact
// filename was checked against the Hugging Face API on 2026-09-21, the
// filenames by listing each repository rather than by assuming a convention;
// see docs/local-ai.md for the method. TestCatalogArtifactsExistUpstream in
// test/upstream re-checks them against the live API.
var catalog = map[string]Model{
	"gemma-3n-e2b-it": {
		ID:        "gemma-3n-e2b-it",
		Repo:      "google/gemma-3n-E2B-it-litert-lm",
		Publisher: PublisherGoogle,
		Access:    AccessGated,
		License:   "gemma",
		Modality:  ModalityText,
		Artifact:  "gemma-3n-E2B-it-int4.litertlm",
		Runtime:   "litert-lm",
		Notes:     "Published by the verified Google organisation. Requires accepting the Gemma licence.",
	},
	"gemma-3n-e4b-it": {
		ID:        "gemma-3n-e4b-it",
		Repo:      "google/gemma-3n-E4B-it-litert-lm",
		Publisher: PublisherGoogle,
		Access:    AccessGated,
		License:   "gemma",
		Modality:  ModalityText,
		Artifact:  "gemma-3n-E4B-it-int4.litertlm",
		Runtime:   "litert-lm",
		Notes:     "Larger sibling of E2B. Same provenance and licence.",
	},
	"embeddinggemma-300m": {
		ID:        "embeddinggemma-300m",
		Repo:      "google/embeddinggemma-300m",
		Publisher: PublisherGoogle,
		Access:    AccessGated,
		License:   "gemma",
		Modality:  ModalityEmbedding,
		Artifact:  "model.safetensors",
		Runtime:   "", // no verified local runtime; see docs/local-ai.md
		Notes:     "Google-published, but shipped as safetensors. LiteRT-LM support for generation does not imply embedding support.",
	},
	"gemma-4-e2b-it-community": {
		ID:        "gemma-4-e2b-it-community",
		Repo:      "litert-community/gemma-4-E2B-it-litert-lm",
		Publisher: PublisherCommunity,
		Access:    AccessOpen,
		License:   "gemma",
		Modality:  ModalityText,
		// No -int4 suffix: that is the google/ repositories' convention, not
		// this one's. This is the file that was actually downloaded and run.
		Artifact: "gemma-4-E2B-it.litertlm",
		Runtime:  "litert-lm",
		Notes:    "A COMMUNITY conversion. The litert-community organisation is not verified as Google, whatever the model name suggests.",
	},
}

// Models returns the catalogue in a stable order.
func Models() []Model {
	out := make([]Model, 0, len(catalog))
	for _, m := range catalog {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Lookup returns a catalogued model.
func Lookup(id string) (Model, error) {
	m, ok := catalog[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return Model{}, fmt.Errorf("%w: %q; known models: %s", ErrUnknownModel, id, strings.Join(modelIDs(), ", "))
	}
	return m, nil
}

func modelIDs() []string {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GooglePublished reports whether the artifact comes from the verified Google
// organisation.
func (m Model) GooglePublished() bool { return m.Publisher == PublisherGoogle }

// RequiresCredentials reports whether fetching needs a token and licence
// acceptance.
func (m Model) RequiresCredentials() bool { return m.Access == AccessGated }

// Runnable reports whether a verified runtime exists for this model, and why
// not when one does not.
//
// Answering "no" here is the honest outcome of the runtime audit: a catalogue
// entry is not a promise that CloudBurrow can execute it.
func (m Model) Runnable() (bool, string) {
	if m.Runtime == "" {
		return false, fmt.Sprintf(
			"no verified local runtime for %s: LiteRT-LM executes .litertlm generation models, "+
				"and its support does not extend to this artifact", m.ID)
	}
	if m.Runtime == "litert-lm" {
		return false, "LiteRT-LM publishes no current Linux binary (last one was v0.11.0, 2026-05-07), " +
			"so it cannot run in a CloudBurrow cluster pod; see docs/local-ai.md"
	}
	return true, ""
}
