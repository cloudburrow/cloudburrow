package marketplace

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A Docker Hub image's Configure parameters and plan: a Deployment of the
// tag, a Service on its ports (the image's EXPOSE ports by default) and the
// Application recording both, as a Marketplace Kubernetes app is.

// Service types, as the Workloads page's Expose names them.
var serviceTypes = map[string]string{"Cluster IP": "ClusterIP", "Node port": "NodePort", "Load balancer": "LoadBalancer"}

// Parameters are the image's Configure fields for a tag, defaulted from its
// config.
func (h *DockerHub) Parameters(ctx context.Context, id, version string) ([]Param, error) {
	version = or(version, "latest")
	cfg, err := h.Config(ctx, id, version)
	if err != nil {
		return nil, err
	}
	ports := strings.Join(cfg.ExposedPorts, ", ")
	portHelp := "The container's ports, as port/protocol, separated by commas; the Service exposes each. "
	if len(cfg.ExposedPorts) > 0 {
		portHelp += "From the image's EXPOSE: " + ports + "."
	} else {
		portHelp += "The image declares none (no EXPOSE); leave it empty for no Service."
	}
	envHelp := "NAME=value, one per line, added to the image's own environment."
	if len(cfg.Env) > 0 {
		envHelp += " The image sets " + strings.Join(envNames(cfg.Env), ", ") + "."
	}
	return []Param{
		{Name: "tag", Label: "Image tag", Type: "text", Default: version, Required: true, Pattern: tagPattern.String(),
			Help: "The tag the cluster pulls."},
		{Name: "replicas", Label: "Replicas", Type: "number", Default: "1", Required: true, Pattern: `^[0-9]+$`,
			Help: "The number of pods the Deployment runs."},
		{Name: "ports", Label: "Container ports", Type: "text", Default: ports, Pattern: `^\s*([0-9]{1,5}(/(tcp|udp|sctp|TCP|UDP|SCTP))?\s*(,\s*|$))*$`, Help: portHelp},
		{Name: "serviceType", Label: "Service type", Type: "select", Options: []string{"Cluster IP", "Node port", "Load balancer"}, Default: "Cluster IP",
			Help: "A load balancer's external IP is the kind cluster's own: reachable from this machine through `kubectl port-forward`."},
		{Name: "env", Label: "Environment variables", Type: "textarea", Help: envHelp},
		{Name: "args", Label: "Arguments", Type: "text", Help: "Arguments passed to the image's entrypoint, separated by spaces; empty runs its own command."},
		{Name: "cpuRequest", Label: "CPU request", Type: "text", Default: "100m", Pattern: quantity, Section: "Resources"},
		{Name: "memoryRequest", Label: "Memory request", Type: "text", Default: "128Mi", Pattern: quantity, Section: "Resources"},
		{Name: "cpuLimit", Label: "CPU limit", Type: "text", Pattern: quantity, Section: "Resources"},
		{Name: "memoryLimit", Label: "Memory limit", Type: "text", Pattern: quantity, Section: "Resources"},
	}, nil
}

const quantity = `^([0-9]+(\.[0-9]+)?(m|Ki|Mi|Gi|Ti|k|M|G|T)?)?$`

var quantityRe = regexp.MustCompile(quantity)

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		n, _, _ := strings.Cut(e, "=")
		out = append(out, n)
	}
	return out
}

// Port is one container port.
type Port struct {
	Number   int
	Protocol string
}

// ParsePorts reads "80/tcp, 443" into ports.
func ParsePorts(s string) ([]Port, error) {
	var out []Port
	seen := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		num, proto, _ := strings.Cut(f, "/")
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: port %q is not a number from 1 to 65535", ErrBadRequest, f)
		}
		proto = strings.ToUpper(or(strings.TrimSpace(proto), "TCP"))
		if proto != "TCP" && proto != "UDP" && proto != "SCTP" {
			return nil, fmt.Errorf("%w: port %q: the protocol is TCP, UDP or SCTP", ErrBadRequest, f)
		}
		k := strconv.Itoa(n) + "/" + proto
		if !seen[k] {
			seen[k] = true
			out = append(out, Port{Number: n, Protocol: proto})
		}
	}
	return out, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// ParseEnv reads NAME=value lines.
func ParseEnv(s string) ([]map[string]any, error) {
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		n, v, ok := strings.Cut(line, "=")
		n = strings.TrimSpace(n)
		if !ok || !envName.MatchString(n) {
			return nil, fmt.Errorf("%w: environment variable %q is not NAME=value", ErrBadRequest, line)
		}
		out = append(out, map[string]any{"name": n, "value": v})
	}
	return out, nil
}

