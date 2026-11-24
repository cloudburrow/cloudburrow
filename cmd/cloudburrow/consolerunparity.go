package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"gopkg.in/yaml.v3"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Run's console pages as Google's are (#1852): Deploy container opens
// the Create service page, whose fields are Google's sections (Container
// image URL, Service name and Region; Authentication; Billing; Service
// scaling; Ingress) and then "Containers, Volumes, Networking, Security",
// whose fields are on Google's tabs. Every field the adapter maps is still
// on the page; only its place changed.

// Google's sections and the tabs of its last one.
const (
	runSectionConfigure = "Configure"
	runSectionAuth      = "Authentication"
	runSectionBilling   = "Billing"
	runSectionIngress   = "Ingress"
	runSectionMore      = "Containers, Volumes, Networking, Security"

	runTabContainers = "Container(s)"
	runTabVariables  = "Variables & Secrets"
	runTabVolumes    = "Volumes"
	runTabNetworking = "Networking"
	runTabSecurity   = "Security"

	// The Authentication choices, in Google's words.
	runAllowPublic = "Allow public access"
	runRequireAuth = "Require authentication"
	// The one Billing and the one Ingress choice the adapter maps.
	runBillingRequest = "Request-based"
	runIngressAll     = "All"
)

// runFieldTabs is the tab each container-level field is on, in the order
// the tab lists them.
var runFieldTabs = []struct{ name, tab string }{
	{"port", runTabContainers}, {"useHttp2", runTabContainers}, {"command", runTabContainers},
	{"args", runTabContainers}, {"cpu", runTabContainers}, {"memory", runTabContainers},
	{"concurrency", runTabContainers}, {"timeout", runTabContainers},
	{"minInstances", runTabContainers}, {"maxInstances", runTabContainers},
	{"cpuUtilization", runTabContainers}, {"concurrencyUtilization", runTabContainers},
	{"executionEnvironment", runTabContainers}, {"cloudSqlInstances", runTabContainers},
	{"labels", runTabContainers},
	{"env", runTabVariables}, {"secretEnv", runTabVariables},
	{"secretFiles", runTabVolumes}, {"memoryVolumes", runTabVolumes},
	{"nfsVolumes", runTabVolumes}, {"gcsVolumes", runTabVolumes},
	{"vpcMode", runTabNetworking}, {"vpcNetwork", runTabNetworking}, {"vpcSubnet", runTabNetworking}, {"vpcTags", runTabNetworking},
	{"vpcConnector", runTabNetworking}, {"vpcEgress", runTabNetworking}, {"sessionAffinity", runTabNetworking},
	{"serviceAccount", runTabSecurity}, {"encryptionKey", runTabSecurity},
}

// runCreateOnly are the Create service fields that are the service's own
// choice at creation, not a revision's: Edit and deploy new revision leaves
// them out, as Google's does (Authentication is the Security tab's).
var runCreateOnly = []string{"region", "authentication", "billing", "ingress"}

// runCreateLayout orders the deploy form's fields as Google's Create service
// page does and adds its Region, Authentication, Billing and Ingress.
func runCreateLayout(region string, fields []console.Field) []console.Field {
	byName := map[string]console.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	var out []console.Field
	take := func(name, section, tab string) {
		f, ok := byName[name]
		if !ok {
			return
		}
		delete(byName, name)
		f.Section, f.Tab = section, tab
		out = append(out, f)
	}
	take("image", runSectionConfigure, "")
	take("name", runSectionConfigure, "")
	out = append(out,
		console.Field{Name: "region", Label: "Region", Type: "select", Options: []string{region}, Default: region,
			Section: runSectionConfigure,
			Help:    "This instance serves one region, " + region + ", as its Cloud Run location."},
		console.Field{Name: "authentication", Label: "Authentication", Type: "radio",
			Options: []string{runAllowPublic, runRequireAuth}, Default: runRequireAuth, Section: runSectionAuth,
			Help: "Allow public access grants allUsers the Cloud Run Invoker role (roles/run.invoker) on the service. " +
				"Invoker IAM is stored, not enforced, locally: every service is reachable from the host and the cluster."},
		console.Field{Name: "billing", Label: "Billing", Type: "radio", Options: []string{runBillingRequest},
			Default: runBillingRequest, Section: runSectionBilling,
			Help: "Request-based is Cloud Run's default. Instance-based billing is not offered: the adapter does not map it, " +
				"and nothing is billed locally."},
	)
	for _, f := range runServiceScalingFields(nil) {
		take(f.Name, "Service scaling", "")
	}
	out = append(out, console.Field{Name: "ingress", Label: "Ingress", Type: "radio", Options: []string{runIngressAll},
		Default: runIngressAll, Section: runSectionIngress,
		Help: "All: allow direct access to the service from the internet. Internal is not offered: only " +
			"INGRESS_TRAFFIC_ALL is mapped, and every service is reachable from the host and the cluster."})
	for _, t := range runFieldTabs {
		take(t.name, runSectionMore, t.tab)
	}
	// Anything not placed above keeps the order it came in, on the first tab,
	// so a field added to the form later is never dropped from the page.
	for _, f := range fields {
		if _, left := byName[f.Name]; left {
			f.Section, f.Tab = runSectionMore, runTabContainers
			out = append(out, f)
		}
	}
	return out
}

// CreateTitle implements console.CreateTitler: Deploy container opens the
// Create service page.
func (runProvider) CreateTitle() string { return "Create service" }

// CreateSubmit implements console.CreateSubmitter: the page ends in Create.
func (runProvider) CreateSubmit() string { return "Create" }

// withoutCreateOnly drops the fields only Create service has.
func withoutCreateOnly(fields []console.Field) []console.Field {
	return slices.DeleteFunc(fields, func(f console.Field) bool { return slices.Contains(runCreateOnly, f.Name) })
}

// runCreateChoices refuses a Region, Billing or Ingress the adapter does not
// map, and reports whether Authentication asks for public access.
func (p runProvider) runCreateChoices(values map[string]string) (public bool, err error) {
	if r := strings.TrimSpace(values["region"]); r != "" && r != p.location() {
		return false, fmt.Errorf("region %q is not served: this instance's Cloud Run location is %s", r, p.location())
	}
	if b := strings.TrimSpace(values["billing"]); b != "" && b != runBillingRequest {
		return false, fmt.Errorf("billing %q is not offered locally: only %s is mapped", b, runBillingRequest)
	}
	if i := strings.TrimSpace(values["ingress"]); i != "" && i != runIngressAll {
		return false, fmt.Errorf("ingress %q is not offered locally: only INGRESS_TRAFFIC_ALL is mapped", i)
	}
	switch a := strings.TrimSpace(values["authentication"]); a {
	case "", runRequireAuth:
		return false, nil
	case runAllowPublic:
		return true, nil
	default:
		return false, fmt.Errorf("authentication %q is neither %q nor %q", a, runAllowPublic, runRequireAuth)
	}
}

// allowPublicAccess grants allUsers roles/run.invoker on a service, through
// the service's IAM policy, as Google's Allow public access does.
func (p runProvider) allowPublicAccess(ctx context.Context, project, id string) error {
	pol, err := p.GetPolicy(ctx, project, []string{id})
	if err != nil {
		return err
	}
	for i, b := range pol.Bindings {
		if b.Role == runInvokerRole && b.Condition == "" {
			if !slices.Contains(b.Members, "allUsers") {
				pol.Bindings[i].Members = append(pol.Bindings[i].Members, "allUsers")
			}
			return p.SetPolicy(ctx, project, []string{id}, pol)
		}
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: runInvokerRole, Members: []string{"allUsers"}})
	return p.SetPolicy(ctx, project, []string{id}, pol)
}

