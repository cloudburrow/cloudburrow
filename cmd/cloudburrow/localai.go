package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// buildLocalAI returns the local generation endpoint, or nil when no model is
// configured.
//
// Absence is the default and is not an error. The runtime is a from-source
// build and the model a multi-gigabyte download; a developer using Cloud
// Storage should not be made to acquire either.
func buildLocalAI(cfg config.Config) (*vertexai.Server, error) {
	if strings.TrimSpace(cfg.LocalAI.ModelPath) == "" {
		return nil, nil
	}
	modelPath, err := filepath.Abs(cfg.LocalAI.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("resolve -local-ai-model: %w", err)
	}

	image := cfg.LocalAI.Image
	if image == "" {
		image = config.DefaultLocalAIImage
	}
	if err := preflightLocalAI(modelPath, image); err != nil {
		return nil, err
	}
	modelID := cfg.LocalAI.ModelID
	if modelID == "" {
		modelID = catalogueIDFor(modelPath)
	}

	dir, file := filepath.Split(modelPath)
	gen := vertexai.NewDockerGenerator(image, filepath.Clean(dir), file, modelID)

	addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI))
	srv := vertexai.NewServerOn(addr, gen)
	for _, a := range cfg.LocalAI.Aliases {
		srv.Alias(a)
	}
	return srv, nil
}

// catalogueIDFor names the model from its artifact filename when the catalogue
// recognises it.
//
// Falling back to the bare filename is deliberate: an unrecognised artifact is
// still served, and is reported under a name that describes the file rather
// than under a catalogued ID it may not be.
func catalogueIDFor(modelPath string) string {
	base := filepath.Base(modelPath)
	for _, m := range localai.Models() {
		if m.Artifact == base {
			return m.ID
		}
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// printLocalAI reports the endpoint, and what the model actually is.
//
// The provenance line is not decoration. The only ungated model that runs
// today is a community conversion, and a developer who reads "gemma" on their
// screen should not have to look it up to learn it is not Google's.
func printLocalAI(w io.Writer, srv *vertexai.Server, cfg config.Config) {
	if srv == nil {
		return
	}
	fmt.Fprintf(w, "\nlocal AI:  http://%s\n", srv.Addr())
	fmt.Fprintf(w, "  model:   %s\n", srv.Model())

	if m, err := localai.Lookup(srv.Model()); err == nil && m.Publisher == localai.PublisherCommunity {
		fmt.Fprintf(w, "  note:    a COMMUNITY conversion, not published by Google\n")
	}
	if len(cfg.LocalAI.Aliases) > 0 {
		fmt.Fprintf(w, "  aliases: %s (explicitly configured substitutions)\n",
			strings.Join(cfg.LocalAI.Aliases, ", "))
	}
	fmt.Fprintf(w, "  generation options are refused rather than ignored; see docs/generation.md\n")
}

// dockerRun runs a docker command; replaced in tests.
var dockerRun = func(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// preflightLocalAI fails `up` at startup when the model or the runtime is
// missing (#602). Before, both were first touched by the first
// generateContent, which failed with a docker error far from the flag that
// caused it.
//
// The default image is the tag `make litert-lm` builds; it is published
// nowhere, so it is never pulled, and its absence says how to build it. A
// different image is pulled once before giving up.
func preflightLocalAI(modelPath, image string) error {
	info, err := os.Stat(modelPath)
	if err != nil {
		return fmt.Errorf("-local-ai-model %s: %w", modelPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("-local-ai-model %s is not a file", modelPath)
	}
	if _, err := dockerRun("image", "inspect", image); err == nil {
		return nil
	}
	if image == config.DefaultLocalAIImage {
		return fmt.Errorf("the local AI runtime image %s is not built: run `make litert-lm` in a CloudBurrow checkout, "+
			"or pass -local-ai-image with an image you have (no runtime image is published yet)", image)
	}
	if out, err := dockerRun("pull", image); err != nil {
		return fmt.Errorf("the local AI runtime image %s is not present and could not be pulled: %v\n%s", image, err, strings.TrimSpace(out))
	}
	return nil
}
