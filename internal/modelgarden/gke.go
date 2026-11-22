package modelgarden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Deploying to GKE (#2148). Google's Model Garden deploy panel offers two
// deployment environments for a Hugging Face model: Vertex AI (an endpoint,
// Deploy above) and GKE, whose deployment configuration Model Garden
// generates: Kubernetes manifests (PublisherModel's
// supported_actions.deploy_gke.gke_yaml_configs), applied to a GKE cluster
// with kubectl. Here they are generated for the same runtime and pinned
// image the endpoint would run, with the labels Google's Model Garden GKE
// manifests carry (ai.gke.io/model, ai.gke.io/inference-server,
// examples.ai.gke.io/source: model-garden), a gated model's token read from
// the Secret hf-secret (key hf_api_token) that Google's instructions create
// with kubectl, and applied by the console to a CloudBurrow Kubernetes
// Engine cluster (a kind cluster of its own). The workload is the user's,
// as on GKE: it is not an endpoint, and is managed from Kubernetes Engine's
// Workloads page.

// GKESecret and GKESecretKey are the Secret Google's Model Garden GKE
// manifests read the Hugging Face token from.
const (
	GKESecret    = "hf-secret"
	GKESecretKey = "hf_api_token"
)

// GKEPlan is a model's GKE deployment: its manifests, and the commands
// Google's instructions give.
type GKEPlan struct {
	Model       string `json:"model"`
	Engine      string `json:"engine"`
	EngineTitle string `json:"engineTitle"`
	Name        string `json:"name"` // the Deployment's and Service's
	Gated       bool   `json:"gated"`
	// YAML is the manifests, one document each, as Model Garden shows them.
	YAML string `json:"yaml"`
	// Configs are the same documents one by one (gke_yaml_configs).
	Configs       []string `json:"configs"`
	SecretCommand string   `json:"secretCommand,omitempty"`
	ApplyCommand  string   `json:"applyCommand"`
	ForwardCmd    string   `json:"forwardCommand"`
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// gkeName is a model's Deployment name: Google's "{model}-deployment"
// shape, a DNS label.
func gkeName(hfID string) string {
	base := hfID[strings.Index(hfID, "/")+1:]
	n := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if len(n) > 40 {
		n = strings.TrimRight(n[:40], "-")
	}
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "model-" + n
	}
	return n
}

// PlanGKE generates hfID's GKE manifests for a cluster whose nodes are arch.
func (m *Manager) PlanGKE(ctx context.Context, hfID, arch string) (GKEPlan, error) {
	if m.Models == nil {
		return GKEPlan{}, apierror.FailedPrecondition("Hugging Face models are not available on this instance")
	}
	info, eng, file, err := m.Choose(ctx, hfID)
	if err != nil {
		return GKEPlan{}, err
	}
	plan := Plan{HFID: info.ID, Revision: info.Revision, Task: info.PipelineTag, File: file}
	if eng.Name == EngineVLLM || eng.Name == EngineLlamaCpp {
		if b, err := m.Models.ReadFile(ctx, info.ID, info.Revision, "config.json", "", 1<<20); err == nil {
			plan.ContextLen = ContextLength(b)
		}
	}
	return gkePlan(plan, eng, arch, info.Gated != "")
}

func gkePlan(plan Plan, eng Engine, arch string, gated bool) (GKEPlan, error) {
	name := gkeName(plan.HFID)
	pod := eng.Pod(plan)
	modelLabel := strings.ToLower(nonDNS.ReplaceAllString(plan.HFID[strings.Index(plan.HFID, "/")+1:], "-"))
	if len(modelLabel) > 63 {
		modelLabel = modelLabel[:63]
	}
	w := Workload{Name: name, Image: eng.ImageFor(arch), Command: pod.Command, Args: pod.Args, Env: pod.Env, Files: pod.Files,
		ShmSize: eng.ShmSize, HealthRoute: eng.HealthRoute, SecretName: GKESecret, SecretKey: GKESecretKey, Bare: true,
		Labels: map[string]string{"app": name, "ai.gke.io/model": strings.Trim(modelLabel, "-"),
			"ai.gke.io/inference-server": eng.Name, "examples.ai.gke.io/source": "model-garden"}}
	kc := &KubeCluster{Namespace: "default"}
	var docs [][]byte
	if cm := kc.ConfigMapManifest(w); cm != nil {
		docs = append(docs, cm)
	}
	d, s := kc.Manifests(w)
	docs = append(docs, d, s)
	out := GKEPlan{Model: plan.HFID, Engine: eng.Name, EngineTitle: eng.Title, Name: name, Gated: gated,
		ApplyCommand: "kubectl apply -f " + name + ".yaml",
		ForwardCmd:   fmt.Sprintf("kubectl port-forward service/%s %d:%d", name, containerPort, containerPort)}
	if gated {
		out.SecretCommand = "kubectl create secret generic " + GKESecret + " --from-literal=" + GKESecretKey + "=${HF_TOKEN}"
	}
	for _, doc := range docs {
		y, err := orderedYAML(doc)
		if err != nil {
			return GKEPlan{}, err
		}
		out.Configs = append(out.Configs, y)
	}
	out.YAML = strings.Join(out.Configs, "---\n")
	return out, nil
}

// ApplyGKE applies plan to the cluster kubeconfig reaches, in its default
// namespace, after the hf-secret holding token when the model is gated.
// The token goes into that Secret only (server-side apply, so no
// last-applied annotation copies it) and is never returned.
func ApplyGKE(ctx context.Context, kubeconfig string, plan GKEPlan, token string) error {
	kube := k8s.New(kubeconfig, "", "default")
	opts := k8s.ApplyOptions{ServerSide: true, ForceConflicts: true, FieldManager: "cloudburrow-modelgarden"}
	if plan.Gated {
		if token == "" {
			return apierror.InvalidArgument("%s is a gated Hugging Face model: its GKE deployment reads your access token "+
				"from the Secret %s, so give the token", plan.Model, GKESecret)
		}
		secret, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"name": GKESecret, "namespace": "default"}, "stringData": map[string]string{GKESecretKey: token}})
		if err := kube.Apply(ctx, string(secret), opts); err != nil {
			return apierror.FailedPrecondition("create the Secret %s: %s", GKESecret, scrub(err.Error(), token))
		}
	}
	for _, doc := range plan.Configs {
		if err := kube.Apply(ctx, doc, opts); err != nil {
			return apierror.FailedPrecondition("kubectl apply: %v", err)
		}
	}
	return nil
}

