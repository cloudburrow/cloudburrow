package main

// Cloud Run functions console parity, round 4 (#2049), as Google's deploy
// page walks it (https://docs.cloud.google.com/run/docs/deploy-functions,
// read 2026-11-20):
//
//   - Write a function's Create makes the service "using a placeholder
//     revision" through the Cloud Run API: Google's placeholder image, with
//     every setting of the form (Container(s), Variables & Secrets,
//     Volumes, Networking, Security), and Run's build_config naming the
//     function's entry point, base image and the uploaded source. It lands
//     on the service's Source tab, whose Save and redeploy deploys the
//     source: CreateFunction through the Cloud Functions v2 API, which
//     builds it and deploys it onto the same service, keeping what the
//     form set there.
//   - Add trigger is a checkbox revealing Google's Eventarc trigger pane
//     (consolefunctionseventarc.go): the trigger is created on the service
//     once it exists.
//   - The Source tab's Base image, with Edit runtime and security updates:
//     the runtime and the v2 API's runtime update policy (automatic, the
//     default, or on deploy), saved with Save and redeploy.
//   - Deployed by is the service's last_modifier, which the Run API
//     records.
//   - The Testing tab's Run in Cloud Shell runs the test command in the
//     console's terminal (#781).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	functionsEditRuntimeAction = "edit-runtime"
	functionsCloudShellAction  = "run-in-cloud-shell"

	// writeFunctionAddTrigger is Write a function's Add trigger.
	writeFunctionAddTrigger = "addTrigger"
	// writeFunctionTrigger prefixes the Eventarc pane's fields on Write a
	// function, whose own name, region and service account they would
	// otherwise be.
	writeFunctionTrigger = "trigger."

	runtimeUpdateAutomatic = "Automatic"
	runtimeUpdateOnDeploy  = "On deploy"

	// placeholderSecretVolume names the secret volumes Write a function
	// mounts on the placeholder's service: Cloud Run's, kept by the
	// function's deploys.
	placeholderSecretVolume = "secret-"
)

// ---- Deployed by ----

// runLastModifiers are, by service ID, the Run API's last_modifier of
// project's services.
func (p runProvider) runLastModifiers(ctx context.Context, project string) map[string]string {
	out := map[string]string{}
	if p.runEndpoint() == "" {
		return out
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return out
	}
	defer func() { _ = c.Close() }()
	return lastModifiersOf(c.ListServices(ctx, &runpb.ListServicesRequest{
		Parent: "projects/" + project + "/locations/" + p.location()}).Next)
}

// lastModifiersOf drains a ListServices iterator; an error ends it.
func lastModifiersOf(next func() (*runpb.Service, error)) map[string]string {
	out := map[string]string{}
	for {
		s, err := next()
		if err != nil {
			return out
		}
		by := s.GetLastModifier()
		if by == "" {
			by = s.GetCreator()
		}
		out[lastSegment(s.GetName())] = by
	}
}

// functionDeployers are, by function name, the last_modifier of each
// function's Cloud Run service, read through the Run API.
func (p functionsProvider) functionDeployers(ctx context.Context, fns []*functionspb.Function) map[string]string {
	out := map[string]string{}
	opts, err := p.clientOptions()
	if err != nil {
		return out
	}
	c, err := runclient.NewServicesClient(ctx, opts...)
	if err != nil {
		return out
	}
	defer func() { _ = c.Close() }()
	for _, fn := range fns {
		svc := fn.GetServiceConfig().GetService()
		if svc == "" {
			continue
		}
		s, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: svc})
		if err != nil {
			continue
		}
		by := s.GetLastModifier()
		if by == "" {
			by = s.GetCreator()
		}
		out[fn.GetName()] = by
	}
	return out
}

// ---- Base image ----

// runtimeUpdateLabel is a function's runtime update policy in the
// console's words.
func runtimeUpdateLabel(bc *functionspb.BuildConfig) string {
	if bc.GetOnDeployUpdatePolicy() != nil {
		return runtimeUpdateOnDeploy
	}
	return runtimeUpdateAutomatic
}

// baseImageNote is the Source tab's Base image line.
func baseImageNote(fn *functionspb.Function) string {
	bc := fn.GetBuildConfig()
	s := " Base image: " + runadapter.BaseImage(functionRegion(fn), bc.GetRuntime()) + ", security updates " +
		strings.ToLower(runtimeUpdateLabel(bc)) + "."
	if v := bc.GetOnDeployUpdatePolicy().GetRuntimeVersion(); v != "" {
		s += " Runtime version: " + v + "."
	}
	return s
}