// Plan is the image's Deployment and Service.
func (h *DockerHub) Plan(_ context.Context, id, version string, app AppMeta, values map[string]string) (Plan, error) {
	ns, repo, err := splitRepo(id)
	if err != nil {
		return Plan{}, err
	}
	tag := strings.TrimSpace(or(values["tag"], or(version, "latest")))
	if !tagPattern.MatchString(tag) {
		return Plan{}, fmt.Errorf("%w: image tag %q is not a tag", ErrBadRequest, tag)
	}
	image := "docker.io/" + ns + "/" + repo + ":" + tag
	replicas := 1
	if s := strings.TrimSpace(values["replicas"]); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 100 {
			return Plan{}, fmt.Errorf("%w: replicas %q: a whole number from 0 to 100", ErrBadRequest, s)
		}
		replicas = n
	}
	ports, err := ParsePorts(values["ports"])
	if err != nil {
		return Plan{}, err
	}
	env, err := ParseEnv(values["env"])
	if err != nil {
		return Plan{}, err
	}
	svcType := "ClusterIP"
	if t := values["serviceType"]; t != "" {
		k, ok := serviceTypes[t]
		if !ok {
			return Plan{}, fmt.Errorf("%w: service type %q is Cluster IP, Node port or Load balancer", ErrBadRequest, t)
		}
		svcType = k
	}
	resources := map[string]any{}
	for _, r := range []struct{ key, kind, res string }{
		{"cpuRequest", "requests", "cpu"}, {"memoryRequest", "requests", "memory"},
		{"cpuLimit", "limits", "cpu"}, {"memoryLimit", "limits", "memory"},
	} {
		v := strings.TrimSpace(values[r.key])
		if v == "" {
			continue
		}
		if !quantityRe.MatchString(v) {
			return Plan{}, fmt.Errorf("%w: %s %q is not a Kubernetes quantity", ErrBadRequest, r.res, v)
		}
		m, _ := resources[r.kind].(map[string]any)
		if m == nil {
			m = map[string]any{}
			resources[r.kind] = m
		}
		m[r.res] = v
	}
	labels := AppLabels(app.Name)
	container := map[string]any{"name": containerName(repo), "image": image}
	if len(env) > 0 {
		container["env"] = env
	}
	if args := strings.Fields(values["args"]); len(args) > 0 {
		container["args"] = args
	}
	if len(resources) > 0 {
		container["resources"] = resources
	}
	var cports []map[string]any
	for _, p := range ports {
		cports = append(cports, map[string]any{"containerPort": p.Number, "protocol": p.Protocol})
	}
	if len(cports) > 0 {
		container["ports"] = cports
	}
	deployment := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": app.Name, "namespace": app.Namespace, "labels": labels},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     map[string]any{"containers": []any{container}},
			},
		},
	}
	plan := Plan{Manifests: []map[string]any{deployment}, Kinds: []GroupKind{{Group: "apps", Kind: "Deployment"}},
		Descriptor: Descriptor{Type: repo, Version: tag, Description: "",
			Links: []AppLink{{Description: "Docker Hub", URL: hubPage(ns, repo)}}, Keywords: []string{"docker-hub"}},
		Annotations: map[string]string{AnnotationSource: DockerHubID, AnnotationProduct: ns + "/" + repo, AnnotationImage: image}}
	if len(ports) > 0 {
		var sports []map[string]any
		for _, p := range ports {
			sports = append(sports, map[string]any{"name": strings.ToLower(p.Protocol) + "-" + strconv.Itoa(p.Number),
				"port": p.Number, "targetPort": p.Number, "protocol": p.Protocol})
		}
		svc := map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": app.Name, "namespace": app.Namespace, "labels": labels},
			"spec":     map[string]any{"type": svcType, "selector": labels, "ports": sports},
		}
		plan.Manifests = append(plan.Manifests, svc)
		plan.Kinds = append(plan.Kinds, GroupKind{Group: "", Kind: "Service"})
	}
	return plan, nil
}

func hubPage(ns, repo string) string {
	if ns == "library" {
		return "https://hub.docker.com/_/" + repo
	}
	return "https://hub.docker.com/r/" + ns + "/" + repo
}

// containerName is a DNS label from the repository name.
func containerName(repo string) string {
	s := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(repo))
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return or(s, "app")
}
