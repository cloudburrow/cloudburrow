package run

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Fields the adapter neither renders into the Knative Service nor refuses
// used to be dropped in silence (#581): `gcloud run deploy --ingress internal`
// deployed "successfully" and was reachable from everywhere. Every populated
// field below is named in the refusal instead; the ones that map cheaply
// (probes, working directory, labels, annotations, description) are rendered
// in ToKnative and read back in FromKnative.

// droppedFieldGaps names each populated field ToKnative does not render,
// beyond the ones Unsupported has always refused.
func droppedFieldGaps(svc *runpb.Service) []string {
	var gaps []string
	add := func(set bool, field, why string) {
		if set {
			gaps = append(gaps, field+": "+why)
		}
	}
	ingress := svc.GetIngress()
	add(ingress != runpb.IngressTraffic_INGRESS_TRAFFIC_UNSPECIFIED && ingress != runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
		"ingress", "only INGRESS_TRAFFIC_ALL is mapped; every service is reachable from the host and the cluster")
	add(len(svc.GetCustomAudiences()) > 0, "customAudiences", "no ID token is checked locally")
	s := svc.GetScaling()
	add(s.GetMinInstanceCount() > 0 || s.GetScalingMode() != runpb.ServiceScaling_SCALING_MODE_UNSPECIFIED || s.GetManualInstanceCount() > 0,
		"scaling", "service-level scaling is not mapped; set template.scaling")
	add(svc.GetDefaultUriDisabled(), "defaultUriDisabled", "not mapped")
	add(svc.GetIapEnabled(), "iapEnabled", "Identity-Aware Proxy does not exist locally")
	add(svc.GetMultiRegionSettings() != nil, "multiRegionSettings", "there is one region locally")
	add(svc.GetBuildConfig() != nil, "buildConfig", "source builds are not run by the Cloud Run adapter")
	add(svc.GetThreatDetectionEnabled(), "threatDetectionEnabled", "not mapped")

	t := svc.GetTemplate()
	add(t.GetRevision() != "", "template.revision", "revision names are chosen by Knative")
	add(t.GetServiceMesh() != nil, "template.serviceMesh", "no service mesh exists locally")
	add(t.GetHealthCheckDisabled(), "template.healthCheckDisabled", "not mapped")
	add(t.GetEncryptionKeyRevocationAction() != runpb.EncryptionKeyRevocationAction_ENCRYPTION_KEY_REVOCATION_ACTION_UNSPECIFIED ||
		t.GetEncryptionKeyShutdownDuration() != nil, "template.encryptionKeyRevocationAction", "customer-managed encryption keys are not supported")
	// template.nodeSelector and the container fields no pod renders are in
	// podTemplateGaps, shared with jobs.
	return gaps
}

// reservedPrefixes are annotation and label namespaces a caller may not set:
// the first four as Cloud Run v2 refuses them, cloudburrow.dev because the
// adapter's own bookkeeping lives there.
var reservedPrefixes = []string{"run.googleapis.com/", "cloud.googleapis.com/", "serving.knative.dev/",
	"autoscaling.knative.dev/", "cloudburrow.dev/"}

// validateMetadata refuses a label or annotation key in a reserved namespace.
func validateMetadata(field string, m map[string]string) error {
	for k := range m {
		for _, p := range reservedPrefixes {
			if strings.HasPrefix(k, p) {
				return apierror.InvalidArgument("%s key %q is in the reserved %s namespace", field, k, strings.TrimSuffix(p, "/"))
			}
		}
	}
	return nil
}

// The Cloud Run metadata a Knative object has no field for is kept, as
// JSON, in the adapter's own annotations, so it reads back exactly as set
// and never collides with a Kubernetes label rule or a Knative annotation.
const (
	annServiceLabels      = "cloudburrow.dev/cloud-run-labels"
	annServiceAnnotations = "cloudburrow.dev/cloud-run-annotations"
	annDescription        = "cloudburrow.dev/cloud-run-description"
	annTemplateLabels     = "cloudburrow.dev/revision-labels"
	annTemplateAnnotation = "cloudburrow.dev/revision-annotations"
	// annSecretEnv records which Secret Manager secret each secretKeyRef
	// env var came from, so it reads back as the ValueSource that was set
	// rather than as the Kubernetes Secret it resolved to.
	annSecretEnv = "cloudburrow.dev/secret-env"
	// annInjectedEnv records the variables CloudBurrow injected into a
	// revision (#576), by name and value, so the Cloud Run API can report
	// only the caller's env and the console's editor can leave them out.
	annInjectedEnv = "cloudburrow.dev/injected-env"
)

// jsonAnnotation renders one annotation line with a JSON value, or nothing
// for an empty map.
func jsonAnnotation(b *strings.Builder, indent, key string, v any, empty bool) {
	if empty {
		return
	}
	raw, _ := json.Marshal(v)
	fmt.Fprintf(b, "%s%s: %q\n", indent, key, string(raw))
}

// readJSONAnnotation decodes an annotation written by jsonAnnotation.
func readJSONAnnotation(ann map[string]string, key string, into any) {
	if raw := ann[key]; raw != "" {
		_ = json.Unmarshal([]byte(raw), into)
	}
}

