package main

// Cloud Run: "Edit and deploy new revision" (#595).
//
// The service page was read-only, so changing one environment variable meant
// deploying again and retyping the name, image, port, limits, scaling and
// every other variable. The adapter's UpdateService is verified with the
// official client, and the console already had an Editor interface; this is
// the Run provider using both.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runEditLabel is the edit button's label.
//
// It is not claimed to be Google's: the Cloud Run documentation of 2026-09-24
// describes editing on the service page's own tabs and deploying with "View
// diff & redeploy". docs/console-parity.md §1 records the difference.
const runEditLabel = "Edit and deploy new revision"

// runNameImmutable is the service name field's help and, word for word, the
// refusal a submitted change to it gets. One sentence, so the reason a
// request was refused is the reason the form already gave.
const runNameImmutable = "A service name cannot be changed after it is deployed."

// editForm is the service's deploy form, prefilled from the revision that is
// serving.
//
// The serving revision rather than the service's latest template: after a
// failed rollout the latest template is the configuration that failed, and
// prefilling from it would offer the broken image as the starting point for
// the fix. With nothing serving yet, the latest configuration is all there is,
// and the note says which one was used.
func (p runProvider) editForm(ctx context.Context, service string, svc *ksvcStatus) *console.EditForm {
	if p.runEndpoint() == "" {
		// No adapter, no update path: a button here could only fail.
		return nil
	}
	tmpl := svc.Spec.Template
	source := "Prefilled from the service's latest configuration: no revision is serving yet."
	if serving := svc.Status.LatestReadyRevisionName; serving != "" {
		rev, err := p.revisionTemplate(ctx, service, serving)
		if err == nil {
			tmpl, source = rev, "Prefilled from the serving revision, "+serving+"."
		} else {
			source = "Prefilled from the service's latest configuration: the serving revision " +
				serving + " could not be read (" + err.Error() + ")."
		}
	}
	form := runEditForm(service, tmpl, source)
	if form != nil {
		// A service's labels are the service's, not a revision's, so they
		// are read from the Knative Service rather than the template.
		labels := map[string]string{}
		if raw := svc.Metadata.Annotations[runadapter.ServiceLabelsAnnotation]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &labels)
		}
		for i := range form.Fields {
			if form.Fields[i].Name == "labels" {
				form.Fields[i].Default = console.FormatMap(labels)
			}
		}
	}
	return form
}

