package modelgarden

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A model's GKE manifests are the runtime the endpoint would run, in
// Google's Model Garden shape: its labels, hf-secret for a gated model's
// token, ordered YAML documents, and no CloudBurrow ownership labels.
func TestPlanGKE(t *testing.T) {
	m, _ := newClusterManager(t, "text-classification", "transformers.js", "auto", fakeTEI())
	p, err := m.PlanGKE(context.Background(), "acme/tiny", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineONNX || p.Name != "tiny" || !p.Gated || len(p.Configs) != 3 ||
		!strings.Contains(p.SecretCommand, "kubectl create secret generic hf-secret --from-literal=hf_api_token=") {
		t.Fatalf("plan = %+v", p)
	}
	for _, want := range []string{"kind: ConfigMap", "kind: Deployment", "kind: Service", "ai.gke.io/inference-server: onnx",
		"examples.ai.gke.io/source: model-garden", "name: hf-secret", "key: hf_api_token", "image: " + PythonImage,
		"start.sh: |", "---\n"} {
		if !strings.Contains(p.YAML, want) {
			t.Errorf("YAML lacks %q:\n%s", want, p.YAML)
		}
	}
	if strings.Contains(p.YAML, "cloudburrow.dev/") {
		t.Errorf("the user's manifests carry CloudBurrow's labels:\n%s", p.YAML)
	}
	if !strings.HasPrefix(p.Configs[1], "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: tiny\n") {
		t.Errorf("Deployment starts %q", p.Configs[1][:80])
	}
	for _, c := range p.Configs {
		var v map[string]any
		if err := yaml.Unmarshal([]byte(c), &v); err != nil || v["kind"] == nil {
			t.Errorf("not a manifest: %v\n%s", err, c)
		}
	}
	if gkeName("Qwen/Qwen2.5-0.5B-Instruct") != "qwen2-5-0-5b-instruct" || gkeName("a/9b") != "model-9b" {
		t.Errorf("gkeName = %q, %q", gkeName("Qwen/Qwen2.5-0.5B-Instruct"), gkeName("a/9b"))
	}
}

// Google's ai.gke.io/model label is the model's name lowercased: a
// mixed-case Hub ID keeps its letters (measured: "SmolLM2-135M-Instruct-GGUF"
// once came out "mol-2-135---nstruct").
func TestGKEModelLabelLowercases(t *testing.T) {
	p, err := gkePlan(Plan{HFID: "bartowski/SmolLM2-135M-Instruct-GGUF", File: "SmolLM2-135M-Instruct-Q4_K_M.gguf"},
		engines[EngineLlamaCpp], "arm64", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.YAML, "ai.gke.io/model: smollm2-135m-instruct-gguf") || p.Name != "smollm2-135m-instruct-gguf" {
		t.Fatalf("YAML = %s", p.YAML)
	}
}
