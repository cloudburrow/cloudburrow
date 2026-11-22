// Package syntaxnet tags and parses English text with Parsey McParseface,
// the part-of-speech tagger and dependency parser Google published with
// SyntaxNet (Apache-2.0, github.com/tensorflow/models, research/syntaxnet,
// read 2026-11-21 at tag v1.13.0 and at the 2016-05-12 commit 32ab5a58 the
// models were released with), run in pure Go, for Natural Language's
// analyzeSyntax (#1799):
//
//   - the models' TensorFlow V1 checkpoints (parser-params,
//     tagger-params) are read directly (checkpoint.go), their averaged
//     ("slim") parameters as parser_eval loads them;
//   - each network is graph_builder.py's feed-forward one: an embedding
//     per feature group, ReLU hidden layers (64 for the tagger, 512 and 512
//     for the parser, as Parsey's demo.sh runs them) and a linear output;
//   - features are context.pbtxt's, valued as the 2016 sentence and parser
//     features value them (unknown, outside and root values included);
//   - both models decode with beam search, beam 8, path scores the sums of
//     the outputs (structured graph builder, beam_reader_ops.cc); the
//     tagger tags left to right, the parser is arc-standard;
//   - text is tokenized as SyntaxNet's english-text format does
//     (text_formats.cc's Penn Treebank rules), keeping each token's offset.
//
// The model files are fetched once from raw.githubusercontent.com into
// <state-dir>/models and checked against the SHA-256 pinned in
// internal/localai.
package syntaxnet

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Source returns the directory holding the model's Files, fetching them
// first when they are missing: cmd/cloudburrow sets it to localai's pinned
// fetch.
type Source func(ctx context.Context) (string, error)

// ErrNoModel means no source is set: this process has no parser.
var ErrNoModel = errors.New("no syntax model is configured")

// ErrFetch means a model file could not be fetched.
var ErrFetch = errors.New("the syntax model could not be fetched")

var slot struct {
	mu     sync.Mutex
	source Source
	model  *Model
	err    error
	done   bool
}

// SetSource sets where the model comes from, and forgets one loaded from
// the previous source.
func SetSource(s Source) {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	slot.source, slot.model, slot.err, slot.done = s, nil, nil, false
}

// Default is the model of the configured source, loaded the first time
// it is asked for. A failed fetch is retried the next time.
func Default(ctx context.Context) (*Model, error) {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.done {
		return slot.model, slot.err
	}
	if slot.source == nil {
		return nil, ErrNoModel
	}
	dir, err := slot.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetch, err)
	}
	slot.model, slot.err = Open(dir)
	slot.done = true
	return slot.model, slot.err
}