const runInvokerRole = "roles/run.invoker"

// runAuthentication is the Services list's and the Security tab's
// Authentication: Allow public access when allUsers holds the invoker role.
func (p runProvider) runAuthentication(ctx context.Context, project, id string) string {
	pol, err := p.GetPolicy(ctx, project, []string{id})
	if err != nil {
		return ""
	}
	for _, b := range pol.Bindings {
		if b.Role == runInvokerRole && b.Condition == "" && slices.Contains(b.Members, "allUsers") {
			return runAllowPublic
		}
	}
	return runRequireAuth
}

// runServiceColumns are Google's Services list columns (#1852), Req/sec and
// Recommendation aside (no Cloud Monitoring or Recommender locally), then
// CloudBurrow's own: the URL, image, serving revision and a deploy in flight.
var runServiceColumns = append([]string{"Deployment type", "Region", "Authentication", "Ingress", "Last deployed", "Deployed by"},
	runColumns...)

// runIsFunction reports whether a service was deployed as a Cloud Run
// function, through the Cloud Functions v2 API, which labels it.
func runIsFunction(s ksvcStatus) bool {
	return runServiceLabels(s)["goog-managed-by"] == "cloudfunctions"
}

func runServiceLabels(s ksvcStatus) map[string]string {
	labels := map[string]string{}
	if raw := s.Metadata.Annotations[runadapter.ServiceLabelsAnnotation]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &labels)
	}
	return labels
}