func functionRegion(fn *functionspb.Function) string {
	if parts := strings.Split(fn.GetName(), "/"); len(parts) > 3 {
		return parts[3]
	}
	return ""
}

// editRuntimeAction is Edit runtime and security updates, next to Base
// image: a runtime, and the runtime update policy, saved and redeployed.
func editRuntimeAction(fn *functionspb.Function) (console.Action, bool) {
	if fn.GetState() == functionspb.Function_DEPLOYING || fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		return console.Action{}, false
	}
	runtimes := make([]string, 0, len(runadapter.Runtimes))
	labels := map[string]string{}
	for _, r := range runadapter.Runtimes {
		runtimes = append(runtimes, r.Name)
		labels[r.Name] = r.Display
	}
	bc := fn.GetBuildConfig()
	return console.Action{ID: functionsEditRuntimeAction, Label: "Edit runtime and security updates", Submit: "Save and redeploy",
		Fields: []console.Field{
			{Name: "runtime", Label: "Runtime", Type: "select", Options: runtimes, OptionLabels: labels, Default: bc.GetRuntime(), Required: true,
				Help: "The language runtime and the base image the function is built on."},
			{Name: "updatePolicy", Label: "Security updates", Type: "radio", Options: []string{runtimeUpdateAutomatic, runtimeUpdateOnDeploy},
				Default: runtimeUpdateLabel(bc),
				Help: "Automatic: the base image is updated with security patches as they are released, the default. " +
					"On deploy: patches are applied only when the function is deployed (the v2 API's on_deploy_update_policy). " +
					"Locally the base image is the builder's, so no patch is ever applied in between."},
		}}, true
}

// editRuntime is its submit: UpdateFunction with the runtime and policy.
func (p functionsProvider) editRuntime(ctx context.Context, fn *functionspb.Function, v map[string]string) error {
	runtime := strings.TrimSpace(v["runtime"])
	if runtime == "" {
		return errors.New("a runtime is required")
	}
	bc := &functionspb.BuildConfig{Runtime: runtime}
	paths := []string{"build_config.runtime"}
	switch v["updatePolicy"] {
	case "", runtimeUpdateAutomatic:
		bc.RuntimeUpdatePolicy = &functionspb.BuildConfig_AutomaticUpdatePolicy{AutomaticUpdatePolicy: &functionspb.AutomaticUpdatePolicy{}}
		paths = append(paths, "build_config.automatic_update_policy")
	case runtimeUpdateOnDeploy:
		bc.RuntimeUpdatePolicy = &functionspb.BuildConfig_OnDeployUpdatePolicy{OnDeployUpdatePolicy: &functionspb.OnDeployUpdatePolicy{}}
		paths = append(paths, "build_config.on_deploy_update_policy")
	default:
		return fmt.Errorf("security updates %q: %s or %s", v["updatePolicy"], runtimeUpdateAutomatic, runtimeUpdateOnDeploy)
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		_, err := c.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{Function: &functionspb.Function{Name: fn.GetName(), BuildConfig: bc},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		return err
	})
}

// ---- Run in Cloud Shell ----

// cloudShellAction is the Testing tab's Run in Cloud Shell: the test
// command, run in the console's terminal. The terminal is a pod in the
// instance's cluster, where the function's URL names no host, so the
// command reaches the URL's host through the cluster's ingress gateway
// (curl --connect-to), which serves every function's URL.
func cloudShellAction(command string, gen1 bool) console.Action {
	if !gen1 {
		command = strings.Replace(command, "curl -m 70", "curl -m 70 --connect-to ::"+components.IngressService+"."+
			components.IngressNamespace+".svc.cluster.local:80", 1)
	}
	return console.Action{ID: functionsCloudShellAction, Label: "Run in Cloud Shell", Terminal: command}
}

// testCommand is the Testing tab's command, without its heading.
func testCommand(text string) string {
	_, cmd, ok := strings.Cut(text, ":\n\n")
	if !ok {
		return ""
	}
	cmd, _, _ = strings.Cut(cmd, "\n\n")
	return cmd
}

// ---- Write a function: the placeholder revision ----