// keyOrder is the order Kubernetes' own manifests (and Google's) write
// keys in; the rest follow alphabetically.
var keyOrder = map[string]int{"apiVersion": 0, "kind": 1, "type": 2, "metadata": 3, "name": 4, "namespace": 5,
	"labels": 6, "spec": 7, "data": 8, "stringData": 9, "replicas": 10, "strategy": 11, "selector": 12, "template": 13,
	"image": 14, "imagePullPolicy": 15, "command": 16, "args": 17, "env": 18, "value": 19, "valueFrom": 20,
	"ports": 21, "volumeMounts": 22, "readinessProbe": 23, "volumes": 24}

// orderedYAML renders a JSON object as YAML in keyOrder.
func orderedYAML(doc []byte) (string, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(toNode(v)); err != nil {
		return "", err
	}
	return buf.String(), enc.Close()
}

func toNode(v any) *yaml.Node {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, aok := keyOrder[keys[i]]
			b, bok := keyOrder[keys[j]]
			switch {
			case aok && bok:
				return a < b
			case aok != bok:
				return aok
			}
			return keys[i] < keys[j]
		})
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range keys {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, toNode(x[k]))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, e := range x {
			n.Content = append(n.Content, toNode(e))
		}
		return n
	}
	n := &yaml.Node{}
	_ = n.Encode(v)
	if s, ok := v.(string); ok && strings.Contains(s, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}