// runGoogleFields are a service row's Google columns. lastDeployed is when
// its latest revision was created.
func (p runProvider) runGoogleFields(ctx context.Context, project string, s ksvcStatus, lastDeployed string) map[string]string {
	kind, by := "Container", ""
	if runIsFunction(s) {
		kind = "Function"
	}
	return map[string]string{
		"Deployment type": kind, "Region": p.location(), "Authentication": p.runAuthentication(ctx, project, s.Metadata.Name),
		"Ingress": runIngressAll, "Last deployed": orDash(lastDeployed), "Deployed by": orDash(by),
	}
}

// runLatestCreated is, per service ID, when its latest revision was created.
func (p runProvider) runLatestCreated(ctx context.Context, svcs []ksvcStatus) map[string]string {
	out := map[string]string{}
	raw, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "revisions")
	if err != nil {
		return out
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return out
	}
	for _, item := range list.Items {
		m := meta(item)
		labels, _ := m["labels"].(map[string]any)
		for _, s := range svcs {
			if s.ownsRevision(labels) && s.revisionID(str(m, "name")) == s.Status.LatestCreatedRevisionName {
				out[s.Metadata.Name] = str(m, "creationTimestamp")
			}
		}
	}
	return out
}

// runNotLocal is a tab Google's service page has whose data comes from a
// service CloudBurrow does not run.
func runNotLocal(id, label, why string) console.Section {
	return console.Section{ID: id, Label: label, Kind: console.KindText,
		Text: "Not available locally. " + why}
}

// runServiceSections are Google's service page tabs (#1852): Metrics, SLOs,
// Logs, Revisions, Source, Triggers, Networking, Security, YAML; then
// CloudBurrow's Configuration and Domain mappings tabs.
func (p runProvider) runServiceSections(ctx context.Context, project, name string, svc *ksvcStatus) []console.Section {
	listing := p.revisions(ctx, name, svc)
	var ids []string
	for _, r := range listing.Items {
		ids = append(ids, r.Name)
	}
	revisions := console.Section{ID: "revisions", Label: "Revisions", Listing: listing,
		Actions: p.trafficAction(svc, ids)}
	runRevisionTraffic(&revisions.Listing, svc)
	domains := p.domainsSection(ctx, project, serviceIDOf(name))
	domains.Actions = p.domainActions(svc)
	// Containers and Scaling, and the Networking and Security tabs' forms,
	// each ending in View diff & redeploy (#1852).
	networking, security := p.runNetworkingSection(svc), p.runSecuritySection(ctx, project, svc)
	containers, scaling := runEditTabs(p.editForm(ctx, name, svc), &networking, &security)
	return []console.Section{
		runMetricsSection(ctx, project, p.location(), serviceIDOf(name)),
		runNotLocal("slos", "SLOs", "Service level objectives are Cloud Monitoring's service monitoring, "+
			"which CloudBurrow does not run."),
		{ID: "logs", Label: "Logs", Kind: console.KindLogs},
		revisions,
		p.runSourceTab(ctx, project, svc),
		p.triggersSection(ctx, project, svc),
		containers,
		networking,
		security,
		scaling,
		p.runYAMLSection(ctx, project, svc),
		runConfiguration(svc, p.defaultProject),
		domains,
	}
}

// runRevisionTraffic adds each revision's traffic share and tags, which
// Google's Revisions tab shows, to the Revisions listing.
func runRevisionTraffic(l *console.Listing, svc *ksvcStatus) {
	if l.Unavailable != "" {
		return
	}
	l.Columns = append([]string{"Traffic", "Tags"}, l.Columns...)
	for i := range l.Items {
		share, tags := 0, []string{}
		for _, t := range svc.Status.Traffic {
			rev := t.RevisionName
			if rev == "" {
				rev = svc.Status.LatestReadyRevisionName
			}
			if rev != l.Items[i].Name {
				continue
			}
			share += t.Percent
			if t.Tag != "" {
				tag := t.Tag
				if t.URL != "" {
					tag += " (" + t.URL + ")"
				}
				tags = append(tags, tag)
			}
		}
		if l.Items[i].Fields == nil {
			l.Items[i].Fields = map[string]string{}
		}
		l.Items[i].Fields["Traffic"] = fmt.Sprintf("%d%%", share)
		l.Items[i].Fields["Tags"] = orDash(strings.Join(tags, ", "))
	}
}

