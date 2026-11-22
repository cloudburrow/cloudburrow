package main

// Create pipeline as Google's one page (#2098): Pipeline details, then Add
// an enrollment (each enrollment's name, bus and CEL expression, made with
// the pipeline), Event mediation and Destination, whose types include
// Cloud Run service (via HTTP), a service of the project picked by name,
// and Cloud Run job (via HTTP), a job run through the Cloud Run Admin API's
// jobs.run with a message binding and an OAuth token, as
// https://docs.cloud.google.com/eventarc/advanced/docs/receive-events/create-enrollment
// describes them (read 2026-11-21). Both are HTTP endpoints in the API:
// the service's URL and https://run.googleapis.com/v2/{job}:run, which this
// instance serves in process (internal/adapter/run), never Google.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// eaWithout is values without keys.
func eaWithout(values map[string]string, keys ...string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// eaFormAuth is Enable authentication's config, nil when it is off. A
// form with no checkbox (an API caller) and a service account means a
// Google OIDC token, as the page offered before #2098.
func eaFormAuth(values map[string]string) (*eventarcpb.Pipeline_Destination_AuthenticationConfig, error) {
	sa := strings.TrimSpace(values["serviceAccount"])
	on, set := values["authEnabled"]
	if set && on != "true" || !set && sa == "" {
		return nil, nil
	}
	if sa == "" {
		return nil, errors.New("authentication needs the service account that invokes the destination")
	}
	if strings.TrimSpace(values["authType"]) == eaAuthOAuth {
		return &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OauthToken{
			OauthToken: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OAuthToken{ServiceAccount: sa, Scope: strings.TrimSpace(values["scope"])}}}, nil
	}
	return &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_GoogleOidc{
		GoogleOidc: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OidcToken{ServiceAccount: sa, Audience: strings.TrimSpace(values["audience"])}}}, nil
}

// eaFormPipelineDestination is the form's destination. A Cloud Run service's URI
// is its URL, which the caller has put in values["uri"] (resolveRun).
func eaFormPipelineDestination(project, region string, values map[string]string) (*eventarcpb.Pipeline_Destination, error) {
	d := &eventarcpb.Pipeline_Destination{}
	http := func(uri string) error {
		if uri == "" {
			return errors.New("an HTTP endpoint destination needs its URI")
		}
		he := &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: uri, MessageBindingTemplate: strings.TrimSpace(values["messageBinding"])}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: he}
		var err error
		d.AuthenticationConfig, err = eaFormAuth(values)
		return err
	}
	switch kind := strings.TrimSpace(values["destination"]); kind {
	case "", eaDestHTTP:
		if err := http(strings.TrimSpace(values["uri"])); err != nil {
			return nil, err
		}
	case eaDestService:
		if strings.TrimSpace(values["runService"]) == "" {
			return nil, errors.New("a Cloud Run service destination needs its service")
		}
		if err := http(strings.TrimSpace(values["uri"])); err != nil {
			return nil, err
		}
	case eaDestJob:
		job := eaFullName(project, region, "jobs", values["runJob"])
		if job == "" {
			return nil, errors.New("a Cloud Run job destination needs its job")
		}
		if strings.TrimSpace(values["messageBinding"]) == "" {
			return nil, fmt.Errorf("a Cloud Run job destination needs a message binding that builds the Cloud Run Admin API's RunJobRequest, such as %s", eaJobBindingExample)
		}
		if err := http(runadapter.EventarcRunJobURI(job)); err != nil {
			return nil, err
		}
	case eaDestTopic:
		t := strings.TrimSpace(values["topic"])
		if t == "" {
			return nil, errors.New("a Pub/Sub topic destination needs its topic")
		}
		if !strings.HasPrefix(t, "projects/") {
			t = "projects/" + project + "/topics/" + t
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_Topic{Topic: t}
	case eaDestBus, "Message bus":
		b := eaFullName(project, region, "messageBuses", values["bus"])
		if b == "" {
			return nil, errors.New("a message bus destination needs its bus")
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_MessageBus{MessageBus: b}
	case eaDestWorkflw:
		w := eaFullName(project, region, "workflows", values["workflow"])
		if w == "" {
			return nil, errors.New("a Workflows destination needs its workflow")
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_Workflow{Workflow: w}
	default:
		return nil, fmt.Errorf("destination %q is not one of %v", kind, eaPipelineDestinations)
	}
	if na := strings.TrimSpace(values["networkAttachment"]); na != "" && d.GetHttpEndpoint() != nil {
		d.NetworkConfig = &eventarcpb.Pipeline_Destination_NetworkConfig{NetworkAttachment: na}
	}
	return d, nil
}

// resolveRun puts a Cloud Run service destination's URI in the values:
// the service's URL, read from Cloud Run, and the relative path.
func (p eaPipelinesProvider) resolveRun(ctx context.Context, project string, values map[string]string) (map[string]string, error) {
	if strings.TrimSpace(values["destination"]) != eaDestService {
		return values, nil
	}
	name := strings.TrimSpace(values["runService"])
	uri, ok := p.runChoices(ctx, project).services[name]
	if name == "" || !ok || uri == "" {
		return nil, fmt.Errorf("Cloud Run service %q is not a service of the project in %s with a URL", name, p.run.location())
	}
	path := strings.TrimSpace(values["urlPath"])
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	out := eaWithout(values)
	out["uri"] = strings.TrimSuffix(uri, "/") + path
	return out, nil
}

// eaFormEnrollments are Add an enrollment's rows, checked before anything
// is made.
func eaFormEnrollments(project, region, pipeline string, values map[string]string) (map[string]*eventarcpb.Enrollment, []string, error) {
	rows, err := console.ParseGroup(values["enrollments"])
	if err != nil {
		return nil, nil, fmt.Errorf("enrollments: %w", err)
	}
	out := map[string]*eventarcpb.Enrollment{}
	var order []string
	for _, r := range rows {
		id := strings.TrimSpace(r["name"])
		if !eaIDRE.MatchString(id) {
			return nil, nil, fmt.Errorf("enrollment name %q: lowercase letters, digits and hyphens, starting with a letter, at most 63", id)
		}
		if _, dup := out[id]; dup {
			return nil, nil, fmt.Errorf("enrollment %s is added twice", id)
		}
		if strings.TrimSpace(r["bus"]) == "" {
			return nil, nil, fmt.Errorf("enrollment %s needs its message bus", id)
		}
		match := strings.TrimSpace(r["celMatch"])
		if match == "" {
			match = "true"
		}
		en, err := eaFormEnrollment(project, region, map[string]string{"bus": r["bus"], "celMatch": match, "pipeline": pipeline})
		if err != nil {
			return nil, nil, fmt.Errorf("enrollment %s: %w", id, err)
		}
		out[id] = en
		order = append(order, id)
	}
	return out, order, nil
}