// writeFunctionPaneFields are the Eventarc pane's fields behind Add
// trigger: each prefixed, and shown only while Add trigger is checked.
func writeFunctionPaneFields(pane []console.Field) []console.Field {
	out := []console.Field{{Name: writeFunctionAddTrigger, Label: "Add trigger", Type: "checkbox", Section: "Trigger",
		Help: "Optional. An Eventarc trigger that delivers events to the function, created with the service. " +
			"Without one the function is reached at its URL (HTTP)."}}
	cond := regexp.MustCompile(`(^|&)([A-Za-z0-9_.\-]+)`)
	for _, f := range pane {
		f.Name = writeFunctionTrigger + f.Name
		f.Section = "Trigger"
		show := writeFunctionAddTrigger
		if f.ShowWhen != "" {
			show += "&" + cond.ReplaceAllString(f.ShowWhen, "${1}"+writeFunctionTrigger+"${2}")
		}
		f.ShowWhen = show
		out = append(out, f)
	}
	return out
}

// writeFunctionRunFields are the Cloud Run settings Write a function adds
// to the function's own: Container(s)' execution environment and Cloud SQL
// connections, Volumes' in-memory, Cloud Storage and NFS volumes, and
// Networking's HTTP/2, VPC and session affinity, as Google's tabs hold them.
func writeFunctionRunFields() []console.Field {
	tabs := map[string]string{"executionEnvironment": runTabContainers, "cloudSqlInstances": runTabContainers,
		"memoryVolumes": runTabVolumes, "gcsVolumes": runTabVolumes, "nfsVolumes": runTabVolumes,
		"useHttp2": runTabNetworking, "vpcConnector": runTabNetworking, "vpcEgress": runTabNetworking, "sessionAffinity": runTabNetworking}
	var out []console.Field
	add := func(f console.Field) {
		if t, ok := tabs[f.Name]; ok {
			f.Section, f.Tab = runSectionMore, t
			out = append(out, f)
		}
	}
	for _, f := range runSettingsFields("", true) {
		add(f)
	}
	for _, f := range runVolumeFields("") {
		add(f)
	}
	add(console.Field{Name: "useHttp2", Label: "Use HTTP/2 end-to-end", Type: "checkbox",
		Help: "Names the container port h2c, as gcloud's --use-http2 does: requests reach the function over HTTP/2 cleartext."})
	return out
}

// placeholderWriteFields turn round 3's Write a function into Google's:
// no Environment (a function written in Cloud Run is a Cloud Run
// service), Add trigger with the Eventarc pane in place of the trigger
// type, and the Cloud Run settings in their tabs.
func placeholderWriteFields(fields, pane []console.Field) []console.Field {
	var out []console.Field
	trigger := false
	for _, f := range fields {
		switch {
		case f.Name == "environment", f.Name == "workerPool", f.Name == "kmsKey":
			continue
		case f.Section == "Trigger":
			if !trigger {
				out = append(out, writeFunctionPaneFields(pane)...)
				trigger = true
			}
			continue
		}
		out = append(out, f)
	}
	// The Run settings join the function's in Google's tab order.
	run := writeFunctionRunFields()
	first := slices.IndexFunc(out, func(f console.Field) bool { return f.Section == runSectionMore })
	if first < 0 {
		return append(out, run...)
	}
	last := first
	for last < len(out) && out[last].Section == runSectionMore {
		last++
	}
	more := append(slices.Clone(out[first:last]), run...)
	slices.SortStableFunc(more, func(x, y console.Field) int { return functionTabOrder(x) - functionTabOrder(y) })
	return append(append(append([]console.Field{}, out[:first]...), more...), out[last:]...)
}

// placeholderService is the service Write a function creates: the
// function as its deploy would render it, running Google's placeholder,
// with the form's Cloud Run settings.
func placeholderService(fn *functionspb.Function, v map[string]string) (*runpb.Service, error) {
	svc := runadapter.FunctionService(fn, runadapter.PlaceholderImage)
	t := svc.GetTemplate()
	c := t.GetContainers()[0]
	// The function's secret volumes, as Cloud Run's own: its deploys keep
	// them on the service.
	for _, vol := range t.GetVolumes() {
		if vol.GetSecret() != nil {
			old := vol.Name
			vol.Name = placeholderSecretVolume + strings.TrimPrefix(old, "secret-volume-")
			for _, m := range c.GetVolumeMounts() {
				if m.GetName() == old {
					m.Name = vol.Name
				}
			}
		}
	}
	if err := applyRunSettings(runSettings{execEnv: &t.ExecutionEnvironment, vpc: &t.VpcAccess, encryptionKey: &t.EncryptionKey,
		sessionAffinity: &t.SessionAffinity}, v); err != nil {
		return nil, err
	}
	vols := map[string]string{}
	for _, k := range []string{"memoryVolumes", "cloudSqlInstances", "nfsVolumes", "gcsVolumes"} {
		if s, ok := v[k]; ok {
			vols[k] = s
		}
	}
	if err := applyRunVolumes(podFields{serviceAccount: &t.ServiceAccount, volumes: &t.Volumes}, c, vols); err != nil {
		return nil, err
	}
	if v["useHttp2"] == "true" {
		c.Ports = []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 8080}}
	}
	svc.BuildConfig.ImageUri = "" // nothing is built yet
	return svc, nil
}