// runSourceTab is the Source tab: a function's (#2049), or a container's.
func (p runProvider) runSourceTab(ctx context.Context, project string, svc *ksvcStatus) console.Section {
	if sec, ok := p.runFunctionSourceSection(ctx, project, svc); ok {
		return sec
	}
	return p.runSourceSection(svc)
}

// runSourceSection is the Source tab: what the service runs, and Deploy
// from source.
func (p runProvider) runSourceSection(svc *ksvcStatus) console.Section {
	props := []console.Property{{Label: "Container image URL", Value: orDash(svc.image())}}
	return console.Section{ID: "source", Label: "Source", Kind: console.KindProperties,
		Groups:  []console.PropertyGroup{{Properties: props}},
		Actions: p.sourceActions(),
		Note: "A service deployed from an image has no source to show. Deploy from source builds an archive " +
			"into an image, and deploys it as a new revision."}
}

// runNetworkingSection is the Networking tab.
func (p runProvider) runNetworkingSection(svc *ksvcStatus) console.Section {
	pod := runadapter.ReadPodConfig(svc.Spec.Template.Metadata.Annotations, 1)
	props := []console.Property{
		{Label: "Ingress", Value: runIngressAll},
		{Label: "Default URL", Value: unsetAs(svc.Status.URL, runNoURL)},
		{Label: "VPC", Value: runVPCMode(pod.GetVpcAccess())},
		{Label: "VPC connector", Value: orDash(pod.GetVpcAccess().GetConnector())},
		{Label: "Network", Value: orDash(runVPCInterface(pod.GetVpcAccess()).GetNetwork())},
		{Label: "Subnet", Value: orDash(runVPCInterface(pod.GetVpcAccess()).GetSubnetwork())},
		{Label: "Network tags", Value: orDash(strings.Join(runVPCInterface(pod.GetVpcAccess()).GetTags(), ", "))},
		{Label: "Traffic routing", Value: runVPCRouting(pod.GetVpcAccess())},
		{Label: "Session affinity", Value: yesNo(pod.GetSessionAffinity())},
	}
	return console.Section{ID: "networking", Label: "Networking", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: props}},
		Note: "Only INGRESS_TRAFFIC_ALL is mapped: every service is reachable from the host and the cluster. " +
			"A VPC connection (a Serverless VPC Access connector, or Direct VPC egress to a network and subnet) is checked against this instance's " +
			"connectors and networks and recorded; no traffic is routed through it (docs/gaps.md). Change it here, then View diff & redeploy."}
}

// runVPCMode is how a revision connects to a VPC for outbound traffic, in
// Google's words (#2140).
func runVPCMode(v *runpb.VpcAccess) string {
	switch {
	case v.GetConnector() != "":
		return "Serverless VPC Access connector"
	case len(v.GetNetworkInterfaces()) > 0:
		return "Direct VPC egress"
	}
	return "None"
}

func runVPCInterface(v *runpb.VpcAccess) *runpb.VpcAccess_NetworkInterface {
	if l := v.GetNetworkInterfaces(); len(l) > 0 {
		return l[0]
	}
	return nil
}

// runVPCRouting is the Traffic routing choice: requests to private IPs
// only unless ALL_TRAFFIC, Cloud Run's default.
func runVPCRouting(v *runpb.VpcAccess) string {
	switch {
	case v == nil:
		return "-"
	case v.GetEgress() == runpb.VpcAccess_ALL_TRAFFIC:
		return "Route all traffic to the VPC"
	}
	return "Route only requests to private IPs to the VPC"
}

// runSecuritySection is the Security tab.
func (p runProvider) runSecuritySection(ctx context.Context, project string, svc *ksvcStatus) console.Section {
	pod := runadapter.ReadPodConfig(svc.Spec.Template.Metadata.Annotations, 1)
	props := []console.Property{
		{Label: "Authentication", Value: orDash(p.runAuthentication(ctx, project, svc.Metadata.Name))},
		{Label: "Service account", Value: orDash(runRevisionAccount(svc.Spec.Template.Metadata.Annotations, p.defaultProject))},
		{Label: "Encryption key", Value: orDash(pod.GetEncryptionKey())},
	}
	return console.Section{ID: "security", Label: "Security", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: props}},
		Note: "Authentication is the service's IAM policy (allUsers holding roles/run.invoker is public access), " +
			"changed on the Permissions tab; it is stored, not enforced. The service account and encryption key " +
			"are the revision's, changed here, then View diff & redeploy."}
}

