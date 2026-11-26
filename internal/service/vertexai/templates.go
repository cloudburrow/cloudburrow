package vertexai

// Pipeline templates (#2210): a PipelineJob's template_uri, "a template
// uri from where the PipelineJob.pipeline_spec, if empty, will be
// downloaded", from an Artifact Registry Kubeflow Pipelines repository
// (https://{location}-kfp.pkg.dev/{project}/{repository}/{package}/{tag or
// sha256:version}) or any https:// URL
// (https://docs.cloud.google.com/vertex-ai/docs/pipelines/create-pipeline-template,
// "Use the template in a pipeline run"; the PipelineJob proto, read
// 2026-11-24). The template is the compiled pipeline (YAML, as kfp's
// compiler writes it, or a PipelineJob JSON with a pipelineSpec), and it
// is what runs: a pipeline_spec sent beside it (the Vertex AI SDK sends
// both) is replaced by it. template_metadata.version is the Artifact
// Registry version it was read at ("sha256:..."), filled only for a
// template from that "supported template registry". The template is read
// through the runner (cmd/cloudburrow/vertexjobs.go): Artifact Registry
// from this instance, any other URL through internal/egress, so a
// googleapis.com URL is this instance's API and no other Google host is
// dialled.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TemplateFetcher is a PipelineRunner that can read a pipeline template.
type TemplateFetcher interface {
	FetchTemplate(ctx context.Context, uri string) ([]byte, error)
}

var kfpTemplateRE = regexp.MustCompile(`^https://([a-z0-9-]+)-kfp\.pkg\.dev/([^/]+)/([^/]+)/([^/]+)/([^/]+)$`)

// loadTemplate fills j's pipeline_spec (and its runtime defaults) from
// its template_uri.
func (s *Server) loadTemplate(j *aiplatformpb.PipelineJob) error {
	uri := j.GetTemplateUri()
	if uri == "" {
		return nil
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return apierror.InvalidArgument("pipeline_job.template_uri %q is not an https:// URL: an Artifact Registry template (https://LOCATION-kfp.pkg.dev/PROJECT/REPOSITORY/PACKAGE/TAG) or an HTTPS URL", uri)
	}
	ar := kfpTemplateRE.FindStringSubmatch(uri)
	if strings.HasSuffix(strings.ToLower(u.Host), "-kfp.pkg.dev") && ar == nil {
		return apierror.InvalidArgument("pipeline_job.template_uri %q is not https://LOCATION-kfp.pkg.dev/PROJECT/REPOSITORY/PACKAGE/{TAG or VERSION}", uri)
	}
	st := s.notebooks()
	st.mu.Lock()
	runner := st.pipelineRunner
	st.mu.Unlock()
	f, ok := runner.(TemplateFetcher)
	if !ok {
		return apierror.FailedPrecondition("pipeline_job.template_uri: templates are read from this instance's Artifact Registry, which is not served here")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := f.FetchTemplate(ctx, uri)
	if err != nil {
		return apierror.InvalidArgument("pipeline_job.template_uri: %v", err)
	}
	spec, runtimeCfg, err := templateSpec(b)
	if err != nil {
		return apierror.InvalidArgument("pipeline_job.template_uri %s is not a pipeline template: %v", uri, err)
	}
	j.PipelineSpec = spec
	if runtimeCfg != nil {
		if j.RuntimeConfig == nil {
			j.RuntimeConfig = runtimeCfg
		} else if j.RuntimeConfig.GcsOutputDirectory == "" {
			j.RuntimeConfig.GcsOutputDirectory = runtimeCfg.GetGcsOutputDirectory()
		}
	}
	if ar != nil {
		sum := sha256.Sum256(b)
		j.TemplateMetadata = &aiplatformpb.PipelineTemplateMetadata{Version: "sha256:" + hex.EncodeToString(sum[:])}
	}
	return nil
}

// templateSpec reads a template: the KFP IR (YAML or JSON), or a
// PipelineJob JSON naming it as pipelineSpec with its runtimeConfig.
func templateSpec(b []byte) (*structpb.Struct, *aiplatformpb.PipelineJob_RuntimeConfig, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, nil, err
	}
	doc = yamlJSON(doc).(map[string]any)
	var rc *aiplatformpb.PipelineJob_RuntimeConfig
	if inner, ok := doc["pipelineSpec"].(map[string]any); ok {
		if r, ok := doc["runtimeConfig"].(map[string]any); ok {
			raw, _ := json.Marshal(r)
			rc = &aiplatformpb.PipelineJob_RuntimeConfig{}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, rc); err != nil {
				rc = nil
			}
		}
		doc = inner
	}
	if _, ok := doc["pipelineInfo"]; !ok {
		return nil, nil, apierror.InvalidArgument("it has no pipelineInfo")
	}
	spec, err := structpb.NewStruct(doc)
	if err != nil {
		return nil, nil, err
	}
	return spec, rc, nil
}

// yamlJSON makes a YAML document's values JSON's (map keys as strings).
func yamlJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, el := range x {
			x[k] = yamlJSON(el)
		}
		return x
	case map[any]any:
		out := map[string]any{}
		for k, el := range x {
			out[toString(k)] = yamlJSON(el)
		}
		return out
	case []any:
		for i, el := range x {
			x[i] = yamlJSON(el)
		}
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	}
	return v
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