// writeFunctionPlaceholder is Write a function's Create.
func (p runProvider) writeFunctionPlaceholder(ctx context.Context, project string, v map[string]string) error {
	if p.functions == nil || p.runEndpoint() == "" {
		return errors.New("Write a function needs Cloud Run and Cloud Functions, which this instance does not serve both of")
	}
	if project == "" {
		project = p.defaultProject
	}
	id := strings.TrimSpace(v["name"])
	if !regexp.MustCompile(runServiceNamePattern).MatchString(id) {
		return fmt.Errorf("service name %q: a letter first, at most 49 lower case letters, numbers or hyphens, not ending in a hyphen", id)
	}
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "us-central1"
	}
	runtime, entry := strings.TrimSpace(v["runtime"]), strings.TrimSpace(v["entryPoint"])
	if runtime == "" || entry == "" {
		return errors.New("the runtime and the function entry point are required")
	}
	data, err := zipSourceFiles(v[runSourceFilesField])
	if err != nil {
		return err
	}
	public, err := functionPublicChoice(v["authentication"])
	if err != nil {
		return err
	}
	settings, err := functionSettingsOf(v)
	if err != nil {
		return err
	}
	secrets, err := functionSecretsFrom(v)
	if err != nil {
		return err
	}
	memory := v["memory"]
	if memory == "" {
		memory = "256M"
	}
	if mb, err := functionMemoryMB(memory); err != nil || mb <= 0 {
		return fmt.Errorf("memory %q: expected a size such as 256M or 1G", memory)
	}
	trigger := map[string]string{}
	for k, val := range v {
		if rest, ok := strings.CutPrefix(k, writeFunctionTrigger); ok {
			trigger[rest] = val
		}
	}
	parent := "projects/" + project + "/locations/" + region
	_, ss, err := p.functions.uploadArchive(ctx, false, parent, data)
	if err != nil {
		return fmt.Errorf("upload the source: %w", err)
	}
	sc := &functionspb.ServiceConfig{AvailableMemory: memory, BinaryAuthorizationPolicy: strings.TrimSpace(v["binaryAuthorization"])}
	secrets.v2(sc)
	settings.v2(sc)
	fn := &functionspb.Function{Name: parent + "/functions/" + id, ServiceConfig: sc,
		BuildConfig: &functionspb.BuildConfig{Runtime: runtime, EntryPoint: entry,
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}}
	svc, err := placeholderService(fn, v)
	if err != nil {
		return err
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: svc})
	if err != nil {
		return err
	}
	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("the placeholder revision did not deploy: %w", err)
	}
	if public {
		name := parent + "/services/" + id
		pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err == nil {
			_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: grantAllUsers(pol, runInvokerRole)})
		}
		if err != nil {
			return fmt.Errorf("the service was created; Allow public access failed: %w", err)
		}
	}
	if v[writeFunctionAddTrigger] == "true" {
		if err := p.functions.createEventarcTrigger(ctx, project, region, id, trigger); err != nil {
			return fmt.Errorf("the service was created; Add trigger failed: %w", err)
		}
	}
	return nil
}

// grantAllUsers adds allUsers to role in pol.
func grantAllUsers(pol *iampb.Policy, role string) *iampb.Policy {
	if pol == nil {
		pol = &iampb.Policy{}
	}
	for _, b := range pol.GetBindings() {
		if b.GetRole() == role && b.GetCondition() == nil {
			if !slices.Contains(b.GetMembers(), "allUsers") {
				b.Members = append(b.Members, "allUsers")
			}
			return pol
		}
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{"allUsers"}})
	return pol
}