// runYAMLSection is the YAML tab: the service as the Cloud Run Admin API v1
// answers for it, the document Google's YAML tab shows.
func (p runProvider) runYAMLSection(ctx context.Context, project string, svc *ksvcStatus) console.Section {
	sec := console.Section{ID: "yaml", Label: "YAML", Kind: console.KindText}
	c, err := p.runV1(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec
	}
	got, err := c.Namespaces.Services.Get("namespaces/" + p.projectOr(project) + "/services/" + svc.Metadata.Name).Context(ctx).Do()
	if err != nil {
		sec.Unavailable = "cannot read the service through the v1 API: " + err.Error()
		return sec
	}
	return runYAMLText(sec, got)
}

// runJobYAML is a job's YAML tab, as the v1 API answers for it.
func (p runJobsProvider) runJobYAML(ctx context.Context, project, id string) console.Section {
	sec := console.Section{ID: "yaml", Label: "YAML", Kind: console.KindText}
	c, err := runProvider{runAddr: p.runAddr, defaultProject: p.defaultProject}.runV1(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec
	}
	got, err := c.Namespaces.Jobs.Get("namespaces/" + p.project(project) + "/jobs/" + id).Context(ctx).Do()
	if err != nil {
		sec.Unavailable = "cannot read the job through the v1 API: " + err.Error()
		return sec
	}
	return runYAMLText(sec, got)
}

// runYAMLText renders a v1 resource as YAML into a text section.
func runYAMLText(sec console.Section, got json.Marshaler) console.Section {
	raw, err := got.MarshalJSON()
	if err == nil {
		var doc any
		if err = yaml.Unmarshal(raw, &doc); err == nil {
			var out []byte
			if out, err = yaml.Marshal(doc); err == nil {
				sec.Text = string(out)
			}
		}
	}
	if err != nil {
		sec.Unavailable = "cannot render the service as YAML: " + err.Error()
	}
	return sec
}

// runJobEditLabel is Google's: a job is edited with View and edit job
// configuration (#1852).
const runJobEditLabel = "View and edit job configuration"

// runJobLayout orders Create job's fields as Google's page does: Container
// image URL, Job name, Region and Number of tasks, then the container, Task
// capacity and Parallelism, ending in Execute job immediately (#1852).
func runJobLayout(region string, fields []console.Field) []console.Field {
	var image, name, tasks console.Field
	rest := make([]console.Field, 0, len(fields))
	for _, f := range fields {
		switch f.Name {
		case "image":
			image = f
		case "name":
			name = f
		case "taskCount":
			tasks = f
		default:
			rest = append(rest, f)
		}
	}
	image.Section, name.Section, tasks.Section = "Configure", "Configure", "Configure"
	for i := range rest {
		if rest[i].Section == "Job settings" {
			rest[i].Section = "Configure"
		}
	}
	regionField := console.Field{Name: "region", Label: "Region", Type: "select", Options: []string{region}, Default: region,
		Section: "Configure", Help: "This instance serves one region, " + region + ", as its Cloud Run location."}
	out := append([]console.Field{image, name, regionField, tasks}, rest...)
	return append(out, console.Field{Name: "executeNow", Label: "Execute job immediately", Type: "checkbox",
		Help: "Start an execution as soon as the job is created."})
}

// runJobCreateOnly are Create job's fields that View and edit job
// configuration does not have.
var runJobCreateOnly = []string{"region", "executeNow"}

// Google's Parallelism choices and Task timeout units.
const (
	runParallelAll   = "Run as many tasks concurrently as possible"
	runParallelLimit = "Limit the number of concurrent tasks"

	runMaxTaskTimeout = 168 * time.Hour
)

var runTimeUnits = []string{"seconds", "minutes", "hours"}

// runTimeUnit is a Time unit's length; none is seconds, as the API's.
func runTimeUnit(unit string) (time.Duration, error) {
	switch strings.TrimSpace(unit) {
	case "", "seconds":
		return time.Second, nil
	case "minutes":
		return time.Minute, nil
	case "hours":
		return time.Hour, nil
	}
	return 0, fmt.Errorf("time unit %q is not seconds, minutes or hours", unit)
}

// runTimeoutText is a task timeout in the largest unit it is a whole
// number of.
func runTimeoutText(d time.Duration) (value, unit string) {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprint(int64(d / time.Hour)), "hours"
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprint(int64(d / time.Minute)), "minutes"
	}
	return fmt.Sprint(int64(d / time.Second)), "seconds"
}

// CreateSubmit implements console.CreateSubmitter: Create job ends in Create.
func (runJobsProvider) CreateSubmit() string { return "Create" }
