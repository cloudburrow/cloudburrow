package run

// What a Cloud Run service records beside its revisions (#2049):
//
//   - creator and last_modifier, Google's output-only "Email address of the
//     authenticated creator" and "of the last authenticated modifier": the
//     account the request's bearer token was minted for, or the instance's
//     own account for a request with none (as Secure Source Manager names
//     its caller, securesourcemanagerreviews.go). The console's Deployed by
//     column reads last_modifier, as Google's does.
//   - build_config, "Configuration for building a Cloud Run function":
//     stored and returned, as `gcloud run deploy --source --function` and
//     the console's Write a function set it. The build itself is the Builds
//     API's (SubmitBuild), as on Google, where the field only records it.
//   - Google's placeholder image, us-docker.pkg.dev/cloudrun/container/placeholder,
//     which Google's Write a function deploys as the service's first
//     revision before any source is. Its image is Google's, out of reach
//     here, so the revision runs the Envoy image the instance's gateway
//     already runs (by digest, so Knative resolves no tag), answering every
//     request with a placeholder page; it reads back as Google's image.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
)

const (
	// PlaceholderImage is Google's placeholder container image.
	PlaceholderImage = "us-docker.pkg.dev/cloudrun/container/placeholder"
	// PlaceholderBody is what the placeholder revision answers.
	PlaceholderBody = "This service is a placeholder: no source has been deployed to it yet."

	annCreator      = "cloudburrow.dev/creator"
	annLastModifier = "cloudburrow.dev/last-modifier"
	annBuildConfig  = "cloudburrow.dev/build-config"
)

// callerEmail is the account a request came from.
func callerEmail(ctx context.Context) string {
	return ssmCallerEmail(ssmCallerOf(ctx))
}

// renderRecord writes the service's record annotations.
func renderRecord(b *strings.Builder, svc *runpb.Service) {
	if c := svc.GetCreator(); c != "" {
		fmt.Fprintf(b, "    %s: %q\n", annCreator, c)
	}
	if m := svc.GetLastModifier(); m != "" {
		fmt.Fprintf(b, "    %s: %q\n", annLastModifier, m)
	}
	if bc := svc.GetBuildConfig(); bc != nil {
		raw, _ := protojson.Marshal(bc)
		fmt.Fprintf(b, "    %s: %q\n", annBuildConfig, string(raw))
	}
}

// readRecord reads them back.
func readRecord(ann map[string]string, svc *runpb.Service) {
	svc.Creator, svc.LastModifier = ann[annCreator], ann[annLastModifier]
	if raw := ann[annBuildConfig]; raw != "" {
		var bc runpb.BuildConfig
		if protojson.Unmarshal([]byte(raw), &bc) == nil {
			svc.BuildConfig = &bc
		}
	}
}

// checkBuildConfig refuses what build_config cannot name.
func checkBuildConfig(bc *runpb.BuildConfig) error {
	if bc == nil {
		return nil
	}
	if l := bc.GetSourceLocation(); l != "" && !strings.HasPrefix(l, "gs://") {
		return apierror.InvalidArgument("build_config.source_location %q: a Cloud Storage URI, gs://bucket/object", l)
	}
	if p := bc.GetWorkerPool(); p != "" {
		return apierror.Unimplemented("build_config.worker_pool is not supported: no private pool exists locally")
	}
	return nil
}

// placeholder is the gateway's Envoy image, by digest, once read.
var placeholder struct {
	mu  sync.Mutex
	ref string
}

func placeholderRef() string {
	placeholder.mu.Lock()
	defer placeholder.mu.Unlock()
	return placeholder.ref
}

// usesPlaceholder reports whether a container runs Google's placeholder.
func usesPlaceholder(containers []*runpb.Container) bool {
	for _, c := range containers {
		if c.GetImage() == PlaceholderImage {
			return true
		}
	}
	return false
}

// ensurePlaceholder reads the gateway's Envoy image digest, which the
// placeholder revision runs.
func (s *Server) ensurePlaceholder(ctx context.Context, containers []*runpb.Container) error {
	if !usesPlaceholder(containers) || placeholderRef() != "" {
		return nil
	}
	out, err := s.kn.Kube.Do(ctx, "", "get", "pods", "-n", components.IngressNamespace, "-l", "app="+gatewayDeployment,
		"-o", "jsonpath={.items[0].status.containerStatuses[0].imageID}")
	ref := strings.TrimSpace(out)
	ref = strings.TrimPrefix(ref, "docker-pullable://")
	if err != nil || !strings.Contains(ref, "@sha256:") {
		return apierror.Internal(fmt.Errorf("%v %q", err, ref), "find the gateway's image for the placeholder revision")
	}
	placeholder.mu.Lock()
	placeholder.ref = ref
	placeholder.mu.Unlock()
	return nil
}

// placeholderEnvoy is the placeholder revision's Envoy bootstrap: every
// request answered 200 with PlaceholderBody on port.
func placeholderEnvoy(port int32) string {
	return fmt.Sprintf(`{"static_resources":{"listeners":[{"name":"placeholder","address":{"socket_address":`+
		`{"address":"0.0.0.0","port_value":%d}},"filter_chains":[{"filters":[{"name":"envoy.filters.network.http_connection_manager",`+
		`"typed_config":{"@type":"type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",`+
		`"stat_prefix":"placeholder","codec_type":"AUTO","route_config":{"virtual_hosts":[{"name":"all","domains":["*"],`+
		`"routes":[{"match":{"prefix":"/"},"direct_response":{"status":200,"body":{"inline_string":%q}}}]}]},`+
		`"http_filters":[{"name":"envoy.filters.http.router","typed_config":`+
		`{"@type":"type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}]}}]}]}]}}`, port, PlaceholderBody)
}

// placeholderCommand is the command and arguments the placeholder's
// container runs.
func placeholderCommand(c *runpb.Container) (cmd, args []string) {
	port := int32(8080)
	if ps := c.GetPorts(); len(ps) > 0 && ps[0].GetContainerPort() > 0 {
		port = ps[0].GetContainerPort()
	}
	return []string{"/usr/local/bin/envoy"}, []string{"--config-yaml", placeholderEnvoy(port), "--log-level", "warn"}
}