// ---- The service's Source tab ----

// runFunctionOf is the function a function's service runs, nil while the
// service is a placeholder (or not a function's).
func (p runProvider) runFunctionOf(project, id string) *functionspb.Function {
	if p.functions == nil {
		return nil
	}
	fn, err := p.functions.find(project, id)
	if err != nil {
		return nil
	}
	return fn
}

// runFunctionSourceSection is the Source tab of a function's service: the
// function's own (files, Edit source, Base image) once its source is
// deployed, and the placeholder's pending source before.
func (p runProvider) runFunctionSourceSection(ctx context.Context, project string, svc *ksvcStatus) (console.Section, bool) {
	if !runIsFunction(*svc) || p.functions == nil {
		return console.Section{}, false
	}
	if project == "" {
		project = p.defaultProject
	}
	id := svc.Metadata.Name
	if fn := p.runFunctionOf(project, id); fn != nil {
		bc := fn.GetBuildConfig()
		sec := p.functions.sourceSection(ctx, fn, bc.GetRuntime(), bc.GetEntryPoint())
		var acts []console.Action
		for _, a := range sec.Actions {
			if a.ID == functionsEditSourceAction || a.ID == functionsEditRuntimeAction {
				acts = append(acts, a)
			}
		}
		sec.Actions = acts
		return sec, true
	}
	sec := console.Section{ID: "source", Label: "Source"}
	rs, err := p.runService(ctx, project, id)
	if err != nil {
		sec.Kind, sec.Unavailable = console.KindText, "cannot read the service: "+err.Error()
		return sec, true
	}
	bc := rs.GetBuildConfig()
	runtime := runadapter.RuntimeOfBaseImage(bc.GetBaseImage())
	sec.Note = "This service runs a placeholder revision: Save and redeploy builds the function's source and deploys it. " +
		"Runtime: " + orDash(runtime) + ". Function entry point: " + orDash(bc.GetFunctionTarget()) + ". Base image: " + orDash(bc.GetBaseImage()) + "."
	files := map[string]string{}
	if data, err := p.readSourceLocation(ctx, bc.GetSourceLocation()); err != nil {
		sec.Note += " The source cannot be read: " + err.Error()
	} else if files, _, err = archiveFiles(data); err != nil {
		sec.Note += " " + err.Error()
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for i, n := range names {
		sec.Tabs = append(sec.Tabs, console.Section{ID: fmt.Sprintf("file-%d", i), Label: n, Kind: console.KindText, Text: files[n]})
	}
	if len(names) == 0 {
		sec.Kind, sec.Text = console.KindText, "The source holds no text files."
	}
	a, _ := editSourceAction(&functionspb.Function{}, runtime, bc.GetFunctionTarget(), files)
	sec.Actions = []console.Action{a}
	return sec, true
}

// runService reads a service through the Run API.
func (p runProvider) runService(ctx context.Context, project, id string) (*runpb.Service, error) {
	c, err := p.servicesClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.GetService(ctx, &runpb.GetServiceRequest{Name: "projects/" + project + "/locations/" + p.location() + "/services/" + id})
}

// readSourceLocation reads gs://bucket/object from this instance's Cloud
// Storage, through its JSON API.
func (p runProvider) readSourceLocation(ctx context.Context, loc string) ([]byte, error) {
	rest, ok := strings.CutPrefix(loc, "gs://")
	bucket, object, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || object == "" {
		return nil, fmt.Errorf("the service names no source (%q)", loc)
	}
	object, _, _ = strings.Cut(object, "#")
	if p.storageAddr == "" {
		return nil, errors.New("Cloud Storage does not run on this instance")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.storageAddr+"/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+
		url.PathEscape(object)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: time.Minute}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, functionSourceLimit+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return data, nil
}

// actOnFunctionSource performs a function service's Source tab actions:
// the function's own once deployed, and the placeholder's Save and
// redeploy before.
func (p runProvider) actOnFunctionSource(ctx context.Context, project, id, action string, v map[string]string) error {
	if project == "" {
		project = p.defaultProject
	}
	if fn := p.runFunctionOf(project, id); fn != nil {
		return p.functions.ActAt(ctx, project, []string{lastSegment(fn.GetName())}, action, v)
	}
	if action != functionsEditSourceAction {
		return fmt.Errorf("service %s has no function deployed yet: Save and redeploy its source first", id)
	}
	return p.deployPlaceholder(ctx, project, id, v)
}

// deployPlaceholder is the placeholder's Save and redeploy: the files
// uploaded, then CreateFunction through the v2 API with the service's
// settings, which builds the source and deploys it onto the service.
func (p runProvider) deployPlaceholder(ctx context.Context, project, id string, v map[string]string) error {
	svc, err := p.runService(ctx, project, id)
	if err != nil {
		return err
	}
	runtime, entry := strings.TrimSpace(v["runtime"]), strings.TrimSpace(v["entryPoint"])
	if runtime == "" || entry == "" {
		return errors.New("the runtime and the function entry point are required")
	}
	data, err := zipSourceFiles(v[runSourceFilesField])
	if err != nil {
		return err
	}
	parent := strings.TrimSuffix(svc.GetName(), "/services/"+id)
	_, ss, err := p.functions.uploadArchive(ctx, false, parent, data)
	if err != nil {
		return fmt.Errorf("upload the source: %w", err)
	}
	bc := &functionspb.BuildConfig{Runtime: runtime, EntryPoint: entry, EnvironmentVariables: svc.GetBuildConfig().GetEnvironmentVariables(),
		Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}
	if svc.GetBuildConfig() != nil && !svc.GetBuildConfig().GetEnableAutomaticUpdates() {
		bc.RuntimeUpdatePolicy = &functionspb.BuildConfig_OnDeployUpdatePolicy{OnDeployUpdatePolicy: &functionspb.OnDeployUpdatePolicy{}}
	}
	return p.functions.withClient(ctx, func(c *functions.FunctionClient) error {
		_, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			BuildConfig: bc, ServiceConfig: functionServiceConfigOf(svc), Labels: functionLabelsOf(svc)}})
		return err
	})
}