// revisionTemplate reads one Knative revision of a service.
func (p runProvider) revisionTemplate(ctx context.Context, service, revision string) (knTemplate, error) {
	raw, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "revisions")
	if err != nil {
		return knTemplate{}, err
	}
	var list struct {
		Items []knTemplate `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return knTemplate{}, fmt.Errorf("decode revisions: %w", err)
	}
	for _, item := range list.Items {
		if item.Metadata.Name == revision && item.Metadata.Labels["serving.knative.dev/service"] == service {
			return item, nil
		}
	}
	return knTemplate{}, fmt.Errorf("no revision named %s", revision)
}

// runEditForm builds the edit form from a revision template.
//
// The fields are the deploy form's, so the two cannot offer different things:
// what the adapter maps is offered, and nothing it refuses. Nil when the
// template has more than one container, because the form describes one and
// submitting it would have to guess which of several it meant. Nil as well when
// an entrypoint or argument holds whitespace: the form's fields are split on
// spaces, so `sh -c "echo hi"` would come back as four arguments and deploy a
// different command from the one that was serving.
func runEditForm(service string, tmpl knTemplate, source string) *console.EditForm {
	if len(tmpl.Spec.Containers) != 1 {
		return nil
	}
	c := tmpl.Spec.Containers[0]
	for _, word := range append(append([]string(nil), c.Command...), c.Args...) {
		if word == "" || strings.ContainsAny(word, " \t\n") {
			return nil
		}
	}

	// The variables CloudBurrow injected (#576) are not the service's own:
	// they are injected again on every deploy, so the form leaves them out.
	injected := map[string]string{}
	if raw := tmpl.Metadata.Annotations[runadapter.InjectedEnvAnnotation]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &injected)
	}
	// Which Secret Manager secret each secret-backed variable names, as the
	// adapter recorded it; the Knative object holds only the Kubernetes
	// Secret it resolved to.
	var refs map[string]struct {
		Secret  string `json:"secret"`
		Version string `json:"version"`
	}
	if raw := tmpl.Metadata.Annotations[runadapter.SecretEnvAnnotation]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &refs)
	}
	plain, secret := map[string]string{}, map[string]string{}
	var fromSecrets []string
	for _, e := range c.Env {
		if v, ok := injected[e.Name]; ok && v == e.Value && e.ValueFrom.SecretKeyRef.Name == "" {
			continue
		}
		if e.ValueFrom.SecretKeyRef.Name != "" {
			if ref, ok := refs[e.Name]; ok && ref.Secret != "" {
				secret[e.Name] = formatRunSecretRef(ref.Secret, ref.Version)
			} else {
				fromSecrets = append(fromSecrets, e.Name)
			}
			continue
		}
		plain[e.Name] = e.Value
	}
	port := ""
	for _, p := range c.Ports {
		if p.ContainerPort != 0 {
			port = fmt.Sprint(p.ContainerPort)
			break
		}
	}
	positive := func(n int) string {
		if n <= 0 {
			return ""
		}
		return fmt.Sprint(n)
	}
	defaults := map[string]string{
		"name":         service,
		"image":        c.Image,
		"port":         port,
		"command":      strings.Join(c.Command, " "),
		"args":         strings.Join(c.Args, " "),
		"env":          console.FormatMap(plain),
		"secretEnv":    console.FormatMap(secret),
		"cpu":          c.Resources.Limits["cpu"],
		"memory":       c.Resources.Limits["memory"],
		"minInstances": tmpl.Metadata.Annotations["autoscaling.knative.dev/min-scale"],
		"maxInstances": tmpl.Metadata.Annotations["autoscaling.knative.dev/max-scale"],
		"concurrency":  positive(tmpl.Spec.ContainerConcurrency),
		"timeout":      positive(tmpl.Spec.TimeoutSeconds),
	}

	_, fields := runProvider{}.CreateForm()
	for i := range fields {
		f := &fields[i]
		f.Default = defaults[f.Name]
		if f.Name == "name" {
			f.Immutable = true
			f.Help = runNameImmutable
			// Identity, not input: a pattern on a disabled field is a
			// constraint nothing applies.
			f.Pattern = ""
		}
	}

	note := source + " Deploying creates a new revision; traffic moves to it once it is ready, " +
		"and a revision that fails leaves the serving one in place. Annotations, " +
		"probes and the working directory are kept as they are. CloudBurrow's own endpoint variables " +
		"are given to every revision and are not listed here."
	if len(fromSecrets) > 0 {
		note += " Environment variables from a secret this instance has no record of (" + strings.Join(fromSecrets, ", ") +
			") cannot be shown, so the form cannot be deployed while they are there."
	}
	return &console.EditForm{Label: runEditLabel, Fields: fields, Note: note}
}

// Edit implements console.Editor: the form's values, deployed as a new
// revision through the adapter's UpdateService.
//
// The service is read back through the API first and only the form's fields
// are replaced on it. UpdateService replaces the whole configuration, so
// sending the form alone would silently drop what the form does not show —
// annotations, probes, the working directory.
func (p runProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a service can be edited: a revision is immutable")
	}
	service := path[0]
	if v, ok := values["name"]; ok && strings.TrimSpace(v) != service {
		return errors.New(runNameImmutable)
	}
	if p.runEndpoint() == "" {
		return fmt.Errorf("the Cloud Run adapter is not running")
	}
	if project == "" {
		project = p.defaultProject
	}
	if project == "" {
		return fmt.Errorf("choose a project before deploying a service")
	}

	form, err := runFormTemplate(values)
	if err != nil {
		return err
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	name := service
	if !strings.HasPrefix(name, "projects/") {
		name = fmt.Sprintf("projects/%s/locations/%s/services/%s", project, p.location(), service)
	}
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		return err
	}
	_, sentSecrets := values["secretEnv"]
	if err := applyRunForm(svc, form, !sentSecrets); err != nil {
		return err
	}
	labels, ok, err := runLabels(values)
	if err != nil {
		return err
	}
	if ok {
		svc.Labels = labels
	}
	// No etag. The adapter's is the Knative resourceVersion, which Knative's
	// own status writes move while a revision settles, so one read a moment
	// ago would be refused as stale for a change nobody else made.
	svc.Etag = ""

	op, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc})
	if err != nil {
		return err
	}
	// Waited on, as Create waits: an edit reported applied whose revision
	// then never serves is the failure the console exists to show.
	if _, err := op.Wait(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
			return fmt.Errorf("the new revision was not ready when the console stopped waiting; " +
				"it may still become ready, so check the revision history")
		}
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
			return status.Errorf(st.Code(), "the new revision did not become ready: %s", st.Message())
		}
		return fmt.Errorf("the new revision did not become ready: %w", err)
	}
	return nil
}

// applyRunForm replaces the form's fields on a service read from the API and
// leaves everything else as it was read. keepSecrets is a request without the
// secretEnv field, whose secret-backed variables are kept.
func applyRunForm(svc *runpb.Service, form *runpb.RevisionTemplate, keepSecrets bool) error {
	tmpl := svc.GetTemplate()
	if tmpl == nil || len(tmpl.GetContainers()) != 1 {
		return fmt.Errorf("this service runs %d containers, and the form edits exactly one",
			len(tmpl.GetContainers()))
	}
	cur, next := tmpl.Containers[0], form.Containers[0]

	cur.Image = next.GetImage()
	cur.Command = next.GetCommand()
	cur.Args = next.GetArgs()
	cur.Ports = next.GetPorts()
	if cur.Resources == nil {
		cur.Resources = next.GetResources()
	} else {
		cur.Resources.Limits = next.GetResources().GetLimits()
	}

	// The variables are the form's, plain and secret-backed: both are on it,
	// prefilled, so one removed there is removed.
	if err := formEnv(cur, next.GetEnv(), keepSecrets, "redeploy it through the Cloud Run API"); err != nil {
		return err
	}

	tmpl.Scaling = form.GetScaling()
	tmpl.MaxInstanceRequestConcurrency = form.GetMaxInstanceRequestConcurrency()
	tmpl.Timeout = form.GetTimeout()
	return nil
}

var _ console.Editor = runProvider{}