// secretEnvRef is one entry of annSecretEnv.
type secretEnvRef struct {
	Secret  string `json:"secret"`
	Version string `json:"version,omitempty"`
}

// renderProbe writes a Knative container probe for a Cloud Run probe.
// Knative requires a probe to target the serving port, so a probe on any
// other port is refused rather than rendered onto the wrong one.
func renderProbe(b *strings.Builder, kind string, p *runpb.Probe, containerPort int32) error {
	if p == nil {
		return nil
	}
	serving := containerPort
	if serving == 0 {
		serving = 8080 // Cloud Run's and Knative's default
	}
	port := func(n int32) error {
		if n != 0 && n != serving {
			return apierror.Unimplemented("container.%s port %d: a probe on a port other than the container port %d is not mapped", kind, n, serving)
		}
		return nil
	}
	// The port is always written: Knative fills in the serving port for a
	// liveness probe (rewriteUserLivenessProbe) but not for a startup
	// probe, and a startup probe with no port probes port 0 until the
	// revision gives up. CI measured exactly that (#581).
	fmt.Fprintf(b, "          %s:\n", kind)
	switch {
	case p.GetHttpGet() != nil:
		h := p.GetHttpGet()
		if err := port(h.GetPort()); err != nil {
			return err
		}
		b.WriteString("            httpGet:\n")
		fmt.Fprintf(b, "              port: %d\n", serving)
		path := h.GetPath()
		if path == "" {
			path = "/"
		}
		fmt.Fprintf(b, "              path: %q\n", path)
		if hs := h.GetHttpHeaders(); len(hs) > 0 {
			b.WriteString("              httpHeaders:\n")
			for _, hh := range hs {
				fmt.Fprintf(b, "                - name: %q\n                  value: %q\n", hh.GetName(), hh.GetValue())
			}
		}
	case p.GetTcpSocket() != nil:
		if err := port(p.GetTcpSocket().GetPort()); err != nil {
			return err
		}
		fmt.Fprintf(b, "            tcpSocket:\n              port: %d\n", serving)
	case p.GetGrpc() != nil:
		g := p.GetGrpc()
		if err := port(g.GetPort()); err != nil {
			return err
		}
		b.WriteString("            grpc:\n")
		fmt.Fprintf(b, "              port: %d\n", serving)
		if svc := g.GetService(); svc != "" {
			fmt.Fprintf(b, "              service: %q\n", svc)
		}
	default:
		return apierror.InvalidArgument("container.%s must set httpGet, tcpSocket or grpc", kind)
	}
	for _, f := range []struct {
		name string
		v    int32
	}{{"initialDelaySeconds", p.GetInitialDelaySeconds()}, {"timeoutSeconds", p.GetTimeoutSeconds()},
		{"periodSeconds", p.GetPeriodSeconds()}, {"failureThreshold", p.GetFailureThreshold()}} {
		if f.v > 0 {
			fmt.Fprintf(b, "            %s: %d\n", f.name, f.v)
		}
	}
	return nil
}

// kprobe is the subset of a Kubernetes probe read back from Knative.
type kprobe struct {
	HTTPGet *struct {
		Path        string `json:"path"`
		HTTPHeaders []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"httpHeaders"`
	} `json:"httpGet"`
	TCPSocket *struct{} `json:"tcpSocket"`
	GRPC      *struct {
		Service *string `json:"service"`
	} `json:"grpc"`
	InitialDelaySeconds int32 `json:"initialDelaySeconds"`
	TimeoutSeconds      int32 `json:"timeoutSeconds"`
	PeriodSeconds       int32 `json:"periodSeconds"`
	FailureThreshold    int32 `json:"failureThreshold"`
}

// toProbe renders a read-back Kubernetes probe as the Cloud Run probe.
func (k *kprobe) toProbe() *runpb.Probe {
	if k == nil {
		return nil
	}
	p := &runpb.Probe{InitialDelaySeconds: k.InitialDelaySeconds, TimeoutSeconds: k.TimeoutSeconds,
		PeriodSeconds: k.PeriodSeconds, FailureThreshold: k.FailureThreshold}
	switch {
	case k.HTTPGet != nil:
		h := &runpb.HTTPGetAction{Path: k.HTTPGet.Path}
		for _, hh := range k.HTTPGet.HTTPHeaders {
			h.HttpHeaders = append(h.HttpHeaders, &runpb.HTTPHeader{Name: hh.Name, Value: hh.Value})
		}
		p.ProbeType = &runpb.Probe_HttpGet{HttpGet: h}
	case k.TCPSocket != nil:
		p.ProbeType = &runpb.Probe_TcpSocket{TcpSocket: &runpb.TCPSocketAction{}}
	case k.GRPC != nil:
		g := &runpb.GRPCAction{}
		if k.GRPC.Service != nil {
			g.Service = *k.GRPC.Service
		}
		p.ProbeType = &runpb.Probe_Grpc{Grpc: g}
	}
	return p
}

// sortedKeys returns a map's keys in order, for deterministic manifests.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