// functionLabelsOf are the service's labels but the one functions add.
func functionLabelsOf(svc *runpb.Service) map[string]string {
	out := map[string]string{}
	for k, v := range svc.GetLabels() {
		if k != "goog-managed-by" {
			out[k] = v
		}
	}
	return out
}

// functionServiceConfigOf is the v2 ServiceConfig a placeholder's service
// was made from: what FunctionService renders, read back.
func functionServiceConfigOf(svc *runpb.Service) *functionspb.ServiceConfig {
	t := svc.GetTemplate()
	sc := &functionspb.ServiceConfig{ServiceAccountEmail: t.GetServiceAccount(),
		MaxInstanceRequestConcurrency: t.GetMaxInstanceRequestConcurrency(),
		MinInstanceCount:              t.GetScaling().GetMinInstanceCount(), MaxInstanceCount: t.GetScaling().GetMaxInstanceCount(),
		IngressSettings: functionspb.ServiceConfig_ALLOW_ALL, AllTrafficOnLatestRevision: true}
	if d := t.GetTimeout(); d != nil {
		sc.TimeoutSeconds = int32(d.GetSeconds())
	}
	switch m := svc.GetBinaryAuthorization().GetBinauthzMethod().(type) {
	case *runpb.BinaryAuthorization_UseDefault:
		sc.BinaryAuthorizationPolicy = "default"
	case *runpb.BinaryAuthorization_Policy:
		sc.BinaryAuthorizationPolicy = m.Policy
	}
	if len(t.GetContainers()) == 0 {
		return sc
	}
	c := t.GetContainers()[0]
	if m := c.GetResources().GetLimits()["memory"]; m != "" {
		sc.AvailableMemory = strings.TrimSuffix(m, "i")
	}
	sc.AvailableCpu = c.GetResources().GetLimits()["cpu"]
	project := strings.Split(svc.GetName(), "/")[1]
	for _, e := range c.GetEnv() {
		if ref := e.GetValueSource().GetSecretKeyRef(); ref != nil {
			secret := ref.GetSecret()
			pid := project
			if parts := strings.Split(secret, "/"); len(parts) == 4 && parts[0] == "projects" {
				pid, secret = parts[1], parts[3]
			}
			sc.SecretEnvironmentVariables = append(sc.SecretEnvironmentVariables, &functionspb.SecretEnvVar{Key: e.GetName(),
				ProjectId: pid, Secret: secret, Version: ref.GetVersion()})
			continue
		}
		if sc.EnvironmentVariables == nil {
			sc.EnvironmentVariables = map[string]string{}
		}
		sc.EnvironmentVariables[e.GetName()] = e.GetValue()
	}
	return sc
}
